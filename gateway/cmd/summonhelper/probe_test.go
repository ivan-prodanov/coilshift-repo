package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/carlink/bustest"
	"airgapp/gateway/internal/carlink/mockcar"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/summon/marker"
	"airgapp/gateway/internal/summon/probe"
)

// TestProbeCommandSurvivesDeadOutput re-executes the test binary (TestMain) as SUMMONHELPER_TEST_PROBE: `summonhelper
// probe` through run, as main calls it, with the process's own stdout and stderr, on the private bus whose address is
// SUMMONHELPER_TEST_BUS. SUMMONHELPER_TEST_PROBE's value is the marker path, which the child puts in probeMarker.
const (
	envProbe = "SUMMONHELPER_TEST_PROBE"
	envBus   = "SUMMONHELPER_TEST_BUS"

	// Every probe test's car stands here (fix round 1, I2): a coordinate that leaked into the output would show.
	poseLat = "52.5200066"
	poseLon = "13.404954"
)

var leaks = []string{poseLat, poseLon, "52.52", "13.40"}

func TestMain(m *testing.M) {
	if mk := os.Getenv(envProbe); mk != "" {
		os.Exit(probeChild(mk))
	}
	os.Exit(m.Run())
}

// probeChild is SUMMONHELPER_TEST_PROBE: run(probe -duration 1s) on the private bus, with the marker at mk, writing to
// the process's real fd 1 and 2, then exit with run's code.
func probeChild(mk string) int {
	addr := os.Getenv(envBus)
	newBus = func() (carlink.Bus, error) {
		b, err := carlink.DialBus(addr)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	probeMarker = mk
	return run(context.Background(), []string{"probe", "-duration", "1s"}, os.Stdout, os.Stderr)
}

// setPose puts the car at the distinctive pose.
func setPose(car *mockcar.Car) {
	lat, _ := strconv.ParseFloat(poseLat, 64)
	lon, _ := strconv.ParseFloat(poseLon, 64)
	car.SetPose(lat, lon, 87.5, 0, "off", "P")
}

// useMarker points the probe's marker at a fresh directory for the test, and returns its path.
func useMarker(t *testing.T) string {
	t.Helper()
	mk := filepath.Join(t.TempDir(), "session")
	prev := probeMarker
	probeMarker = mk
	t.Cleanup(func() { probeMarker = prev })
	return mk
}

// probeBus is a FakeBus with the DBusBus's caller lookup and KeysUpdated watch, so the probe command's wiring shows.
type probeBus struct {
	*carlink.FakeBus

	mu   sync.Mutex
	keys []func()
}

var _ callerBus = (*probeBus)(nil)

func (b *probeBus) CallerInfo(context.Context, string) (uint32, uint32, error) { return 4242, 77, nil }

func (b *probeBus) SubscribeKeysUpdated(f func()) error {
	b.mu.Lock()
	b.keys = append(b.keys, f)
	b.mu.Unlock()
	return nil
}

// keysUpdated delivers one KeysUpdated to every subscriber, and says how many there were.
func (b *probeBus) keysUpdated() int {
	b.mu.Lock()
	fs := slices.Clone(b.keys)
	b.mu.Unlock()
	for _, f := range fs {
		f()
	}
	return len(fs)
}

// stubProbe makes newBus return a probeBus with the mock car, at the distinctive pose, on its FakePort, and counts
// the buses made.
func stubProbe(t *testing.T) (*probeBus, *mockcar.Car, *atomic.Int32) {
	t.Helper()
	pb := &probeBus{FakeBus: carlink.NewFakeBus()}
	car := mockcar.New(mockcar.Config{Port: mockcar.FakePort(pb.FakeBus)})
	setPose(car)
	var buses atomic.Int32
	prev := newBus
	newBus = func() (carlink.Bus, error) {
		buses.Add(1)
		return pb, nil
	}
	t.Cleanup(func() { newBus = prev })
	return pb, car, &buses
}

type cmdResult struct {
	code           int
	stdout, stderr string
}

// startCmd runs one command on its own goroutine.
func startCmd(args ...string) <-chan cmdResult {
	done := make(chan cmdResult, 1)
	go func() {
		var out, errb syncBuf
		code := run(context.Background(), args, &out, &errb)
		done <- cmdResult{code, out.String(), errb.String()}
	}()
	return done
}

func waitCmd(t *testing.T, done <-chan cmdResult, d time.Duration) cmdResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(d):
		t.Fatalf("the command did not return within %v", d)
		return cmdResult{}
	}
}

