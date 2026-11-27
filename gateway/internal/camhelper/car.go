package camhelper

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// The car tools, by absolute path (no PATH lookup).
const (
	setLocalDV = "/usr/local/bin/set-local-dv"
	getLocalDV = "/usr/local/bin/get-local-dv"
	audioctl   = "/usr/bin/audioctl"
	pwLink     = "/usr/bin/pw-link"
)

// DVWrite is one of the only DataValue writes the helper can make. There is
// no generic setter: each value maps to a fixed argv below.
type DVWrite int

const (
	RequestAudio0 DVWrite = iota
	RequestAudio1
	SessionActive0
	SessionActive1
	Selectable0
	Selectable1  // interior
	Selectable3  // front: the windshield ultrawide (FrontFisheye)
	Selectable9  // Backup: SUMMON mode, gear R (Tesla's summon webcam)
	Selectable11 // Grid: SUMMON mode, every gear but D and R (Tesla's summon webcam)
	numDVWrites
)

var dvWriteArgv = [numDVWrites][]string{
	RequestAudio0:  {setLocalDV, "CarServer", "WEBCAM_webcamSessionRequestAudio", "0"},
	RequestAudio1:  {setLocalDV, "CarServer", "WEBCAM_webcamSessionRequestAudio", "1"},
	SessionActive0: {setLocalDV, "CarServer", "WEBCAM_webcamSessionActive", "0"},
	SessionActive1: {setLocalDV, "CarServer", "WEBCAM_webcamSessionActive", "1"},
	Selectable0:    {setLocalDV, "CarServer", "GUI_selectableCameraRequest", "0"},
	Selectable1:    {setLocalDV, "CarServer", "GUI_selectableCameraRequest", "1"},
	Selectable3:    {setLocalDV, "CarServer", "GUI_selectableCameraRequest", "3"},
	Selectable9:    {setLocalDV, "CarServer", "GUI_selectableCameraRequest", "9"},
	Selectable11:   {setLocalDV, "CarServer", "GUI_selectableCameraRequest", "11"},
}

func (w DVWrite) String() string {
	if w < 0 || w >= numDVWrites {
		return fmt.Sprintf("DVWrite(%d)", int(w))
	}
	a := dvWriteArgv[w]
	return a[2] + "=" + a[3]
}

// DVRead is one of the DataValues the helper reads (the checkWebcamAllowed
// inputs).
type DVRead int

const (
	ReadGear DVRead = iota
	ReadSentry
	ReadClimate
	ReadCamAccess
	ReadRequestAudio  // readback after a 0 write
	ReadSessionActive // readback after a 0 write
	ReadSelectable    // readback after a 0 write
	numDVReads
)

var dvReadArgv = [numDVReads][]string{
	ReadGear:          {getLocalDV, "CarServer", "VAPI_shiftState"},
	ReadSentry:        {getLocalDV, "CarServer", "GUI_sentryModeState"},
	ReadClimate:       {getLocalDV, "CarServer", "GUI_keepClimateOn"},
	ReadCamAccess:     {getLocalDV, "CarServer", "GUI_mobileCameraAccessEnabled"},
	ReadRequestAudio:  {getLocalDV, "CarServer", "WEBCAM_webcamSessionRequestAudio"},
	ReadSessionActive: {getLocalDV, "CarServer", "WEBCAM_webcamSessionActive"},
	ReadSelectable:    {getLocalDV, "CarServer", "GUI_selectableCameraRequest"},
}

// Gain is one of the only pws-media-gain values the helper applies.
type Gain int

const (
	GainNative Gain = iota // -20 dB, what audiod sets for the webcam route
	GainBoost              // 0 dB, only while push-to-talk (owner choice 2026-11-20; Tesla's own max is -6)
	GainMute               // -200 dB, only at teardown and only with our marker
	numGains
)

var gainArgv = [numGains][]string{
	GainNative: {audioctl, "pws-media-gain", "-20"},
	GainBoost:  {audioctl, "pws-media-gain", "0"},
	GainMute:   {audioctl, "pws-media-gain", "-200"},
}

// DB returns the gain in dB.
func (g Gain) DB() int { return [numGains]int{-20, 0, -200}[g] }

var linksArgv = []string{pwLink, "-l"}

// pipewireEnv is the environment for every PipeWire client (pw-link, pw-cat).
var pipewireEnv = []string{"PIPEWIRE_RUNTIME_DIR=/var/run/pipewire", "PIPEWIRE_CONFIG_NAME=client.conf"}

