package probe_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/carlink/bustest"
	"airgapp/gateway/internal/carlink/mockcar"
	"airgapp/gateway/internal/summon/marker"
	"airgapp/gateway/internal/summon/probe"
)

// The brief's run: real time, a 1.2 s duration, the real 100 ms heartbeat period.
const (
	testDuration = 1200 * time.Millisecond
	testHB       = 100 * time.Millisecond

	tHB        = "autopark:heartbeat_app_webrtc"
	tEcho      = "autopark:heartbeat_car_webrtc"
	tKeepalive = `{"msg_type":"control:keepalive"}`

	// Every rig's car stands here (I2): a coordinate that leaked into a report or a log would show.
	poseLat = "52.5200066"
	poseLon = "13.404954"
)

var (
	h1 = map[string]any{"data_channels_ready": true}
	h4 = map[string]any{"ice_state_change": true, "msg_type": "autopark:set_webrtc", "set_webrtc_value": false}

	// leaks are the pose's values, whole and cut short, that no report or log may carry.
	leaks = []string{poseLat, poseLon, "52.52", "13.40"}
)

// syncBuf is a probe's Out.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// setPose puts the car at the distinctive pose.
func setPose(car *mockcar.Car) {
	lat, _ := strconv.ParseFloat(poseLat, 64)
	lon, _ := strconv.ParseFloat(poseLon, 64)
	car.SetPose(lat, lon, 87.5, 0, "off", "P")
}

// rig is one probe on a FakeBus, with the mock car on the bus's FakePort, standing at the distinctive pose.
type rig struct {
	t      *testing.T
	bus    *carlink.FakeBus
	car    *mockcar.Car
	marker string
	out    syncBuf

	// Whether the marker existed when H1 and when the disconnect was emitted: 1 yes, 2 no, 0 not emitted.
	atH1, atH4 atomic.Int32

	mu   sync.Mutex
	logs []string
}

func newRig(t *testing.T, carCfg ...func(*mockcar.Config)) *rig {
	t.Helper()
	r := &rig{t: t, bus: carlink.NewFakeBus(), marker: filepath.Join(t.TempDir(), "session")}
	r.bus.OnEmit(func(member string, body map[string]any) {
		at := &r.atH1
		switch {
		case reflect.DeepEqual(body, h4):
			at = &r.atH4
		case !reflect.DeepEqual(body, h1):
			return
		}
		v := int32(2)
		if marker.Present(r.marker) {
			v = 1
		}
		at.CompareAndSwap(0, v)
	})
	cfg := mockcar.Config{Port: mockcar.FakePort(r.bus)}
	for _, f := range carCfg {
		f(&cfg)
	}
	r.car = mockcar.New(cfg)
	setPose(r.car)
	return r
}

func (r *rig) logf(format string, args ...any) {
	r.mu.Lock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *rig) logText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

// config is the rig's probe: the test duration at the real cadence, a CallerInfo that answers uid 1337 pid 4242, and
// an Exit that fails the test instead of exiting.
func (r *rig) config() probe.Config {
	return probe.Config{
		Bus:            r.bus,
		Duration:       testDuration,
		HBEvery:        testHB,
		KeepaliveEvery: 15 * time.Second,
		CallerInfo:     func(context.Context, string) (uint32, uint32, error) { return 1337, 4242, nil },
		MarkerPath:     r.marker,
		Out:            &r.out,
		Logf:           r.logf,
		Exit:           func(code int) { r.t.Errorf("an emit overran its deadline: Exit(%d)", code) },
	}
}

type result struct {
	rep probe.Report
	err error
}

// start runs the probe on its own goroutine.
func start(ctx context.Context, cfg probe.Config) <-chan result {
	done := make(chan result, 1)
	go func() {
		rep, err := probe.Run(ctx, cfg)
		done <- result{rep, err}
	}()
	return done
}

func waitResult(t *testing.T, done <-chan result, d time.Duration) result {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(d):
		t.Fatalf("Run did not return within %v", d)
		return result{}
	}
}

func countType(rs []mockcar.Received, mt string) int {
	n := 0
	for _, r := range rs {
		if r.MsgType == mt {
			n++
		}
	}
	return n
}

// heartbeats waits until the car has received n heartbeats.
func (r *rig) heartbeats(n int) {
	r.t.Helper()
	if !r.car.WaitFor(func(rs []mockcar.Received) bool { return countType(rs, tHB) >= n }, 5*time.Second) {
		r.t.Fatalf("the car did not receive %d heartbeats", n)
	}
}

// phoneData is the data of every PeerCommsToCarserverData emitted, in order.
func phoneData(t *testing.T, em []carlink.Emission) []string {
	t.Helper()
	var out []string
	for _, e := range em {
		if e.Member != carlink.SignalPeerCommsToCarserverData {
			continue
		}
		if e.Body["uid"] != carlink.PhoneUID || len(e.Body) != 2 {
			t.Fatalf("a PeerCommsToCarserverData body is %v, want {uid: %s, data}", e.Body, carlink.PhoneUID)
		}
		d, ok := e.Body["data"].(string)
		if !ok {
			t.Fatalf("a PeerCommsToCarserverData data is %T", e.Body["data"])
		}
		out = append(out, d)
	}
	return out
}

// signaling is every LocalToRemoteSignaling body, in order.
func signaling(em []carlink.Emission) []map[string]any {
	var out []map[string]any
	for _, e := range em {
		if e.Member == carlink.SignalLocalToRemoteSignaling {
			out = append(out, e.Body)
		}
	}
	return out
}

// decode decodes one phone message, numbers as json.Number.
func decode(t *testing.T, data string) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(data))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatalf("phone data %q is not a JSON object: %v", data, err)
	}
	return m
}

// heartbeatsSent is the decoded heartbeats among data.
func heartbeatsSent(t *testing.T, data []string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, d := range data {
		if m := decode(t, d); m["msg_type"] == tHB {
			out = append(out, m)
		}
	}
	return out
}

// allowedData fails t unless d is exactly the keepalive, or a heartbeat_app_webrtc whose only keys are car_timestamp,
// created:timestamp, msg_type and timestamp.
func allowedData(t *testing.T, d string) {
	t.Helper()
	if d == tKeepalive {
		return
	}
	m := decode(t, d)
	if m["msg_type"] != tHB {
		t.Fatalf("the probe sent %s: only the keepalive and heartbeat_app_webrtc are allowed", d)
	}
	for k := range m {
		if k != "car_timestamp" && k != "created:timestamp" && k != "msg_type" && k != "timestamp" {
			t.Fatalf("a heartbeat carries %q: %s", k, d)
		}
	}
}

// allowedSends is the no-motion check over a FakeBus's emissions, on any path (I2): only the two signal members; every
// PeerCommsToCarserverData is {uid: test_phone, data} with allowed data; every signaling body is H1 or the
// disconnect, H1 first and the disconnect last when there.
func allowedSends(t *testing.T, em []carlink.Emission) {
	t.Helper()
	for _, e := range em {
		if e.Member != carlink.SignalPeerCommsToCarserverData && e.Member != carlink.SignalLocalToRemoteSignaling {
			t.Fatalf("the probe emitted %s", e.Member)
		}
	}
	for _, d := range phoneData(t, em) {
		allowedData(t, d)
	}
	for i, b := range signaling(em) {
		if reflect.DeepEqual(b, h1) && i == 0 || reflect.DeepEqual(b, h4) && i > 0 {
			continue
		}
		t.Fatalf("signaling body %d is %v: only H1, first, and the disconnect after it", i, b)
	}
}

// allowedReceived is allowedSends over what a mock car received (a real bus): only the two signal members, allowed
// phone data, and signaling that is exactly H1, then the disconnect, last.
func allowedReceived(t *testing.T, rs []mockcar.Received) {
	t.Helper()
	var sig []string
	for _, x := range rs {
		switch x.Member {
		case carlink.SignalPeerCommsToCarserverData:
			allowedData(t, string(x.Data))
		case carlink.SignalLocalToRemoteSignaling:
			sig = append(sig, string(x.Data))
		default:
			t.Fatalf("the car received %s", x.Member)
		}
	}
	wantH1, _ := json.Marshal(h1)
	wantH4, _ := json.Marshal(h4)
	if len(sig) != 2 || sig[0] != string(wantH1) || sig[1] != string(wantH4) {
		t.Fatalf("the car's signaling is %q, want H1, then the disconnect", sig)
	}
	if len(rs) == 0 || rs[0].Member != carlink.SignalLocalToRemoteSignaling ||
		rs[len(rs)-1].Member != carlink.SignalLocalToRemoteSignaling {
		t.Fatal("H1 is not the car's first input, or the disconnect not its last")
	}
}

