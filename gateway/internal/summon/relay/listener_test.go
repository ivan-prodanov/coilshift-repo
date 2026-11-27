package relay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The listener tests play agw on a real unix socket and the server core on the connections Accept returns. Socket
// paths live under os.MkdirTemp("", "rl"): darwin caps them at 104 bytes, which t.TempDir() can pass.

const wait = 2 * time.Second // the bound on anything a test waits for that should happen

func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "relay.sock")
}

// logs records the listener's lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (g *logs) Logf(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lines = append(g.lines, fmt.Sprintf(format, args...))
}

func (g *logs) has(sub string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, l := range g.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// await waits for a line containing sub. The listener closes a refused socket before it logs, so agw can read EOF
// first.
func (g *logs) await(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !g.has(sub) {
		if time.Now().After(deadline) {
			t.Fatalf("no log line contains %q:\n%s", sub, g)
		}
		time.Sleep(time.Millisecond)
	}
}

func (g *logs) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.lines, "\n")
}

// listen starts a listener that allows this process's uid. Unless cfg says otherwise it is ready.
func listen(t *testing.T, cfg ListenConfig) (*Listener, *logs) {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = sockPath(t)
	}
	cfg.AllowUID = os.Getuid()
	if cfg.Ready == nil {
		cfg.Ready = func() bool { return true }
	}
	g := &logs{}
	cfg.Logf = g.Logf
	l, err := Listen(cfg)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, g
}

type accepted struct {
	c   net.Conn
	err error
}

// acceptAsync calls Accept once, on its own goroutine, as the core's Serve does.
func acceptAsync(l *Listener) <-chan accepted {
	ch := make(chan accepted, 1)
	go func() {
		c, err := l.Accept()
		ch <- accepted{c, err}
	}()
	return ch
}

// accept waits for an acceptAsync result, which must be a connection.
func accept(t *testing.T, ch <-chan accepted) net.Conn {
	t.Helper()
	select {
	case a := <-ch:
		if a.err != nil {
			t.Fatalf("Accept: %v", a.err)
		}
		t.Cleanup(func() { a.c.Close() })
		return a.c
	case <-time.After(wait):
		t.Fatal("Accept did not return")
	}
	return nil
}

// notAccepted checks that Accept has not returned.
func notAccepted(t *testing.T, ch <-chan accepted) {
	t.Helper()
	select {
	case a := <-ch:
		t.Fatalf("Accept returned (%v, %v)", a.c, a.err)
	default:
	}
}

// dial connects to the listener as agw does.
func dial(t *testing.T, l *Listener) *net.UnixConn {
	t.Helper()
	c, err := net.DialUnix("unix", nil, l.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// record is one record agw read, or the error that ended its reads.
type record struct {
	typ byte
	p   []byte
	err error
	at  time.Time
}

// peer is agw's end of a relay connection. A goroutine reads its records, as agw's reader does (OWNED first), until
// an error.
type peer struct {
	c    *net.UnixConn
	w    *Writer
	recs chan record
}

func dialPeer(t *testing.T, l *Listener) *peer {
	t.Helper()
	c := dial(t, l)
	p := &peer{c: c, w: NewWriter(c), recs: make(chan record, 64)}
	go func() {
		r := NewReader(c, true)
		for {
			typ, b, err := r.Next()
			p.recs <- record{typ, b, err, time.Now()}
			if err != nil {
				return
			}
		}
	}()
	return p
}

func (p *peer) next(t *testing.T) record {
	t.Helper()
	select {
	case r := <-p.recs:
		return r
	case <-time.After(wait):
		t.Fatal("agw read nothing")
	}
	return record{}
}

// expect waits for a record of type typ.
func (p *peer) expect(t *testing.T, typ byte) record {
	t.Helper()
	r := p.next(t)
	if r.err != nil || r.typ != typ {
		t.Fatalf("agw read 0x%02x % x (err %v), want record 0x%02x", r.typ, r.p, r.err, typ)
	}
	return r
}

// expectEOF waits for the helper to close the connection with no further record.
func (p *peer) expectEOF(t *testing.T) record {
	t.Helper()
	r := p.next(t)
	if r.err != io.EOF {
		t.Fatalf("agw read 0x%02x % x (err %v), want EOF", r.typ, r.p, r.err)
	}
	return r
}

// quiet checks that agw reads nothing for d.
func (p *peer) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case r := <-p.recs:
		t.Fatalf("agw read 0x%02x % x (err %v), want nothing", r.typ, r.p, r.err)
	case <-time.After(d):
	}
}

