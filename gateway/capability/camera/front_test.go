package camera

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
)

// Front is the windshield ultrawide: 9895 with selectable 3, held by default
// while parked and held, whatever view is shown (verified on the car
// 2026-11-20: 9901 is the bumper fisheye, 9895 with selectable 0 is dark).

func (r *rig) lastStreamsRaw() string {
	r.t.Helper()
	all := r.w.Sent(TStreams)
	if len(all) == 0 {
		r.t.Fatal("no STREAMS")
	}
	return string(all[len(all)-1])
}

func TestDefaultSelectableFrontWhileParked(t *testing.T) {
	r, sw := openSeq(t, `{"view":"rear"}`) // 1 h tick: only the settle timer can start 9895
	warm(t, r)
	f := r.helper()
	if calls := strings.Join(f.callList(), ","); !strings.HasPrefix(calls, "status,hold,sel 3,") {
		t.Fatalf("calls %s", calls)
	}
	if d := f.callAt("cap_start 9895", 1).Sub(f.callAt("sel 3", 1)); d < 30*time.Millisecond {
		t.Fatalf("9895 captured %v after the selectable write (settle 30 ms)", d)
	}
	if s := r.lastStreams(); len(s.Streams) != 1 || s.Streams[0].View != "rear" || s.Streams[0].Dewarp {
		t.Fatalf("rear %+v", s)
	}
	// Back to front: answered at once from the warm 9895 GOP, no car write.
	f.au(9895, true, true, "fI")
	f.au(9895, false, false, "fP")
	got := selectAndCollect(t, r, sw, `{"view":"front"}`, 2)
	if s := fmtVF(got); s != fmt.Sprintf("0:fI:%d,0:fP:0", fIDR|fCatch) {
		t.Fatalf("front catch-up %s", s)
	}
	if s := r.lastStreams(); len(s.Streams) != 1 || s.Streams[0].View != "front" || !s.Streams[0].Dewarp {
		t.Fatalf("front %+v", s)
	}
	if f.count("sel 3") != 1 || f.has("sel 0") || f.count("cap_start 9895") != 1 {
		t.Fatalf("selectable churn: %v", f.callList())
	}
	if len(r.w.Events()) != 0 {
		t.Fatalf("hold logged while still held: %q", r.w.Events())
	}
}

// interior -> rear -> front: the selectable goes 1 -> 0 -> 3 with the 9895
// capture stopped first; nothing of the interior camera (cached, in flight or
// still sent by the APE within the settle) reaches the front stream, and the
// capture restarts at the end of the settle without waiting for a tick.
func TestInteriorToRearToFrontSettlesWithoutStaleFrames(t *testing.T) {
	const settle = 150 * time.Millisecond
	r, sw := openSeq(t, `{"view":"interior"}`, func(r *rig) {
		r.tweak = func(c *Config) { c.Settle = settle }
	})
	f := r.helper()
	r.waitCall("cap_start 9895")
	if d := f.callAt("cap_start 9895", 1).Sub(f.callAt("sel 1", 1)); d < settle {
		t.Fatalf("interior captured %v after its write", d)
	}
	f.au(9895, true, true, "intI")
	f.au(9895, false, false, "intP")
	if got := payloads(r.videos()); got != "0:intI,0:intP" {
		t.Fatalf("interior video %s", got)
	}

	n := sw.len()
	r.h.Frame(TSelect, []byte(`{"view":"rear"}`))
	r.waitCall("sel 3")
	if calls := strings.Join(f.callList(), ","); !strings.Contains(calls, "cap_stop 9895,sel 0,sel 3") {
		t.Fatalf("release-then-write order: %s", calls)
	}
	// The interior camera is still in flight (and the APE keeps sending it
	// for up to ~1 s): never cached, never counted as video.
	f.au(9895, true, true, "staleI")
	f.au(9895, false, false, "staleP")
	r.h.vid.mu.Lock()
	cached, seen := r.h.vid.gops[9895] != nil, !r.h.vid.last[9895].IsZero()
	r.h.vid.mu.Unlock()
	if cached || seen {
		t.Fatalf("stale 9895 AUs kept: cached=%v seen=%v", cached, seen)
	}

	// Front inside the settle: STREAMS at once, video only from the new capture.
	r.h.Frame(TSelect, []byte(`{"view":"front"}`))
	waitFor(t, "front STREAMS", func() bool { return r.lastStreams().View == "front" })
	if s := r.lastStreams(); len(s.Streams) != 1 || !s.Streams[0].Dewarp {
		t.Fatalf("front %+v", s)
	}
	f.au(9895, true, true, "staleI2")
	if f.count("cap_start 9895") != 1 {
		t.Fatal("9895 restarted inside the settle")
	}
	waitFor(t, "9895 restarted", func() bool { return f.count("cap_start 9895") == 2 })
	if d := f.callAt("cap_start 9895", 2).Sub(f.callAt("sel 3", 1)); d < settle || d > settle+time.Second {
		t.Fatalf("9895 restarted %v after the write (settle %v, 1 h tick)", d, settle)
	}
	f.au(9895, false, false, "newP0") // the new capture joins mid-GOP: waits for its IDR
	f.au(9895, true, true, "newI")
	f.au(9895, false, false, "newP")
	var out []string
	for _, s := range sw.since(n) {
		if s.t == TVideo {
			out = append(out, fmt.Sprintf("%d:%s:%d", s.body[1], s.body[12:], s.body[2]))
		}
	}
	if got := strings.Join(out, ","); got != fmt.Sprintf("0:newI:%d,0:newP:0", fIDR) {
		t.Fatalf("video after the switch %s", got)
	}
	ev := r.w.Events()
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "interior hold start=") {
		t.Fatalf("session-log events %q", ev)
	}
}