// noValues fails t if any of texts carries the pose.
func noValues(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, v := range leaks {
			if strings.Contains(text, v) {
				t.Fatalf("a report or log carries %q:\n%s", v, text)
			}
		}
	}
}

// clean checks a run that ended well: only allowed phone data; the signaling was H1, then the disconnect, which was
// the last emission, and nothing more is emitted afterwards; the car saw no violation and is disconnected; the
// marker was set before H1, still there when the disconnect went out, and is gone; and neither the report nor the
// logs carry the pose.
func (r *rig) clean() {
	r.t.Helper()
	if h1, h4 := r.atH1.Load(), r.atH4.Load(); h1 != 1 || h4 != 1 {
		r.t.Fatalf("the session marker at H1: %d, at the disconnect: %d (1 present, 2 absent); want it set before "+
			"H1 and kept until the disconnect is confirmed", h1, h4)
	}
	em := r.bus.Emissions()
	allowedSends(r.t, em)
	if sig := signaling(em); len(sig) != 2 || !reflect.DeepEqual(sig[0], h1) || !reflect.DeepEqual(sig[1], h4) {
		r.t.Fatalf("the signaling bodies are %v, want H1, then the disconnect", sig)
	}
	if last := em[len(em)-1]; last.Member != carlink.SignalLocalToRemoteSignaling || !reflect.DeepEqual(last.Body, h4) {
		r.t.Fatalf("the last emission is %v, want the disconnect", last)
	}
	time.Sleep(3 * testHB) // a sender left running would emit again by now
	if n := len(r.bus.Emissions()); n != len(em) {
		r.t.Fatalf("%d emissions after Run returned", n-len(em))
	}
	if !r.car.WaitFor(func([]mockcar.Received) bool { return !r.car.Connected() }, 2*time.Second) {
		r.t.Fatal("the car still sees the session as connected")
	}
	if v := r.car.Violations(); len(v) != 0 {
		r.t.Fatalf("the car saw violations: %q", v)
	}
	if marker.Present(r.marker) {
		r.t.Fatal("the session marker survived a confirmed disconnect")
	}
	noValues(r.t, r.out.String(), r.logText())
}

// handlerBus is a FakeBus that keeps the Handlers the probe serves, so a test can make car calls from any sender. Its
// onServe runs once the handlers are installed (frames then come before H1); its onH1 runs as H1 is emitted, before
// the FakeBus records it (frames then come in the window, ahead of the car's own).
type handlerBus struct {
	*carlink.FakeBus
	onServe, onH1 func(carlink.Handlers)

	mu sync.Mutex
	h  *carlink.Handlers
}

func (b *handlerBus) Serve(h carlink.Handlers) error {
	b.mu.Lock()
	b.h = &h
	b.mu.Unlock()
	err := b.FakeBus.Serve(h)
	if err == nil && b.onServe != nil {
		b.onServe(h)
	}
	return err
}

func (b *handlerBus) Emit(member string, body map[string]any) error {
	if _, ok := body["data_channels_ready"]; ok && b.onH1 != nil {
		b.mu.Lock()
		h := b.h
		b.mu.Unlock()
		b.onH1(*h)
	}
	return b.FakeBus.Emit(member, body)
}

// frame is one car call's CarFrame from sender.
func frame(sender, data string) carlink.CarFrame {
	return carlink.CarFrame{Channel: carlink.ChannelTCP, UID: carlink.PhoneUID, Sender: sender, Data: []byte(data)}
}

// ---- The brief's tests ----

func TestProbeCountsEchoes(t *testing.T) {
	r := newRig(t)
	rep, err := probe.Run(context.Background(), r.config())
	if err != nil {
		t.Fatal(err)
	}
	if rep.HeartbeatsSent < 10 || rep.HeartbeatsSent > 14 {
		t.Fatalf("%d heartbeats sent in %v at %v, want about 12", rep.HeartbeatsSent, testDuration, testHB)
	}
	if rep.Echoes < rep.HeartbeatsSent-1 || rep.Echoes > rep.HeartbeatsSent {
		t.Fatalf("%d echoes for %d heartbeats, want sent - 1 or more", rep.Echoes, rep.HeartbeatsSent)
	}
	if got := rep.ByType[tEcho].Count; got != rep.Echoes {
		t.Fatalf("ByType counts %d echoes, Echoes %d", got, rep.Echoes)
	}
	// The count is what went out.
	if n := len(heartbeatsSent(t, phoneData(t, r.bus.Emissions()))); n != rep.HeartbeatsSent {
		t.Fatalf("the bus carried %d heartbeats, the report says %d", n, rep.HeartbeatsSent)
	}
	// Car frames arrive after H1: what the car pushes on data_channels_ready.
	for _, mt := range []string{"autopark:style", "autopark:status", "vehicle_data:location"} {
		if st := rep.ByType[mt]; st.Count != 1 || st.First < 0 || st.First > testHB {
			t.Fatalf("ByType[%q] = %+v, want one frame right after H1", mt, st)
		}
	}
	if want := []string{"uid=1337 pid=4242 sender=:fake.car"}; !slices.Equal(rep.Callers, want) {
		t.Fatalf("Callers %q, want %q", rep.Callers, want)
	}
	text := r.out.String()
	for _, s := range []string{
		fmt.Sprintf("heartbeats: %d sent, %d echoed", rep.HeartbeatsSent, rep.Echoes),
		`"autopark:status"`, `"` + tEcho + `"`, "uid=1337 pid=4242 sender=:fake.car",
		"keys updated: not watched", "roles: not checked", "disconnect: confirmed",
	} {
		if !strings.Contains(text, s) {
			t.Fatalf("the report lacks %q:\n%s", s, text)
		}
	}
	r.clean()
}

func TestProbeSendsOnlyAllowedMessages(t *testing.T) {
	r := newRig(t)
	if _, err := probe.Run(context.Background(), r.config()); err != nil {
		t.Fatal(err)
	}
	em := r.bus.Emissions()
	keepalives := 0
	for _, d := range phoneData(t, em) {
		allowedData(t, d)
		switch m := decode(t, d); m["msg_type"] {
		case tHB:
			if m["timestamp"] == nil || m["created:timestamp"] == nil {
				t.Fatalf("a heartbeat lacks its timestamps: %s", d)
			}
		default:
			keepalives++
		}
	}
	if keepalives != 1 {
		t.Fatalf("%d keepalives in %v, want the one at the start", keepalives, testDuration)
	}
	if em[0].Member != carlink.SignalLocalToRemoteSignaling {
		t.Fatalf("the first emission is %v, want H1", em[0])
	}
	r.clean()
}

func TestProbeCarTimestampEcho(t *testing.T) {
	// The car's clock runs in 2001, so its car_timestamp cannot be mistaken for the probe's own milliseconds.
	carEpoch := time.UnixMilli(1_000_000_000_000)
	t0 := time.Now()
	r := newRig(t, func(c *mockcar.Config) { c.Now = func() time.Time { return carEpoch.Add(time.Since(t0)) } })
	if _, err := probe.Run(context.Background(), r.config()); err != nil {
		t.Fatal(err)
	}
	hbs := heartbeatsSent(t, phoneData(t, r.bus.Emissions()))
	if len(hbs) < 10 {
		t.Fatalf("%d heartbeats, want about 12", len(hbs))
	}
	if _, ok := hbs[0]["car_timestamp"]; ok {
		t.Fatalf("the first heartbeat carries car_timestamp before any echo: %v", hbs[0])
	}
	lo, hi := carEpoch.UnixMilli(), carEpoch.Add(time.Since(t0)).UnixMilli()
	var prev, first int64
	for i, hb := range hbs[1:] {
		n, ok := hb["car_timestamp"].(json.Number)
		if !ok {
			t.Fatalf("heartbeat %d has no numeric car_timestamp after the echoes began: %v", i+2, hb)
		}
		ts, err := n.Int64()
		if err != nil || ts < lo || ts > hi {
			t.Fatalf("heartbeat %d carries car_timestamp %s, want the car's clock (%d..%d)", i+2, n, lo, hi)
		}
		if ts < prev {
			t.Fatalf("heartbeat %d carries car_timestamp %d, older than the previous %d", i+2, ts, prev)
		}
		if i == 0 {
			first = ts
		}
		prev = ts
		// Its own timestamps are the probe's clock, not the car's.
		for _, k := range []string{"timestamp", "created:timestamp"} {
			own, _ := hb[k].(json.Number).Int64()
			if own < t0.UnixMilli() || own > time.Now().UnixMilli() {
				t.Fatalf("heartbeat %d's %s is %d, want the probe's clock", i+2, k, own)
			}
		}
	}
	// It follows the latest echo, not the first one.
	if prev-first < int64(testDuration/time.Millisecond)/2 {
		t.Fatalf("car_timestamp moved %d ms over the run, want it to follow each echo", prev-first)
	}
}