// handOff dials a peer, which reads OWNED and sends first as its first INNER record; the core accepts it.
func handOff(t *testing.T, l *Listener, first string) (*peer, net.Conn) {
	t.Helper()
	p := dialPeer(t, l)
	p.expect(t, RecOwned)
	if err := p.w.Inner([]byte(first)); err != nil {
		t.Fatal(err)
	}
	return p, accept(t, acceptAsync(l))
}

// readN reads exactly n bytes from the core's side.
func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatalf("core Read: %v", err)
	}
	c.SetReadDeadline(time.Time{})
	return b
}

func TestSocketMode0660(t *testing.T) {
	l, _ := listen(t, ListenConfig{})
	fi, err := os.Lstat(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%v is not a socket", fi.Mode())
	}
	if fi.Mode().Perm() != 0o660 {
		t.Fatalf("mode %v, want 0660", fi.Mode().Perm())
	}
	if uid := int(fi.Sys().(*syscall.Stat_t).Uid); uid != os.Getuid() {
		t.Fatalf("owner %d, want %d", uid, os.Getuid())
	}
}

// TestListenRemovesStale: whatever is at the path is removed before the listen.
func TestListenRemovesStale(t *testing.T) {
	path := sockPath(t)
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, _ := listen(t, ListenConfig{Path: path})
	dialPeer(t, l).expect(t, RecOwned)
}

// TestListenRequires: a socket path and a Ready predicate are required.
func TestListenRequires(t *testing.T) {
	if _, err := Listen(ListenConfig{Ready: func() bool { return true }}); err == nil {
		t.Fatal("Listen with no path succeeded")
	}
	path := sockPath(t)
	if _, err := Listen(ListenConfig{Path: path}); err == nil {
		t.Fatal("Listen with no Ready succeeded")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused Listen left %s (%v)", path, err)
	}
}

// TestPeerUIDRefused: a peer whose uid is not AllowUID, or whose uid cannot be read, reads EOF with no record, and
// the refusal is logged. The listener goes on to the next connection.
func TestPeerUIDRefused(t *testing.T) {
	other := os.Getuid() + 1
	var uid atomic.Int64
	uid.Store(int64(other))
	var fail atomic.Bool
	l, g := listen(t, ListenConfig{PeerUID: func(net.Conn) (int, error) {
		if fail.Load() {
			return -1, errors.New("no credentials")
		}
		return int(uid.Load()), nil
	}})
	acc := acceptAsync(l)

	dialPeer(t, l).expectEOF(t)
	g.await(t, fmt.Sprintf("uid %d", other))
	fail.Store(true)
	dialPeer(t, l).expectEOF(t)
	g.await(t, "no credentials")

	fail.Store(false)
	uid.Store(int64(os.Getuid()))
	dialPeer(t, l).expect(t, RecOwned)
	notAccepted(t, acc)
}

// TestNotReadyNoOwned: while Ready is false a connection is closed with no record; once it is true, OWNED is sent.
func TestNotReadyNoOwned(t *testing.T) {
	var ready atomic.Bool
	l, g := listen(t, ListenConfig{Ready: ready.Load})
	dialPeer(t, l).expectEOF(t)
	g.await(t, "not ready")
	ready.Store(true)
	dialPeer(t, l).expect(t, RecOwned)
}

