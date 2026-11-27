package camhelper

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"airgapp/gateway/camhelper/client"
	"airgapp/gateway/camhelper/proto"
)

// ---- fake car ----

type fakeCar struct {
	mu       sync.Mutex
	ops      []string
	vals     map[DVRead]string
	links    string
	linkErr  error
	reads    int
	delay    time.Duration   // every write/gain takes this long (a slow car tool)
	failW    map[DVWrite]int // fail this write the next n times
	stuck    map[DVRead]bool // the readback keeps showing 1
	writeHit map[DVWrite]int
	onRead   func() // called (without the lock) at the start of every Read
}

func newFakeCar() *fakeCar {
	return &fakeCar{vals: map[DVRead]string{
		ReadGear:      "(true, 'P')",
		ReadSentry:    "(true, 'Armed')",
		ReadClimate:   "(true, 'Off')",
		ReadCamAccess: "(true, 'true')",
	}, failW: map[DVWrite]int{}, stuck: map[DVRead]bool{}, writeHit: map[DVWrite]int{}}
}

// readback is what get-local-dv shows after each write.
var readback = map[DVWrite]struct {
	r DVRead
	v string
}{
	RequestAudio0: {ReadRequestAudio, "(true, 'false')"}, RequestAudio1: {ReadRequestAudio, "(true, 'true')"},
	SessionActive0: {ReadSessionActive, "(true, 'false')"}, SessionActive1: {ReadSessionActive, "(true, 'true')"},
	Selectable0: {ReadSelectable, "(true, '0')"}, Selectable1: {ReadSelectable, "(true, '1')"},
	Selectable3: {ReadSelectable, "(true, '3')"}, Selectable9: {ReadSelectable, "(true, '9')"},
	Selectable11: {ReadSelectable, "(true, '11')"},
}

func (c *fakeCar) Write(w DVWrite) error {
	c.mu.Lock()
	d := c.delay
	c.mu.Unlock()
	time.Sleep(d)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeHit[w]++
	if c.failW[w] > 0 {
		c.failW[w]--
		return errors.New("set-local-dv: timed out")
	}
	c.ops = append(c.ops, w.String())
	if rb, ok := readback[w]; ok {
		c.vals[rb.r] = rb.v
	}
	return nil
}

func (c *fakeCar) Read(r DVRead) (string, error) {
	c.mu.Lock()
	hook := c.onRead
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if c.stuck[r] {
		return "(true, 'true')", nil
	}
	return c.vals[r], nil
}

func (c *fakeCar) SetGain(g Gain) error {
	c.mu.Lock()
	d := c.delay
	c.mu.Unlock()
	time.Sleep(d)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, "gain="+strconv.Itoa(g.DB()))
	return nil
}

func (c *fakeCar) Links() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.links, c.linkErr
}

func (c *fakeCar) set(r DVRead, raw string) {
	c.mu.Lock()
	c.vals[r] = raw
	c.mu.Unlock()
}

func (c *fakeCar) setLinks(l string) {
	c.mu.Lock()
	c.links = l
	c.mu.Unlock()
}

// take returns and clears the recorded ops.
func (c *fakeCar) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.ops
	c.ops = nil
	return o
}

func (c *fakeCar) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ops...)
}

// Short names for the DV writes.
var (
	ra0, ra1 = RequestAudio0.String(), RequestAudio1.String()
	sa0, sa1 = SessionActive0.String(), SessionActive1.String()
	sel0     = Selectable0.String()
	sel1     = Selectable1.String()
	sel3     = Selectable3.String()
)

const feedersOK = "TDM-playback:playback_9\n  |<- webcam:monitor_1\nTDM-playback:playback_10\n  |<- webcam:monitor_2\nwebcam:monitor_1\n  |-> TDM-playback:playback_9\n"

// ---- fake spawner ----

type fakeChild struct {
	pid        int
	spec       ChildSpec
	outR       *io.PipeReader
	outW       *io.PipeWriter
	inR        *io.PipeReader
	inW        *io.PipeWriter
	done       chan struct{}
	once       sync.Once
	err        error
	stopped    atomic.Bool
	stdinBytes atomic.Int64
	stdinMu    sync.Mutex
	stdinData  []byte // the first 4 MiB written to the child's stdin
}

func (c *fakeChild) stdin() []byte {
	c.stdinMu.Lock()
	defer c.stdinMu.Unlock()
	return append([]byte(nil), c.stdinData...)
}

func (c *fakeChild) Pid() int { return c.pid }
func (c *fakeChild) Stdin() io.WriteCloser {
	if !c.spec.Stdin {
		return nil
	}
	return c.inW
}
func (c *fakeChild) Stdout() io.ReadCloser {
	if !c.spec.Stdout {
		return nil
	}
	return c.outR
}
func (c *fakeChild) Done() <-chan struct{} { return c.done }
func (c *fakeChild) ExitErr() error        { <-c.done; return c.err }
func (c *fakeChild) Stop() {
	c.stopped.Store(true)
	c.exit(errors.New("signal: terminated"))
}

func (c *fakeChild) exit(err error) {
	c.once.Do(func() {
		c.err = err
		c.outW.Close()
		c.inR.Close()
		close(c.done)
	})
}

