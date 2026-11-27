// Package summonrelay is agw's SUMMON capability (0x01): a relay of opaque bytes between the phone's outer window and
// the summon helper's relay socket (design §6.2). agw (uid 4950) faces the network and has no bus access, so it can
// never drive the car: it carries the phone's inner session to the helper (uid 1337), which serves it, and never
// parses it.
//
//   - PreArm: the R1 route gate (Tailscale must not advertise the vehicle LAN; fail closed), and the relay socket must
//     be a socket, not a symlink, owned by the helper's uid.
//   - Open: dial the socket; its server must be the helper's uid, and its first record, within OwnedWait, OWNED: the
//     helper owns com.tesla.Webrtc and is healthy, which agw cannot ask the bus itself.
//   - Phone to helper: each 0x10 body becomes INNER records, through a queue of QueueRecords bodies for one writer
//     goroutine, with WriteTimeout per write, so Frame never blocks.
//   - Helper to phone: INNER records go out as 0x90 on the priority lane, and LINK records set the Host's link flag.
//
// The socket is closed as soon as the window is done, so the helper sees EOF and runs H4 without waiting for
// Teardown.
//
// The core logs a capability's refusal only as "PreArm refused: error" or "Open failed: error", and never sends its
// text, so the relay logs its own reasons (Config.Logf): each PreArm refusal and Open failure once, and once per
// window why the relay ended it. Every reason is secret-free: a route refusal never quotes tailscaled's prefs, and
// nothing of the inner stream (the phone's and the car's messages) is ever logged.
package summonrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/internal/guards"
	"airgapp/gateway/internal/peercred"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/summon/relay"
)

const (
	TypeInner     byte = 0x10 // phone → agw
	TypeInnerOut  byte = 0x90 // agw → phone
	DefaultSocket      = relay.DefaultSocket
	HelperUID          = 1337
	OwnedWait          = 500 * time.Millisecond
	QueueRecords       = 64
	WriteTimeout       = time.Second
)

// drainWait bounds how long a window whose write failed stays open while the reader relays what the helper already
// sent: the helper's last records (the inner session's BYE) come right before its close, which is what failed the
// write.
const drainWait = 250 * time.Millisecond

// Config is the relay's socket and checks. The zero value of each field is the car's.
type Config struct {
	Socket     string                           // default DefaultSocket
	HelperUID  int                              // the relay socket's only accepted server; 0 means HelperUID: never root
	PeerUID    func(net.Conn) (int, error)      // default peercred.PeerUID
	RouteCheck func(ctx context.Context) error  // default guards.RouteOff(ctx, guards.DefaultTailscaleSock)
	SocketUID  func(path string) (int, error)   // default: Lstat's Stat_t.Uid
	Logf       func(format string, args ...any) // the relay's reasons; default log.Printf, agw's log
}

type relayCap struct{ cfg Config }

// New returns the relay to the helper at DefaultSocket, as uid HelperUID.
func New() capability.Capability { return NewWithConfig(Config{}) }

// NewWithConfig returns the relay with another socket, uid or checks (tests).
func NewWithConfig(cfg Config) capability.Capability {
	if cfg.Socket == "" {
		cfg.Socket = DefaultSocket
	}
	if cfg.HelperUID == 0 {
		cfg.HelperUID = HelperUID
	}
	if cfg.PeerUID == nil {
		cfg.PeerUID = peercred.PeerUID
	}
	if cfg.RouteCheck == nil {
		cfg.RouteCheck = func(ctx context.Context) error { return guards.RouteOff(ctx, guards.DefaultTailscaleSock) }
	}
	if cfg.SocketUID == nil {
		cfg.SocketUID = lstatUID
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &relayCap{cfg: cfg}
}

// lstatUID is the owner of the file at path itself, never of a symlink's target.
func lstatUID(path string) (int, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return -1, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, errors.New("no owner")
	}
	return int(st.Uid), nil
}

// Spec: both types carry at most one INNER record, on the priority lane. The inner session has its own TICKs.
func (*relayCap) Spec() capability.Spec {
	return capability.Spec{
		Name:    "SUMMON",
		ID:      proto.CapSummon, // Host().SummonOpen() keys on it: CAMERA's rules during a session
		In:      map[byte]int{TypeInner: relay.MaxInner},
		Out:     map[byte]int{TypeInnerOut: relay.MaxInner},
		LeaseMS: 20000,
	}
}

