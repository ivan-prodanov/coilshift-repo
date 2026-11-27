package authority

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// systemBusAt points godbus's system bus at a unix socket in a temp dir (both
// the linux and the darwin variable) and returns the listener.
func systemBusAt(t *testing.T) *net.UnixListener {
	t.Helper()
	sock := filepath.Join(shortDir(t), "bus")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path="+sock)
	t.Setenv("DBUS_LAUNCHD_SESSION_BUS_SOCKET", sock)
	return ln.(*net.UnixListener)
}

// serveOne runs serve on the first connection and closes done when it ends.
func serveOne(ln net.Listener, serve func(net.Conn)) (done chan struct{}) {
	done = make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		serve(c)
	}()
	return done
}

// busAuth is a bus daemon's side of godbus's authentication: it accepts
// EXTERNAL (godbus sends it with no identity; dbus-daemon asks for it with
// DATA, godbus answers an empty DATA and the socket's peer credentials
// decide). It returns once the client sent BEGIN, false if it went away.
func busAuth(c net.Conn, r *bufio.Reader) bool {
	if _, err := r.ReadByte(); err != nil { // the client's NUL byte
		return false
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return false
		}
		switch {
		case strings.HasPrefix(line, "AUTH EXTERNAL"):
			io.WriteString(c, "DATA\r\n")
		case strings.HasPrefix(line, "DATA"):
			io.WriteString(c, "OK 0123456789abcdef0123456789abcdef\r\n")
		case strings.HasPrefix(line, "AUTH"):
			io.WriteString(c, "REJECTED EXTERNAL\r\n")
		case strings.HasPrefix(line, "BEGIN"):
			return true
		default:
			io.WriteString(c, "ERROR\r\n")
		}
	}
}

// replyHello reads the client's Hello and answers it: the unique name, or
// the error errName when it is set.
func replyHello(c net.Conn, r *bufio.Reader, errName string) error {
	m, err := dbus.DecodeMessage(r)
	if err != nil {
		return err
	}
	if member := m.Headers[dbus.FieldMember].Value(); member != "Hello" {
		return fmt.Errorf("first call %v, want Hello", member)
	}
	body := []interface{}{":1.7"}
	reply := &dbus.Message{Type: dbus.TypeMethodReply, Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldReplySerial: dbus.MakeVariant(m.Serial()),
	}}
	if errName != "" {
		reply.Type = dbus.TypeError
		reply.Headers[dbus.FieldErrorName] = dbus.MakeVariant(errName)
		body = []interface{}{"refused by the test daemon"}
	}
	reply.Body = body
	reply.Headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(body...))
	return reply.EncodeTo(c, binary.LittleEndian)
}

// probe runs ProbeSystemBus and requires verdict want, its error naming what
// happened (nil exactly when reachable), within timeout plus the margin.
func probe(t *testing.T, timeout time.Duration, want BusVerdict, why string) error {
	t.Helper()
	start := time.Now()
	v, err := ProbeSystemBus(timeout)
	if d := time.Since(start); d > timeout+probeMargin+500*time.Millisecond {
		t.Fatalf("took %v, timeout %v", d, timeout)
	}
	if v != want || (err == nil) != (want == BusReachable) {
		t.Fatalf("%s: %v (%v), want %v", why, v, err, want)
	}
	return err
}

// waitClosed requires the probe to have closed its connection: it never
// keeps one.
func waitClosed(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ProbeSystemBus kept the connection open")
	}
}

// A bus that lets this process in: reachable, and the connection is closed
// again at once.
func TestProbeSystemBusConnects(t *testing.T) {
	ln := systemBusAt(t)
	done := serveOne(ln, func(c net.Conn) { (&daemon{answer: authdDaemon, srv: c}).serve() })
	probe(t, 2*time.Second, BusReachable, "a bus that accepts us")
	waitClosed(t, done)
}

// dbus-daemon refuses a uid it cannot resolve (uid 4950 on the car) right
// after BEGIN: EXTERNAL takes the socket's credentials, then
// bus_policy_allow_unix_user cannot look the uid up and the daemon
// disconnects. Refused.
func TestProbeSystemBusDisconnectedAfterBegin(t *testing.T) {
	ln := systemBusAt(t)
	done := serveOne(ln, func(c net.Conn) { busAuth(c, bufio.NewReader(c)) })
	err := probe(t, 2*time.Second, BusRefused, "a disconnect right after BEGIN")
	if !strings.Contains(err.Error(), "after BEGIN") {
		t.Fatalf("err %v", err)
	}
	waitClosed(t, done)
}

// Every mechanism REJECTED: refused.
func TestProbeSystemBusRejected(t *testing.T) {
	ln := systemBusAt(t)
	serveOne(ln, func(c net.Conn) {
		r := bufio.NewReader(c)
		r.ReadByte() // the NUL byte
		for {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
			io.WriteString(c, "REJECTED EXTERNAL\r\n")
		}
	})
	probe(t, 2*time.Second, BusRefused, "a refused handshake")
}

