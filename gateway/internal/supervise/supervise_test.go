package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Timing bands. The watchdog kills LivenessTimeout (100 ms) after the last
// byte; the measured kill must land in [killLow, killHigh] after the last
// byte the supervisor saw. The upper bound leaves 150 ms of slack above the
// specified 100 ms for a loaded CI box under -race (timer wake-up, scheduler
// latency, the Wait reap); the lower bound is the spec itself (a kill earlier
// than 100 ms of silence would be a false positive), minus 5 ms for the gap
// between the reader seeing a byte and the test's hook recording it.
const (
	killLow  = 95 * time.Millisecond
	killHigh = 250 * time.Millisecond
)

// The helper child: the test binary re-executed with AGW_TEST_CHILD_MODE set.
// It never runs tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("AGW_TEST_CHILD_MODE"); mode != "" {
		os.Exit(childMain(mode))
	}
	os.Exit(m.Run())
}

func childMain(mode string) int {
	switch mode {
	case "beat": // the production writer, forever
		stop, err := StartLiveness(nil, func(error) { os.Exit(9) })
		if err != nil || stop == nil {
			fmt.Fprintln(os.Stderr, "child: no liveness:", err)
			return 3
		}
		select {}
	case "beat-then-stop": // beat for 300 ms, then stay alive but silent
		stop, err := StartLiveness(nil, nil)
		if err != nil || stop == nil {
			return 3
		}
		time.Sleep(300 * time.Millisecond)
		stop()
		time.Sleep(time.Hour)
	case "beat-then-exit": // beat for 200 ms, then exit 7 on its own
		if _, err := StartLiveness(nil, nil); err != nil {
			return 3
		}
		time.Sleep(200 * time.Millisecond)
		return 7
	case "unhealthy-after": // healthy for 300 ms, then hung: alive, writer running, no bytes
		start := time.Now()
		if _, err := StartLiveness(func() bool { return time.Since(start) < 300*time.Millisecond }, nil); err != nil {
			return 3
		}
		select {}
	case "silent": // never beats
		time.Sleep(time.Hour)
	case "ignore-term": // beats, and ignores SIGTERM/SIGINT/SIGHUP
		signalIgnore()
		if _, err := StartLiveness(nil, nil); err != nil {
			return 3
		}
		select {}
	case "env": // report the liveness env and fd 3, then exit 0
		if os.Getenv(LivenessEnv) != "3" {
			return 4
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(3, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
			return 5
		}
		return 0
	}
	return 2
}

func signalIgnore() { signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP) }

// harness runs Run against the helper child in mode and records finish calls
// and beat times.
type harness struct {
	finishes  atomic.Int32
	finishAt  atomic.Int64 // UnixNano of the first finish call
	finishErr error

	mu       sync.Mutex
	lastBeat time.Time
	pid      atomic.Int64
	beats    chan struct{}
}

func newHarness() *harness { return &harness{beats: make(chan struct{}, 1)} }

func (h *harness) finish() error {
	if h.finishes.Add(1) == 1 {
		h.finishAt.Store(time.Now().UnixNano())
	}
	return h.finishErr
}

func (h *harness) opts() Options {
	return Options{
		LivenessTimeout: 100 * time.Millisecond,
		StartupGrace:    2 * time.Second,
		Logf:            func(string, ...any) {},
		onStart:         func(pid int) { h.pid.Store(int64(pid)) },
		onBeat: func() {
			h.mu.Lock()
			h.lastBeat = time.Now()
			h.mu.Unlock()
			select {
			case h.beats <- struct{}{}:
			default:
			}
		},
	}
}

func (h *harness) last() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastBeat
}

func childArgv() []string { return []string{os.Args[0]} }

func runMode(t *testing.T, ctx context.Context, h *harness, mode string, o Options) error {
	t.Helper()
	t.Setenv("AGW_TEST_CHILD_MODE", mode)
	return Run(ctx, childArgv(), h.finish, o)
}

func exitOf(t *testing.T, err error) *Exit {
	t.Helper()
	var e *Exit
	if !errors.As(err, &e) {
		t.Fatalf("Run returned %v (%T), want *Exit", err, err)
	}
	return e
}

// signalPID sends sig to pid, one of this test's own child processes. It refuses pid <= 1 and sends nothing (CR-1):
// kill(2) with pid 0 signals the caller's whole process group (go test's, and the shell's), -1 every process the
// caller may signal, and 1 is init. Every signal this package's tests send goes through it; TestNoUnguardedKill
// (killguard_test.go) keeps it so.
func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal pid %d: not a single child process", pid)
	}
	return syscall.Kill(pid, sig)
}