// waitHeartbeats waits until the car has received n heartbeats.
func waitHeartbeats(t *testing.T, car *mockcar.Car, n int) {
	t.Helper()
	ok := car.WaitFor(func(rs []mockcar.Received) bool {
		c := 0
		for _, r := range rs {
			if r.MsgType == "autopark:heartbeat_app_webrtc" {
				c++
			}
		}
		return c >= n
	}, 5*time.Second)
	if !ok {
		t.Fatalf("the car did not receive %d heartbeats", n)
	}
}

var (
	probeH1 = map[string]any{"data_channels_ready": true}
	probeH4 = map[string]any{"ice_state_change": true, "msg_type": "autopark:set_webrtc", "set_webrtc_value": false}
)

// allowedPhoneData fails t unless d is exactly the keepalive, or a heartbeat_app_webrtc whose only keys are
// car_timestamp, created:timestamp, msg_type and timestamp.
func allowedPhoneData(t *testing.T, d string) {
	t.Helper()
	if d == `{"msg_type":"control:keepalive"}` {
		return
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(d), &m); err != nil || m["msg_type"] != "autopark:heartbeat_app_webrtc" {
		t.Fatalf("the probe sent %s: only the keepalive and heartbeat_app_webrtc are allowed", d)
	}
	for k := range m {
		if k != "car_timestamp" && k != "created:timestamp" && k != "msg_type" && k != "timestamp" {
			t.Fatalf("a heartbeat carries %q: %s", k, d)
		}
	}
}

// noValues fails t if any of texts carries the pose.
func noValues(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, v := range leaks {
			if strings.Contains(text, v) {
				t.Fatalf("the output carries %q:\n%s", v, text)
			}
		}
	}
}

// disconnected checks a run that ended well (fix round 1, I2, on every command test): only the two signal members;
// every PeerCommsToCarserverData is {uid: test_phone, data} with allowed data; the signaling was H1, then the
// disconnect, which was the last emission; the marker is gone; and the output carries no pose.
func disconnected(t *testing.T, b *carlink.FakeBus, mk string, output ...string) {
	t.Helper()
	em := b.Emissions()
	var sig []map[string]any
	for _, e := range em {
		switch e.Member {
		case carlink.SignalLocalToRemoteSignaling:
			sig = append(sig, e.Body)
		case carlink.SignalPeerCommsToCarserverData:
			d, _ := e.Body["data"].(string)
			if e.Body["uid"] != carlink.PhoneUID || len(e.Body) != 2 {
				t.Fatalf("a PeerCommsToCarserverData body is %v", e.Body)
			}
			allowedPhoneData(t, d)
		default:
			t.Fatalf("the probe emitted %s", e.Member)
		}
	}
	if len(sig) != 2 || !reflect.DeepEqual(sig[0], probeH1) || !reflect.DeepEqual(sig[1], probeH4) {
		t.Fatalf("the signaling bodies are %v, want H1, then the disconnect", sig)
	}
	if !reflect.DeepEqual(em[len(em)-1].Body, probeH4) {
		t.Fatalf("the last emission is %v, want the disconnect", em[len(em)-1])
	}
	if marker.Present(mk) {
		t.Fatal("the session marker survived a confirmed disconnect")
	}
	noValues(t, output...)
}