// TestOwnedThenHandoffOnFirstInner: OWNED comes first; Accept returns only once the first INNER record arrives, and
// the connection's first Read returns that payload.
func TestOwnedThenHandoffOnFirstInner(t *testing.T) {
	l, _ := listen(t, ListenConfig{})
	acc := acceptAsync(l)
	p := dialPeer(t, l)
	p.expect(t, RecOwned)
	select {
	case a := <-acc:
		t.Fatalf("Accept returned (%v, %v) before the first INNER", a.c, a.err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := p.w.Inner([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	c := accept(t, acc)
	c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("first Read = %q, %v; want \"hello\"", buf[:n], err)
	}
}

// TestFirstWaitCloses: with no INNER within FirstWait, the connection is closed and never handed off, and the
// listener goes on to the next.
func TestFirstWaitCloses(t *testing.T) {
	l, g := listen(t, ListenConfig{FirstWait: 100 * time.Millisecond})
	acc := acceptAsync(l)
	t0 := time.Now() // the deadline is set after the accept, so the close comes FirstWait after this or later
	p := dialPeer(t, l)
	p.expect(t, RecOwned)
	eof := p.expectEOF(t)
	if d := eof.at.Sub(t0); d < 100*time.Millisecond || d > wait {
		t.Fatalf("closed %v after the dial, want about 100 ms", d)
	}
	g.await(t, "no INNER")
	notAccepted(t, acc)
	dialPeer(t, l).expect(t, RecOwned)
}

// TestFirstWaitEndsAtHandoff: FirstWait bounds only the wait for the first INNER. The connection handed off has no
// deadline the core did not set.
func TestFirstWaitEndsAtHandoff(t *testing.T) {
	l, _ := listen(t, ListenConfig{FirstWait: 50 * time.Millisecond})
	p, c := handOff(t, l, "A")
	time.Sleep(150 * time.Millisecond)
	if err := p.w.Inner([]byte("B")); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 2)
		n, err := io.ReadFull(c, b) // no deadline of the test's own: the listener's must be gone
		got <- fmt.Sprintf("%q %v", b[:n], err)
	}()
	select {
	case g := <-got:
		if g != `"AB" <nil>` {
			t.Fatalf("core read %s, want \"AB\"", g)
		}
	case <-time.After(wait):
		t.Fatal("core Read did not return")
	}
	if _, err := c.Write([]byte("C")); err != nil {
		t.Fatalf("core Write: %v", err)
	}
	if r := p.expect(t, RecInner); string(r.p) != "C" {
		t.Fatalf("agw read %q, want \"C\"", r.p)
	}
}

// TestFirstRecordNotInner: a first record other than INNER closes the connection without a hand-off.
func TestFirstRecordNotInner(t *testing.T) {
	l, _ := listen(t, ListenConfig{})
	acc := acceptAsync(l)
	p := dialPeer(t, l)
	p.expect(t, RecOwned)
	if _, err := p.c.Write([]byte{0x02, 0x00, 0x01, 0x01}); err != nil { // a LINK, from agw
		t.Fatal(err)
	}
	p.expectEOF(t)
	notAccepted(t, acc)
	dialPeer(t, l).expect(t, RecOwned)
}

// TestOneAtATime: a second connection gets OWNED only after the core closed the first, which it does after Teardown.
// Until then it waits in the kernel's backlog.
func TestOneAtATime(t *testing.T) {
	l, _ := listen(t, ListenConfig{})
	_, a := handOff(t, l, "A")
	acc := acceptAsync(l)
	b := dialPeer(t, l)
	b.quiet(t, time.Second)
	notAccepted(t, acc)

	closed := time.Now()
	a.Close()
	owned := b.expect(t, RecOwned)
	if owned.at.Before(closed) {
		t.Fatal("B's OWNED came before the core closed A")
	}
	if err := b.w.Inner([]byte("B")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, accept(t, acc), 1); string(got) != "B" {
		t.Fatalf("B's first bytes %q", got)
	}
}

// TestSlotHeldUntilCoreCloses: the slot is freed by the core's Close only. agw going away, or breaking the record
// grammar (which closes the socket at once), fails the core's Read, but the next connection still waits for the core.
func TestSlotHeldUntilCoreCloses(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(p *peer) error
	}{
		{"agw closed", func(p *peer) error { return p.c.Close() }},
		{"LINK from agw", func(p *peer) error { return p.w.Link(true) }},
		{"OWNED from agw", func(p *peer) error { _, err := p.c.Write([]byte{0x03, 0x00, 0x00}); return err }},
		{"empty INNER", func(p *peer) error { _, err := p.c.Write([]byte{0x01, 0x00, 0x00}); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := listen(t, ListenConfig{})
			pa, a := handOff(t, l, "A")
			readN(t, a, 1)
			if err := tc.end(pa); err != nil {
				t.Fatal(err)
			}
			a.SetReadDeadline(time.Now().Add(wait))
			if n, err := a.Read(make([]byte, 8)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("core Read = %d, %v; want the connection's end", n, err)
			}
			if tc.name != "agw closed" {
				pa.expectEOF(t) // the helper closed the socket
			}

			acc := acceptAsync(l)
			b := dialPeer(t, l)
			b.quiet(t, 300*time.Millisecond)
			notAccepted(t, acc)
			a.Close()
			b.expect(t, RecOwned)
		})
	}
}