func (c *fakeChild) is(prefix string) bool { return strings.HasSuffix(c.spec.Argv[0], prefix) }

type fakeSpawner struct {
	mu       sync.Mutex
	pid      int
	children []*fakeChild
	fail     bool
	onStart  func(*fakeChild)
}

func (s *fakeSpawner) Start(spec ChildSpec) (Child, error) {
	s.mu.Lock()
	if s.fail {
		s.mu.Unlock()
		return nil, errors.New("spawn refused")
	}
	s.pid++
	c := &fakeChild{pid: 10000 + s.pid, spec: spec, done: make(chan struct{})}
	c.outR, c.outW = io.Pipe()
	c.inR, c.inW = io.Pipe()
	s.children = append(s.children, c)
	hook := s.onStart
	s.mu.Unlock()
	if spec.Stdin {
		go func() { // drain the writer's stdin like pw-cat would
			buf := make([]byte, 4096)
			for {
				n, err := c.inR.Read(buf)
				c.stdinBytes.Add(int64(n))
				c.stdinMu.Lock()
				if len(c.stdinData) < 4<<20 {
					c.stdinData = append(c.stdinData, buf[:n]...)
				}
				c.stdinMu.Unlock()
				if err != nil {
					return
				}
			}
		}()
	}
	if hook != nil {
		go hook(c)
	}
	return c, nil
}

func (s *fakeSpawner) all() []*fakeChild {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fakeChild(nil), s.children...)
}

// find waits for a child whose argv contains want.
func (s *fakeSpawner) find(t *testing.T, want string) *fakeChild {
	t.Helper()
	var got *fakeChild
	waitFor(t, "child "+want, func() bool {
		for _, c := range s.all() {
			if strings.Contains(strings.Join(c.spec.Argv, " "), want) && !c.stopped.Load() {
				got = c
				return true
			}
		}
		return false
	})
	return got
}

// ---- rig ----

type rig struct {
	t     *testing.T
	srv   *Server
	car   *fakeCar
	sp    *fakeSpawner
	sock  string
	state string
	cam   string
}

func newRig(t *testing.T, opts ...func(*Config)) *rig {
	t.Helper()
	dir, err := os.MkdirTemp("", "ch") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real // macOS: /var -> /private/var; the helper passes resolved paths on
	}
	r := &rig{t: t, car: newFakeCar(), sp: &fakeSpawner{}, sock: filepath.Join(dir, "s"),
		state: filepath.Join(dir, "state"), cam: filepath.Join(dir, "TeslaCam")}
	os.MkdirAll(filepath.Join(r.state, cutSubdir), 0o700)
	os.MkdirAll(r.cam, 0o755)
	cfg := Config{
		Car: r.car, Spawner: r.sp, StateDir: r.state, TeslaCam: r.cam, AllowUID: os.Getuid(),
		Log: log.New(io.Discard, "", 0), Tick: time.Hour, EdgeDip: time.Millisecond, RouteGrace: time.Nanosecond,
		// No test reads the host's /run, /proc or signals a real process: SUMMON mode is off unless a test turns
		// it on, the watcher idles unless a test needs it, and the shutdown sweep looks at an empty fake /proc
		// (review T9, T11).
		SummonOK:     func() (bool, string) { return false, "" },
		SummonPoll:   time.Hour,
		SweepProcDir: filepath.Join(dir, "proc"),
		SweepKill: func(pid int, sig syscall.Signal) error {
			t.Errorf("the shutdown sweep signalled pid %d", pid)
			return nil
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	r.srv = New(cfg)
	ln, err := net.Listen("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	go r.srv.Serve(ln)
	t.Cleanup(r.srv.Shutdown)
	return r
}

type au struct {
	port  uint16
	flags byte
	body  []byte
}

type conn struct {
	*client.Client
	aus chan au
}

func (r *rig) dial() *conn {
	r.t.Helper()
	c := &conn{aus: make(chan au, 256)}
	cl, err := client.Dial(r.sock, func(port uint16, flags byte, body []byte) {
		select {
		case c.aus <- au{port, flags, body}:
		default:
		}
	})
	if err != nil {
		r.t.Fatal(err)
	}
	c.Client = cl
	r.t.Cleanup(func() { cl.Close() })
	return c
}

// ctx is a bounded context for one helper call (timer-only cancellation).
func ctx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	time.AfterFunc(6*time.Second, cancel)
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitOps waits until the car ops equal want (then clears them).
func (r *rig) waitOps(want ...string) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := r.car.snapshot()
		if strings.Join(got, ",") == strings.Join(want, ",") {
			r.car.take()
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("car ops\n got %v\nwant %v", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *rig) markers() []string {
	var out []string
	for _, m := range []string{markRequestAudio, markSessionAct, markSelectable, markGain} {
		if (markers{r.state}).has(m) {
			out = append(out, m)
		}
	}
	return out
}

func nextEvent(t *testing.T, c *conn, ev string) proto.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-c.Events():
			if e.Ev == ev {
				return e
			}
		case <-timeout:
			t.Fatalf("no %s event", ev)
		}
	}
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	if client.Code(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
