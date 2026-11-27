package camhelper

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/annexb"
)

func TestHoldRefcount(t *testing.T) {
	r := newRig(t)
	a, b := r.dial(), r.dial()
	if _, err := a.Hold(ctx()); err != nil {
		t.Fatal(err)
	}
	r.waitOps(sa1)
	st, err := b.Hold(ctx())
	if err != nil || !st.Held || st.Gear != "P" || st.Sentry != "Armed" {
		t.Fatalf("hold: %+v %v", st, err)
	}
	r.waitOps() // the second holder writes nothing
	if err := a.Release(ctx()); err != nil {
		t.Fatal(err)
	}
	r.waitOps() // b still holds: no falling edge
	b.Close()
	r.waitOps(sa0)
	// The op is recorded at the write; the marker goes after the readback.
	waitFor(t, "markers cleared", func() bool { return len(r.markers()) == 0 })
}

// The full §4.9 release on disconnect: requestAudio 0 -> sessionActive 0 ->
// selectable 0 -> gain -200 (marked) -> children -> temp files.
func TestDisconnectReleaseOrder(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	if _, err := c.Hold(ctx()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TalkArm(ctx()); err != nil {
		t.Fatal(err)
	}
	if err := c.Gain(ctx(), true); err != nil {
		t.Fatal(err)
	}
	if err := selc(c, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.CaptureStart(ctx(), SelectablePort); err != nil {
		t.Fatal(err)
	}
	if err := c.CaptureStart(ctx(), 9901); err != nil {
		t.Fatal(err)
	}
	r.waitOps(sa1, ra1, sa0, sa1, "gain=0", sel1)
	if m := strings.Join(r.markers(), ","); m != "ra,sa,sel,gain" {
		t.Fatalf("markers %s", m)
	}
	writer := r.sp.find(t, "pw-cat")
	capSel := r.sp.find(t, "port=9895")
	capFront := r.sp.find(t, "port=9901")

	c.Close()
	r.waitOps(ra0, sa0, sel0, "gain=-200")
	waitFor(t, "children stopped", func() bool {
		return writer.stopped.Load() && capSel.stopped.Load() && capFront.stopped.Load()
	})
	waitFor(t, "markers cleared", func() bool { return len(r.markers()) == 0 })
}

func TestTalkEndsWithEdgeWhenOthersHold(t *testing.T) {
	r := newRig(t)
	a, b := r.dial(), r.dial()
	a.Hold(ctx())
	b.Hold(ctx())
	if _, err := b.TalkArm(ctx()); err != nil {
		t.Fatal(err)
	}
	r.waitOps(sa1, ra1, sa0, sa1)
	b.Close()
	// requestAudio 0, then an edge (a still holds), and no gain write: we
	// never changed the gain.
	r.waitOps(ra0, sa0, sa1)
	a.Close()
	r.waitOps(sa0)
}

func TestGainMarkerGated(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	r.waitOps(sa1, ra1, sa0, sa1)
	if err := c.Gain(ctx(), false); err != nil { // no boost yet: nothing to restore
		t.Fatal(err)
	}
	if err := c.TalkDisarm(ctx(), false); err != nil {
		t.Fatal(err)
	}
	r.waitOps(ra0, sa0, sa1) // no -200: a Boombox is never muted
	c.TalkArm(ctx())
	c.Gain(ctx(), true)
	c.Gain(ctx(), false)
	r.waitOps(ra1, sa0, sa1, "gain=0", "gain=-20")
	c.TalkDisarm(ctx(), false)
	r.waitOps(ra0, sa0, sa1, "gain=-200")
	if (markers{r.state}).has(markGain) {
		t.Fatal("gain marker left")
	}
}

func TestGainNeedsTalk(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	expectCode(t, c.Gain(ctx(), true), proto.ErrNotArmed)
	_, err := c.TalkArm(ctx())
	expectCode(t, err, proto.ErrNotHeld)
	r.waitOps()
}

func TestGates(t *testing.T) {
	cases := []struct {
		name       string
		set        map[DVRead]string
		hold, talk string
	}{
		{"parked", nil, "", ""},
		{"drive", map[DVRead]string{ReadGear: "(true, 'D')"}, proto.ErrGateGear, ""},
		{"camera access off", map[DVRead]string{ReadCamAccess: "(true, 'false')"}, proto.ErrGateCamera, ""},
		{"sentry off", map[DVRead]string{ReadSentry: "(true, 'Off')"}, proto.ErrGateSentry, ""},
		{"sentry off, dog mode", map[DVRead]string{ReadSentry: "(true, 'Off')", ReadClimate: "(true, 'Dog')"}, "", ""},
		{"gear invalid", map[DVRead]string{ReadGear: "(false, '')"}, "", proto.ErrGateGear},
		{"sentry unknown", map[DVRead]string{ReadSentry: "garbage"}, "", proto.ErrGateSentry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			for k, v := range tc.set {
				r.car.set(k, v)
			}
			c := r.dial()
			_, err := c.Hold(ctx())
			if tc.hold != "" {
				expectCode(t, err, tc.hold)
				r.waitOps()
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.TalkArm(ctx())
			if tc.talk != "" {
				expectCode(t, err, tc.talk)
				r.waitOps(sa1)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSelectableParkedOnlyAndSingleOwner(t *testing.T) {
	r := newRig(t)
	a, b := r.dial(), r.dial()
	if err := selc(a, 3); err != nil {
		t.Fatal(err)
	}
	expectCode(t, selc(b, 1), proto.ErrBusy)
	expectCode(t, selc(b, 7), proto.ErrBadRequest)
	expectCode(t, selc(b, 12), proto.ErrBadRequest) // fascia is the direct port 9901, never the selectable
	if err := selc(a, 1); err != nil {
		t.Fatal(err)
	}
	if err := selc(a, 0); err != nil {
		t.Fatal(err)
	}
	r.waitOps(sel3, sel1, sel0)
	r.car.set(ReadGear, "(true, 'R')")
	expectCode(t, selc(b, 1), proto.ErrSelRefused)
	r.waitOps()
}

func TestReassertEveryTick(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	c.Gain(ctx(), true)
	selc(c, 3)
	r.car.setLinks(feedersOK)
	r.car.take()
	r.srv.tick()
	r.waitOps(ra1, sa1, sel3, "gain=0")
	st := nextEvent(t, c, proto.EvStatus).Status
	if !st.Held || !st.Talk || st.Route != "ok" || !st.Boost || st.Sel != 3 || st.GainDB == nil || *st.GainDB != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestGearTwiceEndsHoldsAndTalk(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	c.Gain(ctx(), true)
	selc(c, 1)
	c.CaptureStart(ctx(), SelectablePort)
	c.CaptureStart(ctx(), 9897)
	r.car.take()
	capSel := r.sp.find(t, "port=9895")

	r.car.set(ReadGear, "(true, 'D')")
	r.srv.tick()
	// First non-P reading: talk ends (requestAudio 0 + edge, the hold stays),
	// the selectable is released, the hold is still re-asserted.
	r.waitOps(ra0, sa0, sa1, "gain=-200", sel0) // no sessionActive re-assert on a non-P reading
	if e := nextEvent(t, c, proto.EvTalkEnd); e.Reason != proto.ErrGateGear {
		t.Fatalf("talk_end %+v", e)
	}
	nextEvent(t, c, proto.EvSelEnd)
	waitFor(t, "9895 capture stopped", capSel.stopped.Load)

	r.srv.tick()
	r.waitOps(sa0) // second: the hold ends and is never re-asserted
	if e := nextEvent(t, c, proto.EvHoldEnd); e.Reason != proto.ErrGateGear {
		t.Fatalf("hold_end %+v", e)
	}
	r.srv.tick()
	r.waitOps()
	st, _ := c.Status(ctx())
	if st.Held || st.Talk || st.Sel != 0 || !st.Driving {
		t.Fatalf("status after gate %+v", st)
	}
	_, err := c.Hold(ctx())
	expectCode(t, err, proto.ErrGateGear)
	if r.sp.find(t, "port=9897").stopped.Load() {
		t.Fatal("a direct-port capture must survive the gear gate (driving streams flow)")
	}
	if len(r.markers()) != 0 {
		t.Fatalf("markers %v", r.markers())
	}
}

func TestSentryOffTwiceEndsHolds(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	r.car.take()
	r.car.set(ReadSentry, "(true, 'Off')")
	r.srv.tick()
	r.waitOps(sa1)
	r.srv.tick()
	r.waitOps(sa0)
	if e := nextEvent(t, c, proto.EvHoldEnd); e.Reason != proto.ErrGateSentry {
		t.Fatalf("hold_end %+v", e)
	}
}

func TestUnknownGearThriceEndsTalkOnly(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	r.car.setLinks(feedersOK)
	r.car.take()
	r.car.set(ReadGear, "(false, '')")
	r.srv.tick()
	r.srv.tick()
	r.waitOps(ra1, sa1, ra1, sa1)
	r.srv.tick()
	r.waitOps(ra0, sa0, sa1, sa1)
	nextEvent(t, c, proto.EvTalkEnd)
}

func TestRouteWatchdogReEdges(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	r.car.take()
	r.srv.tick() // route missing once
	r.waitOps(ra1, sa1)
	r.srv.tick() // twice: re-fire the edge
	r.waitOps(ra1, sa1, ra1, sa0, sa1)
	r.srv.tick() // backoff: no new edge yet
	r.srv.tick()
	r.waitOps(ra1, sa1, ra1, sa1)
	r.car.setLinks(feedersOK)
	r.srv.tick()
	r.waitOps(ra1, sa1)
	if e := nextEvent(t, c, proto.EvRoute); e.Reason != "ok" {
		t.Fatalf("route %+v", e)
	}
	r.car.setLinks("")
	r.srv.tick()
	if e := nextEvent(t, c, proto.EvRoute); e.Reason != "down" {
		t.Fatalf("route %+v", e)
	}
}

func TestExteriorBusyRefusesTalk(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	r.car.setLinks("TDM-playback:playback_9\n  |<- megaphone:monitor_1\n")
	_, err := c.TalkArm(ctx())
	expectCode(t, err, proto.ErrExterior)
	r.waitOps(sa1)
}

func TestTalkSingleOwnerAndWriterFailure(t *testing.T) {
	r := newRig(t)
	a, b := r.dial(), r.dial()
	a.Hold(ctx())
	b.Hold(ctx())
	if _, err := a.TalkArm(ctx()); err != nil {
		t.Fatal(err)
	}
	_, err := b.TalkArm(ctx())
	expectCode(t, err, proto.ErrBusy)
	a.TalkDisarm(ctx(), false)
	r.car.take()
	r.sp.mu.Lock()
	r.sp.fail = true
	r.sp.mu.Unlock()
	_, err = b.TalkArm(ctx())
	expectCode(t, err, proto.ErrSpawn)
	r.waitOps(ra1, sa0, sa1, ra0, sa0, sa1) // armed, then undone
}

func TestWriterRestartedWhenItDies(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	w := r.sp.find(t, "pw-cat")
	w.exit(nil)
	r.srv.mu.Lock()
	r.srv.talk.restartAt = time.Time{}
	r.srv.mu.Unlock()
	r.srv.tick()
	waitFor(t, "new writer", func() bool {
		n := 0
		for _, ch := range r.sp.all() {
			if ch.is("pw-cat") {
				n++
			}
		}
		return n == 2
	})
}

func TestPCMReachesOnlyTheOwnersWriter(t *testing.T) {
	r := newRig(t)
	a, b := r.dial(), r.dial()
	a.Hold(ctx())
	a.TalkArm(ctx())
	w := r.sp.find(t, "pw-cat")
	if w.spec.Argv[0] != "/usr/bin/pw-cat" || !w.spec.Stdin {
		t.Fatalf("writer spec %+v", w.spec)
	}
	for i := 0; i < 10; i++ {
		if err := a.WritePCM(make([]byte, 1920)); err != nil {
			t.Fatal(err)
		}
		b.WritePCM(make([]byte, 1920)) // not the owner: dropped
	}
	waitFor(t, "pcm at the writer", func() bool { return w.stdinBytes.Load() == 19200 })
	time.Sleep(20 * time.Millisecond)
	if n := w.stdinBytes.Load(); n != 19200 {
		t.Fatalf("writer got %d bytes", n)
	}
}

func TestAplayWriter(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Writer = WriterAplay })
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	if w := r.sp.find(t, "aplay"); w.spec.Argv[0] != "/usr/bin/aplay" {
		t.Fatal(w.spec.Argv)
	}
}

func TestOddPCMEndsSession(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	r.waitOps(sa1)
	c.WritePCM([]byte{1, 2, 3})
	r.waitOps(sa0)
	<-c.Done()
}

func TestUnknownRequestFieldEndsSession(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	r.waitOps(sa1)
	nc, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	proto.WriteFrame(nc, proto.TReq, []byte(`{"id":1,"op":"status","dv":"CAPI_remoteAutopark"}`))
	nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, _ := nc.Read(make([]byte, 16)); n != 0 {
		t.Fatal("a request with an unknown field was answered")
	}
	proto.WriteFrame(nc, proto.TReq, []byte(`{"id":2,"op":"set_dv"}`))
	r.waitOps() // the other session is untouched
}

func TestUnknownOpRefused(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	_, err := c.Call(ctx(), proto.Request{Op: "set_dv"})
	expectCode(t, err, proto.ErrBadRequest)
}

func TestPeerUIDRefused(t *testing.T) {
	r := newRig(t, func(c *Config) { c.AllowUID = os.Getuid() + 1 })
	c := r.dial()
	_, err := c.Status(ctx())
	if err == nil {
		t.Fatal("a session from a foreign uid was served")
	}
}

func TestMaxSessions(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxSessions = 1 })
	a := r.dial()
	if _, err := a.Status(ctx()); err != nil {
		t.Fatal(err)
	}
	b := r.dial()
	if _, err := b.Status(ctx()); err == nil {
		t.Fatal("second session accepted")
	}
}

func TestCaptureAllowlist(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	for _, p := range []int{0, 22, 5600, 9894, 9899, 8901, SelectablePort} {
		expectCode(t, c.CaptureStart(ctx(), p), proto.ErrPort)
	}
	for p := range capturePorts {
		if err := c.CaptureStart(ctx(), p); err != nil {
			t.Fatalf("port %d: %v", p, err)
		}
	}
	if err := c.CaptureStart(ctx(), 9901); err != nil { // idempotent
		t.Fatal(err)
	}
	selc(c, 1)
	if err := c.CaptureStart(ctx(), SelectablePort); err != nil {
		t.Fatal(err)
	}
	expectCode(t, c.CaptureStart(ctx(), 9897+100), proto.ErrPort)
	if n := len(r.sp.all()); n != 8 {
		t.Fatalf("%d children", n)
	}
	for _, ch := range r.sp.all() {
		if ch.spec.Stdin || !ch.spec.Stdout || ch.spec.Argv[0] != "/usr/bin/gst-launch-1.0" {
			t.Fatalf("capture spec %+v", ch.spec)
		}
	}
}

func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(append(b, 0, 0, 0, 1), n...)
	}
	return b
}

func TestCaptureStreamsAccessUnits(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	if err := c.CaptureStart(ctx(), 9900); err != nil {
		t.Fatal(err)
	}
	ch := r.sp.find(t, "port=9900")
	sps, pps := []byte{0x67, 0x64, 0x00, 0x28}, []byte{0x68, 0xEE, 0x3C, 0xB0}
	idr, p := []byte{0x65, 0x88, 0x84, 0x21}, []byte{0x41, 0x9A, 0x02, 0x04}
	go func() {
		ch.outW.Write(annexB(sps, pps, idr, p)[:7]) // split mid-NAL
		ch.outW.Write(annexB(sps, pps, idr, p)[7:])
		ch.outW.Write(annexB(p))
		ch.exit(nil)
	}()
	var got []au
	for len(got) < 3 {
		select {
		case a := <-c.aus:
			got = append(got, a)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d AUs", len(got))
		}
	}
	if got[0].port != 9900 || got[0].flags != proto.FlagIDR|proto.FlagParamSets ||
		!bytes.Equal(got[0].body[proto.AUHeader:], annexB(sps, pps, idr)) {
		t.Fatalf("AU0 %+v", got[0])
	}
	if got[1].flags != 0 || !bytes.Equal(got[1].body[proto.AUHeader:], annexB(p)) {
		t.Fatalf("AU1 %+v", got[1])
	}
	if got[0].body[3] != 0 || binary.BigEndian.Uint64(got[1].body[4:12]) < binary.BigEndian.Uint64(got[0].body[4:12]) {
		t.Fatal("AU header reserved byte or time")
	}
	if e := nextEvent(t, c, proto.EvCapEnd); e.Port != 9900 || e.Reason != "exit" {
		t.Fatalf("cap_end %+v", e)
	}
}

func TestCaptureStopSendsNoEnd(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.CaptureStart(ctx(), 9896)
	ch := r.sp.find(t, "port=9896")
	if err := c.CaptureStop(ctx(), 9896); err != nil {
		t.Fatal(err)
	}
	if !ch.stopped.Load() {
		t.Fatal("not stopped")
	}
	c.Status(ctx())
	select {
	case e := <-c.Events():
		if e.Ev == proto.EvCapEnd {
			t.Fatal("cap_end after cap_stop")
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMediaQueueDropsToNextIDR(t *testing.T) {
	q := newOutQueue()
	push := func(isIDR bool, n int) {
		hdr := []byte{0x41, 0x9A}
		if isIDR {
			hdr = []byte{0x65, 0x88}
		}
		data := append(annexB(hdr), make([]byte, n)...)
		q.pushAU(9901, isIDR, false, encodeAU(9901, annexAU(data, isIDR), time.Now()))
	}
	push(true, 3<<20)
	push(false, 2<<20) // over the cap: dropped, needs an IDR
	push(false, 10)    // dropped: waiting for an IDR
	push(true, 10)     // resumes
	push(false, 10)
	var kinds []byte
	for len(q.media) > 0 {
		f, _ := q.next()
		kinds = append(kinds, f[5+2])
	}
	if string(kinds) != "\x03\x03\x00" { // IDR+SPS/PPS, IDR+SPS/PPS, P
		t.Fatalf("flags %v", kinds)
	}
}

func TestShutdownReleasesEverything(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	c.Gain(ctx(), true)
	selc(c, 3)
	c.CaptureStart(ctx(), 9901)
	r.car.take()
	r.srv.Shutdown()
	r.waitOps(ra0, sa0, sel0, "gain=-200")
	for _, ch := range r.sp.all() {
		if !ch.stopped.Load() {
			t.Fatalf("child %v not stopped", ch.spec.Argv[0])
		}
	}
	if len(r.markers()) != 0 {
		t.Fatalf("markers %v", r.markers())
	}
	<-c.Done()
}

func TestSweep(t *testing.T) {
	dir := t.TempDir()
	proc := filepath.Join(dir, "proc")
	m := markers{dir}
	for _, n := range []string{markRequestAudio, markSessionAct, markSelectable, markGain} {
		m.set(n)
	}
	os.MkdirAll(filepath.Join(dir, cutSubdir), 0o700)
	os.WriteFile(filepath.Join(dir, cutSubdir, "x.mp4"), []byte("x"), 0o600)
	ours := captureArgv(9901)
	m.saveChildren([]childRec{{Pid: 4242, Argv: ours}, {Pid: 4343, Argv: ours}, {Pid: 4444, Argv: ours}})
	// 4242: still ours. 4343: recycled by the car's own dashcam gst-launch. 4444: gone.
	os.MkdirAll(filepath.Join(proc, "4242"), 0o755)
	os.WriteFile(filepath.Join(proc, "4242", "cmdline"), []byte(strings.Join(ours, "\x00")+"\x00"), 0o644)
	os.MkdirAll(filepath.Join(proc, "4343"), 0o755)
	os.WriteFile(filepath.Join(proc, "4343", "cmdline"), []byte("/usr/bin/dashcam-gst-launch\x00-e\x00udpsrc\x00"), 0o644)
	var killed []int
	car := newFakeCar()
	Sweep(SweepConfig{Car: car, StateDir: dir, ProcDir: proc, Settle: time.Millisecond, Log: log.New(io.Discard, "", 0), Kill: func(pid int, sig syscall.Signal) error {
		killed = append(killed, pid)
		return nil
	}})
	if got := strings.Join(car.take(), ","); got != strings.Join([]string{ra0, sa0, sel0, "gain=-200"}, ",") {
		t.Fatalf("sweep ops %s", got)
	}
	if len(killed) != 1 || killed[0] != -4242 {
		t.Fatalf("killed %v", killed)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, cutSubdir)); len(ents) != 0 {
		t.Fatal("cut files left")
	}
	// Nothing marked: nothing touched.
	Sweep(SweepConfig{Car: car, StateDir: dir, ProcDir: proc, Settle: time.Millisecond, Log: log.New(io.Discard, "", 0), Kill: func(int, syscall.Signal) error { t.Fatal("kill"); return nil }})
	if ops := car.take(); len(ops) != 0 {
		t.Fatalf("unmarked sweep wrote %v", ops)
	}
}

func TestChildrenRecordedForSweep(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.CaptureStart(ctx(), 9893)
	ch := r.sp.find(t, "port=9893")
	recs := (markers{r.state}).loadChildren()
	if len(recs) != 1 || recs[0].Pid != ch.pid || strings.Join(recs[0].Argv, " ") != strings.Join(captureArgv(9893), " ") {
		t.Fatalf("records %+v", recs)
	}
	c.CaptureStop(ctx(), 9893)
	waitFor(t, "record removed", func() bool { return len((markers{r.state}).loadChildren()) == 0 })
}

func TestParse(t *testing.T) {
	for raw, want := range map[string]string{
		"(true, 'P')\n": "P", "(true, 'D')": "D", "(false, 'P')": "", "(true, 'X')": "", "": "", "P": "",
	} {
		if g := gearOf(raw); g != want {
			t.Errorf("gearOf(%q) = %q", raw, g)
		}
	}
	if v, ok := parseDV("(true, 'Armed')"); !ok || v != "Armed" {
		t.Fatal(v)
	}
	if v, ok := parseDV("(true, 12)"); !ok || v != "12" {
		t.Fatal(v)
	}
	ok, foreign := routeState(feedersOK)
	if !ok || foreign {
		t.Fatal("feeders ok")
	}
	ok, foreign = routeState("TDM-playback:playback_9\n  |<- webcam:monitor_1\nTDM-playback:playback_10\n  |<- megaphone:monitor_2\n")
	if ok || !foreign {
		t.Fatal("foreign feeder")
	}
	if ok, _ := routeState("TDM-playback:playback_9\n  |<- webcam:monitor_1\n"); ok {
		t.Fatal("one channel is not a route")
	}
}

func annexAU(data []byte, idr bool) annexb.AU { return annexb.AU{Data: data, IDR: idr, ParamSets: idr} }

func selc(c *conn, v int) error { _, err := c.Sel(ctx(), v); return err }

// talk_disarm with no_edge (the SUMMON path): requestAudio 0 and the gain,
// but no sessionActive 0 -> 1 edge while the hold stays; others' holds intact.
func TestTalkDisarmNoEdge(t *testing.T) {
	r := newRig(t)
	a, b := r.dial(), r.dial()
	a.Hold(ctx())
	b.Hold(ctx())
	b.TalkArm(ctx())
	b.Gain(ctx(), true)
	r.waitOps(sa1, ra1, sa0, sa1, "gain=0")
	if err := b.TalkDisarm(ctx(), true); err != nil {
		t.Fatal(err)
	}
	r.waitOps(ra0, "gain=-200")
	b.Close()
	a.Close()
	r.waitOps(sa0)
}