// TestSetLinkWritesRecord: SetLink writes a LINK record on the active connection only.
func TestSetLinkWritesRecord(t *testing.T) {
	l, _ := listen(t, ListenConfig{})
	l.SetLink(true) // no connection: nothing to write to

	pa, a := handOff(t, l, "A")
	l.SetLink(true)
	if r := pa.expect(t, RecLink); !bytes.Equal(r.p, []byte{1}) {
		t.Fatalf("LINK % x, want 01", r.p)
	}
	l.SetLink(false)
	if r := pa.expect(t, RecLink); !bytes.Equal(r.p, []byte{0}) {
		t.Fatalf("LINK % x, want 00", r.p)
	}

	// Once the core closed it, a flag goes nowhere: the next connection starts with OWNED and gets no old flag.
	a.Close()
	pa.expectEOF(t)
	l.SetLink(true)
	pb, _ := handOff(t, l, "B")
	pb.quiet(t, 100*time.Millisecond)
}

// TestNoLinkBeforeHandoff: OWNED is always the first record. A flag set while a connection is still before its
// hand-off is not written to it, before OWNED or after.
func TestNoLinkBeforeHandoff(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	l, _ := listen(t, ListenConfig{Ready: func() bool {
		entered <- struct{}{}
		<-gate
		return true
	}})
	t.Cleanup(open) // before l.Close, which waits for the accept goroutine
	p := dialPeer(t, l)
	select {
	case <-entered:
	case <-time.After(wait):
		t.Fatal("Ready was not called")
	}
	l.SetLink(true)
	open()
	p.expect(t, RecOwned)
	if err := p.w.Inner([]byte("A")); err != nil {
		t.Fatal(err)
	}
	accept(t, acceptAsync(l))
	p.quiet(t, 100*time.Millisecond)
}

