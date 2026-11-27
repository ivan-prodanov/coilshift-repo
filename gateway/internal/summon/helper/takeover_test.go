package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/carlink/bustest"
	"airgapp/gateway/internal/carlink/mockcar"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/summon"
	"airgapp/gateway/internal/summon/logq"
	"airgapp/gateway/internal/summon/marker"
	"airgapp/gateway/internal/supervise"
)

// The takeover tests run on a private dbus-daemon (bustest), with the mock QtCarServer on a connection of its own
// (mockcar.DBusPort). The test binary is re-executed (TestMain) as:
//   - SUMMONHELPER_TEST_CHILD, the stand-in for `summonhelper serve`: it owns the name, serves the two methods with
//     Reply 1, sets the session marker, emits H1 (so the mock car counts as connected) and writes its liveness pipe
//     while link.Healthy() holds (R4). When a liveness write fails (EPIPE) it notes it and exits. SIGUSR1 closes its
//     bus connection while the process runs on;
//   - SUMMONHELPER_TEST_PARENT: Supervise over the child mode, in a process of its own, so a test can stop or kill "the
//     parent" without stopping itself;
//   - SUMMONHELPER_TEST_SERVE: the real Serve (R3), whose Car wrapper makes the next Send block forever after SIGUSR1.
//
// Both child modes ignore SIGHUP and SIGTERM (ignoreHangups), so no signal but SIGKILL ends them: the supervisor's,
// or Linux's Pdeathsig.
//
// The re-executed processes find the private bus and their directory through the environment. The directory holds
// the session marker ("session") and what the children leave for the test: "started" (their first act), "pid",
// "epipe" and "armed".
const (
	envChild    = "SUMMONHELPER_TEST_CHILD"
	envParent   = "SUMMONHELPER_TEST_PARENT"
	envServe    = "SUMMONHELPER_TEST_SERVE"
	envBus      = "SUMMONHELPER_TEST_BUS"      // the private bus's address
	envDir      = "SUMMONHELPER_TEST_DIR"      // the directory of the marker and the notes
	envNoMarker = "SUMMONHELPER_TEST_NOMARKER" // set: the child sets no session marker
	envNoName   = "SUMMONHELPER_TEST_NONAME"   // set: the child takes no name and emits no H1, and beats ungated
	envDelay    = "SUMMONHELPER_TEST_DELAY"    // a duration: the child waits this long before its first liveness byte

	startup = 10 * time.Second // the bound on a re-executed process coming up: not what the tests measure
)

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(envParent) != "":
		os.Exit(testParent())
	case os.Getenv(envServe) != "":
		os.Exit(testServe())
	case os.Getenv(envChild) != "":
		os.Exit(testChild())
	}
	os.Exit(m.Run())
}

// childArgv re-executes the test binary. TestMain picks the mode from the environment; -test.run=^$ runs no test if
// none is set.
func childArgv() []string { return []string{os.Args[0], "-test.run=^$"} }

func note(dir, name string) { os.WriteFile(filepath.Join(dir, name), nil, 0o600) }

// exitOrphaned is the test children's safety net: a second after the child was re-parented (its supervisor is gone),
// it exits. A healthy child sees EPIPE on its next liveness write first (within 50 ms); this ends one whose link went
// unhealthy, which writes no more bytes and so would never see EPIPE. On Linux, Pdeathsig kills it first.
func exitOrphaned() {
	ppid := os.Getppid()
	go func() {
		for range time.Tick(50 * time.Millisecond) {
			if os.Getppid() != ppid {
				time.Sleep(time.Second)
				os.Exit(8)
			}
		}
	}()
}

// ignoreHangups makes a child mode ignore SIGHUP and SIGTERM. When its supervisor dies, the child's process group (a
// group of its own: Setpgid) is orphaned, and if the child is stopped the kernel sends the group SIGHUP, then SIGCONT
// (POSIX). That SIGHUP must not end a child that only Pdeathsig, a SIGKILL, should end (TestSupervisorKilledThenFinish).
// Nothing in these tests ends a child with SIGTERM: the supervisor's only signal is SIGKILL.
func ignoreHangups() { signal.Ignore(syscall.SIGHUP, syscall.SIGTERM) }

// writePID leaves the process's pid in dir/pid, whole (a rename).
func writePID(dir string) error {
	tmp := filepath.Join(dir, "pid.tmp")
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "pid"))
}

// testChild is SUMMONHELPER_TEST_CHILD.
func testChild() int {
	ignoreHangups()
	dir := os.Getenv(envDir)
	note(dir, "started")
	exitOrphaned()
	fail := func(code int, err error) int {
		fmt.Fprintf(os.Stderr, "test child: %v\n", err)
		return code
	}
	b, err := carlink.DialBus(os.Getenv(envBus))
	if err != nil {
		return fail(10, err)
	}
	link := carlink.NewLink(b)
	named := os.Getenv(envNoName) == ""
	var healthy func() bool // nil: always
	if named {
		if err := link.Acquire(); err != nil {
			return fail(11, err)
		}
		healthy = link.Healthy // R4
	}
	if err := b.Serve(carlink.Handlers{Reply: func() int32 { return 1 }}); err != nil { // R4
		return fail(12, err)
	}
	if named {
		if os.Getenv(envNoMarker) == "" {
			if err := marker.New(filepath.Join(dir, "session")).Set(); err != nil {
				return fail(13, err)
			}
		}
		if err := link.Connect(); err != nil {
			return fail(14, err)
		}
	}
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		<-usr1
		b.Close() // the connection drops, the process runs on, and link.Healthy() stops the liveness bytes
	}()
	if err := writePID(dir); err != nil {
		return fail(15, err)
	}
	if d, err := time.ParseDuration(os.Getenv(envDelay)); err == nil {
		time.Sleep(d)
	}
	stop, err := supervise.StartLiveness(healthy, func(error) {
		note(dir, "epipe")
		os.Exit(9)
	})
	if err != nil || stop == nil {
		return fail(16, fmt.Errorf("liveness: %v", err))
	}
	select {}
}

// testParent is SUMMONHELPER_TEST_PARENT: Supervise as `run` calls it, with a SIGTERM context and logq.
func testParent() int {
	dir := os.Getenv(envDir)
	os.Unsetenv(envParent)
	os.Setenv(envChild, "1")
	b, err := carlink.DialBus(os.Getenv(envBus))
	if err != nil {
		fmt.Fprintf(os.Stderr, "test parent: %v\n", err)
		return 20
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	lq := logq.New(os.Stderr, logq.Lines)
	err = Supervise(ctx, b, childArgv(), filepath.Join(dir, "session"), filepath.Join(dir, "live"), lq.Logf)
	if err != nil {
		lq.Logf("test parent: Supervise: %v", err)
	}
	lq.Flush(time.Second)
	if err != nil {
		return 1
	}
	return 0
}

// sigWedge is the SERVE mode's Car (R3): once SIGUSR1 armed it, the next Send blocks forever without reaching the
// Link, so the engine lock stays held, as in a wedged engine.
type sigWedge struct {
	summon.Car
	armed atomic.Bool
}

func (w *sigWedge) Send(data []byte) error {
	if w.armed.CompareAndSwap(true, false) {
		select {}
	}
	return w.Car.Send(data)
}

// testServe is SUMMONHELPER_TEST_SERVE: the real Serve on the private bus, with liveness, as `serve` runs it. Its
// files are in the directory: key.pem, relay.sock, session, live, log; it logs to serve.log.
func testServe() int {
	ignoreHangups()
	dir := os.Getenv(envDir)
	note(dir, "started")
	exitOrphaned()
	fail := func(code int, err error) int {
		fmt.Fprintf(os.Stderr, "test serve: %v\n", err)
		return code
	}
	b, err := carlink.DialBus(os.Getenv(envBus))
	if err != nil {
		return fail(30, err)
	}
	key, err := keys.Load(filepath.Join(dir, "key.pem"))
	if err != nil {
		return fail(31, err)
	}
	logFile, err := os.Create(filepath.Join(dir, "serve.log"))
	if err != nil {
		return fail(32, err)
	}
	w := &sigWedge{}
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		<-usr1
		w.armed.Store(true)
		note(dir, "armed")
	}()
	if err := writePID(dir); err != nil {
		return fail(33, err)
	}
	lq := logq.New(logFile, logq.Lines)
	err = Serve(context.Background(), Config{
		Bus: b, Oracle: authority.Fake{Role: 2, Owner: true}, Key: key, VIN: testVIN,
		Socket: filepath.Join(dir, "relay.sock"), AllowUID: os.Getuid(),
		MarkerPath: filepath.Join(dir, "session"), LivePath: filepath.Join(dir, "live"),
		LogPath: filepath.Join(dir, "log"), Logf: lq.Logf, Liveness: true,
		WrapCar: func(c summon.Car) summon.Car {
			w.Car = c
			return w
		},
	})
	lq.Flush(time.Second)
	return fail(34, fmt.Errorf("Serve returned: %v", err))
}

