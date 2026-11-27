// Package client is the Go phone-side implementation of the gateway protocol.
// It is the bench client: tests and the camera session's laptop tooling use it
// to talk to a real gateway. It verifies the gateway's pinned key and VIN
// binding before it seals anything.
package client

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/session"
	"airgapp/gateway/internal/wire"
)

const (
	maxChallengeFrame = 1024
	maxS2CFrame       = 1<<20 + 1 + 16 // largest Spec.Out body + type byte + GCM tag (same as the phone)
	handshakeTimeout  = 10 * time.Second
	writeTimeout      = 10 * time.Second
)

// heartbeatEvery is the HEARTBEAT interval (spec: 5 s; lease 20 s).
var heartbeatEvery = 5 * time.Second

// Session is one armed gateway session.
type Session struct {
	conn net.Conn

	wmu sync.Mutex // serialises Seal + write (the HPKE counter is ordered)
	snd *session.TestSealer

	closeOnce sync.Once
	done      chan struct{}
	errMu     sync.Mutex
	err       error

	tmu    sync.Mutex // the newest TICK, taken in by Recv
	tickID [8]byte
	tickAt time.Time // when this client read it (monotonic)
	ticked bool
}

// TickMaxAge is the client-side freshness bound: a caller that echoes the
// newest tick should not echo one read this long ago or longer. It only saves
// sending frames the gateway would drop as stale (F = 150 ms, measured there
// from the tick's issue); the gateway's own check is the one that counts.
const TickMaxAge = 150 * time.Millisecond

const tickBodyLen = 16 // id (8) ‖ issued BOOTTIME ms (u64 BE)

// ParsePin decodes the string printed by `agw pin`
// ("airgapp-gw:1:<hex pubkey>:<vin>").
func ParsePin(pin string) (*ecdsa.PublicKey, string, error) {
	parts := strings.Split(strings.TrimSpace(pin), ":")
	if len(parts) != 4 || parts[0] != "airgapp-gw" || parts[1] != "1" || parts[3] == "" {
		return nil, "", errors.New("client: malformed pin")
	}
	b, err := hex.DecodeString(parts[2])
	if err != nil || len(b) != 65 || b[0] != 4 {
		return nil, "", errors.New("client: malformed pin key")
	}
	if _, err := ecdh.P256().NewPublicKey(b); err != nil {
		return nil, "", errors.New("client: pin key not on curve")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), b)
	if x == nil {
		return nil, "", errors.New("client: pin key not on curve")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, parts[3], nil
}

// Dial connects to the gateway at addr, authenticates it against pinned and
// vin, arms capability cap with params (any JSON-marshalable value, or nil),
// and returns once the gateway answers READY. device is the phone's key.
func Dial(ctx context.Context, addr string, cap byte, params any, pinned *ecdsa.PublicKey, vin string, device *ecdh.PrivateKey) (*Session, error) {
	s, _, err := dial(ctx, addr, proto.HelloV1, cap, params, pinned, vin, device)
	return s, err
}

// DialV2 is Dial with a version 2 hello: the gateway answers with a v2
// challenge that carries its long-term public key, signed. With pinned set,
// that key must equal pinned; with pinned nil, it is trusted on first connect.
// Either way DialV2 returns it, for the caller to pin.
func DialV2(ctx context.Context, addr string, cap byte, params any, pinned *ecdsa.PublicKey, vin string, device *ecdh.PrivateKey) (*Session, *ecdsa.PublicKey, error) {
	return dial(ctx, addr, proto.HelloV2, cap, params, pinned, vin, device)
}

func dial(ctx context.Context, addr string, ver, cap byte, params any, pinned *ecdsa.PublicKey, vin string, device *ecdh.PrivateKey) (*Session, *ecdsa.PublicKey, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	s, gk, err := handshake(ctx, conn, ver, cap, params, pinned, vin, device)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return s, gk, nil
}