// The probe command runs the probe on the system bus newBus gives, prints its report on stdout, and wires the
// DBusBus's caller lookup and KeysUpdated watch.
func TestProbeCommand(t *testing.T) {
	pb, car, buses := stubProbe(t)
	mk := useMarker(t)
	done := startCmd("probe", "-duration", "700ms")
	waitHeartbeats(t, car, 2)
	if n := pb.keysUpdated(); n != 1 {
		t.Fatalf("the probe subscribed to KeysUpdated %d times, want once", n)
	}
	r := waitCmd(t, done, 5*time.Second)
	if r.code != 0 {
		t.Fatalf("probe: exit %d: %s", r.code, r.stderr)
	}
	for _, s := range []string{"uid=4242 pid=77 sender=:fake.car", "keys updated: 1", "roles: not checked",
		"disconnect: confirmed; the session marker is cleared", `"autopark:heartbeat_car_webrtc"`} {
		if !strings.Contains(r.stdout, s) {
			t.Fatalf("the report lacks %q:\n%s", s, r.stdout)
		}
	}
	// -duration reached the probe: 700 ms at 10 Hz.
	m := regexp.MustCompile(`heartbeats: (\d+) sent, (\d+) echoed`).FindStringSubmatch(r.stdout)
	if m == nil {
		t.Fatalf("the report has no heartbeat line:\n%s", r.stdout)
	}
	if sent, _ := strconv.Atoi(m[1]); sent < 5 || sent > 9 {
		t.Fatalf("%d heartbeats in a 700 ms probe, want about 7", sent)
	}
	if n := buses.Load(); n != 1 {
		t.Fatalf("the probe made %d buses, want one", n)
	}
	disconnected(t, pb.FakeBus, mk, r.stdout, r.stderr)
}

// m1 and m6: the Config the command hands the probe. The periods and the style gate stay zero, so the probe's
// defaults (100 ms, 15 s, 2 s) apply; -duration defaults to 120 s; the marker is the helper's own, probeMarker,
// whose default is the path finish and the supervisor check; Exit stays the Link's; CallerInfo and KeysUpdated are
// the bus's.
func TestProbeCommandConfig(t *testing.T) {
	if probeMarker != "/run/airgapp-summon/session" {
		t.Fatalf("probeMarker is %q, want the helper's /run/airgapp-summon/session", probeMarker)
	}
	pb, _, _ := stubProbe(t)
	var got []probe.Config
	prev := runProbe
	runProbe = func(_ context.Context, cfg probe.Config) (probe.Report, error) {
		got = append(got, cfg)
		return probe.Report{}, nil
	}
	t.Cleanup(func() { runProbe = prev })
	for _, args := range [][]string{{"probe"}, {"probe", "-duration", "5s"}} {
		if code, _, e := runCmd(t, args...); code != 0 {
			t.Fatalf("%q: exit %d: %s", args, code, e)
		}
	}
	if len(got) != 2 {
		t.Fatalf("the probe ran %d times, want 2", len(got))
	}
	for i, want := range []time.Duration{120 * time.Second, 5 * time.Second} {
		cfg := got[i]
		if cfg.Duration != want || cfg.HBEvery != 0 || cfg.KeepaliveEvery != 0 || cfg.StyleWait != 0 {
			t.Fatalf("run %d: Duration %v, HBEvery %v, KeepaliveEvery %v, StyleWait %v; want %v and the probe's "+
				"defaults (zero)", i+1, cfg.Duration, cfg.HBEvery, cfg.KeepaliveEvery, cfg.StyleWait, want)
		}
		if cfg.MarkerPath != "/run/airgapp-summon/session" || cfg.Exit != nil || cfg.Bus != carlink.Bus(pb) ||
			cfg.Roles != nil || cfg.CallerInfo == nil || cfg.KeysUpdated == nil || cfg.Logf == nil || cfg.Out == nil {
			t.Fatalf("run %d's Config: marker %q, Exit set %v, bus %T, Roles set %v, CallerInfo set %v, "+
				"KeysUpdated set %v", i+1, cfg.MarkerPath, cfg.Exit != nil, cfg.Bus, cfg.Roles != nil,
				cfg.CallerInfo != nil, cfg.KeysUpdated != nil)
		}
	}
}

