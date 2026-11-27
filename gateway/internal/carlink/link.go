package carlink

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	ErrNameTaken = errors.New("carlink: com.tesla.Webrtc has another owner")
	ErrBusDead   = errors.New("carlink: bus latched dead after an emit overran")

	errReleased = errors.New("carlink: the link released the name")
)

// Link is com.tesla.Webrtc on a Bus: its ownership, its health, and the emits the helper may originate (H1, the
// phone's data, H4), each bounded by EmitDeadline.
//
// Healthy latches false on NameLost, a failed emit or an unconfirmed Disconnect, and for good once an emit overran
// (the bus is then dead and the process exits). A Link serves one ownership: after Release it never owns the name
// again. The emits do not check ownership; only Healthy and Disconnect's confirmation do.
type Link struct {
	bus  Bus
	exit func(code int)
	logf func(format string, args ...any)

	mu         sync.Mutex // never held across a bus call or a wait
	err        error      // registering the name callbacks failed
	owned      bool       // Acquire or NameAcquired; cleared by NameLost and Release
	released   bool
	broken     bool // NameLost, a failed emit or an unconfirmed Disconnect
	dead       bool // an emit overran: every later emit returns ErrBusDead
	wasHealthy bool
	fired      bool     // the OnUnhealthy callbacks ran
	down       []func() // OnUnhealthy callbacks still waiting
}

// LinkOption configures a Link.
type LinkOption func(*Link)

// WithExit sets what an emit overrun calls with ExitEmitOverrun. The default is os.Exit.
func WithExit(f func(code int)) LinkOption {
	return func(l *Link) {
		if f != nil {
			l.exit = f
		}
	}
}

// WithLogf sets the Link's logger, which must never block. The default discards.
func WithLogf(f func(format string, args ...any)) LinkOption {
	return func(l *Link) {
		if f != nil {
			l.logf = f
		}
	}
}

// NewLink registers the Link's NameLost and NameAcquired callbacks on b. A Link sees only the name events b reads
// after this call, so a NameLost from an earlier Link's Release on the same Bus never reaches it.
func NewLink(b Bus, opts ...LinkOption) *Link {
	l := &Link{bus: b, exit: os.Exit, logf: func(string, ...any) {}}
	for _, o := range opts {
		o(l)
	}
	if err := b.OnNameLost(WebrtcName, l.nameLost); err != nil {
		l.err = err
	} else if err := b.OnNameAcquired(WebrtcName, l.nameAcquired); err != nil {
		l.err = err
	}
	return l
}

// Acquire requests WebrtcName with DO_NOT_QUEUE: ErrNameTaken if another connection owns it.
func (l *Link) Acquire() error {
	l.mu.Lock()
	err := l.err
	if l.released {
		err = errReleased
	}
	l.mu.Unlock()
	if err != nil {
		return err
	}
	owned, err := l.bus.RequestName(WebrtcName, false)
	if err != nil {
		return err
	}
	if !owned {
		return ErrNameTaken
	}
	l.update(func() { l.owned = !l.released })
	return nil
}

// Owned reports whether the Link holds the name (by Acquire, or by a queued request's NameAcquired).
func (l *Link) Owned() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.owned
}

// Healthy: owned, not dead, and no emit has failed (nor NameLost, nor an unconfirmed Disconnect).
func (l *Link) Healthy() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.healthyLocked()
}

func (l *Link) healthyLocked() bool { return l.err == nil && l.owned && !l.broken && !l.dead }

// OnUnhealthy calls f once, on its own goroutine, when Healthy turns false: the first time the Link is unhealthy
// after it was healthy. Registered after that, f runs at once.
func (l *Link) OnUnhealthy(f func()) {
	l.mu.Lock()
	fired := l.fired
	if !fired {
		l.down = append(l.down, f)
	}
	l.mu.Unlock()
	if fired {
		go f()
	}
}

// update applies change under the lock, then runs the OnUnhealthy callbacks if it turned Healthy false.
func (l *Link) update(change func()) {
	l.mu.Lock()
	change()
	var fire []func()
	if l.healthyLocked() {
		l.wasHealthy = true
	} else if l.wasHealthy && !l.fired {
		l.fired = true
		fire, l.down = l.down, nil
	}
	l.mu.Unlock()
	for _, f := range fire {
		go f()
	}
}