func TestProbeReportHasNoValues(t *testing.T) {
	const vin = "5YJ3E1EA7KF000000"
	r := newRig(t)
	done := start(context.Background(), r.config())
	r.heartbeats(2)
	// A frame whose values carry the VIN and the position.
	r.bus.CallCarserverToPeer(map[string]any{"uid": carlink.PhoneUID, "channel": carlink.ChannelTCP,
		"data": `{"msg_type":"vehicle_data:identity","vin":"` + vin + `","latitude":` + poseLat + `,"longitude":` +
			poseLon + `}`})
	res := waitResult(t, done, 10*time.Second)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.rep.ByType["vehicle_data:location"].Count != 1 || res.rep.ByType["vehicle_data:identity"].Count != 1 {
		t.Fatalf("ByType %v lacks the location or the identity frame", res.rep.ByType)
	}
	text, logs := r.out.String(), r.logText()
	if !strings.Contains(text, `"vehicle_data:location"`) {
		t.Fatalf("the report does not list the location frame:\n%s", text)
	}
	for _, v := range append([]string{vin}, leaks...) {
		if strings.Contains(text, v) || strings.Contains(logs, v) {
			t.Fatalf("the report or the log carries %q:\n%s\n%s", v, text, logs)
		}
		for mt := range res.rep.ByType {
			if strings.Contains(mt, v) {
				t.Fatalf("ByType key %q carries %q", mt, v)
			}
		}
	}
	r.clean()
}

func TestProbeFreezeVisible(t *testing.T) {
	r := newRig(t)
	// Style, status and location on H1, then two echoes, then nothing.
	r.car.FreezeAfter(5)
	rep, err := probe.Run(context.Background(), r.config())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Echoes != 2 || rep.HeartbeatsSent < 10 {
		t.Fatalf("%d echoes for %d heartbeats, want the echoes to stop at 2", rep.Echoes, rep.HeartbeatsSent)
	}
	// The remaining time: from the last echo (the car's answer to the second heartbeat) to the end of the run.
	rs := r.car.Received()
	var h1At, lastEcho time.Time
	hbs := 0
	for _, x := range rs {
		switch {
		case x.Member == carlink.SignalLocalToRemoteSignaling && h1At.IsZero():
			h1At = x.At
		case x.MsgType == tHB:
			if hbs++; hbs == 2 {
				lastEcho = x.At
			}
		}
	}
	remaining := h1At.Add(testDuration).Sub(lastEcho)
	st := rep.ByType[tEcho]
	if st.Count != 2 || st.MaxGap < remaining-50*time.Millisecond || st.MaxGap < testDuration/2 {
		t.Fatalf("the echoes' stats are %+v, want 2 frames and a largest gap of at least the remaining %v",
			st, remaining)
	}
	if text := r.out.String(); !strings.Contains(text, fmt.Sprintf("heartbeats: %d sent, 2 echoed", rep.HeartbeatsSent)) {
		t.Fatalf("the report does not show the echoes stopping:\n%s", text)
	}
	r.clean()
}

func TestProbeRefusesWhenNameTaken(t *testing.T) {
	r := newRig(t)
	r.bus.TakeName(carlink.WebrtcName)
	_, err := probe.Run(context.Background(), r.config())
	if !errors.Is(err, carlink.ErrNameTaken) || !strings.Contains(err.Error(), "stop summonhelper and webrtc-comms") {
		t.Fatalf("Run with the name taken: %v; want ErrNameTaken and to stop summonhelper and webrtc-comms first", err)
	}
	if em := r.bus.Emissions(); len(em) != 0 {
		t.Fatalf("emitted %v without the name", em)
	}
	if marker.Present(r.marker) {
		t.Fatal("the session marker was set without the name")
	}
	if out := r.out.String(); out != "" {
		t.Fatalf("a report after a refusal: %q", out)
	}
}

// countingBus counts the name requests a probe makes.
type countingBus struct {
	*carlink.FakeBus
	requests atomic.Int32
}

func (b *countingBus) RequestName(name string, queue bool) (bool, error) {
	b.requests.Add(1)
	return b.FakeBus.RequestName(name, queue)
}