// ---- the rig ----

// tk is one takeover rig: a private bus with the mock car on it, and a directory for the marker and the children's
// notes. The test process's environment is the children's.
type tk struct {
	t      *testing.T
	bus    *bustest.Bus
	obs    *dbus.Conn // the test's own connection: name queries and car calls
	car    *mockcar.Car
	dir    string
	marker string
	live   string    // the live file: never the default under /run
	logs   *logLines // the in-process Supervise's and Finish's
}

func newTK(t *testing.T) *tk {
	t.Helper()
	bus := bustest.Start(t)
	dir, err := os.MkdirTemp("", "tk") // not t.TempDir(): the SERVE child's socket lives here (104-byte cap on darwin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	k := &tk{t: t, bus: bus, obs: bus.Conn(t), dir: dir, marker: filepath.Join(dir, "session"),
		live: filepath.Join(dir, "live"), logs: &logLines{}}
	k.car = mockcar.New(mockcar.Config{Port: mockcar.DBusPort(bus.Conn(t))})
	k.car.SetPose(carLat, carLon, 90, 0, "off", "P")
	ctx, cancel := context.WithCancel(context.Background())
	go k.car.Run(ctx)
	t.Cleanup(cancel)
	t.Setenv(envBus, bus.Addr())
	t.Setenv(envDir, dir)
	t.Setenv(envChild, "1")
	for _, e := range []string{envParent, envServe, envNoMarker, envNoName, envDelay} {
		t.Setenv(e, "")
		os.Unsetenv(e)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s\nthe car received %q", k.logs, k.carKinds(0))
			for _, f := range []string{"parent.log", "serve.log"} {
				if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
					t.Logf("%s:\n%s", f, b)
				}
			}
		}
	})
	return k
}

// dial is a new connection to the private bus, closed at cleanup.
func (k *tk) dial() *carlink.DBusBus {
	k.t.Helper()
	b, err := carlink.DialBus(k.bus.Addr())
	if err != nil {
		k.t.Fatal(err)
	}
	k.t.Cleanup(func() { b.Close() })
	return b
}

func (k *tk) driver() dbus.BusObject { return k.obs.BusObject() }

// queue is the name's owner and its queue, in order (ListQueuedOwners), or nil when it has no owner.
func (k *tk) queue() []string {
	var q []string
	if k.driver().Call("org.freedesktop.DBus.ListQueuedOwners", 0, carlink.WebrtcName).Store(&q) != nil {
		return nil
	}
	return q
}

func (k *tk) hasOwner() bool {
	var has bool
	if err := k.driver().Call("org.freedesktop.DBus.NameHasOwner", 0, carlink.WebrtcName).Store(&has); err != nil {
		k.t.Fatal(err)
	}
	return has
}

// callCar makes QtCarServer's CarserverToPeerCommsData call on the name, and returns the reply's value.
func (k *tk) callCar() (int32, error) {
	var reply map[string]dbus.Variant
	err := k.obs.Object(carlink.WebrtcName, carlink.WebrtcPath).Call(
		carlink.WebrtcIface+"."+carlink.MethodCarserverToPeerCommsData, 0, map[string]dbus.Variant{
			"uid": dbus.MakeVariant(carlink.PhoneUID), "channel": dbus.MakeVariant(carlink.ChannelTCP),
			"data": dbus.MakeVariant(`{"msg_type":"autopark:status"}`),
		}).Store(&reply)
	v, _ := reply[carlink.ReplyKey].Value().(int32)
	return v, err
}

// callSignaling makes QtCarServer's RemoteToLocalSignaling call on the name (a type 9), and returns the reply.
func (k *tk) callSignaling() (uint32, error) {
	var u uint32
	err := k.obs.Object(carlink.WebrtcName, carlink.WebrtcPath).Call(
		carlink.WebrtcIface+"."+carlink.MethodRemoteToLocalSignaling, 0, map[string]dbus.Variant{
			"type": dbus.MakeVariant(int32(9)), "uid": dbus.MakeVariant(carlink.PhoneUID),
			"session_id": dbus.MakeVariant(""),
		}).Store(&u)
	return u, err
}

// freed waits, without failing the test (it may run off the test goroutine), until the name has no owner.
func (k *tk) freed() {
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		var has bool
		if k.driver().Call("org.freedesktop.DBus.NameHasOwner", 0, carlink.WebrtcName).Store(&has) == nil && !has {
			return
		}
	}
}

// pid is the pid a child left in dir/name, or 0.
func (k *tk) pid(name string) int {
	b, err := os.ReadFile(filepath.Join(k.dir, name))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(string(b))
	return pid
}

// up waits until n connections hold or queue for the name, the child first and the parent last, and returns the
// child's pid.
func (k *tk) up(n int) int {
	k.t.Helper()
	until(k.t, startup, fmt.Sprintf("%d connections in the name's queue", n), func() bool { return len(k.queue()) == n })
	pid := k.pid("pid")
	if pid <= 1 {
		k.t.Fatalf("the child left no pid (%d)", pid)
	}
	return pid
}

// until is eventually with a bound of its own.
func until(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, d)
		}
		time.Sleep(time.Millisecond)
	}
}

// carKinds is what the car received after its first from inputs, by kind (rkind).
func (k *tk) carKinds(from int) []string {
	got := k.car.Received()
	if from > len(got) {
		return nil
	}
	var out []string
	for _, rec := range got[from:] {
		out = append(out, rkind(rec))
	}
	return out
}

// afterH1 waits for the car's first data_channels_ready, and returns the index of the input after it.
func (k *tk) afterH1() int {
	k.t.Helper()
	k.waitH1(1)
	for i, rec := range k.car.Received() {
		if rkind(rec) == kindH1 {
			return i + 1
		}
	}
	return 0
}

// waitH1 waits until the car has received n data_channels_ready, and returns how many inputs it has had.
func (k *tk) waitH1(n int) int {
	k.t.Helper()
	ok := k.car.WaitFor(func(got []mockcar.Received) bool {
		c := 0
		for _, rec := range got {
			if rkind(rec) == kindH1 {
				c++
			}
		}
		return c >= n
	}, startup)
	if !ok {
		k.t.Fatalf("the car did not receive %d data_channels_ready; it got %q", n, k.carKinds(0))
	}
	return len(k.car.Received())
}

