// Package supervise is the Summon helper's stop for its own death (design
// §6.1). The car has no software deadman while it moves, so if
// `summonhelper serve` dies, hangs or is stopped mid-summon, something outside
// it must send the abort.
//
// `summonhelper run` calls Run with `summonhelper serve` as the child and a
// liveness pipe on fd 3. The child writes one byte to it every LivenessPeriod
// while it is healthy (StartLiveness). The parent SIGKILLs the child when no
// byte has arrived for LivenessTimeout (measured on the monotonic clock from
// the last byte; StartupGrace before the first one), or when the parent's
// context ends (SIGTERM, SIGINT), and it notices a child that exits on its
// own. It then reaps the child and runs the finish hook exactly once, before
// it logs anything about the child's death. summonhelper's hook is the
// takeover (internal/summon/helper): from the child's first byte
// (Options.OnFirstBeat) the parent is queued for com.tesla.Webrtc behind the
// child, the bus hands it the name once the child's connection is gone, and it
// sends cmd_abort and the disconnect map while the session marker says a
// session may be open. A supervisor that dies itself is covered by runit's
// finish script, `summonhelper finish`, which does the same on a connection of
// its own; on Linux its child dies with it (Pdeathsig), and elsewhere a
// healthy child's next liveness write fails (EPIPE) and it shuts down.
//
// What the liveness byte proves: the writer is a ticker goroutine inside the
// child, so on its own it detects child death, SIGSTOP and a runtime that no
// longer schedules goroutines. A single wedged goroutine (an engine stuck
// under its lock, a hung accept loop) is caught only through the healthy gate
// passed to StartLiveness: the writer skips the byte whenever healthy() is
// false, so a server whose loops stop making progress is killed like a dead
// one. summonhelper's gate (helper.Health) holds while the car link is owned
// and healthy and the D2 loop stamped its progress within 250 ms.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Liveness contract between the supervisor and the child.
const (
	LivenessEnv            = "AGW_LIVENESS_FD" // set to "3" in the child's environment
	LivenessFD             = 3                 // the pipe's write end in the child
	LivenessPeriod         = 50 * time.Millisecond
	DefaultLivenessTimeout = 100 * time.Millisecond
	DefaultStartupGrace    = 2 * time.Second
	DefaultReapTimeout     = time.Second
)

// Options configures Run. Zero fields take the defaults.
type Options struct {
	// LivenessTimeout: the longest silence on the pipe, from the last byte,
	// before the child is SIGKILLed. Default 100 ms (R17).
	LivenessTimeout time.Duration
	// StartupGrace: how long the child may take to write its first byte
	// (loading keys, connecting to the bus, binding). Default 2 s.
	StartupGrace time.Duration
	// ReapTimeout: how long to wait for a SIGKILLed child to be reaped before
	// running finish anyway (a child stuck in uninterruptible sleep must not
	// hold up the abort). Default 1 s.
	ReapTimeout time.Duration
	// Stdout and Stderr of the child; default the supervisor's own.
	Stdout, Stderr io.Writer
	// Logf receives the supervisor's log lines; default log.Printf.
	Logf func(format string, args ...any)
	// OnFirstBeat, if set, is called once, on a goroutine of its own, after the child's first liveness byte.
	OnFirstBeat func()

	onStart func(pid int) // test hook: the child's pid, right after Start
	onBeat  func()        // test hook: called on the reader goroutine per read
}