// PreArm runs the R1 route gate, then checks that the relay socket is the helper's. A refusal is logged once, with
// its reason. Neither the log nor the error names any detail of the route check: they never echo tailscaled's prefs.
func (c *relayCap) PreArm(ctx context.Context, _ []byte) error {
	if err := c.preArm(ctx); err != nil {
		c.cfg.Logf("summonrelay: PreArm refused: %v", err)
		return fmt.Errorf("summonrelay: %w", err)
	}
	return nil
}

// preArm is PreArm's checks; its error is the reason, without the package prefix.
func (c *relayCap) preArm(ctx context.Context) error {
	if err := c.cfg.RouteCheck(ctx); err != nil {
		return errors.New("the route gate refused")
	}
	fi, err := os.Lstat(c.cfg.Socket)
	if err != nil {
		return fmt.Errorf("no relay socket: %w", err)
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return errors.New("the relay path is not a socket")
	}
	switch uid, err := c.cfg.SocketUID(c.cfg.Socket); {
	case err != nil:
		return fmt.Errorf("the relay socket's owner cannot be read (%v), and must be uid %d", err, c.cfg.HelperUID)
	case uid != c.cfg.HelperUID:
		return fmt.Errorf("the relay socket is owned by uid %d, not %d", uid, c.cfg.HelperUID)
	}
	return nil
}

// Open connects to the helper, checked as connect says, and starts the relay. A failure is logged once, with its
// reason. The ARM params are not used.
func (c *relayCap) Open(ctx context.Context, w capability.Window, _ json.RawMessage) (capability.Handler, error) {
	conn, in, err := connect(ctx, c.cfg.Socket, c.cfg.HelperUID, c.cfg.PeerUID, OwnedWait)
	if err != nil {
		c.cfg.Logf("summonrelay: Open failed: %v", err)
		return nil, fmt.Errorf("summonrelay: %w", err)
	}
	h := &handler{
		logf:       c.cfg.Logf,
		w:          w,
		conn:       conn,
		out:        relay.NewWriter(conn),
		in:         in,
		q:          make(chan []byte, QueueRecords),
		readerDone: make(chan struct{}),
		stop:       make(chan struct{}),
	}
	h.wg.Add(3)
	go h.watch()
	go h.reader()
	go h.writer()
	return h, nil
}

// Probe checks the relay socket as Open does, then closes it without sending a record: `agw probe-summon`. wait
// bounds the OWNED read: OwnedWait, to replay Open exactly. It logs nothing; its caller prints the error.
func Probe(ctx context.Context, socket string, uid int, wait time.Duration) error {
	conn, _, err := connect(ctx, socket, uid, peercred.PeerUID, wait)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// connect dials the relay socket within ctx. Its server must be uid, which the socket's owner cannot fake: a
// connection served by anyone else is closed before anything is read. The first record, within wait and ctx, must be
// OWNED: the helper sends it only while it owns the bus name and is healthy, and otherwise closes. It returns the
// connection, with no deadline, and its reader, past OWNED.
func connect(ctx context.Context, socket string, uid int, peerUID func(net.Conn) (int, error), wait time.Duration) (net.Conn, *relay.Reader, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (net.Conn, *relay.Reader, error) {
		conn.Close()
		return nil, nil, err
	}
	got, err := peerUID(conn)
	if err != nil {
		return fail(fmt.Errorf("the relay socket's server uid: %w", err))
	}
	if got != uid {
		return fail(fmt.Errorf("the relay socket is served by uid %d, not %d", got, uid))
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return fail(err)
	}
	// The OWNED read ends at wait, or at ctx's end, its deadline included.
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Unix(1, 0)) })
	in := relay.NewReader(conn, true) // its first record is OWNED, or an error
	typ, _, err := in.Next()
	if !stop() {
		return fail(fmt.Errorf("waiting for OWNED: %w", context.Cause(ctx)))
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fail(fmt.Errorf("no OWNED within %v", wait))
	case errors.Is(err, io.EOF):
		return fail(errors.New("closed before OWNED: the helper refused this uid, does not own the bus name, or is " +
			"unhealthy"))
	case err != nil:
		return fail(fmt.Errorf("first record: %w", err))
	case typ != relay.RecOwned:
		return fail(fmt.Errorf("first record 0x%02x is not OWNED", typ))
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return conn, in, nil
}

// handler relays one window. Three goroutines run until the socket is closed: watch, reader and writer. Teardown
// waits for all three.
type handler struct {
	logf func(format string, args ...any)
	w    capability.Window
	conn net.Conn
	out  *relay.Writer // to the helper
	in   *relay.Reader // from the helper, past OWNED
	q    chan []byte   // the phone's bodies, for the writer

	failed     atomic.Bool   // a write failed or missed WriteTimeout: Frame discards, and the window ends "drop"
	endLogged  atomic.Bool   // logEnd logged why the relay ends the window
	readerDone chan struct{} // the reader returned
	stop       chan struct{} // closed by shut, with the socket
	shutOnce   sync.Once
	wg         sync.WaitGroup
}