func TestProbeRefusesWhileMarkerPresent(t *testing.T) {
	r := newRig(t)
	if err := os.WriteFile(r.marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cb := &countingBus{FakeBus: r.bus}
	cfg := r.config()
	cfg.Bus = cb
	_, err := probe.Run(context.Background(), cfg)
	if !errors.Is(err, probe.ErrMarkerPresent) || !strings.Contains(err.Error(), "summonhelper finish") {
		t.Fatalf("Run with a session marker: %v; want a refusal naming `summonhelper finish`", err)
	}
	if em := r.bus.Emissions(); len(em) != 0 {
		t.Fatalf("emitted %v while the marker was present", em)
	}
	if n := cb.requests.Load(); n != 0 {
		t.Fatalf("requested the name %d times while the marker was present", n)
	}
	if !marker.Present(r.marker) {
		t.Fatal("the refusal removed the session marker")
	}
	if out := r.out.String(); out != "" {
		t.Fatalf("a report after a refusal: %q", out)
	}
}

func TestProbeDisconnectsOnCancel(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.Duration = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := start(ctx, cfg)
	r.heartbeats(5)
	cancel()
	res := waitResult(t, done, 2*time.Second)
	if res.err != nil {
		t.Fatalf("a cancelled run with a confirmed disconnect: %v", res.err)
	}
	if res.rep.HeartbeatsSent < 5 || res.rep.Echoes == 0 {
		t.Fatalf("the cancelled run reports %d heartbeats and %d echoes", res.rep.HeartbeatsSent, res.rep.Echoes)
	}
	r.clean()
	if text := r.out.String(); !strings.Contains(text, "cancelled") || !strings.Contains(text, "disconnect: confirmed") {
		t.Fatalf("the report does not say the run was cancelled and disconnected:\n%s", text)
	}
}

// slowBus makes each phone-data emit take send before the FakeBus records it, and the NameOwner call that confirms
// the disconnect take confirm, both well inside their 100 ms bounds. sending, if set, is closed when the first
// phone-data emit starts.
type slowBus struct {
	*carlink.FakeBus
	send, confirm time.Duration
	sending       chan struct{}
	once          sync.Once
}

func (b *slowBus) Emit(member string, body map[string]any) error {
	if member == carlink.SignalPeerCommsToCarserverData {
		if b.sending != nil {
			b.once.Do(func() { close(b.sending) })
		}
		time.Sleep(b.send)
	}
	return b.FakeBus.Emit(member, body)
}

func (b *slowBus) NameOwner(ctx context.Context, name string) (string, error) {
	time.Sleep(b.confirm)
	return b.FakeBus.NameOwner(ctx, name)
}

// Rule 7: the senders stop, and are waited for, before the disconnect. At a 3 ms cadence a send of 20 ms is always
// under way when the run ends, and a confirmation of 30 ms leaves time for more: a sender not waited for, or still
// running while the disconnect goes out, would emit after it.
func TestProbeStopsSendersBeforeDisconnect(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.Bus = &slowBus{FakeBus: r.bus, send: 20 * time.Millisecond, confirm: 30 * time.Millisecond}
	cfg.Duration = 30 * time.Second
	cfg.HBEvery, cfg.KeepaliveEvery = 3*time.Millisecond, 3*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := start(ctx, cfg)
	r.heartbeats(10)
	cancel()
	if res := waitResult(t, done, 2*time.Second); res.err != nil {
		t.Fatal(res.err)
	}
	r.clean()
}

func TestProbeKeepsMarkerWhenDisconnectFails(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.Duration = 30 * time.Second
	done := start(context.Background(), cfg)
	r.heartbeats(3)
	r.bus.LoseName(carlink.WebrtcName)
	res := waitResult(t, done, 2*time.Second) // the lost name ends the run at once
	if res.err == nil || !strings.Contains(res.err.Error(), "summonhelper finish") {
		t.Fatalf("Run after the name was lost: %v; want an error naming `summonhelper finish`", res.err)
	}
	if !marker.Present(r.marker) {
		t.Fatal("the session marker was cleared without a confirmed disconnect")
	}
	// Only allowed messages on this path too, and the disconnect was still sent, last.
	em := r.bus.Emissions()
	allowedSends(t, em)
	if last := em[len(em)-1]; !reflect.DeepEqual(last.Body, h4) {
		t.Fatalf("the last emission is %v, want the disconnect", last)
	}
	text := r.out.String()
	if !strings.Contains(text, "not confirmed") || !strings.Contains(text, "summonhelper finish") {
		t.Fatalf("the report does not say the marker stays:\n%s", text)
	}
	noValues(t, text, r.logText())
}

// Global Constraints: the probe's package has no code path for a verb, an abort or a device location. Neither do the
// packages of this module it links (carlink and the marker): it never links internal/summon's engine or the helper.
// Fix round 1 (I2) widens the scan from the five names to their prefixes: no "cmd_", "device_" or "autopark:info"
// anywhere. That catches a name concatenated from pieces that keep its prefix whole ("autopark:cmd_" + "abort"), but
// not one split inside the prefix ("autopark:c" + "md_forward"): a text scan cannot rule out every spelling. What
// makes a verb unreachable whatever its spelling is the structure TestProbeSendsOnlyThroughPhone pins: phone data
// reaches the Link only through phone.post, which builds the keepalive or a heartbeat itself, and nothing in the
// package calls a Bus's Emit. The one allowed literal is "autopark:cmd_result", a msg_type the car sends, which the
// report reserves a row for (m5); nothing sends it.
func TestProbeHasNoVerbCode(t *testing.T) {
	forbidden := []string{"cmd_", "device_", "autopark:info"}
	allowed := []string{"autopark:cmd_result"}
	root, module := moduleRoot(t)
	const self = "airgapp/gateway/internal/summon/probe"
	seen := map[string]bool{}
	var walk func(path string)
	walk = func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, module)))
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			n++
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			scan := src
			for _, a := range allowed {
				scan = bytes.ReplaceAll(scan, []byte(a), nil)
			}
			for _, s := range forbidden {
				if bytes.Contains(scan, []byte(s)) {
					t.Errorf("%s contains %q", f, s)
				}
			}
			ast, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range ast.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(p, module+"/") {
					walk(p)
				}
			}
		}
		if n == 0 {
			t.Fatalf("no non-test .go file in %s", dir)
		}
	}
	walk(self)
	for _, p := range []string{module + "/internal/summon", module + "/internal/summon/helper"} {
		if seen[p] {
			t.Errorf("the probe links %s", p)
		}
	}
	if !seen[module+"/internal/carlink"] || !seen[module+"/internal/summon/marker"] {
		t.Fatalf("the walk saw %v: want carlink and the marker", seen)
	}
}

// moduleRoot is the directory holding go.mod, and the module's path.
func moduleRoot(t *testing.T) (string, string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		f, err := os.Open(filepath.Join(dir, "go.mod"))
		if err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if m, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
					return dir, strings.TrimSpace(m)
				}
			}
			t.Fatal("go.mod names no module")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}

func TestProbeRealBus(t *testing.T) {
	b := bustest.Start(t)
	bus, err := carlink.DialBus(b.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	carConn := b.Conn(t)
	car := mockcar.New(mockcar.Config{Port: mockcar.DBusPort(carConn)})
	setPose(car)
	auth := b.Conn(t)
	if r, err := auth.RequestName(carlink.AuthName, dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("taking %s: %v, %v", carlink.AuthName, r, err)
	}
	other := b.Conn(t) // a second caller, after the car
	mk := filepath.Join(t.TempDir(), "session")
	var out syncBuf
	var logs []string
	var logMu sync.Mutex
	done := start(context.Background(), probe.Config{
		Bus: bus, Duration: testDuration, HBEvery: testHB, KeepaliveEvery: 15 * time.Second,
		CallerInfo: bus.CallerInfo, KeysUpdated: bus.SubscribeKeysUpdated,
		MarkerPath: mk, Out: &out,
		Logf: func(f string, a ...any) { logMu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); logMu.Unlock() },
		Exit: func(code int) { t.Errorf("an emit overran its deadline: Exit(%d)", code) },
	})
	if !car.WaitFor(func(rs []mockcar.Received) bool { return countType(rs, tHB) >= 2 }, 5*time.Second) {
		t.Fatal("the car received no heartbeat")
	}
	// authd's key set changes once during the session, and a second connection calls once.
	if err := auth.Emit(dbus.ObjectPath(carlink.AuthPath), carlink.AuthIface+"."+carlink.SignalKeysUpdated); err != nil {
		t.Fatal(err)
	}
	if err := other.Object(carlink.WebrtcName, carlink.WebrtcPath).Call(carlink.WebrtcIface+"."+
		carlink.MethodCarserverToPeerCommsData, 0, map[string]dbus.Variant{"uid": dbus.MakeVariant(carlink.PhoneUID),
		"channel": dbus.MakeVariant(carlink.ChannelTCP), "data": dbus.MakeVariant(`{"msg_type":"autopark:status"}`)}).Err; err != nil {
		t.Fatal(err)
	}
	res := waitResult(t, done, 10*time.Second)
	if res.err != nil {
		t.Fatal(res.err)
	}
	rep := res.rep
	if rep.HeartbeatsSent < 10 || rep.Echoes < rep.HeartbeatsSent/2 || rep.ByType[tEcho].Count != rep.Echoes {
		t.Fatalf("%d heartbeats, %d echoes (ByType %d): want the echoes counted", rep.HeartbeatsSent, rep.Echoes,
			rep.ByType[tEcho].Count)
	}
	if len(rep.Callers) != 2 {
		t.Fatalf("Callers %q, want the car's connection, then the second one", rep.Callers)
	}
	for i, conn := range []*dbus.Conn{carConn, other} {
		var uid, pid int
		var sender string
		if n, err := fmt.Sscanf(rep.Callers[i], "uid=%d pid=%d sender=%s", &uid, &pid, &sender); n != 3 || err != nil {
			t.Fatalf("caller %q does not parse: %v", rep.Callers[i], err)
		}
		if uid != os.Getuid() || sender != conn.Names()[0] {
			t.Fatalf("caller %d is %q, want uid=%d and sender=%s", i+1, rep.Callers[i], os.Getuid(), conn.Names()[0])
		}
	}
	if rep.KeysUpdated != 1 {
		t.Fatalf("KeysUpdated %d, want the one authd sent", rep.KeysUpdated)
	}
	if v := car.Violations(); len(v) != 0 {
		t.Fatalf("the car saw violations: %q", v)
	}
	if marker.Present(mk) {
		t.Fatal("the session marker survived a confirmed disconnect")
	}
	if !car.WaitFor(func([]mockcar.Received) bool { return !car.Connected() }, 2*time.Second) {
		t.Fatal("the car still sees the session as connected")
	}
	allowedReceived(t, car.Received())
	logMu.Lock()
	defer logMu.Unlock()
	noValues(t, out.String(), strings.Join(logs, "\n"))
}

// ---- Beyond the brief, Task 14 ----

