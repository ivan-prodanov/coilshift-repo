package carlink

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
)

// busDriver is the bus daemon's own name: the only sender of a real NameLost or NameAcquired.
const busDriver = "org.freedesktop.DBus"

// DBusBus is Bus on a godbus connection.
//
// Everything the connection receives passes an interceptor on godbus's single read goroutine first, in the bus's
// order (H3):
//   - the car's two method calls on /Webrtc go to the Handlers there, so CarFrame sees them in the car's call order
//     (godbus then runs each call on a goroutine of its own, where the exported methods only reply);
//   - NameLost and NameAcquired from the bus driver, and the sender-pinned KeysUpdated, queue a snapshot of their
//     callbacks for one runner goroutine. A callback registered after the bus read an event never sees that event.
//
// The connection's signal handler is godbus's sequential one, so signals keep the bus's order wherever godbus
// delivers them too; DBusBus itself needs no signal channel. The read goroutine must never block, so nothing holds mu
// across a bus call.
type DBusBus struct {
	conn *dbus.Conn

	handlers atomic.Pointer[Handlers]
	logf     atomic.Pointer[func(format string, args ...any)]

	mu       sync.Mutex // guards the fields below; taken on the read goroutine
	lost     map[string][]func()
	acquired map[string][]func()
	keys     []func()
	served   bool
	closed   bool // the runner fired the NameLost callbacks for the closed connection

	keysMu sync.Mutex // serializes SubscribeKeysUpdated's AddMatch; never taken on the read goroutine
	keysOn bool

	qmu   sync.Mutex // guards queue
	queue []func()
	wake  chan struct{}
}

// DialBus connects to the bus at addr: a private bus, for tests and the bench.
func DialBus(addr string) (*DBusBus, error) {
	return newBus(func(opts ...dbus.ConnOption) (*dbus.Conn, error) {
		c, err := dbus.Dial(addr, opts...)
		if err != nil {
			return nil, err
		}
		if err := c.Auth(nil); err != nil {
			c.Close()
			return nil, err
		}
		if err := c.Hello(); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	})
}

// SystemBus opens a private connection to the car's system bus.
func SystemBus() (*DBusBus, error) {
	return newBus(dbus.ConnectSystemBus)
}

// newBus builds a DBusBus on the connection connect makes with the bus's options.
func newBus(connect func(opts ...dbus.ConnOption) (*dbus.Conn, error)) (*DBusBus, error) {
	b := &DBusBus{
		lost:     map[string][]func(){},
		acquired: map[string][]func(){},
		wake:     make(chan struct{}, 1),
	}
	b.SetLogf(nil)
	// The interceptor may run before connect returns (during Hello); it never reads b.conn.
	conn, err := connect(
		dbus.WithIncomingInterceptor(b.intercept),
		dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()),
	)
	if err != nil {
		return nil, err
	}
	b.conn = conn
	go b.run()
	return b, nil
}

// SetLogf sets where the bus logs the car calls it drops, by size and never by content. f runs on godbus's read
// goroutine, so it must never block. nil discards, the default.
func (b *DBusBus) SetLogf(f func(format string, args ...any)) {
	if f == nil {
		f = func(string, ...any) {}
	}
	b.logf.Store(&f)
}

func (b *DBusBus) log(format string, args ...any) { (*b.logf.Load())(format, args...) }

// intercept runs on godbus's read goroutine for every message this connection receives, before godbus dispatches it.
func (b *DBusBus) intercept(msg *dbus.Message) {
	switch msg.Type {
	case dbus.TypeMethodCall:
		b.carCall(msg)
	case dbus.TypeSignal:
		b.signal(msg)
	}
}

// header is a string-valued header field of msg, or "".
func header(msg *dbus.Message, f dbus.HeaderField) string {
	v, ok := msg.Headers[f]
	if !ok {
		return ""
	}
	switch s := v.Value().(type) {
	case string:
		return s
	case dbus.ObjectPath:
		return string(s)
	}
	return ""
}