// Frame queues a 0x10 body for the writer; it never blocks. A full queue (the helper is not reading) is an error,
// which ends the window. After a failed write the body is discarded instead: the window is about to close with
// "drop", and a refusal would close it with "error" before the helper's last records reach the phone.
func (h *handler) Frame(t byte, body []byte) error {
	if t != TypeInner {
		return fmt.Errorf("summonrelay: frame type 0x%02x", t)
	}
	if h.failed.Load() {
		return nil
	}
	select {
	case h.q <- append([]byte(nil), body...): // body is valid only during Frame
		return nil
	default:
		return errors.New("summonrelay: the queue to the helper is full")
	}
}

// Teardown closes the socket, if the window's end did not already, waits for the goroutines, and then clears the link
// flag, so that no LINK read before the close can set it again.
func (h *handler) Teardown(string) {
	h.shut()
	h.wg.Wait()
	h.w.Host().SetSummonLinkDegraded(false)
}

// shut closes the socket, once: the helper reads EOF and runs H4, and the goroutines end.
func (h *handler) shut() {
	h.shutOnce.Do(func() {
		close(h.stop)
		h.conn.Close()
	})
}

// watch closes the socket as soon as the window is done, not only in Teardown.
func (h *handler) watch() {
	defer h.wg.Done()
	select {
	case <-h.w.Done():
	case <-h.stop:
	}
	h.shut()
}

// logEnd logs why the relay ends its window: at most once per window, so only the first cause. It logs nothing
// once the window is done or the socket was closed on our side: the core's close, which the core logs, then ends the
// reads and writes too. No reason holds any of the inner stream: record types and sizes, and socket errors only.
func (h *handler) logEnd(format string, args ...any) {
	select {
	case <-h.stop:
		return
	case <-h.w.Done():
		return
	default:
	}
	if h.endLogged.CompareAndSwap(false, true) {
		h.logf("summonrelay: the window ends: "+format, args...)
	}
}

// reader relays the helper's records until the socket ends: INNER to the phone as 0x90, LINK to the Host. Any read
// error ends the window, not only EOF: on Linux the helper's close can read as ECONNRESET.
func (h *handler) reader() {
	defer h.wg.Done()
	defer close(h.readerDone)
	for {
		typ, p, err := h.in.Next()
		if err != nil {
			h.logEnd("reading the helper's records: %v", err)
			h.w.Close(h.endReason())
			return
		}
		switch typ {
		case relay.RecInner:
			if !h.w.TrySend(TypeInnerOut, p) { // the priority lane is full, or the window is closing
				h.logEnd("the phone's priority lane refused a 0x%02x of %d bytes (full, or the window was closing)",
					TypeInnerOut, len(p))
				h.w.Close("drop")
				return
			}
		case relay.RecLink:
			h.w.Host().SetSummonLinkDegraded(p[0] == 1)
		default: // the Reader refuses a second OWNED
			h.logEnd("the helper sent record 0x%02x after OWNED", typ)
			h.w.Close(h.endReason())
			return
		}
	}
}

// endReason is the window's reason when the socket's reads end: "drop" once a write failed (R13), else "close".
func (h *handler) endReason() string {
	if h.failed.Load() {
		return "drop"
	}
	return "close"
}

// writer writes the queued bodies as INNER records, each within WriteTimeout.
func (h *handler) writer() {
	defer h.wg.Done()
	for {
		var p []byte
		select {
		case p = <-h.q:
		case <-h.stop:
			return
		}
		h.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
		if err := h.out.Inner(p); err != nil {
			h.logEnd("writing to the helper (%v deadline): %v", WriteTimeout, err)
			h.writeFailed()
			return
		}
	}
}

// writeFailed ends the window with "drop" after a write that failed or missed WriteTimeout (R13). It first gives the
// reader up to drainWait to relay what the helper sent before it closed, so the phone still sees the inner
// session's BYE; the abort and the disconnect have gone out either way. If the socket was closed meanwhile (the
// window ended, or Teardown), there is nothing left to do.
func (h *handler) writeFailed() {
	h.failed.Store(true)
	t := time.NewTimer(drainWait)
	defer t.Stop()
	select {
	case <-h.readerDone:
	case <-t.C:
	case <-h.stop:
		return
	}
	h.w.Close("drop")
	h.shut()
}