// alive reports whether pid is a live process (signal 0 sends nothing); never for pid <= 1.
func alive(pid int) bool { return signalPID(pid, 0) == nil }

func TestChildGetsLivenessFD(t *testing.T) {
	h := newHarness()
	o := h.opts()
	o.StartupGrace = 5 * time.Second
	err := runMode(t, context.Background(), h, "env", o)
	e := exitOf(t, err)
	if e.Reason != ChildExited || e.Code != 0 {
		t.Fatalf("child saw no liveness fd/env: %+v", e)
	}
}

func TestFinishOnChildExit(t *testing.T) {
	h := newHarness()
	err := runMode(t, context.Background(), h, "beat-then-exit", h.opts())
	e := exitOf(t, err)
	if e.Reason != ChildExited || e.Code != 7 {
		t.Fatalf("got %+v, want child exit code 7", e)
	}
	if n := h.finishes.Load(); n != 1 {
		t.Fatalf("finish ran %d times, want 1", n)
	}
	if ExitCode(err) != 7 {
		t.Fatalf("ExitCode = %d, want 7", ExitCode(err))
	}
}

func TestChildExitZeroStillNonZero(t *testing.T) {
	h := newHarness()
	err := runMode(t, context.Background(), h, "env", h.opts())
	if err == nil || ExitCode(err) == 0 {
		t.Fatalf("a child that died on its own must give a non-zero status, got %v (%d)", err, ExitCode(err))
	}
}

func TestKillOnMissedLiveness(t *testing.T) {
	h := newHarness()
	start := time.Now()
	err := runMode(t, context.Background(), h, "beat-then-stop", h.opts())
	e := exitOf(t, err)
	if e.Reason != LivenessMissed {
		t.Fatalf("reason %v, want LivenessMissed", e.Reason)
	}
	if e.Signal != syscall.SIGKILL {
		t.Fatalf("child ended by %v, want SIGKILL", e.Signal)
	}
	if e.Silence < 100*time.Millisecond || e.Silence > killHigh {
		t.Fatalf("supervisor measured %v of silence, want [100ms, %v]", e.Silence, killHigh)
	}
	last := h.last()
	if last.IsZero() || last.Sub(start) < 250*time.Millisecond {
		t.Fatalf("child should have beaten for ~300 ms (last beat %v after start)", last.Sub(start))
	}
	gap := time.Unix(0, h.finishAt.Load()).Sub(last)
	t.Logf("silence measured by supervisor %v; last byte → finish %v", e.Silence, gap)
	if gap < killLow || gap > killHigh {
		t.Fatalf("killed+finished %v after the last byte, want [%v, %v]", gap, killLow, killHigh)
	}
	if n := h.finishes.Load(); n != 1 {
		t.Fatalf("finish ran %d times, want 1", n)
	}
	if pid := int(h.pid.Load()); alive(pid) {
		t.Fatalf("child %d still alive", pid)
	}
	if ExitCode(err) == 0 {
		t.Fatal("liveness kill must give a non-zero status")
	}
}

// Review Focus 3: a SIGSTOPped child (alive, holding the bus name, silent)
// must be treated as a liveness miss and SIGKILLed.
func TestSIGSTOPChild(t *testing.T) {
	h := newHarness()
	o := h.opts()
	var stoppedAt atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Setenv("AGW_TEST_CHILD_MODE", "beat")
	done := make(chan error, 1)
	go func() { done <- Run(ctx, childArgv(), h.finish, o) }()

	// Let it beat a few times, then stop it right after a byte arrives.
	for i := 0; i < 4; i++ {
		select {
		case <-h.beats:
		case <-time.After(3 * time.Second):
			t.Fatal("no beats from child")
		}
	}
	pid := int(h.pid.Load())
	if err := signalPID(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	stoppedAt.Store(time.Now().UnixNano())

	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		signalPID(pid, syscall.SIGKILL)
		t.Fatal("stopped child was not killed")
	}
	e := exitOf(t, err)
	if e.Reason != LivenessMissed || e.Signal != syscall.SIGKILL {
		t.Fatalf("got %+v, want LivenessMissed/SIGKILL", e)
	}
	// Lower bound from the last byte the supervisor saw (a kill sooner than
	// the timeout after it would be a false positive); upper bound from
	// SIGSTOP (the last byte precedes it, so this is the stricter bound).
	finishAt := time.Unix(0, h.finishAt.Load())
	fromLast := finishAt.Sub(h.last())
	fromStop := finishAt.Sub(time.Unix(0, stoppedAt.Load()))
	t.Logf("last byte → finish %v; SIGSTOP → finish %v (supervisor silence %v)", fromLast, fromStop, e.Silence)
	if fromLast < killLow || fromLast > killHigh {
		t.Fatalf("killed+finished %v after the last byte, want [%v, %v]", fromLast, killLow, killHigh)
	}
	if fromStop > killHigh {
		t.Fatalf("killed+finished %v after SIGSTOP, want <= %v", fromStop, killHigh)
	}
	if n := h.finishes.Load(); n != 1 {
		t.Fatalf("finish ran %d times, want 1", n)
	}
	if alive(pid) {
		t.Fatalf("child %d still alive", pid)
	}
}