// handshake signs in over conn with a hello of version ver. It returns the
// gateway key the challenge verified with: pinned for v1, the key a v2
// challenge carries for v2.
func handshake(ctx context.Context, conn net.Conn, ver, cap byte, params any, pinned *ecdsa.PublicKey, vin string, device *ecdh.PrivateKey) (*Session, *ecdsa.PublicKey, error) {
	dl := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	conn.SetDeadline(dl)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	var h proto.Hello
	h.Version = ver
	h.Cap = cap
	copy(h.PkS[:], device.PublicKey().Bytes())
	hello := h.Bytes()
	if err := wire.WriteFrame(conn, hello); err != nil {
		return nil, nil, err
	}
	chB, err := wire.ReadFrame(conn, maxChallengeFrame)
	if err != nil {
		return nil, nil, fmt.Errorf("client: challenge: %w", err)
	}
	var ch proto.Challenge // the fields both versions share; Sig unused for v2
	gk := pinned
	if ver == proto.HelloV2 {
		ch2, err := proto.ParseChallengeV2(chB)
		if err != nil {
			return nil, nil, err
		}
		if gk = proto.VerifyChallengeV2(hello, ch2); gk == nil {
			return nil, nil, errors.New("client: v2 challenge signature does not match the key it carries")
		}
		if pinned != nil && !pinned.Equal(gk) {
			return nil, nil, errors.New("client: v2 challenge carries a gateway key other than the pinned one")
		}
		ch = proto.Challenge{N: ch2.N, PkE: ch2.PkE, ExpiryMS: ch2.ExpiryMS, Binding: ch2.Binding}
	} else {
		if ch, err = proto.ParseChallenge(chB); err != nil {
			return nil, nil, err
		}
		if !proto.VerifyChallenge(pinned, hello, ch) {
			return nil, nil, errors.New("client: challenge signature does not match the pinned gateway key")
		}
	}
	if ch.Binding != proto.Binding(vin) {
		return nil, nil, errors.New("client: gateway is bound to a different vehicle")
	}
	enc, snd, err := session.TestSender(device, ch.PkE[:], proto.Info(hello, chB))
	if err != nil {
		return nil, nil, err
	}
	if params == nil {
		params = map[string]any{}
	}
	arm, err := json.Marshal(map[string]any{"n": hex.EncodeToString(ch.N[:]), "cap": cap, "exp": ch.ExpiryMS, "params": params})
	if err != nil {
		return nil, nil, err
	}
	first := append(append([]byte(nil), enc...), snd.Seal(append([]byte{proto.TypeArm}, arm...))...)
	if err := wire.WriteFrame(conn, first); err != nil {
		return nil, nil, err
	}

	s := &Session{conn: conn, snd: snd, done: make(chan struct{})}
	t, body, err := s.recv()
	if err != nil {
		return nil, nil, err
	}
	if t != proto.TypeReady {
		return nil, nil, fmt.Errorf("client: expected READY, got type 0x%02x", t)
	}
	var r struct {
		LeaseMS *int `json:"lease_ms"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.LeaseMS == nil {
		return nil, nil, errors.New("client: malformed READY")
	}
	conn.SetDeadline(time.Time{})
	if *r.LeaseMS > 0 {
		go s.heartbeat(heartbeatEvery)
	}
	return s, gk, nil
}

func (s *Session) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if err := s.Send(proto.TypeHeartbeat, nil); err != nil {
				s.fail(err)
				return
			}
		}
	}
}

// Send seals and writes one phone->car message.
func (s *Session) Send(t byte, body []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return s.failure()
	default:
	}
	pt := append([]byte{t}, body...)
	s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := wire.WriteFrame(s.conn, s.snd.Seal(pt)); err != nil {
		go s.fail(err)
		return err
	}
	return nil
}

// Recv returns the next car->phone message. BYE is returned as an error that
// contains the reason; any frame or decrypt error closes the session.
//
// TICKs (0x82) are never returned: Recv takes each one in as the newest tick
// (id and the time it was read) and reads on. A caller that echoes the tick
// must keep a Recv loop running.
func (s *Session) Recv() (byte, []byte, error) {
	t, body, err := s.recv()
	for err == nil && t == proto.TypeTick {
		if len(body) != tickBodyLen {
			err = errors.New("client: malformed TICK")
			break
		}
		s.tmu.Lock()
		copy(s.tickID[:], body[:8])
		s.tickAt, s.ticked = time.Now(), true
		s.tmu.Unlock()
		t, body, err = s.recv()
	}
	if err != nil {
		s.fail(err)
		return 0, nil, err
	}
	if t == proto.TypeBye {
		err := fmt.Errorf("client: gateway said BYE: %s", body)
		s.fail(err)
		return 0, nil, err
	}
	return t, body, nil
}

// NewestTick returns the newest TICK Recv has taken in and how long ago it
// was read; ok is false before the first one.
func (s *Session) NewestTick() (id [8]byte, age time.Duration, ok bool) {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if !s.ticked {
		return id, 0, false
	}
	return s.tickID, time.Since(s.tickAt), true
}

// recv reads and opens one frame. Any s2c open failure is fatal (spec 4.4):
// the caller closes the session.
func (s *Session) recv() (byte, []byte, error) {
	select {
	case <-s.done:
		return 0, nil, s.failure()
	default:
	}
	ct, err := wire.ReadFrame(s.conn, maxS2CFrame)
	if err != nil {
		return 0, nil, err
	}
	pt, err := s.snd.OpenS2C(ct)
	if err != nil {
		return 0, nil, err
	}
	if len(pt) == 0 {
		return 0, nil, errors.New("client: empty message")
	}
	return pt[0], pt[1:], nil
}

// fail records the first error and closes the connection (no CLOSE frame).
func (s *Session) fail(err error) {
	s.closeOnce.Do(func() {
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.done)
		s.conn.Close()
	})
}

func (s *Session) failure() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err != nil {
		return s.err
	}
	return net.ErrClosed
}

// Close sends CLOSE (best effort) and closes the connection. Idempotent.
func (s *Session) Close() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	s.wmu.Lock()
	select {
	case <-s.done:
	default:
		s.conn.SetWriteDeadline(time.Now().Add(time.Second))
		wire.WriteFrame(s.conn, s.snd.Seal([]byte{proto.TypeClose}))
	}
	s.wmu.Unlock()
	s.fail(net.ErrClosed)
	return nil
}
