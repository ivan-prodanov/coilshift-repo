package authority

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

// dbusConn is authBus over a private godbus connection.
type dbusConn struct{ c *dbus.Conn }

func (d dbusConn) NameOwner(ctx context.Context, name string) (string, error) {
	return only[string](d.c.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, name))
}

func (d dbusConn) UnixUser(ctx context.Context, name string) (uint32, error) {
	return only[uint32](d.c.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionUnixUser", 0, name))
}

// KeyInfo calls com.tesla.Auth.KeyInfo() at /Auth on dest. It takes no
// arguments, so nothing from the phone reaches the bus.
func (d dbusConn) KeyInfo(ctx context.Context, dest string) ([]interface{}, error) {
	call := d.c.Object(dest, "/Auth").CallWithContext(ctx, "com.tesla.Auth.KeyInfo", 0)
	return call.Body, call.Err
}

func (d dbusConn) Connected() bool { return d.c.Connected() }

func (d dbusConn) Close() error { return d.c.Close() }

// only returns a call's single reply value if it has exactly type T. Unlike
// Store it never converts, for example an int32 into a uint32.
func only[T any](call *dbus.Call) (T, error) {
	var zero T
	if call.Err != nil {
		return zero, call.Err
	}
	if len(call.Body) != 1 {
		return zero, fmt.Errorf("%w: %s: %d reply values, want 1", ErrShape, call.Method, len(call.Body))
	}
	v, ok := call.Body[0].(T)
	if !ok {
		return zero, fmt.Errorf("%w: %s: reply is %T, want %T", ErrShape, call.Method, call.Body[0], zero)
	}
	return v, nil
}

// handshake authenticates c (EXTERNAL first, as dbus.Connect does) and sends
// Hello, closing c if that takes longer than timeout: a stalled bus daemon
// must not hold the oracle's only connection attempt forever.
func handshake(c *dbus.Conn, timeout time.Duration) error {
	t := time.AfterFunc(timeout, func() { c.Close() })
	err := c.Auth(nil)
	if err == nil {
		err = c.Hello()
	}
	if !t.Stop() && err == nil {
		err = errors.New("timed out")
	}
	if err != nil {
		c.Close()
		return fmt.Errorf("handshake: %w", err)
	}
	return nil
}

// BusVerdict is what ProbeSystemBus found out about this process and the
// system bus.
type BusVerdict int

const (
	// BusUnknown: no verdict. The bus did not answer within the timeout, or
	// failed in a way that says nothing about this uid: a dial error other
	// than the three BusRefused names (EAGAIN from a full accept backlog,
	// say), the connection closed before authentication ended (the
	// daemon's own limits), an error reply to Hello, or anything else. A
	// bus too slow to answer may still let this uid in: treat it as
	// reachable.
	BusUnknown BusVerdict = iota
	// BusReachable: the bus took the connection (EXTERNAL and Hello).
	BusReachable
	// BusRefused: the bus definitely refused this process. There is no
	// socket (ENOENT), nobody listens on it (ECONNREFUSED), the socket is
	// not ours to use (EACCES), every authentication mechanism was
	// REJECTED, or the daemon disconnected right after BEGIN, which is how
	// dbus-daemon refuses a uid it cannot resolve (EXTERNAL takes the
	// socket's credentials, then bus_policy_allow_unix_user fails). That
	// disconnect can also be one of dbus-daemon's own limits: it closes the
	// oldest connection that has not finished Hello when more than
	// max_incomplete_connections (64 by default) are incomplete. A refusal
	// of the uid repeats every time, so agw's self-test asks for two refusals
	// in a row.
	BusRefused
)

func (v BusVerdict) String() string {
	switch v {
	case BusReachable:
		return "reachable"
	case BusRefused:
		return "refused"
	}
	return "no verdict"
}

// probeMargin: ProbeSystemBus stops waiting this long after timeout for a
// dial that has not returned (a unix connect does not block, but the bus
// address may name another transport). The probe still closes what it
// opens.
const probeMargin = 500 * time.Millisecond

// ProbeSystemBus finds out whether this process can open a system bus
// connection: a private connection, the EXTERNAL handshake and Hello within
// timeout, then closed at once (it never keeps one). agw runs it as a
// self-test: the gateway uid must not reach the bus, and only BusRefused
// shows that. err is nil exactly with BusReachable; otherwise it says what
// happened.
func ProbeSystemBus(timeout time.Duration) (BusVerdict, error) {
	type result struct {
		v   BusVerdict
		err error
	}
	res := make(chan result, 1)
	go func() {
		v, err := probeBus(timeout)
		res <- result{v, err}
	}()
	t := time.NewTimer(timeout + probeMargin)
	defer t.Stop()
	select {
	case r := <-res:
		return r.v, r.err
	case <-t.C:
		return BusUnknown, fmt.Errorf("system bus: no connection within %v", timeout+probeMargin)
	}
}

// errAuthFailed is godbus's error once the bus REJECTED every mechanism.
const errAuthFailed = "dbus: authentication failed"

// probeBus is one probe. It closes its connection, at timeout at the latest.
func probeBus(timeout time.Duration) (BusVerdict, error) {
	c, err := dbus.SystemBusPrivate()
	if err != nil {
		return dialVerdict(err), fmt.Errorf("system bus: connect: %w", err)
	}
	defer c.Close()
	t := time.AfterFunc(timeout, func() { c.Close() })
	stage := "authentication"
	err = c.Auth(nil)
	if err == nil {
		stage = "Hello"
		err = c.Hello()
	}
	timedOut := !t.Stop()
	switch {
	case err == nil:
		return BusReachable, nil // Hello answered, even if at the deadline
	case timedOut: // the errors are our own close's
		return BusUnknown, fmt.Errorf("system bus: no answer within %v (%s): %w", timeout, stage, err)
	case stage == "authentication" && err.Error() == errAuthFailed:
		return BusRefused, fmt.Errorf("system bus: every mechanism REJECTED: %w", err)
	case stage == "Hello" && connLost(err):
		return BusRefused, fmt.Errorf("system bus: disconnected after BEGIN: %w", err)
	}
	return BusUnknown, fmt.Errorf("system bus: %s: %w", stage, err)
}

// dialVerdict: no socket, nobody listening or no permission is a refusal;
// any other dial error (EAGAIN from a full accept backlog, say) is none.
func dialVerdict(err error) BusVerdict {
	for _, e := range []error{syscall.ENOENT, syscall.ECONNREFUSED, syscall.EACCES} {
		if errors.Is(err, e) {
			return BusRefused
		}
	}
	return BusUnknown
}

// connLost reports whether a call failed because the connection ended under
// it: the peer closed or reset it (a write then fails with EPIPE on Linux,
// ENOTCONN on darwin), or godbus closed it after its reader saw that (our
// own close is the timer's, checked first).
func connLost(err error) bool {
	for _, e := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.EPIPE, syscall.ENOTCONN,
		dbus.ErrClosed, net.ErrClosed} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