// carCall hands the car's calls on /Webrtc to the Handlers, exactly those godbus will dispatch to the exported
// methods (godbus also dispatches a call without an interface by its member name alone).
func (b *DBusBus) carCall(msg *dbus.Message) {
	member := header(msg, dbus.FieldMember)
	if header(msg, dbus.FieldPath) != WebrtcPath {
		return
	}
	if iface := header(msg, dbus.FieldInterface); iface != WebrtcIface && iface != "" {
		return
	}
	if member != MethodCarserverToPeerCommsData && member != MethodRemoteToLocalSignaling {
		return
	}
	h := b.handlers.Load()
	if h == nil {
		return // not serving yet: godbus answers with an error
	}
	sender := header(msg, dbus.FieldSender)
	in, ok := argMap(msg.Body)
	if !ok {
		b.log("carlink: dropped a %s call from %s: its argument is not one a{sv}", member, sender)
		return
	}
	if member == MethodRemoteToLocalSignaling {
		if h.Signaling != nil {
			h.Signaling(in)
		}
		return
	}
	f, drop, size := frameOf(in, sender)
	if drop != "" {
		b.log("carlink: dropped a car frame from %s (%d bytes): %s", sender, size, drop)
		return
	}
	if h.CarFrame != nil {
		h.CarFrame(f)
	}
}

// argMap unwraps a call's body when it is exactly one a{sv}.
func argMap(body []any) (map[string]any, bool) {
	if len(body) != 1 {
		return nil, false
	}
	m, ok := body[0].(map[string]dbus.Variant)
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v.Value()
	}
	return out, true
}

// signal queues the callbacks for a name event from the bus driver, or for a KeysUpdated broadcast. A broadcast
// reaches this connection only through a match rule, and the only one is SubscribeKeysUpdated's, pinned to
// com.tesla.Auth's owner. A KeysUpdated sent straight to this connection needs no rule, so it does not count.
func (b *DBusBus) signal(msg *dbus.Message) {
	iface, member := header(msg, dbus.FieldInterface), header(msg, dbus.FieldMember)
	var fs []func()
	switch {
	case header(msg, dbus.FieldSender) == busDriver && iface == busDriver &&
		(member == "NameLost" || member == "NameAcquired"):
		name, ok := firstString(msg.Body)
		if !ok {
			return
		}
		b.mu.Lock()
		if member == "NameLost" {
			fs = slices.Clone(b.lost[name])
		} else {
			fs = slices.Clone(b.acquired[name])
		}
		b.mu.Unlock()
	case iface == AuthIface && member == SignalKeysUpdated && header(msg, dbus.FieldPath) == AuthPath &&
		header(msg, dbus.FieldDestination) == "":
		b.mu.Lock()
		fs = slices.Clone(b.keys)
		b.mu.Unlock()
	}
	b.post(fs)
}

func firstString(body []any) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	s, ok := body[0].(string)
	return s, ok
}

