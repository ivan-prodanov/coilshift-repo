package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/session"
	"airgapp/gateway/internal/wire"
)

const (
	preAuthMaxFrame  = 1024 // every inbound frame before ARM
	challengeValidMS = 5000 // challenge expiry, BOOTTIME
)

// preAuthError is a pre-auth refusal. Pre-auth failures close the
// socket with no message (no oracle), so the specific stage is only logged.
type preAuthError string

func (e preAuthError) Error() string { return "preauth: " + string(e) }

// handle runs one connection: pre-auth, then (on success) the window.
func (s *Server) handle(c net.Conn, deadline time.Time) {
	defer s.wg.Done()
	w, err := s.preAuth(c, deadline)
	s.leavePreAuth(c) // before Close, so a peer that sees EOF sees the slot free
	if err != nil {
		c.Close()
		var pe preAuthError
		if errors.As(err, &pe) {
			log.Printf("agw: %v", err)
		}
		return
	}
	w.run()
}

type armMsg struct {
	N      string          `json:"n"`
	Cap    int             `json:"cap"`
	Exp    uint64          `json:"exp"`
	Params json.RawMessage `json:"params"`
}

// preAuth runs hello -> authority (filter) -> challenge -> ARM -> authority
// (gate) -> slot pre-check -> PreArm -> slot -> Open. On success the returned window holds the capability
// slot and has READY queued. Every failure returns an error and the caller
// closes the socket without a message.
func (s *Server) preAuth(c net.Conn, deadline time.Time) (*window, error) {
	if err := c.SetDeadline(deadline); err != nil {
		return nil, err
	}

	// 1. hello
	helloB, err := wire.ReadFrame(c, preAuthMaxFrame)
	if err != nil {
		return nil, err // I/O, timeout or oversize: not worth a log line
	}
	h, err := proto.ParseHello(helloB)
	if err != nil {
		return nil, preAuthError("bad hello")
	}
	ce := s.caps[h.Cap]
	if ce == nil {
		return nil, preAuthError("capability not registered")
	}
	// The authority as a filter, before the challenge: the challenge carries
	// the VIN binding (and, for v2, the gateway's key), so a key authd does not
	// list as OWNER or DRIVER gets no byte from the car, only the silent close.
	// The hello does not prove possession of pkS: the check after the ARM
	// (step 4) is the real gate, and the slot's state is told only after it.
	pkS := h.PkS[:]
	hctx, hcancel := context.WithDeadline(s.ctx, deadline)
	err = authority.Check(hctx, s.cfg.Oracle, pkS)
	hcancel()
	if err != nil {
		return nil, preAuthError("hello: " + err.Error() + " fp=" + keys.Fingerprint(pkS)) // no key material in authority errors
	}

	// 2. challenge: v1, or for a v2 hello v2, which also carries the
	// gateway's long-term public key (signed). The rest of the handshake uses
	// the exact bytes sent (chB): the HPKE info, and N and the expiry, which
	// the two versions share.
	skE, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var ch proto.Challenge
	if _, err := rand.Read(ch.N[:]); err != nil {
		return nil, err
	}
	copy(ch.PkE[:], skE.PublicKey().Bytes())
	ch.ExpiryMS = s.clk.BootMS() + challengeValidMS
	ch.Binding = proto.Binding(s.cfg.VIN)
	var chB []byte
	if h.Version == proto.HelloV2 {
		ch2 := proto.ChallengeV2{N: ch.N, PkE: ch.PkE, ExpiryMS: ch.ExpiryMS, Binding: ch.Binding}
		if err := proto.SignChallengeV2(s.cfg.Key, helloB, &ch2); err != nil {
			return nil, err
		}
		chB = ch2.Bytes()
	} else {
		if err := proto.SignChallenge(s.cfg.Key, helloB, &ch); err != nil {
			return nil, err
		}
		chB = ch.Bytes()
	}
	if err := wire.WriteFrame(c, chB); err != nil {
		return nil, err
	}

	// 3. ARM
	first, err := wire.ReadFrame(c, preAuthMaxFrame)
	if err != nil {
		return nil, err
	}
	in, pt, err := session.OpenArm(skE, pkS, proto.Info(helloB, chB), first)
	if err != nil {
		return nil, preAuthError("ARM does not open")
	}
	if len(pt) == 0 || pt[0] != proto.TypeArm {
		return nil, preAuthError("first frame is not ARM")
	}
	var arm armMsg
	if err := json.Unmarshal(pt[1:], &arm); err != nil {
		return nil, preAuthError("ARM json")
	}
	n, err := hex.DecodeString(arm.N)
	if err != nil || len(n) != len(ch.N) || subtle.ConstantTimeCompare(n, ch.N[:]) != 1 {
		return nil, preAuthError("ARM n mismatch")
	}
	if arm.Cap != int(h.Cap) || arm.Exp != ch.ExpiryMS {
		return nil, preAuthError("ARM cap/exp mismatch")
	}
	if s.clk.BootMS() > ch.ExpiryMS {
		return nil, preAuthError("challenge expired")
	}
	if !s.useNonce(ch.N) {
		return nil, preAuthError("N reused")
	}

	// 4. authority, PreArm, slot, Open
	gen := s.keysGeneration()
	ctx, cancel := context.WithDeadline(s.ctx, deadline)
	err = authority.Check(ctx, s.cfg.Oracle, pkS)
	cancel()
	if err != nil {
		return nil, preAuthError(err.Error() + " fp=" + keys.Fingerprint(pkS)) // authority errors carry no key material
	}
	if s.afterCheckHook != nil {
		s.afterCheckHook()
	}
	// The slot's state only now, and before PreArm: reserve still decides a
	// race with another sign-in.
	if s.slotBusy(h.Cap) {
		return nil, preAuthError(ce.spec.Name + " window already open")
	}
	if _, err := callBounded(s.ctx, preArmBudget, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, ce.cap.PreArm(ctx, pkS)
	}, nil); err != nil {
		return nil, preAuthError(ce.spec.Name + " PreArm refused: " + boundedReason(err))
	}
	out, err := session.NewOutbound(in)
	if err != nil {
		return nil, err
	}
	w := newWindow(s, c, ce, append([]byte(nil), pkS...), in, out)
	if !s.reserve(w, gen) {
		return nil, preAuthError(ce.spec.Name + " slot lost")
	}
	// The slot is held: a slow Open must not keep one of the global
	// pre-auth places. Shutdown now reaches this connection through w.
	s.leavePreAuth(c)
	w.queueReady() // first frame on the wire, ahead of anything Open sends
	name := ce.spec.Name
	hd, err := callBounded(s.ctx, openBudget, func(ctx context.Context) (capability.Handler, error) {
		h, err := ce.cap.Open(ctx, w, arm.Params)
		if err == nil && h == nil {
			err = errors.New("nil Handler")
		}
		return h, err
	}, func(late capability.Handler) {
		// Open returned a Handler after its budget. This may run before
		// abort (inline, or racing it from the timer goroutine), so close
		// the window first: Teardown may wait on Done(), as on every other
		// path. abort's own Close is then a no-op and abort alone frees the
		// slot.
		w.Close(reasonError)
		teardownHandler(name, late, "open overran")
	})
	if err != nil {
		if s.overrunHook != nil && errors.Is(err, errOverrun) {
			s.overrunHook()
		}
		w.abort()
		return nil, preAuthError(name + " Open failed: " + boundedReason(err))
	}
	// From here a Handler exists: never abort, always return w so run()
	// does the ordered teardown (Handler.Teardown, session log). Shutdown
	// may have closed w while Open ran (w sits in s.windows).
	w.h = hd
	w.opened()
	if err := c.SetDeadline(time.Time{}); err != nil {
		w.Close(reasonDrop)
	}
	return w, nil
}

