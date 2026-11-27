package camhelper

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// ---- fixed child argv ----

const mcastGroup = "224.0.0.155"

// capturePorts are the single-camera multicast ports (PLAN §4.9): pillars
// 9892/9893, repeaters 9896/9897, dashcam front 9898, rear 9900, front bumper
// fisheye 9901 (the fascia view; verified on the car 2026-11-20).
// SelectablePort is allowed only during the session's selectable hold (front,
// the windshield ultrawide, is selectable 3 on it).
var capturePorts = map[int]string{
	9892: "right_pillar", 9893: "left_pillar", 9896: "right_repeater", 9897: "left_repeater",
	9898: "front_driving", 9900: "rear", 9901: "fascia",
}

const SelectablePort = 9895

// captureArgv is the receive-only capture pipeline for an allowlisted port,
// or nil. The only variable element is the port number from the allowlist.
func captureArgv(port int) []string {
	if _, ok := capturePorts[port]; !ok && port != SelectablePort {
		return nil
	}
	return []string{
		"/usr/bin/gst-launch-1.0", "-q",
		"udpsrc", "multicast-group=" + mcastGroup, "multicast-iface=eth0", "port=" + strconv.Itoa(port),
		"caps=application/x-rtp,media=video,encoding-name=H264,payload=96,clock-rate=90000",
		"!", "rtpjitterbuffer", "latency=150",
		"!", "rtph264depay",
		"!", "h264parse", "config-interval=-1",
		"!", "video/x-h264,stream-format=byte-stream,alignment=au",
		"!", "fdsink", "fd=1",
	}
}

// gstEnv mirrors /etc/gst-registry.vars (the car's prebuilt registry; never
// rebuilt or forked) so a capture starts fast as an unprivileged uid.
var gstEnv = []string{
	"PATH=/usr/bin:/bin",
	"GST_REGISTRY_1_0=/home/gst-register/gstreamer-1.0/registry.bin",
	"GST_REGISTRY_UPDATE=no",
	"GST_REGISTRY_FORK=no",
	"GST_PLUGIN_SYSTEM_PATH_1_0=/usr/lib/gstreamer-1.0",
	"GST_PLUGIN_PATH_1_0=/usr/lib/gstreamer-1.0:/usr/tesla/UI/lib/gst_plugin",
}

// WriterKind selects one of the two fixed talk-writer argvs. The car visit
// (probe_media.sh step 3) decides which one works with raw PCM on stdin.
type WriterKind string

const (
	WriterPWCat WriterKind = "pw-cat" // default
	WriterAplay WriterKind = "aplay"  // documented fallback
)

var writerArgv = map[WriterKind][]string{
	WriterPWCat: {"/usr/bin/pw-cat", "--playback", "--target", "webcam", "--format", "s16", "--rate", "48000", "--channels", "1", "-"},
	WriterAplay: {"/usr/bin/aplay", "-D", "webcam", "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "1", "-"},
}

var writerEnv = append([]string{"PATH=/usr/bin:/bin"}, pipewireEnv...)

// cutArgv is the ffmpeg stream-copy of [ss, ss+t] from in to out, at most
// limit bytes (-fs). in is a resolved TeslaCam path and out a helper temp
// path (both validated, passed as single argv elements with an explicit file:
// protocol). The demuxer is forced to mov and only the file protocol is
// allowed, so a crafted clip cannot make ffmpeg open anything else.
func cutArgv(in, out string, ss, t float64, limit int64) []string {
	return []string{
		"/usr/bin/ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-protocol_whitelist", "file",
		"-ss", strconv.FormatFloat(ss, 'f', 2, 64), "-t", strconv.FormatFloat(t, 'f', 2, 64),
		"-f", "mov", "-i", "file:" + in,
		"-c", "copy", "-movflags", "+faststart", "-fs", strconv.FormatInt(limit, 10), "-f", "mp4", "file:" + out,
	}
}

var cutEnv = []string{"PATH=/usr/bin:/bin"}

// ---- spawning ----

// ChildSpec describes one child process.
type ChildSpec struct {
	Argv   []string
	Env    []string
	Stdin  bool // give the parent a pipe to the child's stdin
	Stdout bool // give the parent a pipe from the child's stdout
}

// Child is a running child in its own process group.
type Child interface {
	Pid() int
	Stdin() io.WriteCloser // nil unless requested
	Stdout() io.ReadCloser // nil unless requested
	Done() <-chan struct{} // closed once the child has exited and been reaped
	ExitErr() error        // valid after Done
	Stop()                 // SIGTERM the group, then SIGKILL; returns after exit (bounded)
}

// Spawner starts children. Tests replace it with a fake.
type Spawner interface {
	Start(ChildSpec) (Child, error)
}

// ExecSpawner runs children with fixed credentials (uid 4951, groups audio 29
// and pipewire 1021 on the car), each in its own process group, killed with
// the helper (Pdeathsig on Linux).
type ExecSpawner struct {
	Cred *syscall.Credential // nil: inherit (tests, non-root)
}

type execChild struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *ring
	done   chan struct{}
	err    error
}

func (s ExecSpawner) Start(spec ChildSpec) (Child, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("camhelper: empty argv")
	}
	c := &execChild{done: make(chan struct{}), stderr: &ring{max: 2048}}
	cmd := &exec.Cmd{Path: spec.Argv[0], Args: spec.Argv, Env: spec.Env, Dir: "/"}
	cmd.SysProcAttr = sysProcAttr(s.Cred)
	cmd.Stderr = c.stderr
	cmd.WaitDelay = 2 * time.Second
	var childIn, childOut *os.File
	closeAll := func() {
		for _, f := range []*os.File{childIn, childOut, c.stdin, c.stdout} {
			if f != nil {
				f.Close()
			}
		}
	}
	if spec.Stdin {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		childIn, c.stdin, cmd.Stdin = r, w, r
	}
	if spec.Stdout {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll()
			return nil, err
		}
		c.stdout, childOut, cmd.Stdout = r, w, w
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	if childIn != nil {
		childIn.Close()
	}
	if childOut != nil {
		childOut.Close()
	}
	c.cmd = cmd
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *execChild) Pid() int { return c.cmd.Process.Pid }

func (c *execChild) Stdin() io.WriteCloser {
	if c.stdin == nil {
		return nil
	}
	return c.stdin
}

func (c *execChild) Stdout() io.ReadCloser {
	if c.stdout == nil {
		return nil
	}
	return c.stdout
}

func (c *execChild) Done() <-chan struct{} { return c.done }

func (c *execChild) ExitErr() error {
	<-c.done
	if c.err != nil && c.stderr.Len() > 0 {
		return fmt.Errorf("%w: %s", c.err, c.stderr.String())
	}
	return c.err
}

// Stop signals the child's own process group (never anything by name) while
// the child is unreaped, so the pgid cannot have been recycled.
func (c *execChild) Stop() {
	if c.stdin != nil {
		c.stdin.Close()
	}
	signal := func(sig syscall.Signal) {
		select {
		case <-c.done:
		default:
			syscall.Kill(-c.cmd.Process.Pid, sig)
		}
	}
	signal(syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(time.Second):
		signal(syscall.SIGKILL)
		select {
		case <-c.done:
		case <-time.After(3 * time.Second):
		}
	}
	if c.stdout != nil {
		c.stdout.Close()
	}
}

// ring keeps the last max bytes written (a child's stderr, for the log).
type ring struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	if len(r.b) > r.max {
		r.b = append(r.b[:0], r.b[len(r.b)-r.max:]...)
	}
	return len(p), nil
}

func (r *ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.b)
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.b)
}