// post queues fs for the runner without blocking.
func (b *DBusBus) post(fs []func()) {
	if len(fs) == 0 {
		return
	}
	b.qmu.Lock()
	b.queue = append(b.queue, fs...)
	b.qmu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// run calls the queued callbacks in order. When the connection closes, it calls what is still queued, then every
// NameLost callback once: the names went with the connection.
func (b *DBusBus) run() {
	done := b.conn.Context().Done()
	for {
		select {
		case <-b.wake:
			b.drain()
		case <-done:
			b.drain()
			b.mu.Lock()
			b.closed = true
			var fs []func()
			for _, l := range b.lost {
				fs = append(fs, l...)
			}
			b.mu.Unlock()
			for _, f := range fs {
				f()
			}
			return
		}
	}
}

func (b *DBusBus) drain() {
	for {
		b.qmu.Lock()
		fs := b.queue
		b.queue = nil
		b.qmu.Unlock()
		if len(fs) == 0 {
			return
		}
		for _, f := range fs {
			f()
		}
	}
}

// RequestName asks for name: with DO_NOT_QUEUE only, or, when queue is true, with no flags (the supervisor parent).
// Never ALLOW_REPLACEMENT, never REPLACE_EXISTING. owned is true when this connection is the primary owner.
func (b *DBusBus) RequestName(name string, queue bool) (bool, error) {
	flags := dbus.NameFlagDoNotQueue
	if queue {
		flags = 0
	}
	r, err := b.conn.RequestName(name, flags)
	if err != nil {
		return false, err
	}
	switch {
	case r == dbus.RequestNameReplyPrimaryOwner || r == dbus.RequestNameReplyAlreadyOwner:
		return true, nil
	case queue && r == dbus.RequestNameReplyInQueue, !queue && r == dbus.RequestNameReplyExists:
		return false, nil
	}
	return false, fmt.Errorf("carlink: RequestName(%s, queue %v) replied %d", name, queue, r)
}

// ReleaseName gives up name. Not owning it is not an error.
func (b *DBusBus) ReleaseName(name string) error {
	_, err := b.conn.ReleaseName(name)
	return err
}

// Emit sends the signal com.tesla.Webrtc.<member> from /Webrtc, with body as its one a{sv} argument. Like godbus, it
// reports a failed write as success; only Link's confirmation tells.
func (b *DBusBus) Emit(member string, body map[string]any) error {
	if err := checkBody(body); err != nil {
		return err
	}
	m := make(map[string]dbus.Variant, len(body))
	for k, v := range body {
		m[k] = dbus.MakeVariant(v)
	}
	return b.conn.Emit(dbus.ObjectPath(WebrtcPath), WebrtcIface+"."+member, m)
}

// Serve hands the car's calls to h (see DBusBus) and exports the two methods on /Webrtc, which only reply:
// CarserverToPeerCommsData with {CarserverToPeerCommsData_Reply: h.Reply()}, RemoteToLocalSignaling with 0.
// h.Reply runs on godbus's per-call goroutines, so it must be safe for concurrent use.
func (b *DBusBus) Serve(h Handlers) error {
	b.mu.Lock()
	served := b.served
	b.served = true
	b.mu.Unlock()
	if served {
		return errServedTwice
	}
	b.handlers.Store(&h)
	return b.conn.ExportMethodTable(map[string]any{
		MethodCarserverToPeerCommsData: func(map[string]dbus.Variant) (map[string]dbus.Variant, *dbus.Error) {
			return map[string]dbus.Variant{ReplyKey: dbus.MakeVariant(h.reply())}, nil
		},
		MethodRemoteToLocalSignaling: func(map[string]dbus.Variant) (uint32, *dbus.Error) {
			return 0, nil
		},
	}, dbus.ObjectPath(WebrtcPath), WebrtcIface)
}

// OnNameLost calls f for each NameLost(name) from the bus driver read after this call, and once when the
// connection closes.
func (b *DBusBus) OnNameLost(name string, f func()) error {
	return b.register(b.lost, name, f)
}

// OnNameAcquired calls f for each NameAcquired(name) from the bus driver read after this call.
func (b *DBusBus) OnNameAcquired(name string, f func()) error {
	return b.register(b.acquired, name, f)
}

func (b *DBusBus) register(m map[string][]func(), name string, f func()) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	m[name] = append(m[name], f)
	return nil
}

// NameOwner asks the bus driver for name's owner (GetNameOwner).
func (b *DBusBus) NameOwner(ctx context.Context, name string) (string, error) {
	var owner string
	err := b.conn.BusObject().CallWithContext(ctx, busDriver+".GetNameOwner", 0, name).Store(&owner)
	return owner, err
}

// UniqueName is this connection's unique name.
func (b *DBusBus) UniqueName() string { return b.conn.Names()[0] }

// Close closes the connection; the bus then releases its names. It deadlocks behind a stuck write, so Link never
// calls it.
func (b *DBusBus) Close() error { return b.conn.Close() }

// CallerInfo returns the caller's uid, and its pid when the bus knows it (0 otherwise; Homebrew's dbus-daemon on
// darwin cannot tell). A missing pid is not an error.
func (b *DBusBus) CallerInfo(ctx context.Context, sender string) (uid, pid uint32, err error) {
	obj := b.conn.BusObject()
	if err := obj.CallWithContext(ctx, busDriver+".GetConnectionUnixUser", 0, sender).Store(&uid); err != nil {
		return 0, 0, err
	}
	if obj.CallWithContext(ctx, busDriver+".GetConnectionUnixProcessID", 0, sender).Store(&pid) != nil {
		pid = 0
	}
	return uid, pid, nil
}

// SubscribeKeysUpdated adds the sender-pinned match
// sender='com.tesla.Auth',path='/Auth',interface='com.tesla.Auth',member='KeysUpdated' (once) and calls f for each
// such signal (see signal).
func (b *DBusBus) SubscribeKeysUpdated(f func()) error {
	b.keysMu.Lock()
	defer b.keysMu.Unlock()
	if !b.keysOn {
		if err := b.conn.AddMatchSignal(
			dbus.WithMatchSender(AuthName),
			dbus.WithMatchObjectPath(dbus.ObjectPath(AuthPath)),
			dbus.WithMatchInterface(AuthIface),
			dbus.WithMatchMember(SignalKeysUpdated),
		); err != nil {
			return err
		}
		b.keysOn = true
	}
	b.mu.Lock()
	b.keys = append(b.keys, f)
	b.mu.Unlock()
	return nil
}
