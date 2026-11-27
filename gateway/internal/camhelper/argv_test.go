package camhelper

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
)

func allArgv() [][]string {
	var all [][]string
	for _, a := range dvWriteArgv {
		all = append(all, a)
	}
	for _, a := range dvReadArgv {
		all = append(all, a)
	}
	for _, a := range gainArgv {
		all = append(all, a)
	}
	all = append(all, linksArgv)
	for p := range capturePorts {
		all = append(all, captureArgv(p))
	}
	all = append(all, captureArgv(SelectablePort))
	for _, a := range writerArgv {
		all = append(all, a)
	}
	all = append(all, cutArgv("/mnt/usb/TeslaCam/a.mp4", "/run/x/cut/b.mp4", 1, 2, 1<<20))
	return all
}

func TestEveryArgvIsFixedAndSafe(t *testing.T) {
	tools := map[string]bool{
		setLocalDV: true, getLocalDV: true, audioctl: true, pwLink: true, "/usr/bin/gst-launch-1.0": true,
		"/usr/bin/pw-cat": true, "/usr/bin/aplay": true, "/usr/bin/ffmpeg": true,
	}
	for _, a := range allArgv() {
		if len(a) == 0 || !tools[a[0]] {
			t.Fatalf("unexpected program %v", a)
		}
		for _, el := range a {
			for _, bad := range []string{"192.168.90", "pkill", "killall", "/bin/sh", "bash"} {
				if strings.Contains(el, bad) {
					t.Fatalf("%q in %v", bad, a)
				}
			}
		}
		if len(a) > 1 && a[1] == "-c" {
			t.Fatalf("shell-style argv %v", a)
		}
	}
}

func TestDVWritesAreOnlyTheCameraSet(t *testing.T) {
	allowed := map[string][]string{
		"WEBCAM_webcamSessionRequestAudio": {"0", "1"},
		"WEBCAM_webcamSessionActive":       {"0", "1"},
		// 9 (Backup) and 11 (Grid) only ever in SUMMON mode (Tesla's summon webcam).
		"GUI_selectableCameraRequest": {"0", "1", "3", "9", "11"},
	}
	for _, a := range dvWriteArgv {
		vals, ok := allowed[a[2]]
		known := false
		for _, v := range vals {
			known = known || len(a) == 4 && a[3] == v
		}
		if !ok || len(a) != 4 || a[1] != "CarServer" || !known {
			t.Fatalf("DV write %v", a)
		}
		for _, motion := range []string{"CAPI_", "Drive", "gear", "Gear", "Brake", "Creep", "Summon", "selfPark"} {
			if strings.Contains(a[2], motion) {
				t.Fatalf("motion DV %v", a)
			}
		}
	}
	for _, a := range dvReadArgv {
		if a[0] != getLocalDV {
			t.Fatalf("read %v", a)
		}
	}
	if gainArgv[GainNative][2] != "-20" || gainArgv[GainBoost][2] != "0" || gainArgv[GainMute][2] != "-200" {
		t.Fatal("gain table")
	}
}

func TestCaptureArgvIsReceiveOnly(t *testing.T) {
	for p := range capturePorts {
		a := strings.Join(captureArgv(p), " ")
		for _, must := range []string{"udpsrc multicast-group=224.0.0.155 multicast-iface=eth0 port=", "rtph264depay", "h264parse config-interval=-1",
			"video/x-h264,stream-format=byte-stream,alignment=au", "fdsink fd=1"} {
			if !strings.Contains(a, must) {
				t.Fatalf("port %d: missing %q", p, must)
			}
		}
		for _, bad := range []string{"udpsink", "tcpclientsink", "tcpserversink", "multiudpsink", "host=", "filesink", "-e"} {
			if strings.Contains(a, " "+bad) {
				t.Fatalf("port %d: %q in %s", p, bad, a)
			}
		}
	}
	for _, p := range []int{0, 9894, 9899, 8888, 8901} {
		if captureArgv(p) != nil {
			t.Fatalf("port %d allowed", p)
		}
	}
}

func TestCutArgvKeepsPathsWhole(t *testing.T) {
	in := "/mnt/usb/TeslaCam/a b;rm -rf /$(x)'.mp4"
	a := cutArgv(in, "/run/c/cut/o.mp4", 3.14159, 12, 5000000)
	if a[15] != "file:"+in || a[len(a)-1] != "file:/run/c/cut/o.mp4" || a[9] != "3.14" || a[11] != "12.00" ||
		a[20] != "-fs" || a[21] != "5000000" {
		t.Fatalf("%q", a)
	}
	if a[13] != "mov" || a[7] != "file" {
		t.Fatalf("demuxer / protocol whitelist %q", a)
	}
}

// No source file of the helper (or its command) names the vehicle LAN.
func TestNoVehicleLANInSources(t *testing.T) {
	for _, dir := range []string{".", "../../cmd/camhelper", "../../camhelper/proto", "../../camhelper/client"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			if bytes.Contains(b, []byte("192.168.90")) {
				t.Fatalf("%s names the vehicle LAN", f)
			}
		}
	}
}

// The real spawner on the desk: process groups, pipes, Stop.
func TestExecSpawner(t *testing.T) {
	sp := ExecSpawner{}
	c, err := sp.Start(ChildSpec{Argv: []string{"/bin/cat"}, Env: []string{"PATH=/bin"}, Stdin: true, Stdout: true})
	if err != nil {
		t.Fatal(err)
	}
	if pg, _ := syscall.Getpgid(c.Pid()); pg != c.Pid() {
		t.Fatalf("child not in its own process group (%d != %d)", pg, c.Pid())
	}
	c.Stdin().Write([]byte("hello"))
	c.Stdin().Close()
	out, _ := io.ReadAll(c.Stdout())
	<-c.Done()
	if string(out) != "hello" || c.ExitErr() != nil {
		t.Fatalf("%q %v", out, c.ExitErr())
	}

	s, err := sp.Start(ChildSpec{Argv: []string{"/bin/sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Stop()
	select {
	case <-s.Done():
	default:
		t.Fatal("Stop returned before the child exited")
	}
	if time.Since(start) > 2*time.Second || s.ExitErr() == nil {
		t.Fatalf("stop took %v, err %v", time.Since(start), s.ExitErr())
	}
	s.Stop() // idempotent on an exited child

	l, _ := sp.Start(ChildSpec{Argv: []string{"/bin/ls", "/nonexistent-camhelper"}})
	<-l.Done()
	if err := l.ExitErr(); err == nil || !strings.Contains(err.Error(), "nonexistent-camhelper") {
		t.Fatalf("stderr not kept: %v", err)
	}
}

func TestRefusedHoldCarriesStatus(t *testing.T) {
	r := newRig(t)
	r.car.set(ReadGear, "(true, 'D')")
	c := r.dial()
	st, err := c.Hold(ctx())
	expectCode(t, err, proto.ErrGateGear)
	if st.Gear != "D" || !st.Driving || st.Held {
		t.Fatalf("status %+v", st)
	}
}