// KeysUpdated and Roles reach the report, and so does a watch or a Roles that failed. A nil CallerInfo still lists
// the caller.
func TestProbeReportsKeysAndRoles(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		r := newRig(t)
		var keys atomic.Pointer[func()]
		cfg := r.config()
		cfg.Duration = 500 * time.Millisecond
		cfg.CallerInfo = nil
		cfg.KeysUpdated = func(f func()) error { keys.Store(&f); return nil }
		cfg.Roles = func(context.Context) ([]string, error) { return []string{"entry 1: role 2", "entry 2: role 3"}, nil }
		done := start(context.Background(), cfg)
		r.heartbeats(1)
		f := keys.Load()
		if f == nil {
			t.Fatal("the probe did not subscribe to KeysUpdated before H1")
		}
		(*f)()
		(*f)()
		res := waitResult(t, done, 5*time.Second)
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.rep.KeysUpdated != 2 || !slices.Equal(res.rep.Roles, []string{"entry 1: role 2", "entry 2: role 3"}) {
			t.Fatalf("KeysUpdated %d, Roles %q; want 2 and the two entries", res.rep.KeysUpdated, res.rep.Roles)
		}
		if want := []string{"uid=? pid=? sender=:fake.car"}; !slices.Equal(res.rep.Callers, want) {
			t.Fatalf("Callers %q without CallerInfo, want %q", res.rep.Callers, want)
		}
		text := r.out.String()
		for _, s := range []string{"keys updated: 2", "entry 1: role 2", "entry 2: role 3", "uid=? pid=? sender=:fake.car"} {
			if !strings.Contains(text, s) {
				t.Fatalf("the report lacks %q:\n%s", s, text)
			}
		}
		r.clean()
	})
	t.Run("failed", func(t *testing.T) {
		r := newRig(t)
		cfg := r.config()
		cfg.Duration = 300 * time.Millisecond
		cfg.KeysUpdated = func(func()) error { return errors.New("AddMatch: access denied") }
		cfg.Roles = func(context.Context) ([]string, error) { return nil, errors.New("asking authd: denied") }
		rep, err := probe.Run(context.Background(), cfg)
		if err != nil {
			t.Fatalf("a failed watch or Roles ended the run: %v", err)
		}
		if rep.KeysUpdated != 0 || rep.Roles != nil {
			t.Fatalf("KeysUpdated %d, Roles %q after failures", rep.KeysUpdated, rep.Roles)
		}
		text := r.out.String()
		for _, s := range []string{"keys updated: not watched: AddMatch: access denied",
			"roles: failed: asking authd: denied"} {
			if !strings.Contains(text, s) {
				t.Fatalf("the report lacks %q:\n%s", s, text)
			}
		}
		r.clean()
	})
}

// car_timestamp is the last echo's, re-encoded from an integer above 0 and at most 2^53 (m4); it is omitted while
// that is 0, absent, or anything else: a fraction, an exponent, a sign, a string, a longer number, a huge literal.
func TestProbeCarTimestampZeroOmitted(t *testing.T) {
	r := newRig(t)
	r.car.FreezeAfter(3) // no echoes from the car: the test sends them
	cfg := r.config()
	cfg.Duration = 60 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := start(ctx, cfg)
	r.heartbeats(1)
	// next sends an echo with the given car_timestamp JSON (none when ""), then returns the car_timestamp a heartbeat
	// built after it carries, and that heartbeat's size.
	next := func(ts string) (string, bool, int) {
		t.Helper()
		field := ""
		if ts != "" {
			field = `"car_timestamp":` + ts + `,`
		}
		r.bus.CallCarserverToPeer(map[string]any{"uid": carlink.PhoneUID, "channel": carlink.ChannelUDP,
			"data": `{` + field + `"msg_type":"autopark:heartbeat_car_webrtc","timestamp":1}`})
		r.heartbeats(countType(r.car.Received(), tHB) + 2) // the second was built after the echo
		data := phoneData(t, r.bus.Emissions())
		var last string
		for _, d := range data {
			if strings.Contains(d, tHB) {
				last = d
			}
		}
		v, ok := decode(t, last)["car_timestamp"]
		if !ok {
			return "", false, len(last)
		}
		return v.(json.Number).String(), true, len(last)
	}
	for _, c := range []struct {
		ts, want string
	}{
		{"1234", "1234"},
		{"0", ""},
		{"5678", "5678"},
		{`"99"`, ""},
		{"5679", "5679"},
		{"", ""},
		{"1.5", ""},
		{"-7", ""},
		{"-0", ""},
		{"0.0", ""},
		{"1E+2", ""},
		{"1e308", ""},
		{"9223372036854775808", ""},
		{"1" + strings.Repeat("0", 300), ""},
		{"1." + strings.Repeat("0", 60000) + "1", ""},
		{`{"a":1}`, ""},
		{"[1]", ""},
		{"true", ""},
		{"null", ""},
		{strconv.FormatInt(1<<53+1, 10), ""},
		{strconv.FormatInt(1<<53, 10), strconv.FormatInt(1<<53, 10)},
		{" 4321 ", "4321"},
	} {
		got, ok, size := next(c.ts)
		if got != c.want || ok != (c.want != "") {
			t.Fatalf("after an echo with car_timestamp %.40q the heartbeat carries %q (present %v), want %q",
				c.ts, got, ok, c.want)
		}
		if size > 200 {
			t.Fatalf("after an echo with car_timestamp %.40q a heartbeat is %d bytes", c.ts, size)
		}
	}
	cancel()
	if res := waitResult(t, done, 2*time.Second); res.err != nil {
		t.Fatal(res.err)
	}
	r.clean()
}

// A failed send ends the run as an error: the disconnect is still attempted, but it cannot be confirmed, so the
// marker stays for `summonhelper finish`. Only allowed messages went out on this path too.
func TestProbeSendFailureKeepsMarker(t *testing.T) {
	for _, c := range []struct {
		name  string
		after int // heartbeats the car receives before the emits fail; -1: they fail from the start
		ended string
	}{{"H1", -1, "ended: H1 failed"}, {"a heartbeat", 2, "ended: a send failed"}} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			cfg := r.config()
			cfg.Duration = 30 * time.Second
			if c.after < 0 {
				r.bus.SetEmitErr(errors.New("write: broken pipe"))
			}
			done := start(context.Background(), cfg)
			if c.after >= 0 {
				r.heartbeats(c.after)
				r.bus.SetEmitErr(errors.New("write: broken pipe"))
			}
			res := waitResult(t, done, 2*time.Second)
			if res.err == nil || !strings.Contains(res.err.Error(), "summonhelper finish") {
				t.Fatalf("Run after a failed send: %v; want an error naming `summonhelper finish`", res.err)
			}
			if !marker.Present(r.marker) {
				t.Fatal("the session marker was cleared without a confirmed disconnect")
			}
			text := r.out.String()
			if !strings.Contains(text, c.ended) || !strings.Contains(text, "not confirmed") {
				t.Fatalf("the report does not say %q and that the marker stays:\n%s", c.ended, text)
			}
			allowedSends(t, r.bus.Emissions())
			noValues(t, text, r.logText())
		})
	}
}

// ---- Fix round 1 ----