func TestParentSIGTERM(t *testing.T) {
	// The child ignores SIGTERM: the supervisor must not rely on it.
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	t.Setenv("AGW_TEST_CHILD_MODE", "ignore-term")
	done := make(chan error, 1)
	go func() { done <- Run(ctx, childArgv(), h.finish, h.opts()) }()
	select {
	case <-h.beats:
	case <-time.After(3 * time.Second):
		t.Fatal("no beats")
	}
	cancel() // what SIGTERM/SIGINT do to the supervisor's context
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if err != nil {
		t.Fatalf("SIGTERM path must return nil (exit 0), got %v", err)
	}
	if ExitCode(err) != 0 {
		t.Fatal("ExitCode(nil) != 0")
	}
	if n := h.finishes.Load(); n != 1 {
		t.Fatalf("finish ran %d times, want 1", n)
	}
	if pid := int(h.pid.Load()); alive(pid) {
		t.Fatalf("child %d still alive", pid)
	}
}

func TestFinishOnce(t *testing.T) {
	// finish failing must neither retry it nor hide the child's status.
	h := newHarness()
	h.finishErr = errors.New("bus gone")
	err := runMode(t, context.Background(), h, "beat-then-exit", h.opts())
	e := exitOf(t, err)
	if e.Code != 7 || !errors.Is(err, h.finishErr) {
		t.Fatalf("got %+v, want code 7 carrying the finish error", e)
	}
	if n := h.finishes.Load(); n != 1 {
		t.Fatalf("finish ran %d times, want 1", n)
	}
	// And across a liveness kill.
	h2 := newHarness()
	_ = runMode(t, context.Background(), h2, "beat-then-stop", h2.opts())
	if n := h2.finishes.Load(); n != 1 {
		t.Fatalf("finish ran %d times after a kill, want 1", n)
	}
}

func TestStartupGrace(t *testing.T) {
	h := newHarness()
	o := h.opts()
	o.StartupGrace = 400 * time.Millisecond
	start := time.Now()
	err := runMode(t, context.Background(), h, "silent", o)
	took := time.Since(start)
	e := exitOf(t, err)
	if e.Reason != LivenessMissed || e.Signal != syscall.SIGKILL {
		t.Fatalf("got %+v, want LivenessMissed/SIGKILL", e)
	}
	if took < 400*time.Millisecond || took > 400*time.Millisecond+killHigh {
		t.Fatalf("silent child killed after %v, want ~the 400 ms grace (not the 100 ms timeout)", took)
	}
	if h.finishes.Load() != 1 {
		t.Fatal("finish not run once")
	}
}

func TestDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.LivenessTimeout != 100*time.Millisecond || o.StartupGrace != 2*time.Second {
		t.Fatalf("defaults %v %v", o.LivenessTimeout, o.StartupGrace)
	}
}

func TestChildOwnProcessGroup(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	t.Setenv("AGW_TEST_CHILD_MODE", "beat")
	done := make(chan error, 1)
	go func() { done <- Run(ctx, childArgv(), h.finish, h.opts()) }()
	select {
	case <-h.beats:
	case <-time.After(3 * time.Second):
		t.Fatal("no beats")
	}
	pid := int(h.pid.Load())
	pg, err := syscall.Getpgid(pid)
	cancel()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if pg != pid || pg == syscall.Getpgrp() {
		t.Fatalf("child pgid %d, want its own group %d (parent %d)", pg, pid, syscall.Getpgrp())
	}
}

func TestStartFailureNoFinish(t *testing.T) {
	h := newHarness()
	err := Run(context.Background(), []string{"/nonexistent/agw"}, h.finish, h.opts())
	if err == nil {
		t.Fatal("want start error")
	}
	if h.finishes.Load() != 0 {
		t.Fatal("finish must not run when no child ever started")
	}
	if Run(context.Background(), nil, h.finish, h.opts()) == nil {
		t.Fatal("empty argv must fail")
	}
}