func (l *Link) nameLost() {
	wasOwned := false
	l.update(func() { wasOwned, l.owned, l.broken = l.owned, false, true })
	if wasOwned {
		l.logf("carlink: lost %s", WebrtcName)
	}
}

func (l *Link) nameAcquired() { l.update(func() { l.owned = !l.released }) }

// Connect emits H1: LocalToRemoteSignaling {data_channels_ready: true}.
func (l *Link) Connect() error {
	return l.emit(SignalLocalToRemoteSignaling, map[string]any{"data_channels_ready": true})
}

// Send emits PeerCommsToCarserverData {uid: PhoneUID, data: string(data)}.
func (l *Link) Send(data []byte) error {
	return l.emit(SignalPeerCommsToCarserverData, map[string]any{"uid": PhoneUID, "data": string(data)})
}

// Disconnect emits the H4 disconnect body, then confirms it: within EmitDeadline, NameOwner(WebrtcName) must return
// this connection's unique name while Healthy() is still true. The daemon handles one connection's messages in
// order, so that reply proves the signal left while the name was ours. Anything else is an error.
//
// An unconfirmed Disconnect latches Healthy false (R7). The confirmation needs godbus's read goroutine, so callers
// must not hold a lock the Handlers take.
func (l *Link) Disconnect() error {
	healthy := l.Healthy() // the name was ours before the signal left
	if err := l.emit(SignalLocalToRemoteSignaling, map[string]any{
		"ice_state_change": true,
		"msg_type":         "autopark:set_webrtc",
		"set_webrtc_value": false,
	}); err != nil {
		return err
	}
	if err := l.confirm(healthy); err != nil {
		l.logf("carlink: disconnect not confirmed: %v", err)
		l.update(func() { l.broken = true })
		return err
	}
	return nil
}

// confirm is Disconnect's round trip. It runs on a goroutine of its own, because a stuck write blocks godbus's call
// whatever its context says.
func (l *Link) confirm(healthyBefore bool) error {
	if !healthyBefore {
		return errors.New("carlink: the link was unhealthy when the disconnect was emitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), EmitDeadline)
	defer cancel()
	type reply struct {
		owner string
		err   error
	}
	got := make(chan reply, 1)
	go func() {
		owner, err := l.bus.NameOwner(ctx, WebrtcName)
		got <- reply{owner, err}
	}()
	var r reply
	select {
	case r = <-got:
	case <-ctx.Done():
		return fmt.Errorf("carlink: no NameOwner reply within %v", EmitDeadline)
	}
	switch {
	case r.err != nil:
		return fmt.Errorf("carlink: NameOwner: %w", r.err)
	case r.owner != l.bus.UniqueName():
		return fmt.Errorf("carlink: %s is owned by %s, not %s", WebrtcName, r.owner, l.bus.UniqueName())
	case !l.Healthy():
		return errors.New("carlink: the link turned unhealthy before the confirmation")
	}
	return nil
}

// emit runs one Bus.Emit on its own goroutine with a timer of EmitDeadline. A failed emit latches Healthy false. An
// emit that overruns latches the bus dead and calls exit(ExitEmitOverrun): a stuck godbus write blocks every later
// send, and Close deadlocks behind it, so the parent's takeover is then the only way to deliver the abort.
func (l *Link) emit(member string, body map[string]any) error {
	l.mu.Lock()
	dead := l.dead
	l.mu.Unlock()
	if dead {
		return ErrBusDead // at once, without a goroutine
	}
	done := make(chan error, 1)
	go func() { done <- l.bus.Emit(member, body) }()
	t := time.NewTimer(EmitDeadline)
	defer t.Stop()
	select {
	case err := <-done:
		if err != nil {
			l.logf("carlink: %s emit failed: %v", member, err)
			l.update(func() { l.broken = true })
			return fmt.Errorf("carlink: %s: %w", member, err)
		}
		return nil
	case <-t.C:
		first := false
		l.update(func() { first, l.dead = !l.dead, true })
		if first {
			// The log line is handed off, never waited for: nothing may delay the exit.
			go l.logf("carlink: %s emit overran %v: the bus is dead, exiting %d", member, EmitDeadline, ExitEmitOverrun)
			l.exit(ExitEmitOverrun)
		}
		return ErrBusDead
	}
}

// Release gives up the name. The Link never owns it again.
func (l *Link) Release() error {
	l.update(func() { l.released, l.owned = true, false })
	return l.bus.ReleaseName(WebrtcName)
}