// abortDisconnect waits until the car's inputs after the first from begin with cmd_abort, then the disconnect, and
// checks that the disconnect arrived within bound of at.
func (k *tk) abortDisconnect(from int, at time.Time, bound time.Duration) {
	k.t.Helper()
	k.car.WaitFor(func(got []mockcar.Received) bool { return len(got) >= from+2 }, wait)
	got := k.car.Received()
	if kinds := k.carKinds(from); len(kinds) < 2 || kinds[0] != typeAbort || kinds[1] != kindDisconnect {
		k.t.Fatalf("after its first %d inputs the car received %q, want cmd_abort, then the disconnect", from, kinds)
	}
	if d := got[from+1].At.Sub(at); d > bound {
		k.t.Fatalf("the disconnect reached the car %v after the trigger, want at most %v (the abort after %v)",
			d.Round(time.Millisecond), bound, got[from].At.Sub(at).Round(time.Millisecond))
	} else {
		k.t.Logf("abort %v, disconnect %v after the trigger", got[from].At.Sub(at).Round(time.Millisecond),
			d.Round(time.Millisecond))
	}
}

// settled checks, after a pause for anything still in flight, that the car's inputs after the first from are want.
func (k *tk) settled(from int, want ...string) {
	k.t.Helper()
	time.Sleep(100 * time.Millisecond)
	if got := k.carKinds(from); !slices.Equal(got, want) {
		k.t.Fatalf("after its first %d inputs the car received %q, want exactly %q", from, got, want)
	}
	if v := k.car.Violations(); len(v) > 0 {
		k.t.Fatalf("the mock car saw rules broken: %q", v)
	}
}

// noChild checks that no child process ever started.
func (k *tk) noChild() {
	k.t.Helper()
	if _, err := os.Lstat(filepath.Join(k.dir, "started")); !errors.Is(err, fs.ErrNotExist) {
		k.t.Fatalf("a child started (its note: %v)", err)
	}
}

func (k *tk) markerGone() bool {
	_, err := os.Lstat(k.marker)
	return errors.Is(err, fs.ErrNotExist)
}

// The live file's removal, as the takeover and Finish log it.
const (
	takeoverRemoved = "summonhelper: takeover removed the live file"
	finishRemoved   = "summonhelper: finish removed the live file"
)

// leaveLive leaves at k.live what a helper that died mid-session leaves: its pid, and no lock, which went with it.
// Supervise's own Finish removes it, so a takeover test leaves it once the child is up.
func (k *tk) leaveLive() {
	k.t.Helper()
	if err := os.WriteFile(k.live, []byte("123\n"), 0o600); err != nil {
		k.t.Fatal(err)
	}
}

func (k *tk) liveGone() bool {
	_, err := os.Lstat(k.live)
	return errors.Is(err, fs.ErrNotExist)
}

// connectCar leaves what a helper that died mid-session leaves on the bus: the car connected by its H1. The
// connection that sent it is gone, and so is its name.
func (k *tk) connectCar() {
	k.t.Helper()
	b, err := carlink.DialBus(k.bus.Addr())
	if err != nil {
		k.t.Fatal(err)
	}
	defer b.Close()
	link := carlink.NewLink(b)
	if err := link.Acquire(); err != nil {
		k.t.Fatal(err)
	}
	if err := link.Connect(); err != nil {
		k.t.Fatal(err)
	}
	until(k.t, wait, "the car connected", k.car.Connected)
	b.Close()
	until(k.t, wait, "the dead session's name released", func() bool { return !k.hasOwner() })
}

// openSession is connectCar plus the session marker, as the engine sets it before H1.
func (k *tk) openSession() {
	k.t.Helper()
	if err := marker.New(k.marker).Set(); err != nil {
		k.t.Fatal(err)
	}
	k.connectCar()
}

// sup is one in-process Supervise.
type sup struct {
	done chan struct{}
	err  error
}

// supervise runs Supervise on bus over the child mode. A nil logf logs to k.logs. Cleanup ends it and waits for it.
func (k *tk) supervise(bus carlink.Bus, logf func(string, ...any)) *sup {
	k.t.Helper()
	if logf == nil {
		logf = k.logs.Logf
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &sup{done: make(chan struct{})}
	go func() {
		s.err = Supervise(ctx, bus, childArgv(), k.marker, k.live, logf)
		close(s.done)
	}()
	k.t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			k.t.Errorf("Supervise did not return within 5 s of its context's end")
		}
	})
	return s
}

// wait waits up to d for Supervise to return, and returns its error.
func (s *sup) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case <-s.done:
		return s.err
	case <-time.After(d):
		t.Fatalf("Supervise did not return within %v", d)
	}
	return nil
}

// exit is Supervise's result as a *supervise.Exit.
func exit(t *testing.T, err error) *supervise.Exit {
	t.Helper()
	var e *supervise.Exit
	if !errors.As(err, &e) {
		t.Fatalf("Supervise returned %v (%T), want a *supervise.Exit", err, err)
	}
	return e
}

// signalPID sends sig to pid, one of this test's own child processes. It refuses pid <= 1 and sends nothing (CR-1):
// kill(2) with pid 0 signals the caller's whole process group (go test's, and the shell's), -1 every process the
// caller may signal, and 1 is init. Every signal this package's tests send goes through it; internal/supervise's
// TestNoUnguardedKill keeps it so.
func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal pid %d: not a single child process", pid)
	}
	return syscall.Kill(pid, sig)
}

// alive reports whether pid is a live process (signal 0 sends nothing); never for pid <= 1.
func alive(pid int) bool { return signalPID(pid, 0) == nil }

// zombie reports whether pid has exited and waits to be reaped (Linux: state Z in /proc/<pid>/stat). It is false where
// /proc does not exist.
func zombie(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(b, ')') // the state follows "pid (comm) "
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}

// ourChild reports whether pid is still one of this test binary's re-executed processes (Linux: /proc/<pid>/cmdline),
// so a cleanup never signals a process that reused the pid. It is false where /proc does not exist.
func ourChild(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.HasPrefix(b, append([]byte(os.Args[0]), 0))
}

// parent is Supervise in a process of its own (SUMMONHELPER_TEST_PARENT), its output in dir/parent.log.
type parent struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error // Wait's, once done is closed
}

func (k *tk) startParent() *parent {
	k.t.Helper()
	out, err := os.Create(filepath.Join(k.dir, "parent.log"))
	if err != nil {
		k.t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envParent+"=1")
	cmd.Stdout, cmd.Stderr = out, out // a file, not a pipe: Wait never waits for the orphaned child
	if err := cmd.Start(); err != nil {
		k.t.Fatal(err)
	}
	p := &parent{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	k.t.Cleanup(func() {
		cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			k.t.Errorf("the parent process was not reaped")
		}
	})
	return p
}

func (p *parent) signal(t *testing.T, s syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(s); err != nil {
		t.Fatal(err)
	}
}

// wait waits up to d for the parent process to exit, and returns its status.
func (p *parent) wait(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(d):
		t.Fatalf("the parent process did not exit within %v", d)
	}
	return p.cmd.ProcessState.ExitCode()
}

// wrapBus is a DBusBus with test hooks on the parent's name requests and on the takeover's emits.
type wrapBus struct {
	*carlink.DBusBus
	beforeQueue func() // runs before a queue request goes out
	queueErr    error  // set: a queue request fails with it, and never reaches the bus
	failOwner   atomic.Bool
	failAbort   atomic.Bool

	mu   sync.Mutex
	reqs []nameReq
	logf func(string, ...any) // what SetLogf was given
}

func (w *wrapBus) SetLogf(f func(string, ...any)) {
	w.mu.Lock()
	w.logf = f
	w.mu.Unlock()
	w.DBusBus.SetLogf(f)
}