// No bus socket (ENOENT): refused.
func TestProbeSystemBusNoBus(t *testing.T) {
	ln := systemBusAt(t)
	ln.Close()
	probe(t, 2*time.Second, BusRefused, "no bus socket")
}

// The socket is there but nobody listens on it (ECONNREFUSED): refused.
func TestProbeSystemBusNobodyListening(t *testing.T) {
	ln := systemBusAt(t)
	ln.SetUnlinkOnClose(false)
	ln.Close()
	probe(t, 2*time.Second, BusRefused, "nobody listening")
}

// No permission on the socket (EACCES): refused.
func TestProbeSystemBusNoPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the socket's mode")
	}
	ln := systemBusAt(t)
	if err := os.Chmod(ln.Addr().String(), 0); err != nil {
		t.Fatal(err)
	}
	probe(t, 2*time.Second, BusRefused, "no permission on the socket")
}

// A daemon that never answers: no verdict, within the timeout. A bus too
// slow to answer may still let this uid in, so a timeout is never a
// refusal.
func TestProbeSystemBusTimesOut(t *testing.T) {
	ln := systemBusAt(t)
	done := serveOne(ln, func(c net.Conn) { io.Copy(io.Discard, c) })
	err := probe(t, 200*time.Millisecond, BusUnknown, "a silent daemon")
	if !strings.Contains(err.Error(), "no answer within 200ms") {
		t.Fatalf("err %v", err)
	}
	waitClosed(t, done)
}

// A busy daemon that would let this uid in, but only after the probe's
// bound (the review's 2.0-2.5 s case): no verdict, never refused, and the
// probe closed its connection before the daemon got to Hello.
func TestProbeSystemBusSlowToLetUsIn(t *testing.T) {
	ln := systemBusAt(t)
	hello := make(chan error, 1)
	done := serveOne(ln, func(c net.Conn) {
		time.Sleep(400 * time.Millisecond)
		r := bufio.NewReader(c)
		if !busAuth(c, r) {
			hello <- errors.New("the client left during authentication")
			return
		}
		hello <- replyHello(c, r, "")
	})
	probe(t, 200*time.Millisecond, BusUnknown, "a bus that lets us in late")
	waitClosed(t, done)
	if err := <-hello; err == nil {
		t.Fatal("the probe was still there for Hello after its bound")
	}
}

// The daemon closes the connection before authentication ends. dbus-daemon
// only decides on a uid after BEGIN (or REJECTs it), so this is its own
// limits (too many incomplete connections, say), not a verdict.
func TestProbeSystemBusClosedDuringAuthentication(t *testing.T) {
	ln := systemBusAt(t)
	serveOne(ln, func(c net.Conn) {
		r := bufio.NewReader(c)
		r.ReadByte()
		r.ReadString('\n') // AUTH: closed instead of an answer
	})
	probe(t, 2*time.Second, BusUnknown, "a close before BEGIN")
}

// The bus answers Hello with an error: past authentication, so not a
// refusal of the uid, and not a usable connection either. No verdict.
func TestProbeSystemBusHelloError(t *testing.T) {
	ln := systemBusAt(t)
	done := serveOne(ln, func(c net.Conn) {
		r := bufio.NewReader(c)
		if busAuth(c, r) {
			replyHello(c, r, "org.freedesktop.DBus.Error.LimitsExceeded")
			io.Copy(io.Discard, c)
		}
	})
	probe(t, 2*time.Second, BusUnknown, "an error reply to Hello")
	waitClosed(t, done)
}

// Only no socket, nobody listening or no permission refuses at the dial:
// EAGAIN (a full accept backlog) and anything else is no verdict.
func TestProbeDialVerdict(t *testing.T) {
	dialErr := func(e error) error {
		return &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", e)}
	}
	for _, c := range []struct {
		err  error
		want BusVerdict
	}{
		{dialErr(syscall.ENOENT), BusRefused},
		{dialErr(syscall.ECONNREFUSED), BusRefused},
		{dialErr(syscall.EACCES), BusRefused},
		{dialErr(syscall.EAGAIN), BusUnknown},
		{dialErr(syscall.ETIMEDOUT), BusUnknown},
		{dialErr(syscall.EPERM), BusUnknown},
		{errors.New("dbus: invalid bus address (no transport)"), BusUnknown},
	} {
		if got := dialVerdict(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}

func TestBusVerdictString(t *testing.T) {
	for v, want := range map[BusVerdict]string{BusUnknown: "no verdict", BusReachable: "reachable", BusRefused: "refused", 7: "no verdict"} {
		if got := v.String(); got != want {
			t.Errorf("%d: %q, want %q", int(v), got, want)
		}
	}
}