func TestProbeCommandRefusals(t *testing.T) {
	t.Run("a session marker", func(t *testing.T) {
		pb, _, _ := stubProbe(t)
		mk := useMarker(t)
		if err := os.WriteFile(mk, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, e := runCmd(t, "probe", "-duration", "300ms")
		if code != 1 || !strings.Contains(e, "summonhelper finish") || out != "" {
			t.Fatalf("probe with a session marker: exit %d, stderr %q, stdout %q; want exit 1 naming "+
				"`summonhelper finish`", code, e, out)
		}
		if em := pb.Emissions(); len(em) != 0 {
			t.Fatalf("emitted %v while the marker was present", em)
		}
		if !marker.Present(mk) {
			t.Fatal("the refusal removed the session marker")
		}
	})
	t.Run("the name is held", func(t *testing.T) {
		pb, _, _ := stubProbe(t)
		pb.TakeName(carlink.WebrtcName)
		mk := useMarker(t)
		code, out, e := runCmd(t, "probe", "-duration", "300ms")
		if code != 1 || !strings.Contains(e, "stop summonhelper and webrtc-comms first") || out != "" {
			t.Fatalf("probe with the name held: exit %d, stderr %q, stdout %q; want exit 1 and to stop "+
				"summonhelper and webrtc-comms first", code, e, out)
		}
		if em := pb.Emissions(); len(em) != 0 {
			t.Fatalf("emitted %v without the name", em)
		}
		if marker.Present(mk) {
			t.Fatal("the session marker was set without the name")
		}
	})
	// m2: the run directory is missing (a reboot with the helper kept down): fixed words name it and how to make it.
	t.Run("the run directory is missing", func(t *testing.T) {
		pb, _, _ := stubProbe(t)
		dir := filepath.Join(t.TempDir(), "airgapp-summon")
		prev := probeMarker
		probeMarker = filepath.Join(dir, "session")
		t.Cleanup(func() { probeMarker = prev })
		code, out, e := runCmd(t, "probe", "-duration", "300ms")
		want := dir + " is missing; the helper's run script creates it, or create it as root with `install -d " +
			"-o 1337 -g 4950 -m 2750 " + dir + "`"
		if code != 1 || !strings.Contains(e, want) || out != "" {
			t.Fatalf("probe without the run directory: exit %d, stderr %q, stdout %q; want exit 1 and %q",
				code, e, out, want)
		}
		if em := pb.Emissions(); len(em) != 0 {
			t.Fatalf("emitted %v without a marker", em)
		}
		if _, err := pb.NameOwner(context.Background(), carlink.WebrtcName); err == nil {
			t.Fatal("the name is still held after the refusal")
		}
	})
	t.Run("usage", func(t *testing.T) {
		_, _, buses := stubProbe(t)
		for _, a := range [][]string{{"-duration", "0"}, {"-duration=-1s"}, {"positional"}, {"-nonsense"},
			{"-duration"}, {"-duration", "300ms", "-keyinfo", "-authd-sha256", sentinelVIN},
			{"-duration", "300ms", "-keyinfo", "-authd-sha256", ""},
			{"-duration", "300ms", "-keyinfo"},                                          // no -pub: nothing to ask
			{"-duration", "300ms", "-pub", strings.Repeat("ab", 65)},                    // -pub without -keyinfo
			{"-duration", "300ms", "-keyinfo", "-pub", "zz" + sentinelVIN},              // a malformed key
			{"-duration", "300ms", "-keyinfo", "-pub", "04" + strings.Repeat("ab", 32)}, // a short key
			{"-duration", "300ms", "-keyinfo", "-pub", "04" + strings.Repeat("ab", 64)}, // not on the curve
			{"-duration", "300ms", "-marker", "/nonexistent-airgapp-probe/session"}} {   // m6: no typed marker path
			code, out, e := runCmd(t, append([]string{"probe"}, a...)...)
			if code != 2 || out != "" || !strings.HasPrefix(e, "summonhelper probe: ") {
				t.Fatalf("probe %q: exit %d, stdout %q, stderr %q; want the probe's usage error", a, code, out, e)
			}
			if strings.Contains(e, sentinelVIN) {
				t.Fatalf("probe %q echoes what was typed: %q", a, e)
			}
		}
		if n := buses.Load(); n != 0 {
			t.Fatalf("a usage error made %d buses", n)
		}
	})
}

// sentinelVIN is a VIN typed where the probe must never echo it.
const sentinelVIN = "1HGCM82633A004352"

// I1: a car that pushes no autopark:style after H1 gets no heartbeat and no keepalive, only H1 and the disconnect, and
// the command exits 1 with the fixed words.
func TestProbeCommandNoStyle(t *testing.T) {
	pb, car, _ := stubProbe(t)
	car.FreezeAfter(0)
	mk := useMarker(t)
	prev := runProbe
	runProbe = func(ctx context.Context, cfg probe.Config) (probe.Report, error) {
		cfg.StyleWait = 300 * time.Millisecond // the probe's own 2 s, shortened
		return probe.Run(ctx, cfg)
	}
	t.Cleanup(func() { runProbe = prev })
	code, out, e := runCmd(t, "probe", "-duration", "20s")
	if code != 1 || !strings.Contains(e, "no autopark:style after H1") {
		t.Fatalf("probe with no style: exit %d, stderr %q; want exit 1 and ErrNoStyle", code, e)
	}
	if !strings.Contains(out, "no autopark:style within 300ms of H1: the car made no fresh session (a session may "+
		"have been open, or the car did not accept the helper); only H1 and the disconnect were sent") {
		t.Fatalf("the report lacks the fixed words:\n%s", out)
	}
	for _, e := range pb.Emissions() {
		if e.Member == carlink.SignalPeerCommsToCarserverData {
			t.Fatalf("the probe sent %v without the car's style", e.Body)
		}
	}
	disconnected(t, pb.FakeBus, mk, out, e)
}

// roleOracle answers KeyRole from a table, and records the keys asked, in order.
type roleOracle struct {
	roles map[string]int32

	mu    sync.Mutex
	asked [][]byte
}

func (o *roleOracle) KeyRole(_ context.Context, pk []byte) (int32, error) {
	o.mu.Lock()
	o.asked = append(o.asked, slices.Clone(pk))
	o.mu.Unlock()
	r, ok := o.roles[string(pk)]
	if !ok {
		return 0, fmt.Errorf("authority: %w", authority.ErrKeyUnknown)
	}
	return r, nil
}

func (o *roleOracle) AuthOwnerIsAuthd(context.Context) (bool, error) { return true, nil }

func newPhoneKey(t *testing.T) []byte {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKey().Bytes()
}

// -keyinfo asks authd for the role of each -pub key, in -pub's order (each distinct key where it first appears),
// through the oracle -authd-sha256 pins, and prints only each entry's index and role: never a key, its fingerprint
// or its KeyInfo Id.
func TestProbeCommandKeyinfo(t *testing.T) {
	pb, _, _ := stubProbe(t)
	k1, k2, k3 := newPhoneKey(t), newPhoneKey(t), newPhoneKey(t)
	o := &roleOracle{roles: map[string]int32{string(k1): 2, string(k2): 3}}
	var gotPin atomic.Pointer[string]
	prev := newOracle
	newOracle = func(pin string) (authority.Oracle, error) {
		gotPin.Store(&pin)
		return o, nil
	}
	t.Cleanup(func() { newOracle = prev })
	pubs := hex.EncodeToString(k1) + ", " + hex.EncodeToString(k2) + "," + strings.ToUpper(hex.EncodeToString(k1)) +
		"," + hex.EncodeToString(k3)
	mk := useMarker(t)
	pin := strings.Repeat("cd", 32)
	code, out, e := runCmd(t, "probe", "-duration", "300ms", "-keyinfo", "-pub", pubs, "-authd-sha256", pin)
	if code != 0 {
		t.Fatalf("probe -keyinfo: exit %d: %s", code, e)
	}
	want := "roles:\n  entry 1: role 2\n  entry 2: role 3\n  entry 3: role error: authority: " +
		authority.ErrKeyUnknown.Error() + "\n"
	if !strings.Contains(out, want) {
		t.Fatalf("the report lacks\n%s\nin:\n%s", want, out)
	}
	if p := gotPin.Load(); p == nil || *p != pin {
		t.Fatalf("-authd-sha256 did not reach the oracle: %v", p)
	}
	o.mu.Lock()
	asked := o.asked
	o.mu.Unlock()
	if len(asked) != 3 || !slices.Equal(asked[0], k1) || !slices.Equal(asked[1], k2) || !slices.Equal(asked[2], k3) {
		t.Fatalf("the oracle was asked %d keys, want the three distinct -pub keys, in order", len(asked))
	}
	for i, pk := range [][]byte{k1, k2, k3} {
		h := hex.EncodeToString(pk)
		for _, v := range []string{h, strings.ToUpper(h), h[2:18], h[len(h)-16:], keys.Fingerprint(pk),
			hex.EncodeToString(authority.KeyID(pk))} {
			if strings.Contains(out, v) || strings.Contains(e, v) {
				t.Fatalf("the output carries key %d's material %q:\n%s\n%s", i+1, v, out, e)
			}
		}
	}
	disconnected(t, pb.FakeBus, mk, out, e)
}

// Without an oracle (darwin's authority.NewDBus fails), -keyinfo reports the error once and skips the roles; the
// rest of the probe runs.
func TestProbeCommandKeyinfoWithoutOracle(t *testing.T) {
	pb, _, _ := stubProbe(t)
	const why = "authority: D-Bus oracle requires linux"
	var gotPin atomic.Pointer[string]
	prev := newOracle
	newOracle = func(pin string) (authority.Oracle, error) {
		gotPin.Store(&pin)
		return nil, errors.New(why)
	}
	t.Cleanup(func() { newOracle = prev })
	mk := useMarker(t)
	code, out, e := runCmd(t, "probe", "-duration", "300ms", "-keyinfo", "-pub", hex.EncodeToString(newPhoneKey(t)))
	if code != 0 {
		t.Fatalf("probe -keyinfo without an oracle: exit %d: %s", code, e)
	}
	if p := gotPin.Load(); p == nil || *p != authority.DefaultAuthdSHA256 {
		t.Fatalf("the oracle got pin %v, want the default authority.DefaultAuthdSHA256", p)
	}
	if n := strings.Count(e, why); n != 1 {
		t.Fatalf("the oracle's error is logged %d times, want once:\n%s", n, e)
	}
	for _, s := range []string{"roles: not checked", "heartbeats: ", "disconnect: confirmed"} {
		if !strings.Contains(out, s) {
			t.Fatalf("the report lacks %q:\n%s", s, out)
		}
	}
	disconnected(t, pb.FakeBus, mk, out, e)
}

// A dead output reader cannot skip the probe's teardown (the pre-review ruling). The probe runs in a process of its
// own whose stdout and stderr are pipes with their read ends closed, as when a pipe's reader dies with the SSH
// session. Its first log line, right after H1, fails with EPIPE; unless SIGPIPE is ignored, Go kills the process there,
// before the disconnect. It must still send H1, then the disconnect, clear the marker and exit 0.
func TestProbeCommandSurvivesDeadOutput(t *testing.T) {
	b := bustest.Start(t)
	car := mockcar.New(mockcar.Config{Port: mockcar.DBusPort(b.Conn(t))})
	setPose(car)
	mk := filepath.Join(t.TempDir(), "session")
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// The readers are gone before the probe writes a byte.
	outR.Close()
	errR.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envProbe+"="+mk, envBus+"="+b.Addr())
	cmd.Stdout, cmd.Stderr = outW, errW // *os.File: the child's fd 1 and 2 themselves, with no copier
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outW.Close()
	errW.Close()
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("the probe did not exit within 15 s")
	}
	// The car saw H1 first, heartbeats, and the disconnect last.
	isH4 := func(r mockcar.Received) bool {
		return r.Member == carlink.SignalLocalToRemoteSignaling && r.MsgType == "autopark:set_webrtc"
	}
	gotH4 := car.WaitFor(func(rs []mockcar.Received) bool { return len(rs) > 0 && isH4(rs[len(rs)-1]) }, 3*time.Second)
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		t.Fatalf("the probe died from %v with its output gone; the car got the disconnect: %v; the marker stays: %v",
			ws.Signal(), gotH4, marker.Present(mk))
	}
	if code := ws.ExitStatus(); code != 0 {
		t.Fatalf("the probe exited %d with its output gone, want 0", code)
	}
	if !gotH4 {
		t.Fatalf("the car's last input is not the disconnect: %d inputs", len(car.Received()))
	}
	rs := car.Received()
	if first := rs[0]; first.Member != carlink.SignalLocalToRemoteSignaling ||
		!strings.Contains(string(first.Data), "data_channels_ready") {
		t.Fatalf("the car's first input is %s %q, want H1", first.Member, first.Data)
	}
	hbs, signals := 0, 0
	for _, r := range rs {
		switch r.Member {
		case carlink.SignalPeerCommsToCarserverData:
			allowedPhoneData(t, string(r.Data))
			if r.MsgType == "autopark:heartbeat_app_webrtc" {
				hbs++
			}
		case carlink.SignalLocalToRemoteSignaling:
			signals++
		default:
			t.Fatalf("the car received %s", r.Member)
		}
	}
	if hbs < 5 || signals != 2 {
		t.Fatalf("the car got %d heartbeats and %d signaling calls in a 1 s probe, want about 10 and H1 and the "+
			"disconnect", hbs, signals)
	}
	if v := car.Violations(); len(v) != 0 {
		t.Fatalf("the car saw violations: %q", v)
	}
	if car.Connected() {
		t.Fatal("the car still sees the session as connected")
	}
	if marker.Present(mk) {
		t.Fatal("the session marker survived the probe")
	}
}