// busLogs checks that the bus was handed a logger, and that it is the one given to Supervise or Finish.
func (w *wrapBus) busLogs(t *testing.T, k *tk) {
	t.Helper()
	w.mu.Lock()
	f := w.logf
	w.mu.Unlock()
	if f == nil {
		t.Fatal("the bus was not given the logger for the car calls it drops")
	}
	f("a line through the bus's logger")
	if len(k.logs.with("a line through the bus's logger")) != 1 {
		t.Fatal("the bus's logger is not the one passed in")
	}
}

// nameReq is one RequestName, with the name's owner just before a queue request.
type nameReq struct {
	queue       bool
	ownerBefore string
	owned       bool
	err         error
}

func (w *wrapBus) RequestName(name string, queue bool) (bool, error) {
	r := nameReq{queue: queue}
	if queue {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		r.ownerBefore, _ = w.DBusBus.NameOwner(ctx, name)
		cancel()
		if w.beforeQueue != nil {
			w.beforeQueue()
		}
	}
	if r.err = w.queueErr; !queue || r.err == nil {
		r.owned, r.err = w.DBusBus.RequestName(name, queue)
	}
	w.mu.Lock()
	w.reqs = append(w.reqs, r)
	w.mu.Unlock()
	return r.owned, r.err
}

func (w *wrapBus) requests() []nameReq {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.reqs)
}

// NameOwner fails once failOwner is set: Disconnect's confirmation fails.
func (w *wrapBus) NameOwner(ctx context.Context, name string) (string, error) {
	if w.failOwner.Load() {
		return "", errors.New("test: GetNameOwner failed")
	}
	return w.DBusBus.NameOwner(ctx, name)
}

// Emit fails the phone-data signal (the abort) once failAbort is set, without sending it.
func (w *wrapBus) Emit(member string, body map[string]any) error {
	if member == carlink.SignalPeerCommsToCarserverData && w.failAbort.Load() {
		return errors.New("test: the abort's write failed")
	}
	return w.DBusBus.Emit(member, body)
}

// ---- the takeover ----