// Car is everything the helper does to the car outside its own children.
// Tests replace it with a fake.
type Car interface {
	Write(DVWrite) error
	Read(DVRead) (string, error) // raw get-local-dv output, e.g. "(true, 'P')"
	SetGain(Gain) error
	Links() (string, error) // raw `pw-link -l` output
}

// ExecCar runs the car tools as the helper's own user (root on the car),
// each in its own process group that dies with the helper (Pdeathsig), so a
// crashed helper leaves no write in flight behind its sweep.
type ExecCar struct {
	Timeout time.Duration // per command; 0 means 10 s (the tools can be slow under load)
}

func (c ExecCar) run(argv []string, env []string) (string, error) {
	t := c.Timeout
	if t == 0 {
		t = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}, env...)
	cmd.Dir = "/"
	cmd.SysProcAttr = sysProcAttr(nil)
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: 256 << 10}
	err := cmd.Run()
	return out.String(), err
}

func (c ExecCar) Write(w DVWrite) error {
	if w < 0 || w >= numDVWrites {
		return fmt.Errorf("camhelper: no such DV write %d", int(w))
	}
	_, err := c.run(dvWriteArgv[w], nil)
	return err
}

func (c ExecCar) Read(r DVRead) (string, error) {
	if r < 0 || r >= numDVReads {
		return "", fmt.Errorf("camhelper: no such DV read %d", int(r))
	}
	return c.run(dvReadArgv[r], nil)
}

func (c ExecCar) SetGain(g Gain) error {
	if g < 0 || g >= numGains {
		return fmt.Errorf("camhelper: no such gain %d", int(g))
	}
	_, err := c.run(gainArgv[g], nil)
	return err
}

func (c ExecCar) Links() (string, error) { return c.run(linksArgv, pipewireEnv) }

type limitedWriter struct {
	w interface{ Write([]byte) (int, error) }
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	k := len(p)
	if k > l.n {
		p = p[:l.n]
	}
	l.n -= len(p)
	if len(p) > 0 {
		l.w.Write(p)
	}
	return k, nil
}

// ---- parsing ----

var dvRe = regexp.MustCompile(`^\((true|false), '?(.*?)'?\)$`)

// parseDV returns the value of a get-local-dv output such as "(true, 'P')"
// and whether it is valid. Anything unparsable is invalid.
func parseDV(raw string) (string, bool) {
	m := dvRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil || m[1] != "true" {
		return "", false
	}
	return m[2], true
}

// isCleared reports whether a readback shows a camera DV at 0 (or unset).
func isCleared(raw string) bool {
	m := dvRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return false
	}
	if m[1] != "true" {
		return true // invalid/unset
	}
	switch m[2] {
	case "0", "false", "False", "None", "":
		return true
	}
	return false
}

// gearOf maps VAPI_shiftState to "P", "R", "N", "D" or "" (unknown/invalid).
func gearOf(raw string) string {
	v, ok := parseDV(raw)
	if !ok {
		return ""
	}
	switch v {
	case "P", "R", "N", "D":
		return v
	}
	return ""
}

// feeders parses `pw-link -l` and returns the source ports linked into the
// exterior channels TDM-playback:playback_9 and playback_10.
func feeders(links string) (ch9, ch10 []string) {
	cur := ""
	for _, line := range strings.Split(links, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			cur = strings.TrimSpace(line)
			continue
		}
		if !strings.Contains(line, "|<-") {
			continue
		}
		f := strings.Fields(line)
		src := f[len(f)-1]
		switch cur {
		case "TDM-playback:playback_9":
			ch9 = append(ch9, src)
		case "TDM-playback:playback_10":
			ch10 = append(ch10, src)
		}
	}
	return ch9, ch10
}

// routeState reports whether the webcam monitor feeds both exterior channels
// (ok) and whether anything else feeds them (foreign: Boombox, megaphone).
func routeState(links string) (ok, foreign bool) {
	ch9, ch10 := feeders(links)
	has := func(list []string) bool {
		for _, s := range list {
			if strings.HasPrefix(s, "webcam:monitor") {
				return true
			}
		}
		return false
	}
	for _, s := range append(append([]string(nil), ch9...), ch10...) {
		if !strings.HasPrefix(s, "webcam:monitor") {
			foreign = true
		}
	}
	return has(ch9) && has(ch10), foreign
}