// signalPID sends sig to pid: here this test binary itself, which runs the probe in-process. It refuses pid <= 1 and
// sends nothing (CR-1): kill(2) with pid 0 signals the caller's whole process group (go test's, and the shell's), -1
// every process the caller may signal, and 1 is init. Every signal this package's tests send goes through it;
// internal/supervise's TestNoUnguardedKill keeps it so.
func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal pid %d: not a single child process", pid)
	}
	return syscall.Kill(pid, sig)
}

// SIGINT, SIGTERM, SIGHUP and SIGQUIT (m8) each end the probe early, with the disconnect. The test holds its own
// Notify for the signal, so a signal the probe does not map leaves the probe running (and the test failing) instead
// of ending the test binary.
func TestProbeCommandSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			sink := make(chan os.Signal, 1)
			signal.Notify(sink, sig)
			defer signal.Stop(sink)
			pb, car, _ := stubProbe(t)
			mk := useMarker(t)
			done := startCmd("probe", "-duration", "20s")
			waitHeartbeats(t, car, 2)
			if err := signalPID(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			r := waitCmd(t, done, 3*time.Second)
			if r.code != 0 || !strings.Contains(r.stdout, "ended: cancelled") {
				t.Fatalf("probe after %v: exit %d, stdout %q, stderr %q; want exit 0 and a cancelled run",
					sig, r.code, r.stdout, r.stderr)
			}
			disconnected(t, pb.FakeBus, mk, r.stdout, r.stderr)
		})
	}
}