// TestSetLinkNeverBlocks (R11): with agw not reading and the core's Write stuck, SetLink returns within 1 ms. The
// LINK writer holds the value it took when the Write got stuck; of the values set after that, only the latest is
// written once agw reads again.
func TestSetLinkNeverBlocks(t *testing.T) {
	l, _ := listen(t, ListenConfig{})
	c := dial(t, l)
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if typ, _, err := NewReader(c, true).Next(); err != nil || typ != RecOwned {
		t.Fatalf("first record 0x%02x %v, want OWNED", typ, err)
	}
	if err := NewWriter(c).Inner([]byte("A")); err != nil {
		t.Fatal(err)
	}
	core := accept(t, acceptAsync(l))

	// The core writes 4 MiB that agw does not read. The socket fills and the Write blocks, holding the connection's
	// record writer, so no LINK record can go out.
	big := bytes.Repeat([]byte{0x5a}, 4<<20)
	wrote := make(chan error, 1)
	go func() {
		_, err := core.Write(big)
		wrote <- err
	}()
	var hdr [3]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil { // the Write is under way; agw reads no more
		t.Fatal(err)
	}
	setLink := func(i int, v bool) {
		t.Helper()
		took := make(chan time.Duration, 1)
		go func() {
			t0 := time.Now()
			l.SetLink(v)
			took <- time.Since(t0)
		}()
		select {
		case d := <-took:
			if d > time.Millisecond {
				t.Fatalf("SetLink #%d took %v with agw not reading, want at most 1 ms", i, d)
			}
		case <-time.After(time.Second):
			t.Fatalf("SetLink #%d blocked with agw not reading", i)
		}
	}
	setLink(0, true)
	// Wait until the LINK writer took that value from the mailbox: it now waits behind the stuck Write with it.
	cc := core.(*conn)
	deadline := time.Now().Add(wait)
	for {
		cc.lmu.Lock()
		taken := !cc.linkSet
		cc.lmu.Unlock()
		if taken {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the LINK writer did not take the flag")
		}
		time.Sleep(time.Millisecond)
	}
	for i, v := range []bool{false, true, false, true} {
		setLink(i+1, v)
	}

	// agw reads again: all of the core's bytes, then exactly two LINK records, the held value and the latest. The
	// values between them were superseded and dropped.
	rest := NewReader(io.MultiReader(bytes.NewReader(hdr[:]), c), false)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := 0
	var links []byte
	for got < len(big) || len(links) < 2 {
		typ, p, err := rest.Next()
		if err != nil {
			t.Fatalf("after %d bytes and LINK values %v: %v", got, links, err)
		}
		switch typ {
		case RecInner:
			got += len(p)
		case RecLink:
			links = append(links, p[0])
		}
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("core Write: %v", err)
		}
	case <-time.After(wait):
		t.Fatal("the core's Write did not finish")
	}
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if typ, p, err := rest.Next(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("record 0x%02x % x %v after the two LINK records, want none", typ, p, err)
	}
	if !bytes.Equal(links, []byte{1, 1}) {
		t.Fatalf("LINK values %v, want [1 1]: the held value, then the latest", links)
	}
}

// TestConnStream: Read yields the INNER payloads as one stream, starting with the first; Write sends INNER records of
// at most MaxInner bytes.
func TestConnStream(t *testing.T) {
	l, _ := listen(t, ListenConfig{})
	p, c := handOff(t, l, "first")
	tail := bytes.Repeat([]byte("x"), 20000) // two records, more than a socket buffer on darwin
	sent := make(chan error, 1)
	go func() { // as agw's writer does, while the core reads
		err := p.w.Inner([]byte("-second-"))
		if err == nil {
			err = p.w.Inner(tail)
		}
		sent <- err
	}()
	want := append([]byte("first-second-"), tail...)
	if got := readN(t, c, len(want)); !bytes.Equal(got, want) {
		t.Fatal("the core read a different stream")
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 40000)
	for i := range data {
		data[i] = byte(i)
	}
	if n, err := c.Write(data); n != len(data) || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	var sizes []int
	var got []byte
	for len(got) < len(data) {
		r := p.expect(t, RecInner)
		sizes = append(sizes, len(r.p))
		got = append(got, r.p...)
	}
	if !bytes.Equal(got, data) || len(sizes) != 3 || sizes[0] != MaxInner || sizes[1] != MaxInner {
		t.Fatalf("agw got records of %v bytes, want 16384, 16384, 7232 rebuilding the data", sizes)
	}
	if c.LocalAddr().String() != l.Addr().String() {
		t.Fatalf("LocalAddr %v, want %v", c.LocalAddr(), l.Addr())
	}
}

// TestConnDeadlines: the deadline methods reach the unix connection.
func TestConnDeadlines(t *testing.T) {
	l, _ := listen(t, ListenConfig{})
	_, c := handOff(t, l, "A")
	readN(t, c, 1)
	t0 := time.Now()
	c.SetReadDeadline(t0.Add(50 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read past the read deadline: %v", err)
	}
	if d := time.Since(t0); d < 50*time.Millisecond {
		t.Fatalf("Read timed out after %v, want 50 ms", d)
	}
	c.SetWriteDeadline(time.Unix(1, 0))
	if _, err := c.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write past the write deadline: %v", err)
	}
	c.Close()

	_, c = handOff(t, l, "B")
	readN(t, c, 1)
	c.SetDeadline(time.Unix(1, 0))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read past SetDeadline: %v", err)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write past SetDeadline: %v", err)
	}
}