func (o Options) withDefaults() Options {
	if o.LivenessTimeout <= 0 {
		o.LivenessTimeout = DefaultLivenessTimeout
	}
	if o.StartupGrace <= 0 {
		o.StartupGrace = DefaultStartupGrace
	}
	if o.ReapTimeout <= 0 {
		o.ReapTimeout = DefaultReapTimeout
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	return o
}

// Reason says why the supervised child ended.
type Reason int

const (
	ChildExited    Reason = iota + 1 // the child died on its own
	LivenessMissed                   // no byte within the timeout: SIGKILLed
	Stopped                          // the supervisor's context ended: SIGKILLed
)

func (r Reason) String() string {
	switch r {
	case ChildExited:
		return "child exited"
	case LivenessMissed:
		return "liveness missed"
	case Stopped:
		return "stopped"
	}
	return fmt.Sprintf("Reason(%d)", int(r))
}

// Exit is Run's result whenever the child ended other than by the
// supervisor's own stop. It is an error: the supervisor must exit non-zero
// so runit restarts it.
type Exit struct {
	Reason    Reason
	Code      int            // the child's exit status; -1 if a signal ended it or it was not reaped
	Signal    syscall.Signal // the signal that ended the child, or 0
	Silence   time.Duration  // LivenessMissed: time since the last byte (or since start)
	FinishErr error          // the finish hook's error, if any
}

func (e *Exit) Error() string {
	var s string
	switch {
	case e.Reason == LivenessMissed:
		s = fmt.Sprintf("supervise: child killed: liveness missed after %v of silence", e.Silence.Round(time.Millisecond))
	case e.Signal != 0:
		s = fmt.Sprintf("supervise: %v (signal %v)", e.Reason, e.Signal)
	default:
		s = fmt.Sprintf("supervise: %v with status %d", e.Reason, e.Code)
	}
	if e.FinishErr != nil {
		s += "; finish: " + e.FinishErr.Error()
	}
	return s
}

func (e *Exit) Unwrap() error { return e.FinishErr }

// ExitCode is the supervisor's process status: the child's own non-zero
// status when it exited with one, otherwise 1 (a child that died on its own
// with 0, or was killed, still means the supervisor must be restarted).
func (e *Exit) ExitCode() int {
	if e.Code > 0 {
		return e.Code
	}
	return 1
}

// ExitCode maps Run's result to a process status: 0 for nil.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *Exit
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return 1
}

// Run supervises argv (argv[0] is the program) until it ends; see the package
// comment. finish runs exactly once after the child has started and then
// ended (it is not run if the child could not be started at all: no window
// can have opened), and no log line comes between the child's end and
// finish. Run returns nil after a stop through ctx, the start error, or an
// *Exit.
//
// The child runs in its own process group, so a terminal's SIGINT or a
// group-wide signal never reaches it directly: only the supervisor decides how
// it ends, and the only signal it ever sends is SIGKILL.
func Run(ctx context.Context, argv []string, finish func() error, opts Options) error {
	if len(argv) == 0 {
		return errors.New("supervise: empty argv")
	}
	if finish == nil {
		return errors.New("supervise: nil finish hook")
	}
	o := opts.withDefaults()

	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("supervise: liveness pipe: %w", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = childEnv(os.Environ())
	cmd.ExtraFiles = []*os.File{w} // fd 3
	cmd.Stdout, cmd.Stderr = o.Stdout, o.Stderr
	cmd.SysProcAttr = childSysProcAttr()
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return fmt.Errorf("supervise: starting child: %w", err)
	}
	w.Close() // only the child holds the write end: EOF once it is gone
	start := time.Now()
	pid := cmd.Process.Pid
	o.Logf("supervise: child pid %d started", pid)
	if o.onStart != nil {
		o.onStart(pid)
	}

	waited := make(chan *os.ProcessState, 1)
	go func() {
		cmd.Wait() // a non-zero status is in ProcessState
		waited <- cmd.ProcessState
	}()

	// last is the monotonic offset from start of the latest byte; -1 = none.
	var last atomic.Int64
	last.Store(-1)
	beat := make(chan struct{}, 1)
	go func() {
		defer r.Close()
		buf := make([]byte, 64)
		first := true
		for {
			n, err := r.Read(buf)
			if n > 0 {
				last.Store(int64(time.Since(start)))
				if o.onBeat != nil {
					o.onBeat()
				}
				if first && o.OnFirstBeat != nil {
					go o.OnFirstBeat() // never on this reader: a slow callback must not stall the bytes
				}
				first = false
				select {
				case beat <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return // EOF: the child closed fd 3 or died; the timer decides
			}
		}
	}()

	timer := time.NewTimer(o.StartupGrace)
	defer timer.Stop()
	var (
		reason  Reason
		silence time.Duration
		ps      *os.ProcessState
	)
loop:
	for {
		select {
		case ps = <-waited:
			reason = ChildExited
			break loop
		case <-ctx.Done():
			reason = Stopped
			break loop
		case <-beat:
			timer.Reset(o.LivenessTimeout)
		case <-timer.C:
			// Decide on the recorded byte time, not on the timer alone: a
			// byte may have landed after the timer was armed.
			now := time.Since(start)
			due, since := o.StartupGrace, now
			if l := last.Load(); l >= 0 {
				due, since = time.Duration(l)+o.LivenessTimeout, now-time.Duration(l)
			}
			if now >= due {
				reason, silence = LivenessMissed, since
				break loop
			}
			timer.Reset(due - now)
		}
	}

	var killErr error
	reaped := true
	if reason != ChildExited {
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			killErr = err
		}
		select {
		case ps = <-waited:
		case <-time.After(o.ReapTimeout):
			reaped = false
		}
	}

	// finish comes first: no log line stands between the child's death and the abort (R2), since a log line may be
	// slow and the finish hook is what tells the car.
	ferr := callFinish(finish)

	switch reason {
	case LivenessMissed:
		o.Logf("supervise: liveness missed (%v of silence): SIGKILLed child pid %d", silence.Round(time.Millisecond), pid)
	case Stopped:
		o.Logf("supervise: stopping: SIGKILLed child pid %d", pid)
	default:
		o.Logf("supervise: child pid %d exited on its own: %v", pid, ps)
	}
	if killErr != nil {
		o.Logf("supervise: SIGKILL child pid %d: %v", pid, killErr)
	}
	if !reaped {
		o.Logf("supervise: child pid %d not reaped after %v; finish ran anyway", pid, o.ReapTimeout)
	}
	if ferr != nil {
		o.Logf("supervise: finish: %v", ferr)
	}

	e := &Exit{Reason: reason, Code: -1, Silence: silence, FinishErr: ferr}
	if ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			e.Signal = ws.Signal()
		} else {
			e.Code = ps.ExitCode()
		}
	}
	if reason == Stopped {
		return nil // SIGTERM/SIGINT: exit 0 (a finish failure is logged above)
	}
	return e
}