// I1: a car that sends nothing after H1, no style either, gets no heartbeat and no keepalive: H1, then at once the
// disconnect. The run returns ErrNoStyle and says so in fixed words.
func TestProbeNoCarFrames(t *testing.T) {
	r := newRig(t)
	r.car.FreezeAfter(0)
	cfg := r.config()
	cfg.Duration = 30 * time.Second
	cfg.StyleWait = 300 * time.Millisecond
	t0 := time.Now()
	rep, err := probe.Run(context.Background(), cfg)
	if !errors.Is(err, probe.ErrNoStyle) {
		t.Fatalf("a silent car: %v, want ErrNoStyle", err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("Run took %v, want the 300 ms gate, not the 30 s duration", d)
	}
	if data := phoneData(t, r.bus.Emissions()); len(data) != 0 || rep.HeartbeatsSent != 0 {
		t.Fatalf("a silent car got %d phone messages (%d heartbeats): want none", len(data), rep.HeartbeatsSent)
	}
	if len(rep.ByType) != 0 || rep.Echoes != 0 || len(rep.Callers) != 0 {
		t.Fatalf("a silent car's report: %+v", rep)
	}
	text := r.out.String()
	for _, s := range []string{"no autopark:style within 300ms of H1: the car made no fresh session (a session may " +
		"have been open, or the car did not accept the helper); only H1 and the disconnect were sent",
		"car frames after H1: none\n", "heartbeats: 0 sent, 0 echoed", "callers: none",
		"disconnect: confirmed; the session marker is cleared"} {
		if !strings.Contains(text, s) {
			t.Fatalf("the report lacks %q:\n%s", s, text)
		}
	}
	r.clean()
}

// I1's regression test (the reviewer's TestReviewStaleSessionStillHeartbeats): the car still holds a session another
// client left open, H1 and an accepted cmd_forward, with no disconnect. The probe's H1 then makes no connect edge, so
// the car pushes no style: the probe sends that session no heartbeat and no keepalive, and its disconnect ends it.
// The stale session keeps sending what it samples (the car moves as the probe's H1 goes out), so car frames do arrive
// after H1: only a style opens the gate, not any frame.
func TestProbeStaleSessionGetsNoHeartbeat(t *testing.T) {
	r := newRig(t, func(c *mockcar.Config) { c.SampleEvery = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.car.Run(ctx)
	r.car.Signal(carlink.SignalLocalToRemoteSignaling, map[string]any{"data_channels_ready": true})
	r.car.Signal(carlink.SignalPeerCommsToCarserverData, map[string]any{"uid": carlink.PhoneUID,
		"data": `{"client_version":2,"latitude":1,"longitude":2,"msg_type":"autopark:cmd_forward"}`})
	before := countType(r.car.Received(), tHB)
	cfg := r.config()
	cfg.StyleWait = 300 * time.Millisecond
	cfg.Bus = &handlerBus{FakeBus: r.bus, onH1: func(carlink.Handlers) {
		r.car.SetPose(52.6, 13.5, 90, 0.3, "off", "D") // the next sample goes to the probe, after H1
	}}
	rep, err := probe.Run(context.Background(), cfg)
	if rep.ByType["vehicle_data:location"].Count == 0 {
		t.Fatalf("no location frame came after H1 (ByType %v): the test does not show that frames do not open "+
			"the gate", rep.ByType)
	}
	if !errors.Is(err, probe.ErrNoStyle) {
		t.Fatalf("a stale session: %v, want ErrNoStyle", err)
	}
	if n := countType(r.car.Received(), tHB) - before; n != 0 || rep.HeartbeatsSent != 0 {
		t.Fatalf("%d heartbeats reached the stale session (%d sent), want none", n, rep.HeartbeatsSent)
	}
	em := r.bus.Emissions()
	if data := phoneData(t, em); len(data) != 0 {
		t.Fatalf("the probe sent the stale session %q, want nothing", data)
	}
	if sig := signaling(em); len(sig) != 2 || !reflect.DeepEqual(sig[0], h1) || !reflect.DeepEqual(sig[1], h4) {
		t.Fatalf("the signaling bodies are %v, want H1, then the disconnect", sig)
	}
	if !r.car.WaitFor(func([]mockcar.Received) bool { return !r.car.Connected() }, 2*time.Second) {
		t.Fatal("the stale session is still connected after the probe's disconnect")
	}
	if text := r.out.String(); !strings.Contains(text, "no autopark:style within 300ms of H1") {
		t.Fatalf("the report does not say why:\n%s", text)
	}
	if marker.Present(r.marker) {
		t.Fatal("the marker survived a confirmed disconnect")
	}
}

// I1: car frames that come before H1 (a session the car still holds sends them to the name's new owner) are counted
// and reported, never listed.
func TestProbeCountsFramesBeforeH1(t *testing.T) {
	r := newRig(t)
	hb := &handlerBus{FakeBus: r.bus, onServe: func(h carlink.Handlers) {
		for range 3 {
			h.CarFrame(frame(":fake.car", `{"msg_type":"vehicle_data:location","latitude":`+poseLat+`}`))
		}
	}}
	cfg := r.config()
	cfg.Bus = hb
	rep, err := probe.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st := rep.ByType["vehicle_data:location"]; st.Count != 1 {
		t.Fatalf("the location row counts %d frames, want only the one after H1", st.Count)
	}
	if text := r.out.String(); !strings.Contains(text, "3 car frame(s) before H1: a session may have been open") {
		t.Fatalf("the report does not count the frames before H1:\n%s", text)
	}
	r.clean()
}

// ---- Fix round 2: the gate's own paths ----

// onlyH1AndDisconnect fails t unless the probe emitted H1, then the disconnect, and no phone data at all: nothing
// before the car's style.
func onlyH1AndDisconnect(t *testing.T, em []carlink.Emission) {
	t.Helper()
	allowedSends(t, em)
	if d := phoneData(t, em); len(d) != 0 {
		t.Fatalf("phone data with no style after H1: %q", d)
	}
	if sig := signaling(em); len(sig) != 2 || !reflect.DeepEqual(sig[0], h1) || !reflect.DeepEqual(sig[1], h4) {
		t.Fatalf("the signaling bodies are %v, want H1, then the disconnect", sig)
	}
}

// h1 waits until the car has received H1.
func (r *rig) h1() {
	r.t.Helper()
	if !r.car.WaitFor(func(rs []mockcar.Received) bool { return len(rs) > 0 }, 5*time.Second) {
		r.t.Fatal("the car did not receive H1")
	}
}

// A cancel during the wait for the car's style ends the run at once, with only H1 and the disconnect sent, a nil
// error (the owner's cancel), and the marker cleared after the confirmed disconnect.
func TestProbeCancelDuringStyleWait(t *testing.T) {
	r := newRig(t)
	r.car.FreezeAfter(0)
	cfg := r.config()
	cfg.Duration = 30 * time.Second
	cfg.StyleWait = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t0 := time.Now()
	done := start(ctx, cfg)
	r.h1()
	cancel()
	res := waitResult(t, done, 3*time.Second)
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("Run took %v: the cancel did not end the style wait", d)
	}
	if res.err != nil {
		t.Fatalf("a cancel during the style wait: %v", res.err)
	}
	onlyH1AndDisconnect(t, r.bus.Emissions())
	if text := r.out.String(); !strings.Contains(text, "ended: cancelled before the car's autopark:style") {
		t.Fatalf("the report does not say the run was cancelled before the style:\n%s", text)
	}
	r.clean()
}

// The name lost during the wait for the car's style ends the run at once as an error, not ErrNoStyle, with only H1
// and the disconnect sent; the disconnect cannot be confirmed, so the marker stays.
func TestProbeLostNameDuringStyleWait(t *testing.T) {
	r := newRig(t)
	r.car.FreezeAfter(0)
	cfg := r.config()
	cfg.Duration = 30 * time.Second
	cfg.StyleWait = 5 * time.Second
	done := start(context.Background(), cfg)
	r.h1()
	r.bus.LoseName(carlink.WebrtcName)
	res := waitResult(t, done, 3*time.Second)
	if res.err == nil || errors.Is(res.err, probe.ErrNoStyle) {
		t.Fatalf("a lost name during the style wait: %v; want the lost name's error, at once", res.err)
	}
	onlyH1AndDisconnect(t, r.bus.Emissions())
	if !marker.Present(r.marker) {
		t.Fatal("the session marker was cleared without a confirmed disconnect")
	}
	noValues(t, r.out.String(), r.logText())
}

// A style that came before H1 (from a session the car still holds) cannot open the gate: only a style after the
// probe's own H1 shows a fresh session.
func TestProbeStyleBeforeH1KeepsGateShut(t *testing.T) {
	r := newRig(t)
	r.car.FreezeAfter(0)
	cfg := r.config()
	cfg.Bus = &handlerBus{FakeBus: r.bus, onServe: func(h carlink.Handlers) {
		h.CarFrame(frame(":fake.car", `{"autopark_style":"dead_man","msg_type":"autopark:style"}`))
	}}
	cfg.StyleWait = 300 * time.Millisecond
	rep, err := probe.Run(context.Background(), cfg)
	if !errors.Is(err, probe.ErrNoStyle) {
		t.Fatalf("a style before H1 only: %v, want ErrNoStyle", err)
	}
	if rep.HeartbeatsSent != 0 {
		t.Fatalf("%d heartbeats after a style that came before H1", rep.HeartbeatsSent)
	}
	onlyH1AndDisconnect(t, r.bus.Emissions())
	if text := r.out.String(); !strings.Contains(text, "1 car frame(s) before H1: a session may have been open") {
		t.Fatalf("the report does not count the style before H1:\n%s", text)
	}
	r.clean()
}

// I2: phone data reaches the Link at exactly one place. In the probe's non-test code, function bodies and
// package-level declarations alike: the Link's Send appears once, as the method value acquire hands to phone; the
// Link is made once, by acquire's NewLink, so there is no second Link to send through; nothing calls a Bus's Emit, the
// way around the Link (fix round 2); phone's field send is called only in post; phone has no other method; post's one
// parameter is a phoneMsg; and a phoneMsg holds no bytes or text, so no caller can pass a payload.
func TestProbeSendsOnlyThroughPhone(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var sendAt, sendFieldAt, emitAt, newLinkAt, phoneMethods []string
	var phoneMsg *ast.StructType
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			where := "(package level)"
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					if ts, ok := s.(*ast.TypeSpec); ok && ts.Name.Name == "phoneMsg" {
						phoneMsg, _ = ts.Type.(*ast.StructType)
					}
				}
			case *ast.FuncDecl:
				where = d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					if id, ok := d.Recv.List[0].Type.(*ast.Ident); ok && id.Name == "phone" {
						params := d.Type.Params.List
						if len(params) != 1 || len(params[0].Names) != 1 || types(params[0].Type) != "phoneMsg" {
							t.Errorf("phone.%s's parameters are not one phoneMsg", d.Name.Name)
						}
						phoneMethods = append(phoneMethods, d.Name.Name)
					}
				}
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "Send":
						sendAt = append(sendAt, where)
					case "send":
						sendFieldAt = append(sendFieldAt, where)
					case "Emit":
						emitAt = append(emitAt, where)
					case "NewLink":
						newLinkAt = append(newLinkAt, where)
					}
				}
				return true
			})
		}
	}
	if !slices.Equal(sendAt, []string{"acquire"}) {
		t.Errorf("Send appears in %q, want only in acquire", sendAt)
	}
	if !slices.Equal(newLinkAt, []string{"acquire"}) {
		t.Errorf("NewLink appears in %q, want once, in acquire", newLinkAt)
	}
	if len(emitAt) != 0 {
		t.Errorf("Emit appears in %q: nothing may emit around the Link", emitAt)
	}
	if !slices.Equal(sendFieldAt, []string{"post"}) {
		t.Errorf("phone's send is used in %q, want only in post", sendFieldAt)
	}
	if !slices.Equal(phoneMethods, []string{"post"}) {
		t.Errorf("phone's methods are %q, want post alone", phoneMethods)
	}
	if phoneMsg == nil {
		t.Fatal("no phoneMsg struct")
	}
	for _, fld := range phoneMsg.Fields.List {
		if typ := types(fld.Type); typ != "bool" && typ != "int64" {
			t.Errorf("phoneMsg has a field of type %s: only bool and int64 carry no payload", typ)
		}
	}
}