func TestGridFrontTileOn9895(t *testing.T) {
	r, sw := openSeq(t, `{"view":"rear"}`)
	warm(t, r)
	f := r.helper()
	f.au(9895, true, true, "fI")
	f.au(9895, false, false, "fP")
	f.au(9901, true, true, "bumper") // fascia: never a grid tile
	got := selectAndCollect(t, r, sw, `{"view":"grid","focus":"front"}`, 2)
	time.Sleep(10 * time.Millisecond)
	if s := fmtVF(got); s != fmt.Sprintf("0:fI:%d,0:fP:0", fIDR|fCatch) || len(r.videos()) != 2 {
		t.Fatalf("front tile %s", s)
	}
	s := r.lastStreams()
	if len(s.Streams) != 4 || s.Focus != "front" { // no pillars
		t.Fatalf("grid %+v", s)
	}
	for i, st := range s.Streams {
		if st.Index != i || st.View != gridViews[i] || st.Dewarp != (i == 0) || st.Full != (i == 0) {
			t.Fatalf("tile %d %+v", i, st)
		}
	}
	if f.count("sel 3") != 1 || f.has("sel 0") {
		t.Fatalf("selectable churn: %v", f.callList())
	}
}

func TestFasciaOn9901AndOffWhileDrivingOrSummon(t *testing.T) {
	r, host := openHost(t, `{"view":"fascia"}`)
	f := r.helper()
	r.waitCall("cap_start 9901")
	if s := r.lastStreams(); len(s.Streams) != 1 || s.Streams[0].View != "fascia" || s.Streams[0].Dewarp {
		t.Fatalf("fascia %+v", s)
	}
	f.au(9895, true, true, "front") // the default hold's camera: not fascia
	f.au(9901, true, true, "bI")
	if got := payloads(r.videos()); got != "0:bI" {
		t.Fatalf("video %s", got)
	}
	r.waitStatus("fascia ok", func(s statusJSON) bool { return s.StreamsOK && s.Reason == "" })

	// SUMMON: 9901 needs sessionActive, which SUMMON drops: no stream.
	host.SetSummonOpen(true)
	waitFor(t, "no fascia stream", func() bool { return len(r.lastStreams().Streams) == 0 })
	r.waitStatus("summon", func(s statusJSON) bool { return s.Reason == "summon" })
	waitFor(t, "nothing captured", func() bool { return len(startedPorts(f)) == 0 })
	host.SetSummonOpen(false)
	waitFor(t, "fascia again", func() bool { return len(r.lastStreams().Streams) == 1 })

	// Driving: no stream either, reported like a refused selectable view.
	f.events <- proto.Event{Ev: proto.EvStatus, Status: &proto.Status{Gear: "D", Driving: true, Held: true}}
	waitFor(t, "no fascia stream while driving", func() bool { return len(r.lastStreams().Streams) == 0 })
	r.waitStatus("select_refused", func(s statusJSON) bool { return s.Reason == "select_refused" && s.Driving })
	if f.has("sel 12") {
		t.Fatal("fascia used the selectable")
	}
}