func TestStartLivenessEnv(t *testing.T) {
	t.Setenv(LivenessEnv, "")
	stop, err := StartLiveness(nil, nil)
	if err != nil || stop != nil {
		t.Fatalf("unset env: stop=%v err=%v, want nil,nil", stop != nil, err)
	}
	for _, v := range []string{"4", "x", " 3"} {
		t.Setenv(LivenessEnv, v)
		if _, err := StartLiveness(nil, nil); err == nil {
			t.Fatalf("%s=%q must be refused", LivenessEnv, v)
		}
	}
}

func TestWriterBeatsAndReportsFailure(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	stop := startWriter(w, 10*time.Millisecond, nil, func(e error) { failed <- e })
	buf := make([]byte, 1)
	for i := 0; i < 3; i++ {
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	r.Close() // the supervisor is gone: the next write fails (EPIPE)
	select {
	case e := <-failed:
		if e == nil {
			t.Fatal("nil failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write failure not reported")
	}
	stop()
	stop() // idempotent
}

// Ruling: a server that reports itself unhealthy stops getting its bytes
// written, so the supervisor kills it like a dead one.
func TestKillOnUnhealthy(t *testing.T) {
	h := newHarness()
	err := runMode(t, context.Background(), h, "unhealthy-after", h.opts())
	e := exitOf(t, err)
	if e.Reason != LivenessMissed || e.Signal != syscall.SIGKILL {
		t.Fatalf("got %+v, want LivenessMissed/SIGKILL", e)
	}
	gap := time.Unix(0, h.finishAt.Load()).Sub(h.last())
	t.Logf("last byte → finish %v (supervisor silence %v)", gap, e.Silence)
	if gap < killLow || gap > killHigh {
		t.Fatalf("killed+finished %v after the last byte, want [%v, %v]", gap, killLow, killHigh)
	}
	if h.finishes.Load() != 1 {
		t.Fatal("finish not run once")
	}
}

// startRun runs Run on the helper child in mode, in the background. Cleanup stops it and waits for it.
func startRun(t *testing.T, h *harness, mode string, o Options) (cancel context.CancelFunc, done chan error) {
	t.Helper()
	t.Setenv("AGW_TEST_CHILD_MODE", mode)
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- Run(ctx, childArgv(), h.finish, o) }()
	return cancel, done
}

// OnFirstBeat runs exactly once, after the child's first byte, however many follow; a child that never beats never
// gets it.
func TestOnFirstBeatCalledOnce(t *testing.T) {
	h := newHarness()
	o := h.opts()
	var (
		calls       atomic.Int32
		firstBeat   atomic.Int64 // UnixNano of the first byte the reader saw
		calledAt    atomic.Int64
		beats       atomic.Int32
		hookOnBeat  = o.onBeat
		called      = make(chan struct{}, 1)
		manyBeats   = make(chan struct{})
		manyBeatsOK sync.Once
	)
	o.onBeat = func() {
		firstBeat.CompareAndSwap(0, time.Now().UnixNano())
		hookOnBeat()
		if beats.Add(1) == 6 {
			manyBeatsOK.Do(func() { close(manyBeats) })
		}
	}
	o.OnFirstBeat = func() {
		if calls.Add(1) == 1 {
			calledAt.Store(time.Now().UnixNano())
		}
		select {
		case called <- struct{}{}:
		default:
		}
	}
	cancel, done := startRun(t, h, "beat", o)
	defer cancel()
	select {
	case <-manyBeats:
	case <-time.After(3 * time.Second):
		t.Fatalf("the child beat %d times, want 6", beats.Load())
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("OnFirstBeat was not called")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v after a stop, want nil", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("OnFirstBeat ran %d times over %d beats, want once", n, beats.Load())
	}
	if c, b := calledAt.Load(), firstBeat.Load(); c < b {
		t.Fatalf("OnFirstBeat ran %v before the first byte", time.Duration(b-c))
	}

	// A child that never writes a byte never gets OnFirstBeat.
	h2 := newHarness()
	o2 := h2.opts()
	o2.StartupGrace = 300 * time.Millisecond
	var calls2 atomic.Int32
	o2.OnFirstBeat = func() { calls2.Add(1) }
	e := exitOf(t, runMode(t, context.Background(), h2, "silent", o2))
	if e.Reason != LivenessMissed {
		t.Fatalf("silent child: reason %v, want LivenessMissed", e.Reason)
	}
	if n := calls2.Load(); n != 0 {
		t.Fatalf("OnFirstBeat ran %d times for a child that never beat", n)
	}
}

// OnFirstBeat runs on a goroutine of its own: a slow one neither stalls the liveness reader (the healthy child is
// kept) nor the supervisor's loop (a stop while it blocks still kills the child and runs finish).
func TestSlowOnFirstBeatKeepsChild(t *testing.T) {
	t.Run("200 ms", func(t *testing.T) {
		h := newHarness()
		o := h.opts()
		var inside atomic.Bool
		var during atomic.Int32 // bytes the reader took while OnFirstBeat ran
		hookOnBeat := o.onBeat
		o.onBeat = func() {
			hookOnBeat()
			if inside.Load() {
				during.Add(1)
			}
		}
		returned := make(chan struct{})
		o.OnFirstBeat = func() {
			inside.Store(true)
			time.Sleep(200 * time.Millisecond)
			inside.Store(false)
			close(returned)
		}
		cancel, done := startRun(t, h, "beat", o)
		defer cancel()
		select {
		case <-returned:
		case err := <-done:
			t.Fatalf("Run returned %v while OnFirstBeat ran: the healthy child was killed", err)
		case <-time.After(3 * time.Second):
			t.Fatal("OnFirstBeat never returned")
		}
		select {
		case err := <-done:
			t.Fatalf("Run returned %v after a slow OnFirstBeat: the healthy child was killed", err)
		case <-time.After(150 * time.Millisecond): // past the liveness timeout
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run = %v, want nil: the child must have lived until the stop", err)
		}
		if n := during.Load(); n < 2 {
			t.Fatalf("the reader took %d bytes during a 200 ms OnFirstBeat, want at least 2", n)
		}
		if n := h.finishes.Load(); n != 1 {
			t.Fatalf("finish ran %d times, want 1", n)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		h := newHarness()
		o := h.opts()
		began, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		o.OnFirstBeat = func() {
			close(began)
			<-release
		}
		cancel, done := startRun(t, h, "beat", o)
		defer cancel()
		select {
		case <-began:
		case <-time.After(3 * time.Second):
			t.Fatal("OnFirstBeat was not called")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run = %v after a stop, want nil", err)
			}
		case <-time.After(time.Second):
			t.Fatal("a stop did not end Run while OnFirstBeat blocked")
		}
		if n := h.finishes.Load(); n != 1 {
			t.Fatalf("finish ran %d times, want 1", n)
		}
		if pid := int(h.pid.Load()); alive(pid) {
			t.Fatalf("child %d still alive", pid)
		}
	})
}

// R2: no Logf call stands between a child's death and finish. Logf blocks forever from the moment the child started
// (after the start line, the only one allowed before the child's death), on every way a child can end.
func TestFinishRunsBeforeLogs(t *testing.T) {
	for _, c := range []struct {
		name, mode string
		stop       bool
	}{
		{"liveness missed", "beat-then-stop", false},
		{"startup grace", "silent", false},
		{"exited on its own", "beat-then-exit", false},
		{"stopped", "beat", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			o := h.opts()
			o.StartupGrace = 300 * time.Millisecond
			var armed atomic.Bool
			release := make(chan struct{})
			o.Logf = func(string, ...any) {
				if armed.Load() {
					<-release
				}
			}
			hookOnStart := o.onStart
			o.onStart = func(pid int) {
				hookOnStart(pid)
				armed.Store(true) // right after the start line
			}
			finished := make(chan struct{})
			var once sync.Once
			finish := func() error {
				once.Do(func() { close(finished) })
				return nil
			}
			t.Setenv("AGW_TEST_CHILD_MODE", c.mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Run(ctx, childArgv(), finish, o) }()
			if c.stop {
				select {
				case <-h.beats:
				case <-time.After(3 * time.Second):
					t.Fatal("no beats")
				}
				cancel()
			}
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("finish did not run while Logf blocked: a log line stands before it")
			}
			close(release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Run did not return once Logf was released")
			}
			if pid := int(h.pid.Load()); alive(pid) {
				t.Fatalf("child %d still alive", pid)
			}
		})
	}
}

func TestWriterSkipsWhileUnhealthy(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var ok atomic.Bool
	got := make(chan struct{}, 16)
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := r.Read(buf); err != nil {
				return
			}
			got <- struct{}{}
		}
	}()
	stop := startWriter(w, 10*time.Millisecond, ok.Load, nil)
	defer stop()
	select {
	case <-got:
		t.Fatal("wrote a byte while unhealthy")
	case <-time.After(100 * time.Millisecond):
	}
	ok.Store(true)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("no byte once healthy")
	}
}