// TestClose: Close interrupts a connection waiting for its first INNER at once, unblocks Accept with net.ErrClosed,
// and leaves the connection the core holds open until the core closes it.
func TestClose(t *testing.T) {
	l, _ := listen(t, ListenConfig{}) // FirstWait is the default 10 s
	pa, a := handOff(t, l, "A")
	a.Close()
	pa.expectEOF(t)

	pb, b := handOff(t, l, "B")
	l.SetLink(true)
	pb.expect(t, RecLink)
	l2, _ := listen(t, ListenConfig{})
	acc := acceptAsync(l2)
	pending := dialPeer(t, l2)
	pending.expect(t, RecOwned)

	t0 := time.Now()
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("Close took %v: it must not wait out FirstWait", d)
	}
	pending.expectEOF(t)
	select {
	case r := <-acc:
		if !errors.Is(r.err, net.ErrClosed) {
			t.Fatalf("Accept = (%v, %v), want net.ErrClosed", r.c, r.err)
		}
	case <-time.After(wait):
		t.Fatal("Close did not unblock Accept")
	}
	if _, err := l2.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close: %v, want net.ErrClosed", err)
	}

	// The core's connection outlives the listener: it still carries INNER and LINK until the core closes it.
	l.Close()
	if _, err := b.Write([]byte("bye")); err != nil {
		t.Fatalf("Write after the listener closed: %v", err)
	}
	if r := pb.expect(t, RecInner); string(r.p) != "bye" {
		t.Fatalf("INNER %q, want \"bye\"", r.p)
	}
	l.SetLink(false)
	pb.expect(t, RecLink)
	b.Close()
	pb.expectEOF(t)
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close: %v, want net.ErrClosed", err)
	}
}

// TestNoGoroutineLeaks: a peer that vanishes at any step, and Close at any step, leave no goroutine behind.
func TestNoGoroutineLeaks(t *testing.T) {
	base := runtime.NumGoroutine()
	var clients []*net.UnixConn
	func() {
		gone := make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(gone) }) }
		var calls atomic.Int32
		l, _ := listen(t, ListenConfig{Ready: func() bool {
			if calls.Add(1) == 1 {
				<-gone // the first peer is gone before its OWNED
			}
			return true
		}})
		t.Cleanup(release)
		c := dial(t, l)
		c.Close()
		release()

		p := dialPeer(t, l) // gone after OWNED, before its first INNER
		clients = append(clients, p.c)
		p.expect(t, RecOwned)
		p.c.Close()

		q, cq := handOff(t, l, "q") // gone mid-session, with its LINK writer running
		clients = append(clients, q.c)
		readN(t, cq, 1)
		l.SetLink(true)
		q.expect(t, RecLink)
		q.c.Close()
		cq.SetReadDeadline(time.Now().Add(wait))
		if _, err := cq.Read(make([]byte, 8)); err != io.EOF {
			t.Fatalf("core Read after agw left: %v, want EOF", err)
		}
		cq.Close()

		r := dialPeer(t, l) // pending at Close
		clients = append(clients, r.c)
		r.expect(t, RecOwned)
		l.Close()
		r.expectEOF(t)

		l2, _ := listen(t, ListenConfig{}) // active at Close
		s, cs := handOff(t, l2, "s")
		clients = append(clients, s.c)
		l2.SetLink(true)
		s.expect(t, RecLink)
		l2.Close()
		cs.Close()
		s.expectEOF(t)
	}()
	for _, c := range clients {
		c.Close()
	}
	deadline := time.Now().Add(wait)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak: %d > %d\n%s", runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(5 * time.Millisecond)
	}
}