// types is a type expression's source form, enough for idents, selectors, slices and pointers.
func types(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return types(x.X) + "." + x.Sel.Name
	case *ast.ArrayType:
		return "[]" + types(x.Elt)
	case *ast.StarExpr:
		return "*" + types(x.X)
	}
	return fmt.Sprintf("%T", e)
}

// I2: the no-motion and no-values checks on the paths the owner takes: a long run that a cancel ends (MX1, MX3, MX4),
// and a long run that a lost name ends (MX2). The reviewer's ready test (zz_skeptic_test.go), folded in.
func TestProbeLongRunPaths(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		r := newRig(t)
		cfg := r.config()
		cfg.Duration = 30 * time.Second
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := start(ctx, cfg)
		r.heartbeats(5)
		cancel()
		if res := waitResult(t, done, 3*time.Second); res.err != nil {
			t.Fatal(res.err)
		}
		r.clean()
	})
	t.Run("lost name", func(t *testing.T) {
		r := newRig(t)
		cfg := r.config()
		cfg.Duration = 30 * time.Second
		done := start(context.Background(), cfg)
		r.heartbeats(5)
		r.bus.LoseName(carlink.WebrtcName)
		if res := waitResult(t, done, 3*time.Second); res.err == nil {
			t.Fatal("a lost name returned nil")
		}
		allowedSends(t, r.bus.Emissions())
		noValues(t, r.out.String(), r.logText())
	})
}

// m1, rule 8 (R15): MaxGap is the largest gap between two frames of a type, not only the tail. A type that stalls for
// 600 ms mid-run, then resumes until the end, shows a gap of about 600 ms.
func TestProbeMidRunStallShows(t *testing.T) {
	r := newRig(t)
	r.car.FreezeAfter(3) // style, status, location only: the test sends the frames of its own type
	done := start(context.Background(), r.config())
	r.heartbeats(1)
	call := func() {
		r.bus.CallCarserverToPeer(map[string]any{"uid": carlink.PhoneUID, "channel": carlink.ChannelTCP,
			"data": `{"msg_type":"review:tick"}`})
	}
	t0 := time.Now()
	call()
	time.Sleep(600 * time.Millisecond)
	for time.Since(t0) < 1100*time.Millisecond { // then every 50 ms until shortly before the 1.2 s end
		call()
		time.Sleep(50 * time.Millisecond)
	}
	res := waitResult(t, done, 5*time.Second)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if st := res.rep.ByType["review:tick"]; st.MaxGap < 550*time.Millisecond {
		t.Fatalf("review:tick stats %+v: the 600 ms mid-run stall does not show", st)
	}
	r.clean()
}

// m1, rule 6: a keepalive at the start, and every KeepaliveEvery.
func TestProbePeriodicKeepalive(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.KeepaliveEvery = 250 * time.Millisecond
	if _, err := probe.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range phoneData(t, r.bus.Emissions()) {
		if d == tKeepalive {
			n++
		}
	}
	// 1.2 s: one at the start, then one at 250, 500, 750 and 1000 ms.
	if n < 4 || n > 6 {
		t.Fatalf("%d keepalives in %v at a 250 ms period, want about 5", n, testDuration)
	}
	r.clean()
}

// m1, rule 3: the car's CarserverToPeerCommsData is answered with 1 during the run.
func TestProbeReplyIsOne(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.Duration = 400 * time.Millisecond
	done := start(context.Background(), cfg)
	r.heartbeats(1)
	got := r.bus.CallCarserverToPeer(map[string]any{"uid": carlink.PhoneUID, "channel": carlink.ChannelTCP,
		"data": `{"msg_type":"autopark:status"}`})
	if res := waitResult(t, done, 5*time.Second); res.err != nil {
		t.Fatal(res.err)
	}
	if got[carlink.ReplyKey] != int32(1) {
		t.Fatalf("CarserverToPeerCommsData replied %v, want 1", got)
	}
	r.clean()
}

// m1, rule 7: the name is released once the run is over.
func TestProbeReleasesName(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.Duration = 300 * time.Millisecond
	if _, err := probe.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if owner, err := r.bus.NameOwner(context.Background(), carlink.WebrtcName); err == nil {
		t.Fatalf("%s is still owned by %s after the run", carlink.WebrtcName, owner)
	}
	r.clean()
}

// m1: the defaults, as constants and as what a zero Config gets: 120 s, 100 ms, 15 s and a 2 s style gate. A 1.2 s
// run with every period zero sends about 12 heartbeats and exactly one keepalive.
func TestProbeDefaults(t *testing.T) {
	if probe.DefaultDuration != 120*time.Second || probe.DefaultHBEvery != 100*time.Millisecond ||
		probe.DefaultKeepaliveEvery != 15*time.Second || probe.DefaultStyleWait != 2*time.Second {
		t.Fatalf("defaults %v, %v, %v, %v; want 120s, 100ms, 15s, 2s", probe.DefaultDuration, probe.DefaultHBEvery,
			probe.DefaultKeepaliveEvery, probe.DefaultStyleWait)
	}
	r := newRig(t)
	cfg := r.config()
	cfg.HBEvery, cfg.KeepaliveEvery, cfg.StyleWait = 0, 0, 0
	rep, err := probe.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	keepalives := 0
	for _, d := range phoneData(t, r.bus.Emissions()) {
		if d == tKeepalive {
			keepalives++
		}
	}
	if rep.HeartbeatsSent < 10 || rep.HeartbeatsSent > 14 || keepalives != 1 {
		t.Fatalf("a zero Config sent %d heartbeats and %d keepalives in %v, want about 12 and 1",
			rep.HeartbeatsSent, keepalives, testDuration)
	}
	r.clean()
}