// SUMMON replaces front's default selectable 3 with the summon camera (Grid
// in P): front itself moves to 9898. When SUMMON closes, 3 and front on 9895
// come back.
func TestSummonReplacesTheDefaultSelectable(t *testing.T) {
	r, host := openHost(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	n := len(f.callList())
	host.SetSummonOpen(true)
	r.waitCall("sel 11")
	if got := strings.Join(f.callsAfter(n), ","); !strings.HasPrefix(got, "summon true,cap_stop 9895,sel 0,sel 11") {
		t.Fatalf("summon order: %s", got)
	}
	r.waitCall("cap_start 9898")
	waitFor(t, "front on 9898", func() bool {
		s := r.lastStreams()
		return len(s.Streams) == 1 && !s.Streams[0].Dewarp && portSet(startedPorts(f)) == "9898"
	})
	if ev := r.w.Events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "front hold start=") {
		t.Fatalf("front hold not logged: %q", ev)
	}
	time.Sleep(100 * time.Millisecond)
	if f.count("sel 3") != 1 || f.count("sel 11") != 1 {
		t.Fatalf("selectable changed again while SUMMON is open: %v", f.callList())
	}
	host.SetSummonOpen(false)
	waitFor(t, "front hold again", func() bool { return f.count("sel 3") == 2 && f.count("cap_start 9895") == 2 })
	waitFor(t, "front on 9895 again", func() bool { s := r.lastStreams(); return len(s.Streams) == 1 && s.Streams[0].Dewarp })
	if ev := r.w.Events(); len(ev) != 2 || !strings.HasPrefix(ev[1], "grid hold start=") {
		t.Fatalf("grid hold not logged: %q", ev)
	}
}

func TestDewarpOnlyOnTheWindshieldUltrawide(t *testing.T) {
	r := open(t, `{"view":"front"}`)
	warm(t, r)
	if raw := r.lastStreamsRaw(); !strings.Contains(raw, `"dewarp":true`) {
		t.Fatalf("front parked %s", raw)
	}
	for _, v := range []string{"rear", "left_repeater", "right_repeater", "left_pillar", "right_pillar", "fascia", "interior"} {
		r.h.Frame(TSelect, []byte(`{"view":"`+v+`"}`))
		waitFor(t, v+" STREAMS", func() bool { s := r.lastStreams(); return s.View == v && len(s.Streams) == 1 })
		if raw := r.lastStreamsRaw(); strings.Contains(raw, "dewarp") {
			t.Fatalf("%s %s", v, raw)
		}
	}
	r.h.Frame(TSelect, []byte(`{"view":"grid"}`))
	waitFor(t, "grid", func() bool { s := r.lastStreams(); return s.View == "grid" && len(s.Streams) == 4 })
	if raw := r.lastStreamsRaw(); strings.Count(raw, `"dewarp":true`) != 1 {
		t.Fatalf("grid %s", raw)
	}
	// Driving: front falls back to 9898, which never gets it.
	r.h.Frame(TSelect, []byte(`{"view":"front"}`))
	r.helper().events <- proto.Event{Ev: proto.EvStatus, Status: &proto.Status{Gear: "D", Driving: true, Held: true}}
	waitFor(t, "front on 9898", func() bool { return r.helper().has("cap_start 9898") })
	waitFor(t, "front STREAMS", func() bool { s := r.lastStreams(); return s.View == "front" && len(s.Streams) == 1 })
	var s streamsJSON
	if raw := r.lastStreamsRaw(); json.Unmarshal([]byte(raw), &s) != nil || strings.Contains(raw, "dewarp") {
		t.Fatalf("front driving %s", raw)
	}
}