func TestTakeoverAfterChildSIGKILL(t *testing.T) {
	for _, blocking := range []bool{false, true} {
		name := "logging"
		if blocking {
			name = "a logf that blocks forever"
		}
		t.Run(name, func(t *testing.T) {
			k := newTK(t)
			var armed atomic.Bool
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			logf := func(format string, args ...any) {
				if armed.Load() {
					<-release // from the kill on, every log line blocks until the test ends
				}
				k.logs.Logf(format, args...)
			}
			s := k.supervise(k.dial(), logf)
			t.Cleanup(unblock) // before Supervise's cleanup, which waits for it to return
			pid := k.up(2)
			from := k.waitH1(1)
			k.leaveLive()

			armed.Store(blocking)
			at := time.Now()
			if err := signalPID(pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			k.abortDisconnect(from, at, 400*time.Millisecond)
			// The marker, the live file and the name go before any log line too.
			until(t, wait, "the marker removed", k.markerGone)
			until(t, wait, "the live file removed", k.liveGone)
			until(t, wait, "the name released", func() bool { return !k.hasOwner() })
			select {
			case <-s.done:
				if blocking {
					t.Fatal("Supervise returned through a log line that blocks forever")
				}
			default:
			}

			unblock()
			e := exit(t, s.wait(t, wait))
			if e.Reason != supervise.ChildExited || e.Signal != syscall.SIGKILL || e.FinishErr != nil {
				t.Fatalf("Supervise returned %+v, want the child's SIGKILL and a takeover without error", e)
			}
			k.settled(from, typeAbort, kindDisconnect)
			if !k.markerGone() || !k.liveGone() || k.hasOwner() {
				t.Fatal("the marker, the live file or the name came back")
			}
			if n := len(k.logs.with(takeoverRemoved)); n != 1 {
				t.Fatalf("%q logged %d times, want once\n%s", takeoverRemoved, n, k.logs)
			}
		})
	}
}

func TestTakeoverAfterChildSIGSTOP(t *testing.T) {
	k := newTK(t)
	s := k.supervise(k.dial(), nil)
	pid := k.up(2)
	from := k.waitH1(1)

	at := time.Now()
	if err := signalPID(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	k.abortDisconnect(from, at, 500*time.Millisecond)
	e := exit(t, s.wait(t, wait))
	if e.Reason != supervise.LivenessMissed || e.Signal != syscall.SIGKILL || e.FinishErr != nil {
		t.Fatalf("Supervise returned %+v, want a liveness kill and a takeover without error", e)
	}
	k.settled(from, typeAbort, kindDisconnect)
	if !k.markerGone() {
		t.Fatal("the marker survived a confirmed abort and disconnect")
	}
	if k.hasOwner() {
		t.Fatal("the name has an owner after the takeover")
	}
	if got := k.logs.with("removed the live file"); len(got) != 0 {
		t.Fatalf("no live file was left, but the removal was logged: %q", got)
	}
}

// Ownership is state: the child's connection closes first, so the name reaches the parent while the child still runs,
// before its liveness stops and it is killed. The takeover must count it.
func TestTakeoverChildBusDropsFirst(t *testing.T) {
	k := newTK(t)
	pb := k.dial()
	s := k.supervise(pb, nil)
	pid := k.up(2)
	from := k.waitH1(1)

	owners := make(chan *dbus.Signal, 16)
	k.obs.Signal(owners)
	if err := k.obs.AddMatchSignal(dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, carlink.WebrtcName)); err != nil {
		t.Fatal(err)
	}

	at := time.Now()
	if err := signalPID(pid, syscall.SIGUSR1); err != nil { // the child closes its bus connection
		t.Fatal(err)
	}
	// The bus hands the name to the parent at once (NameOwnerChanged: child to parent), while the child still runs.
	for got := false; !got; {
		select {
		case s := <-owners:
			got = len(s.Body) == 3 && s.Body[2] == pb.UniqueName()
		case <-time.After(wait):
			t.Fatal("the name never passed to the parent")
		}
	}
	t.Logf("the name reached the parent %v after the close; the car had %q since", time.Since(at).Round(time.Millisecond),
		k.carKinds(from))
	// The parent answers the car's calls while it holds the name: Reply 0, and 0 to RemoteToLocalSignaling. A child still
	// alive after the calls was not reaped yet, so the takeover had not begun and the name was the parent's throughout.
	v, err := k.callCar()
	u, serr := k.callSignaling()
	if alive(pid) {
		if err != nil || v != 0 {
			t.Fatalf("a car call while the parent held the name got Reply %d, %v; want 0", v, err)
		}
		if serr != nil || u != 0 {
			t.Fatalf("RemoteToLocalSignaling while the parent held the name got %d, %v; want 0", u, serr)
		}
	} else {
		t.Log("the child was reaped before the calls returned: the parent may have released the name; not checked")
	}
	k.abortDisconnect(from, at, 400*time.Millisecond)
	e := exit(t, s.wait(t, wait))
	if e.Reason != supervise.LivenessMissed || e.FinishErr != nil {
		t.Fatalf("Supervise returned %+v, want a liveness kill (the child's link went unhealthy) and a takeover", e)
	}
	k.settled(from, typeAbort, kindDisconnect)
	if !k.markerGone() || k.hasOwner() {
		t.Fatal("the marker or the name survived the takeover")
	}
}

// SIGTERM to the parent process mid-session: the child is SIGKILLed and the takeover still runs; the parent exits 0
// (R5: its context ended, and the takeover cleared the marker).
func TestTakeoverOnSIGTERM(t *testing.T) {
	k := newTK(t)
	p := k.startParent()
	pid := k.up(2)
	from := k.waitH1(1)

	at := time.Now()
	p.signal(t, syscall.SIGTERM)
	k.abortDisconnect(from, at, 400*time.Millisecond)
	if code := p.wait(t, wait); code != 0 {
		t.Fatalf("the parent exited %d after SIGTERM and a clean takeover, want 0", code)
	}
	k.settled(from, typeAbort, kindDisconnect)
	if !k.markerGone() || k.hasOwner() {
		t.Fatal("the marker or the name survived the takeover")
	}
	until(t, wait, "the child gone", func() bool { return !alive(pid) })
}

func TestTakeoverNoMarkerSendsNothing(t *testing.T) {
	k := newTK(t)
	t.Setenv(envNoMarker, "1")
	s := k.supervise(k.dial(), nil)
	pid := k.up(2)
	from := k.waitH1(1)
	k.leaveLive()

	if err := signalPID(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	e := exit(t, s.wait(t, wait))
	if e.FinishErr != nil {
		t.Fatalf("the takeover failed without a marker: %v", e.FinishErr)
	}
	k.settled(from) // nothing
	if k.hasOwner() {
		t.Fatal("the name has an owner after the takeover")
	}
	if !k.liveGone() {
		t.Fatal("the live file survived a takeover that found no marker")
	}
	if len(k.logs.with("no session marker")) == 0 {
		t.Fatalf("the takeover did not log that it found no marker\n%s", k.logs)
	}
}

// The takeover's marker check is fail-safe: a marker it cannot check counts as present, so the abort goes out. It
// cannot be removed either, so the takeover fails and runit's finish tries again.
func TestTakeoverUnreadableMarkerCountsAsPresent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	k := newTK(t)
	s := k.supervise(k.dial(), nil)
	pid := k.up(2)
	from := k.waitH1(1)
	if err := os.Chmod(k.dir, 0); err != nil { // Lstat of the marker now fails with EACCES
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(k.dir, 0o700) })

	at := time.Now()
	if err := signalPID(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	k.abortDisconnect(from, at, 400*time.Millisecond)
	if e := exit(t, s.wait(t, wait)); e.FinishErr == nil {
		t.Fatalf("Supervise returned %+v: want the takeover's failure to remove the marker", e)
	}
	k.settled(from, typeAbort, kindDisconnect)
}

// Another connection is ahead of the parent in the queue, so it holds the name when the child dies: the takeover
// waits TakeoverWait, emits nothing, keeps the marker, leaves the queue and fails. runit's Finish succeeds once the
// name is free.
func TestTakeoverWithoutNameKeepsMarker(t *testing.T) {
	k := newTK(t)
	x := k.dial()
	w := &wrapBus{DBusBus: k.dial()}
	w.beforeQueue = func() {
		if owned, err := x.RequestName(carlink.WebrtcName, true); owned || err != nil {
			t.Errorf("the other connection's queue request: owned %v, %v", owned, err)
		}
	}
	s := k.supervise(w, nil)
	pid := k.up(3) // the child, the other connection, the parent
	from := k.waitH1(1)
	k.leaveLive()

	at := time.Now()
	if err := signalPID(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err := s.wait(t, wait)
	took := time.Since(at)
	if e := exit(t, err); e.FinishErr == nil {
		t.Fatalf("Supervise returned %+v: want the takeover's failure", e)
	}
	if took < TakeoverWait {
		t.Fatalf("Supervise returned %v after the kill, before TakeoverWait (%v) passed", took, TakeoverWait)
	}
	k.settled(from) // nothing
	if !marker.Present(k.marker) {
		t.Fatal("the marker is gone, though no abort was sent")
	}
	if q := k.queue(); !slices.Equal(q, []string{x.UniqueName()}) {
		t.Fatalf("the queue is %q, want the other connection alone: the parent must leave it", q)
	}
	if !k.liveGone() {
		t.Fatal("the live file survived a takeover without the name")
	}

	// The other connection lets go; runit's finish then sends the abort and the disconnect.
	if err := x.ReleaseName(carlink.WebrtcName); err != nil {
		t.Fatal(err)
	}
	until(t, wait, "the name free", func() bool { return !k.hasOwner() })
	at = time.Now()
	if err := Finish(k.dial(), k.marker, k.live, k.logs.Logf); err != nil {
		t.Fatalf("Finish = %v once the name was free", err)
	}
	k.abortDisconnect(from, at, wait)
	if !k.markerGone() {
		t.Fatal("Finish left the marker after a confirmed abort and disconnect")
	}
}

func TestTakeoverEmitFailureKeepsMarker(t *testing.T) {
	for _, c := range []struct {
		name string
		arm  func(w *wrapBus)
		want []string // what reaches the car
	}{
		{"the disconnect's confirmation fails", func(w *wrapBus) { w.failOwner.Store(true) }, []string{typeAbort, kindDisconnect}},
		{"the abort's emit fails", func(w *wrapBus) { w.failAbort.Store(true) }, []string{kindDisconnect}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newTK(t)
			w := &wrapBus{DBusBus: k.dial()}
			s := k.supervise(w, nil)
			pid := k.up(2)
			from := k.waitH1(1)

			c.arm(w)
			if err := signalPID(pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			if e := exit(t, s.wait(t, wait)); e.FinishErr == nil {
				t.Fatalf("Supervise returned %+v: want the takeover's failure", e)
			}
			k.settled(from, c.want...)
			if !marker.Present(k.marker) {
				t.Fatal("the marker is gone after a failed emit")
			}
			if k.hasOwner() {
				t.Fatal("the name has an owner after the takeover")
			}
		})
	}
}

// The parent's queue request comes after the child's first liveness byte, when the child owns the name, and its reply
// is InQueue.
func TestParentQueuesAfterFirstBeat(t *testing.T) {
	k := newTK(t)
	w := &wrapBus{DBusBus: k.dial()}
	k.supervise(w, nil)
	k.up(2)
	k.waitH1(1)

	// The bus shows the queue a moment before the request's reply is back.
	until(t, wait, "the queue request's reply", func() bool { return len(w.requests()) > 0 })
	reqs := w.requests()
	if len(reqs) != 1 || !reqs[0].queue {
		t.Fatalf("the parent made the requests %+v, want one queue request", reqs)
	}
	r := reqs[0]
	if r.owned || r.err != nil {
		t.Fatalf("the queue request replied owned %v, %v; want InQueue (false, nil)", r.owned, r.err)
	}
	if r.ownerBefore == "" || r.ownerBefore == w.UniqueName() {
		t.Fatalf("the name's owner before the queue request was %q: the child did not own it yet", r.ownerBefore)
	}
	if q := k.queue(); len(q) != 2 || q[0] != r.ownerBefore || q[1] != w.UniqueName() {
		t.Fatalf("the queue is %q, want the child, then the parent (%s)", q, w.UniqueName())
	}
	w.busLogs(t, k)
}

// A queue request with any answer but InQueue gets the child SIGKILLed, and Supervise fails: no session runs without
// a queued parent. After an error the parent holds no name, so it sends nothing and runit's finish covers the session;
// with the name at once it takes over (TestQueueNameAtOnceTakesOver), here with no session to abort.
func TestQueueFailureKillsChild(t *testing.T) {
	t.Run("an error", func(t *testing.T) {
		k := newTK(t)
		errQueue := errors.New("test: RequestName failed")
		w := &wrapBus{DBusBus: k.dial(), queueErr: errQueue}
		// After Supervise's own Finish, before the takeover: the child's live file. Errorf: off the test goroutine.
		w.beforeQueue = func() {
			if err := os.WriteFile(k.live, []byte("123\n"), 0o600); err != nil {
				t.Errorf("leaving the live file: %v", err)
			}
		}
		s := k.supervise(w, nil)
		err := s.wait(t, startup)
		if !errors.Is(err, errQueue) {
			t.Fatalf("Supervise = %v, want the queue request's error", err)
		}
		if !k.liveGone() {
			t.Fatal("the live file survived the takeover after the queue request failed")
		}
		pid := k.pid("pid")
		if pid <= 0 || alive(pid) {
			t.Fatalf("the child (pid %d) is alive after the queue request failed", pid)
		}
		from := k.waitH1(1)
		k.settled(from) // the child's H1, and nothing from the parent
		if !marker.Present(k.marker) {
			t.Fatal("the child's marker is gone, though no abort was sent")
		}
		if err := Finish(k.dial(), k.marker, k.live, k.logs.Logf); err != nil {
			t.Fatalf("Finish = %v", err)
		}
		k.abortDisconnect(from, time.Now(), wait)
	})
	t.Run("the name at once", func(t *testing.T) {
		k := newTK(t)
		t.Setenv(envNoName, "1") // the child beats without the name: the queue request gets it
		w := &wrapBus{DBusBus: k.dial()}
		s := k.supervise(w, nil)
		err := s.wait(t, startup)
		if err == nil {
			t.Fatal("Supervise = nil after the queue request got the name at once")
		}
		if reqs := w.requests(); len(reqs) != 1 || !reqs[0].owned || reqs[0].err != nil {
			t.Fatalf("the parent made the requests %+v, want one queue request that got the name", reqs)
		}
		if pid := k.pid("pid"); pid <= 0 || alive(pid) {
			t.Fatalf("the child (pid %d) is alive after the queue request got the name", pid)
		}
		if k.hasOwner() {
			t.Fatal("the parent kept the name it should not have got")
		}
		k.settled(0) // nothing
	})
}

// Fix round 1, finding 1: the queue request gets the name at once because the child had it and lost it. Its bus
// connection dropped after its first byte, with a session open, while it still runs. The parent SIGKILLs the child and
// takes over with the name it holds: cmd_abort, then the confirmed disconnect, and the marker goes. Supervise still
// fails, so runsv restarts the service.
func TestQueueNameAtOnceTakesOver(t *testing.T) {
	k := newTK(t)
	w := &wrapBus{DBusBus: k.dial()}
	var dropped atomic.Int64
	w.beforeQueue = func() {
		dropped.Store(time.Now().UnixNano())
		// The child closes its bus connection and runs on. Error, not Fatal: this runs off the test goroutine.
		if err := signalPID(k.pid("pid"), syscall.SIGUSR1); err != nil {
			t.Errorf("the child left no pid: %v", err)
		}
		k.freed()
	}
	s := k.supervise(w, nil)
	err := s.wait(t, startup)
	if err == nil || !strings.Contains(err.Error(), "at once") {
		t.Fatalf("Supervise = %v, want a failure that says the queue request got the name at once", err)
	}
	if reqs := w.requests(); len(reqs) != 1 || !reqs[0].owned || reqs[0].err != nil {
		t.Fatalf("the parent made the requests %+v, want one queue request that got the name", reqs)
	}
	from := k.afterH1()
	k.abortDisconnect(from, time.Unix(0, dropped.Load()), 400*time.Millisecond)
	k.settled(from, typeAbort, kindDisconnect)
	if !k.markerGone() {
		t.Fatal("the marker survived a confirmed abort and disconnect")
	}
	if k.hasOwner() {
		t.Fatal("the name has an owner after the takeover")
	}
	if pid := k.pid("pid"); alive(pid) {
		t.Fatalf("the child (pid %d) is alive", pid)
	}
}

// Fix round 1, finding 2: the parent's own bus connection drops while the child serves, which takes the parent out
// of the queue. No session may run without a queued parent: the child is SIGKILLed at once and Supervise fails; the
// parent cannot send on a closed connection, so runit's finish sends the abort and the disconnect.
func TestParentConnectionLossKillsChild(t *testing.T) {
	k := newTK(t)
	pb := k.dial()
	s := k.supervise(pb, nil)
	pid := k.up(2)
	from := k.waitH1(1)
	k.leaveLive()

	at := time.Now()
	pb.Close()
	err := s.wait(t, wait)
	took := time.Since(at)
	if err == nil {
		t.Fatal("Supervise = nil after its own bus connection closed")
	}
	if took > 400*time.Millisecond {
		t.Fatalf("Supervise returned %v after its connection closed: the child served on without a queued parent", took)
	}
	if alive(pid) {
		t.Fatalf("the child (pid %d) is alive after the parent's connection closed", pid)
	}
	if !k.liveGone() {
		t.Fatal("the live file survived the takeover after the parent's connection closed")
	}
	k.settled(from) // nothing from the parent
	if !marker.Present(k.marker) {
		t.Fatal("the marker is gone, though no abort was sent")
	}
	at = time.Now()
	if err := Finish(k.dial(), k.marker, k.live, k.logs.Logf); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	k.abortDisconnect(from, at, wait)
	if !k.markerGone() {
		t.Fatal("Finish left the marker")
	}
}

// R5: Supervise runs Finish for a stale marker before it starts the child, so the car gets the stale session's abort
// and disconnect before the new child's data_channels_ready. When that Finish fails, no child starts.
func TestRunFinishesStaleMarkerFirst(t *testing.T) {
	t.Run("finished, then the child", func(t *testing.T) {
		k := newTK(t)
		k.openSession()
		from := k.waitH1(1)
		s := k.supervise(k.dial(), nil)
		pid := k.up(2)
		k.waitH1(2)
		k.settled(from, typeAbort, kindDisconnect, kindH1)

		// The supervisor's Link came after that Finish released the name, so its NameLost did not break the Link:
		// the takeover still confirms its disconnect.
		from += 3
		at := time.Now()
		if err := signalPID(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		k.abortDisconnect(from, at, 400*time.Millisecond)
		if e := exit(t, s.wait(t, wait)); e.FinishErr != nil {
			t.Fatalf("the takeover after a stale Finish failed: %v", e.FinishErr)
		}
		if !k.markerGone() {
			t.Fatal("the marker survived the takeover")
		}
	})
	t.Run("a failed finish starts no child", func(t *testing.T) {
		k := newTK(t)
		k.openSession()
		from := k.waitH1(1)
		x := k.dial()
		if owned, err := x.RequestName(carlink.WebrtcName, false); !owned || err != nil {
			t.Fatalf("taking the name: %v, %v", owned, err)
		}
		start := time.Now()
		s := k.supervise(k.dial(), nil)
		err := s.wait(t, startup)
		if err == nil {
			t.Fatal("Supervise = nil although the stale session could not be finished")
		}
		if took := time.Since(start); took < FinishFor {
			t.Fatalf("Supervise gave up after %v, before FinishFor (%v)", took, FinishFor)
		}
		var e *supervise.Exit
		if errors.As(err, &e) {
			t.Fatalf("Supervise = %v: a child ran", err)
		}
		k.noChild()
		k.settled(from) // nothing
		if !marker.Present(k.marker) {
			t.Fatal("the stale marker is gone, though no abort was sent")
		}
	})
}

// A stop that comes before the child starts (during the stale Finish, say) starts no child and is no failure: the stale
// session is still finished.
func TestSuperviseStoppedBeforeChild(t *testing.T) {
	k := newTK(t)
	k.openSession()
	from := k.waitH1(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Supervise(ctx, k.dial(), childArgv(), k.marker, k.live, k.logs.Logf); err != nil {
		t.Fatalf("Supervise = %v after a stop before any child started, want nil", err)
	}
	k.noChild()
	k.settled(from, typeAbort, kindDisconnect)
	if !k.markerGone() {
		t.Fatal("the stale marker survived")
	}
}

// The child gets StartupGrace (5 s) for its first liveness byte, not supervise's default (2 s): its startup includes the
// oracle's warm-up (OracleWarm, 3 s).
func TestChildGetsStartupGrace(t *testing.T) {
	k := newTK(t)
	t.Setenv(envDelay, "2500ms")
	s := k.supervise(k.dial(), nil)
	k.up(2) // the parent queues after the first byte
	select {
	case <-s.done:
		t.Fatalf("Supervise returned %v: the child was killed before its first byte", s.err)
	default:
	}
}

// ---- the finish hook ----

func TestFinish(t *testing.T) {
	t.Run("a marker and a free name", func(t *testing.T) {
		k := newTK(t)
		k.openSession()
		k.leaveLive()
		from := len(k.car.Received())
		w := &wrapBus{DBusBus: k.dial()}
		at := time.Now()
		if err := Finish(w, k.marker, k.live, k.logs.Logf); err != nil {
			t.Fatalf("Finish = %v", err)
		}
		k.abortDisconnect(from, at, wait)
		k.settled(from, typeAbort, kindDisconnect)
		if !k.markerGone() || !k.liveGone() || k.hasOwner() {
			t.Fatal("the marker, the live file or the name survived Finish")
		}
		if n := len(k.logs.with(finishRemoved)); n != 1 {
			t.Fatalf("%q logged %d times, want once\n%s", finishRemoved, n, k.logs)
		}
		if reqs := w.requests(); len(reqs) == 0 || reqs[0].queue {
			t.Fatalf("Finish's requests %+v, want DO_NOT_QUEUE ones", reqs)
		}
		w.busLogs(t, k)
	})
	t.Run("a logf that blocks forever", func(t *testing.T) {
		k := newTK(t)
		k.openSession()
		k.leaveLive()
		from := len(k.car.Received())
		release := make(chan struct{})
		done := make(chan error, 1)
		b := k.dial()
		at := time.Now()
		go func() { done <- Finish(b, k.marker, k.live, func(string, ...any) { <-release }) }()
		k.abortDisconnect(from, at, wait)
		until(t, wait, "the marker removed", k.markerGone)
		until(t, wait, "the live file removed", k.liveGone)
		until(t, wait, "the name released", func() bool { return !k.hasOwner() })
		select {
		case err := <-done:
			t.Fatalf("Finish returned %v through a log line that blocks forever", err)
		default:
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("Finish = %v", err)
		}
	})
	t.Run("no marker", func(t *testing.T) {
		k := newTK(t)
		k.connectCar()
		k.leaveLive()
		from := len(k.car.Received())
		w := &wrapBus{DBusBus: k.dial()}
		if err := Finish(w, k.marker, k.live, k.logs.Logf); err != nil {
			t.Fatalf("Finish = %v without a marker", err)
		}
		if reqs := w.requests(); len(reqs) != 0 {
			t.Fatalf("Finish asked for the name without a marker: %+v", reqs)
		}
		k.settled(from) // nothing
		if !k.liveGone() {
			t.Fatal("the live file survived Finish without a marker")
		}
		if n := len(k.logs.with(finishRemoved)); n != 1 {
			t.Fatalf("%q logged %d times, want once\n%s", finishRemoved, n, k.logs)
		}
	})
	t.Run("an unreadable marker counts as present", func(t *testing.T) {
		for _, c := range []struct {
			name string
			path func(t *testing.T, dir string) string
		}{
			{"a directory without search permission", func(t *testing.T, dir string) string {
				if os.Geteuid() == 0 {
					t.Skip("root reads any directory")
				}
				locked := filepath.Join(dir, "locked")
				if err := os.Mkdir(locked, 0o700); err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(locked, "session")
				if err := marker.New(p).Set(); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(locked, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(locked, 0o700) })
				return p
			}},
			{"a path through a regular file", func(t *testing.T, dir string) string {
				f := filepath.Join(dir, "file")
				if err := os.WriteFile(f, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(f, "session")
			}},
		} {
			t.Run(c.name, func(t *testing.T) {
				k := newTK(t)
				p := c.path(t, k.dir)
				if _, err := os.Lstat(p); err == nil || errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("Lstat = %v, want an error other than ENOENT", err)
				}
				k.connectCar()
				from := len(k.car.Received())
				err := Finish(k.dial(), p, k.live, k.logs.Logf)
				k.settled(from, typeAbort, kindDisconnect)
				if err == nil {
					t.Fatal("Finish = nil, though the marker it cannot read cannot be removed either")
				}
			})
		}
	})
	t.Run("a name held by another connection for 2 s", func(t *testing.T) {
		k := newTK(t)
		k.openSession()
		from := len(k.car.Received())
		x := k.dial()
		if owned, err := x.RequestName(carlink.WebrtcName, false); !owned || err != nil {
			t.Fatalf("taking the name: %v, %v", owned, err)
		}
		held := time.AfterFunc(2*time.Second, func() { x.ReleaseName(carlink.WebrtcName) })
		defer held.Stop()
		start := time.Now()
		err := Finish(k.dial(), k.marker, k.live, k.logs.Logf)
		took := time.Since(start)
		if err == nil {
			t.Fatal("Finish = nil while another connection held the name")
		}
		if took < FinishFor || took >= 2*time.Second {
			t.Fatalf("Finish gave up after %v, want it to retry for FinishFor (%v)", took, FinishFor)
		}
		k.settled(from) // nothing
		if !marker.Present(k.marker) {
			t.Fatal("the marker is gone, though no abort was sent")
		}
	})
	t.Run("a name freed within FinishFor", func(t *testing.T) {
		k := newTK(t)
		k.openSession()
		from := len(k.car.Received())
		x := k.dial()
		if owned, err := x.RequestName(carlink.WebrtcName, false); !owned || err != nil {
			t.Fatalf("taking the name: %v, %v", owned, err)
		}
		const hold = 300 * time.Millisecond
		held := time.AfterFunc(hold, func() { x.ReleaseName(carlink.WebrtcName) })
		defer held.Stop()
		start := time.Now()
		if err := Finish(k.dial(), k.marker, k.live, k.logs.Logf); err != nil {
			t.Fatalf("Finish = %v, though the name came free within FinishFor", err)
		}
		if took := time.Since(start); took < hold {
			t.Fatalf("Finish succeeded after %v, while the name was still held", took)
		}
		k.abortDisconnect(from, start, wait)
		if !k.markerGone() {
			t.Fatal("the marker survived Finish")
		}
	})
}

// Review focus 5: a live file Finish cannot remove (an unwritable directory) never makes it fail, or Supervise would
// start no helper (R4). Its lock went with the process that held it, so it proves nothing; Finish logs the error.
func TestFinishNeverFailsOnLiveRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for _, withMarker := range []bool{false, true} {
		name := "no marker"
		if withMarker {
			name = "a marker"
		}
		t.Run(name, func(t *testing.T) {
			locked := t.TempDir()
			live := filepath.Join(locked, "live")
			if err := os.WriteFile(live, []byte("123\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(locked, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(locked, 0o700) })
			mk := filepath.Join(t.TempDir(), "session")
			if withMarker {
				if err := marker.New(mk).Set(); err != nil {
					t.Fatal(err)
				}
			}
			bus, logs := carlink.NewFakeBus(), &logLines{}
			if err := Finish(bus, mk, live, logs.Logf); err != nil {
				t.Fatalf("Finish = %v, though only the live file's removal failed", err)
			}
			_, rmErr := RemoveLive(live) // the same failure, once more
			if rmErr == nil {
				t.Fatal("the live file in the 0555 directory could be removed: the test's setup is wrong")
			}
			const line = "summonhelper: finish could not remove the live file"
			if got := logs.with(line); len(got) != 1 || !strings.Contains(got[0], rmErr.Error()) {
				t.Fatalf("the removal's failure was logged as %q, want one line %q with %q\n%s", got, line, rmErr, logs)
			}
			want := 0
			if withMarker {
				want = 2 // the abort and the disconnect
			}
			if em := bus.Emissions(); len(em) != want {
				t.Fatalf("Finish emitted %v, want %d emissions", em, want)
			}
			if marker.Present(mk) {
				t.Fatal("the marker survived Finish")
			}
		})
	}
}

// Review focus 5, for the run: a live file nothing can remove changes neither Supervise's start (its own Finish) nor
// the takeover's result (R4). Both log the error.
func TestTakeoverNeverFailsOnLiveRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	k := newTK(t)
	locked := filepath.Join(k.dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	k.live = filepath.Join(locked, "live")
	k.leaveLive()
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	s := k.supervise(k.dial(), nil)
	pid := k.up(2)
	from := k.waitH1(1)

	if err := signalPID(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	e := exit(t, s.wait(t, wait))
	if e.Reason != supervise.ChildExited || e.FinishErr != nil {
		t.Fatalf("Supervise returned %+v, want the child's SIGKILL and a takeover without error", e)
	}
	k.settled(from, typeAbort, kindDisconnect)
	if !k.markerGone() || k.hasOwner() {
		t.Fatal("the marker or the name survived the takeover")
	}
	for _, end := range []string{"finish", "takeover"} {
		if n := len(k.logs.with("summonhelper: " + end + " could not remove the live file")); n != 1 {
			t.Fatalf("the %s's removal failure was logged %d times, want once\n%s", end, n, k.logs)
		}
	}
}

// ---- the supervisor process ----

// The parent process is stopped: its child does not depend on it, keeps answering the car's calls (Reply 1, its own),
// and no abort is sent.
func TestSupervisorSIGSTOPChildKeepsServing(t *testing.T) {
	k := newTK(t)
	p := k.startParent()
	pid := k.up(2)
	from := k.waitH1(1)
	q := k.queue()

	p.signal(t, syscall.SIGSTOP)
	t.Cleanup(func() { p.cmd.Process.Signal(syscall.SIGCONT) })
	for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if v, err := k.callCar(); err != nil || v != 1 {
			t.Fatalf("a car call got %d, %v while the parent was stopped; want the child's Reply 1", v, err)
		}
	}
	if got := k.queue(); !slices.Equal(got, q) {
		t.Fatalf("the name's queue went from %q to %q while the parent was stopped", q, got)
	}
	k.settled(from) // nothing

	// The parent resumes and is stopped the usual way: it kills its child and takes over.
	p.signal(t, syscall.SIGCONT)
	p.signal(t, syscall.SIGTERM)
	p.wait(t, wait)
	until(t, wait, "the child gone", func() bool { return !alive(pid) })
}

// The parent process is SIGKILLed: its child dies with it, and runit's finish sends the abort and the disconnect.
func TestSupervisorKilledThenFinish(t *testing.T) {
	k := newTK(t)
	p := k.startParent()
	pid := k.up(2)
	from := k.waitH1(1)

	linux := runtime.GOOS == "linux"
	if linux {
		// On Linux only Pdeathsig, a SIGKILL, can end the stopped child before it runs any code. Stopped, it writes no
		// liveness byte (no EPIPE) and runs no exitOrphaned. The parent's death orphans the child's process group (the
		// child alone: Setpgid), and with a member stopped the kernel sends that group SIGHUP, then SIGCONT (POSIX).
		// The child ignores SIGHUP (ignoreHangups), so the SIGCONT resumes it: without Pdeathsig, or with a Pdeathsig
		// it ignores (SIGTERM), it would exit through EPIPE on its next liveness write, which this branch refuses.
		// SIGSTOP and the parent's SIGKILL go back to back, well inside the parent's 100 ms watchdog.
		t.Cleanup(func() {
			if ourChild(pid) && !zombie(pid) {
				signalPID(pid, syscall.SIGKILL)
			}
		})
		if err := signalPID(pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now()
	p.signal(t, syscall.SIGKILL)
	// The child is gone when its connection is: on Linux by Pdeathsig; elsewhere by itself, through EPIPE on its next
	// liveness write (every 50 ms).
	until(t, time.Second, "the child's name released", func() bool { return !k.hasOwner() })
	gone := time.Since(at)
	_, epipe := os.Lstat(filepath.Join(k.dir, "epipe"))
	t.Logf("the child was gone %v after the parent's SIGKILL (EPIPE note: %v)", gone.Round(time.Millisecond), epipe == nil)
	if gone > 150*time.Millisecond {
		t.Fatalf("the child was gone %v after the parent's SIGKILL, want within 150 ms", gone)
	}
	if linux {
		if epipe == nil {
			t.Fatal("the stopped child exited through EPIPE: Pdeathsig did not end it")
		}
		until(t, 150*time.Millisecond, "the child's process gone", func() bool { return !alive(pid) || zombie(pid) })
	} else {
		if epipe != nil {
			t.Fatalf("the child did not exit through EPIPE: %v", epipe)
		}
		until(t, wait, "the child's process gone", func() bool { return !alive(pid) })
	}
	k.settled(from) // the parent died without a takeover
	k.leaveLive()   // the dead child's

	at = time.Now()
	if err := Finish(k.dial(), k.marker, k.live, k.logs.Logf); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	k.abortDisconnect(from, at, wait)
	k.settled(from, typeAbort, kindDisconnect)
	if !k.markerGone() || !k.liveGone() {
		t.Fatal("the marker or the live file survived Finish")
	}
}

// ---- the deadman chain ----

// The deadman chain in real time: the child runs the real Serve under Supervise, with liveness. The test is the phone,
// holding a verb. SIGUSR1 makes the child's next Send block forever under the engine lock; the D2 loop stops stamping,
// the liveness gate turns false, the parent kills the child and takes over.
func TestWedgedEngineIsKilledAndAborted(t *testing.T) {
	k := newTK(t)
	t.Setenv(envServe, "1")
	keyPath := filepath.Join(k.dir, "key.pem")
	if err := keys.Generate(keyPath); err != nil {
		t.Fatal(err)
	}
	key, err := keys.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, cfg: Config{Socket: filepath.Join(k.dir, "relay.sock"), Key: key}, dev: newDeviceKey(t),
		logs: k.logs, car: k.car}
	s := k.supervise(k.dial(), nil)
	pid := k.up(2)
	_, p := r.holdVerbWith(presenceOn) // opted in, so the session makes a live file; its LIVEs keep the lease fresh
	// The serving child holds its session's live file: this process's probe, as camhelper's, gets EWOULDBLOCK.
	if !liveLocked(t, k.live) {
		t.Fatal("the serving child does not hold the live file's lock while its session is up")
	}
	from := len(k.car.Received())

	at := time.Now()
	if err := signalPID(pid, syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	until(t, time.Second, "the wedge armed", func() bool {
		_, err := os.Lstat(filepath.Join(k.dir, "armed"))
		return err == nil
	})
	// The phone's next message wedges the engine: its Send never returns, and never reaches the car.
	if err := p.s.Send(summon.TypeMsg, []byte(`{"msg_type":"control:keepalive"}`)); err != nil {
		t.Fatal(err)
	}
	k.abortDisconnect(from, at, 600*time.Millisecond)
	e := exit(t, s.wait(t, wait))
	if e.Reason != supervise.LivenessMissed || e.Signal != syscall.SIGKILL || e.FinishErr != nil {
		t.Fatalf("Supervise returned %+v, want a liveness kill and a takeover without error", e)
	}
	k.settled(from, typeAbort, kindDisconnect)
	if !k.markerGone() || !k.liveGone() || k.hasOwner() {
		t.Fatal("the marker, the live file or the name survived the takeover")
	}
}