// callFinish runs finish once, turning a panic into an error so the
// supervisor still exits with the child's status.
func callFinish(finish func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("finish panicked: %v", p)
		}
	}()
	return finish()
}

// childEnv is env with LivenessEnv=3 (any inherited value replaced).
func childEnv(env []string) []string {
	return append(EnvWithout(env, LivenessEnv), LivenessEnv+"=3")
}

// EnvWithout returns env minus every entry for key.
func EnvWithout(env []string, key string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// StartLiveness is the child's side. With LivenessEnv unset or empty it does
// nothing and returns (nil, nil). With LivenessEnv=3 it takes fd 3 (which
// must be a pipe), marks it close-on-exec, and writes one byte to it now and
// every LivenessPeriod until stop is called. Any other value is refused: a
// supervisor that set it expects bytes, and a child that cannot send them
// would only be killed.
//
// healthy (may be nil: always healthy) is asked before every byte; while it
// returns false no byte is written, so the supervisor kills the child after
// LivenessTimeout. It runs on the writer goroutine every LivenessPeriod and
// must be cheap and never block: callers feed it from a progress timestamp
// that their loops store atomically (for example "the summon loop ticked
// within the last N ms"), never by calling into or locking those loops.
//
// onFail (may be nil) is called once if a write fails, typically EPIPE because
// the supervisor is gone; the child should then tear down by itself, since no
// finish hook will run for it. Writing stops after a failure.
func StartLiveness(healthy func() bool, onFail func(error)) (stop func(), err error) {
	v, set := os.LookupEnv(LivenessEnv)
	if !set || v == "" {
		return nil, nil
	}
	if v != "3" {
		return nil, fmt.Errorf("%s=%q: only 3 is supported", LivenessEnv, v)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(LivenessFD, &st); err != nil {
		return nil, fmt.Errorf("%s=3 but fd 3 is not open: %w", LivenessEnv, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return nil, fmt.Errorf("%s=3 but fd 3 is not a pipe", LivenessEnv)
	}
	syscall.CloseOnExec(LivenessFD)
	f := os.NewFile(LivenessFD, "liveness")
	return startWriter(f, LivenessPeriod, healthy, onFail), nil
}

// startWriter writes one byte to w immediately and then every period,
// skipping each byte for which healthy (if non-nil) returns false.
func startWriter(w io.Writer, period time.Duration, healthy func() bool, onFail func(error)) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(period)
		defer t.Stop()
		b := []byte{'.'}
		for {
			if healthy == nil || healthy() {
				if _, err := w.Write(b); err != nil {
					if onFail != nil {
						onFail(err)
					}
					return
				}
			}
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