// m5: only a msg_type in Tesla's form, ^[a-z_]+:[a-z_]+$ in at most 64 bytes, is listed. Any other frame is counted
// as one with an unlisted msg_type, never quoted: not JSON, no msg_type, one that carries a VIN, coordinates or
// control characters, or one too long. Past 64 listed types, others are counted the same way.
func TestProbeOddFrames(t *testing.T) {
	r := newRig(t)
	r.car.FreezeAfter(3) // style, status and location only
	cfg := r.config()
	cfg.Duration = 600 * time.Millisecond
	done := start(context.Background(), cfg)
	r.heartbeats(1)
	call := func(data string) {
		r.bus.CallCarserverToPeer(map[string]any{"uid": carlink.PhoneUID, "channel": carlink.ChannelTCP, "data": data})
	}
	odd := []string{`not json`, `[1,2]`, `{"type":"x"}`, `{"msg_type":7}`, `{"msg_type":""}`, `null`}
	long := "a:" + strings.Repeat("b", 63) // 65 bytes
	for _, mt := range []string{
		"5YJ3E1EA7KF000000", "vehicle_data:location " + poseLat + "," + poseLon, "x\n\x1b[2J\x1b[31mFAKE\r\tz",
		"Autopark:style", "a:b1", "a:b:c", ":x", "x:", "‮evil:x", long,
		"disconnect: confirmed; the session marker is cleared",
	} {
		b, _ := json.Marshal(map[string]any{"msg_type": mt})
		odd = append(odd, string(b))
	}
	for _, d := range odd {
		call(d)
	}
	// 70 well-formed types: with the 3 reserved rows already there, 59 others fit, and 11 do not.
	name := func(i int) string { return "x:t_" + string(rune('a'+i/26)) + string(rune('a'+i%26)) }
	for i := range 70 {
		call(`{"msg_type":"` + name(i) + `"}`)
	}
	res := waitResult(t, done, 5*time.Second)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if n := len(res.rep.ByType); n != 62 {
		t.Fatalf("ByType has %d msg_types, want the 3 reserved rows and 59 others", n)
	}
	for i := range 70 {
		if _, ok := res.rep.ByType[name(i)]; ok != (i < 59) {
			t.Fatalf("%s listed %v, want %v", name(i), ok, i < 59)
		}
	}
	for mt := range res.rep.ByType {
		if !regexpForm(mt) {
			t.Fatalf("ByType lists %q, which is not in Tesla's form", mt)
		}
	}
	text := r.out.String()
	if want := fmt.Sprintf("and %d frame(s) with an unlisted msg_type", len(odd)+11); !strings.Contains(text, want) {
		t.Fatalf("the report lacks %q:\n%s", want, text)
	}
	for _, v := range []string{"5YJ3E1EA7KF000000", long, "\x1b", "\r", "‮", "FAKE", "disconnect: confirmed; the session marker is cleared\"",
		"Autopark"} {
		if strings.Contains(text, v) {
			t.Fatalf("the report carries %q:\n%s", v, text)
		}
	}
	if strings.Count(text, "disconnect: confirmed") != 1 {
		t.Fatalf("a msg_type passes for a report line:\n%s", text)
	}
	r.clean()
}

// regexpForm is ^[a-z_]+:[a-z_]+$ written out, for the test's own check.
func regexpForm(s string) bool {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	for j, c := range s {
		if j != i && (c < 'a' || c > 'z') && c != '_' {
			return false
		}
	}
	return true
}

// m5, and m1's caller cap: frames that come first from 20 other senders, with 70 other msg_types, cannot crowd out
// the car. The reserved rows (the echoes, style, status, location) stay; 16 callers are kept, and the car, which
// sends the echoes, keeps its line on top of them.
func TestProbeCapsCrowdOut(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.CallerInfo = nil
	cfg.Bus = &handlerBus{FakeBus: r.bus, onH1: func(h carlink.Handlers) {
		for i := range 70 {
			sender := fmt.Sprintf(":hostile.%02d", i%20)
			h.CarFrame(frame(sender, `{"msg_type":"x:t_`+string(rune('a'+i/26))+string(rune('a'+i%26))+`"}`))
		}
	}}
	rep, err := probe.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, mt := range []string{tEcho, "autopark:style", "autopark:status", "vehicle_data:location"} {
		if rep.ByType[mt].Count == 0 {
			t.Fatalf("ByType lost the car's %s row to the crowd: %v", mt, rep.ByType)
		}
	}
	if rep.ByType[tEcho].Count != rep.Echoes || rep.Echoes < 5 {
		t.Fatalf("the echo row counts %d, Echoes %d", rep.ByType[tEcho].Count, rep.Echoes)
	}
	if len(rep.Callers) != 17 {
		t.Fatalf("Callers %q: want 16 of the 20 others, then the car", rep.Callers)
	}
	for i, c := range rep.Callers[:16] {
		if want := fmt.Sprintf("uid=? pid=? sender=:hostile.%02d", i); c != want {
			t.Fatalf("caller %d is %q, want %q", i+1, c, want)
		}
	}
	if rep.Callers[16] != "uid=? pid=? sender=:fake.car" {
		t.Fatalf("the car's line is %q, want it kept past the cap", rep.Callers[16])
	}
	r.clean()
}

// markerBus makes the marker appear between the probe's first marker check and its Set: the moment the name is
// granted.
type markerBus struct {
	*carlink.FakeBus
	path string
}

func (b *markerBus) RequestName(name string, queue bool) (bool, error) {
	if err := os.WriteFile(b.path, nil, 0o600); err != nil {
		return false, err
	}
	return b.FakeBus.RequestName(name, queue)
}

// m7 (the findings' m6): a marker that appears after the first check (a second probe, or a hand) is not this probe's.
// It checks again once it holds the name, then refuses, gives the name back, and leaves the marker.
func TestProbeRefusesMarkerAfterAcquire(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.Bus = &markerBus{FakeBus: r.bus, path: r.marker}
	_, err := probe.Run(context.Background(), cfg)
	if !errors.Is(err, probe.ErrMarkerPresent) {
		t.Fatalf("a marker that appeared with the name: %v, want ErrMarkerPresent", err)
	}
	if em := r.bus.Emissions(); len(em) != 0 {
		t.Fatalf("emitted %v over a marker that is not the probe's", em)
	}
	if _, err := r.bus.NameOwner(context.Background(), carlink.WebrtcName); err == nil {
		t.Fatal("the name is still held after the refusal")
	}
	if !marker.Present(r.marker) {
		t.Fatal("the probe removed a marker it did not set")
	}
	if out := r.out.String(); out != "" {
		t.Fatalf("a report after a refusal: %q", out)
	}
}

// m9: a cancel during the first keepalive's send: that send completes, but no heartbeat starts after the cancel.
func TestProbeCancelDuringFirstKeepalive(t *testing.T) {
	r := newRig(t)
	sending := make(chan struct{})
	cfg := r.config()
	cfg.Bus = &slowBus{FakeBus: r.bus, send: 60 * time.Millisecond, sending: sending}
	cfg.Duration = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := start(ctx, cfg)
	select {
	case <-sending:
	case <-time.After(5 * time.Second):
		t.Fatal("no keepalive was sent")
	}
	cancel()
	res := waitResult(t, done, 2*time.Second)
	if res.err != nil {
		t.Fatal(res.err)
	}
	data := phoneData(t, r.bus.Emissions())
	if !slices.Equal(data, []string{tKeepalive}) || res.rep.HeartbeatsSent != 0 {
		t.Fatalf("after a cancel during the first keepalive the probe sent %q (%d heartbeats), want the keepalive "+
			"alone", data, res.rep.HeartbeatsSent)
	}
	r.clean()
}

// m2: a missing run directory refuses before H1, in fixed words that name the directory and how to make it.
func TestProbeMissingRunDir(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(t.TempDir(), "airgapp-summon")
	cfg := r.config()
	cfg.MarkerPath = filepath.Join(dir, "session")
	_, err := probe.Run(context.Background(), cfg)
	want := dir + " is missing; the helper's run script creates it, or create it as root with `install -d -o 1337 " +
		"-g 4950 -m 2750 " + dir + "`"
	if !errors.Is(err, probe.ErrNoRunDir) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Run without the run directory: %v; want ErrNoRunDir and %q", err, want)
	}
	if em := r.bus.Emissions(); len(em) != 0 {
		t.Fatalf("emitted %v without a marker", em)
	}
	if _, err := r.bus.NameOwner(context.Background(), carlink.WebrtcName); err == nil {
		t.Fatal("the name is still held after the refusal")
	}
}