const (
	preArmBudget = time.Second     // Capability.PreArm ctx
	openBudget   = 2 * time.Second // Capability.Open ctx
)

// overrunGrace lets callBounded wait slightly past the ctx deadline, so a
// call that returns as soon as its ctx expires is handled on the caller's
// goroutine (still an overrun) rather than abandoned to a background one.
const overrunGrace = 50 * time.Millisecond

var errOverrun = errors.New("overran its budget")

type boundedPanic struct{ v any }

func (p boundedPanic) Error() string { return fmt.Sprintf("panic: %v", p.v) }

// boundedReason is the local log text for a callBounded failure: the kind
// only (capability error text stays out of the pre-auth log line).
func boundedReason(err error) string {
	var p boundedPanic
	switch {
	case errors.Is(err, errOverrun):
		return "overran its budget"
	case errors.As(err, &p):
		return p.Error()
	}
	return "error"
}

// callBounded runs fn on its own goroutine with a context bounded by budget
// (and cancelled with parent, i.e. by Shutdown). A panic in fn is recovered
// and returned as an error. If fn is still running when the budget ends, or
// returns after its context's deadline, the result is errOverrun; a value fn
// produces late (with a nil error) is handed to late, if set, on whichever
// goroutine sees it.
func callBounded[T any](parent context.Context, budget time.Duration, fn func(context.Context) (T, error), late func(T)) (T, error) {
	type result struct {
		v   T
		err error
	}
	var zero T
	ctx, cancel := context.WithTimeout(parent, budget)
	res := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			if p := recover(); p != nil {
				r = result{err: boundedPanic{p}}
			}
			res <- r
		}()
		r.v, r.err = fn(ctx)
	}()
	lateFn := func(r result) {
		if late != nil && r.err == nil {
			late(r.v)
		}
	}
	timer := time.NewTimer(budget + overrunGrace)
	defer timer.Stop()
	select {
	case r := <-res:
		overran := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if overran {
			lateFn(r)
			return zero, errOverrun
		}
		return r.v, r.err
	case <-timer.C:
		cancel()
		go func() { lateFn(<-res) }()
		return zero, errOverrun
	}
}
