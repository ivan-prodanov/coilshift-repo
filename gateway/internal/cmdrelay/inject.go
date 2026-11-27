package cmdrelay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/camhelper/proto"
)

// The router entry the relay uses: only the authd user may call it (com.tesla.CommandRouter.conf).
const (
	routerName   = "com.tesla.CommandRouter"
	routerPath   = "/CommandRouter"
	routerMethod = "com.tesla.CommandRouter.RouteFromAuthd"
	callBudget   = 2 * time.Second
)

// The injector child's frames on its stdin/stdout (camhelper's framing).
const (
	childMsg byte = 0x01 // parent -> child: one message to inject
	childOK  byte = 0x81 // child -> parent: injected
	childErr byte = 0x82 // child -> parent: the call failed; body: 'N' (the router certainly did not get it) or 'U'
	//                    (it may have: no reply in time), then the error text
)

// ExecInjector runs `<Path> <Args...>` (camhelper cmd-inject) as Cred (the authd user) and hands it one message at a
// time. A child that fails or overruns is killed and started again for the next message.
type ExecInjector struct {
	Path string
	Args []string
	Cred *syscall.Credential
	Log  *log.Logger

	once  sync.Once
	turn  chan struct{} // capacity 1: whose turn it is (a mutex that a deadline can give up on)
	mu    sync.Mutex    // guards cmd, stdin, out (held only inside a turn, and by Close)
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
}

func (e *ExecInjector) start() error {
	cmd := exec.Command(e.Path, e.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: e.Cred, Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if e.Log != nil {
		cmd.Stderr = e.Log.Writer()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	e.cmd, e.stdin, e.out = cmd, stdin, bufio.NewReader(stdout)
	return nil
}

func (e *ExecInjector) stopLocked() {
	if e.cmd == nil {
		return
	}
	e.stdin.Close()
	e.cmd.Process.Kill()
	e.cmd.Wait()
	e.cmd = nil
}

// Close stops the child.
func (e *ExecInjector) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
}

// Inject hands msg to the child and waits for its answer, at most until ctx ends. Waiting for the turn past ctx, or a
// child that cannot be (re)started or written to, is ErrNotSent: nothing reached the router. Once the message is
// written, an overrun or a lost child may have let it through, so those errors are not ErrNotSent.
func (e *ExecInjector) Inject(ctx context.Context, msg []byte) error {
	e.once.Do(func() { e.turn = make(chan struct{}, 1) })
	select {
	case e.turn <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("injector busy: %w", ErrNotSent)
	}
	defer func() { <-e.turn }()
	e.mu.Lock()
	defer e.mu.Unlock()

	// Write, restarting a dead child once: a failed write sent nothing.
	var werr error
	for try := 0; try < 2; try++ {
		if e.cmd == nil {
			if err := e.start(); err != nil {
				return fmt.Errorf("start the injector: %v: %w", err, ErrNotSent)
			}
		}
		if werr = proto.WriteFrame(e.stdin, childMsg, msg); werr == nil {
			break
		}
		e.stopLocked()
	}
	if werr != nil {
		return fmt.Errorf("injector write: %v: %w", werr, ErrNotSent)
	}

	type answer struct {
		t    byte
		body []byte
		err  error
	}
	done := make(chan answer, 1)
	out := e.out
	go func() {
		t, body, err := proto.ReadFrame(out, 1+4096)
		done <- answer{t, body, err}
	}()
	select {
	case a := <-done:
		switch {
		case a.err != nil:
			e.stopLocked()
			return fmt.Errorf("injector: %w", a.err)
		case a.t == childOK:
			return nil
		case a.t == childErr && len(a.body) > 0 && a.body[0] == 'N':
			return fmt.Errorf("RouteFromAuthd: %s: %w", a.body[1:], ErrNotSent)
		case a.t == childErr:
			return fmt.Errorf("RouteFromAuthd: %s", trimCode(a.body))
		default:
			e.stopLocked()
			return fmt.Errorf("injector: unknown frame 0x%02x", a.t)
		}
	case <-ctx.Done():
		e.stopLocked() // unblocks the reader (its pipe closes)
		return ctx.Err()
	}
}

func trimCode(b []byte) []byte {
	if len(b) > 0 && (b[0] == 'N' || b[0] == 'U') {
		return b[1:]
	}
	return b
}

// RunChild is the injector child (`camhelper cmd-inject`, as the authd user): it reads messages on in and calls
// RouteFromAuthd for each on its own system bus connection, answering on out. It returns when in ends.
func RunChild(in io.Reader, out io.Writer, dial func() (*dbus.Conn, error)) error {
	if dial == nil {
		dial = systemBus
	}
	var conn *dbus.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	// connected returns a live connection, redialling a dropped one (a dbus-daemon restart, a disconnect).
	connected := func() (*dbus.Conn, error) {
		if conn != nil && conn.Connected() {
			return conn, nil
		}
		if conn != nil {
			conn.Close()
			conn = nil
		}
		c, err := dial()
		if err != nil {
			return nil, err
		}
		conn = c
		return c, nil
	}
	if _, err := connected(); err != nil {
		return err
	}
	r := bufio.NewReader(in)
	for {
		t, body, err := proto.ReadFrame(r, 1+MaxMsg)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if t != childMsg {
			return fmt.Errorf("cmd-inject: unknown frame 0x%02x", t)
		}
		c, derr := connected()
		if derr != nil {
			if err := proto.WriteFrame(out, childErr, append([]byte{'N'}, clip(derr.Error())...)); err != nil {
				return err
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), callBudget)
		call := c.Object(routerName, routerPath).CallWithContext(ctx, routerMethod, 0, body)
		cancel()
		if call.Err != nil {
			// An error reply from the bus or the router means the message was refused before routing; no reply in
			// time (our deadline, or the bus's NoReply) means it may have been routed.
			code := byte('N')
			var de dbus.Error
			if errors.As(call.Err, &de) {
				if de.Name == "org.freedesktop.DBus.Error.NoReply" {
					code = 'U'
				}
			} else if errors.Is(call.Err, context.DeadlineExceeded) || !c.Connected() {
				code = 'U'
			}
			err = proto.WriteFrame(out, childErr, append([]byte{code}, clip(call.Err.Error())...))
		} else {
			err = proto.WriteFrame(out, childOK, nil)
		}
		if err != nil {
			return err
		}
	}
}

func clip(s string) []byte {
	if len(s) > 200 {
		s = s[:200]
	}
	return []byte(s)
}
