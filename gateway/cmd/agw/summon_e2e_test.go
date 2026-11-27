package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/capability"
	"airgapp/gateway/capability/summonrelay"
	"airgapp/gateway/client"
	"airgapp/gateway/internal/authsrv"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/carlink/bustest"
	"airgapp/gateway/internal/carlink/mockcar"
	"airgapp/gateway/internal/dialout/dialouttest"
	"airgapp/gateway/internal/guards"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/server"
	"airgapp/gateway/internal/summon"
	"airgapp/gateway/internal/summon/helper"
	"airgapp/gateway/internal/summon/marker"
)

// The Summon bench, end to end (design §8.1): phone → Pi → agw relay → helper → mock car, all in this process. The
// phone (client) dials the fake Pi (dialouttest) through a delay proxy; agw runs serve -dial with the real SUMMON relay
// and the real sign-in; the summon helper runs helper.Serve; the mock QtCarServer (mockcar) plays the car on a FakeBus
// or on a private dbus-daemon (bustest). Nothing leaves loopback and unix sockets: no car, no Pi, no system bus.

// TestSummonHappyPath: Tesla's flow through every hop, on the in-memory bus.
func TestSummonHappyPath(t *testing.T) {
	summonHappyPath(t, startSummonRig(t, summonOpts{}))
}

// TestSummonHelloV2: the happy path with version 2 hellos, outer (agw) and inner (the helper), nothing pinned: each
// v2 challenge carries its signer's long-term key, which the phone trusts on first connect, and each must be the
// key the pins name (agw's and the helper's).
func TestSummonHelloV2(t *testing.T) {
	summonHappyPath(t, startSummonRig(t, summonOpts{helloV2: true}))
}

// TestSummonRealBus: the happy path with the helper on a private dbus-daemon (bustest, carlink.DialBus) and the car
// on mockcar.DBusPort. R10 (part): the session marker is cleared after the release and the session's end.
func TestSummonRealBus(t *testing.T) {
	summonHappyPath(t, startSummonRig(t, summonOpts{realBus: true}))
}

// summonHappyPath presses Forward from 11 m, holds with heartbeats, releases, and ends the session.
func summonHappyPath(t *testing.T, r *summonRig) {
	p := r.phone()
	if got := r.carKinds(); !slices.Equal(got, []string{smH1}) {
		t.Fatalf("at session-up the car received %q, want data_channels_ready alone", got)
	}
	if !marker.Present(r.markerPath) {
		t.Fatal("no session marker while the session is up")
	}

	verb := smCmdArrow(smForward, 11)
	p.hold(verb)
	p.waitCar(smHBCar, 5) // a steady hold: five heartbeats echoed
	p.release()
	p.waitResult(smAbort, 1, smWait) // the car's answer to the release

	// The car: data_channels_ready, the verb (canonical), the heartbeats (canonical), then the abort.
	recs := r.car.Received()
	hbs := p.hbsSent()
	want := append(append([]string{smH1, smForward}, smRepeat(smHBApp, len(hbs))...), smAbort)
	if got := smCarKinds(recs); !slices.Equal(got, want) {
		t.Fatalf("the car received %q, want %q", got, want)
	}
	if got, w := string(recs[1].Data), string(smCanonical(t, summon.KindVerb, verb)); got != w {
		t.Fatalf("the car's verb is %s, want the canonical %s", got, w)
	}
	for i, js := range hbs {
		if got, w := string(recs[2+i].Data), string(smCanonical(t, summon.KindHeartbeat, js)); got != w {
			t.Fatalf("the car's heartbeat %d is %s, want the canonical %s", i, got, w)
		}
	}
	if got := string(recs[len(recs)-1].Data); got != summon.AbortJSON {
		t.Fatalf("the car's abort is %s, want %s", got, summon.AbortJSON)
	}

	// The phone: style, status, location, the verb's cmd_result, one echo per heartbeat (dcUdp), the release's result.
	frames := p.frames()
	wantTypes := append(append([]string{smStyle, smStatus, smLocation, smCmdResult}, smRepeat(smHBCar, len(hbs))...),
		smCmdResult)
	if got := smFrameTypes(frames); !slices.Equal(got, wantTypes) {
		t.Fatalf("the phone's CAR frames are %q, want %q", got, wantTypes)
	}
	for i, f := range frames {
		wantCh := summon.ChanTCP
		if f.msgType() == smHBCar {
			wantCh = summon.ChanUDP
		}
		if f.ch != wantCh {
			t.Fatalf("CAR frame %d (%s) has channel byte %d, want %d", i, f.msgType(), f.ch, wantCh)
		}
	}
	if f := frames[3]; f.str("cmd_type") != smForward || f.obj["result"] != true {
		t.Fatalf("the verb's cmd_result is %s, want the car's success for %s", f.raw, smForward)
	}
	for i, js := range hbs {
		if got, sent := frames[4+i].num("timestamp"), smDecodeObj([]byte(js))["timestamp"]; got != fmt.Sprint(sent) {
			t.Fatalf("echo %d carries timestamp %s, want heartbeat %d's %v", i, got, i, sent)
		}
	}
	if f := frames[len(frames)-1]; f.str("cmd_type") != smAbort {
		t.Fatalf("the last CAR frame is %s, want the release's cmd_result", f.raw)
	}
	if !marker.Present(r.markerPath) {
		t.Fatal("the session marker went away at the release: only the session's end clears it")
	}

	// The session's end: H4 has no verb to abort, so the car gets the disconnect alone, and the marker is cleared.
	p.end()
	r.waitCar(smDisconnect, 1)
	if got := r.carKinds(); !slices.Equal(got, append(want, smDisconnect)) {
		t.Fatalf("after the session's end the car received %q, want %q, then the disconnect", got, want)
	}
	smEventually(t, "the session marker cleared after the session's end", func() bool {
		return !marker.Present(r.markerPath)
	})
	r.waitSessions(1)
	if v := r.car.Violations(); len(v) > 0 {
		t.Fatalf("the mock car saw rules broken: %q", v)
	}
}

// The e2e bounds derive from D2's constants: a trip reaches the car within TripAfter + 300 ms of the last LIVE that
// got through, and a late link's within Fresh + TripAfter + 250 ms of the delay starting (the newest tick the phone
// echoes goes stale after Fresh, and the trip comes TripAfter later).
const (
	smTripBound = summon.TripAfter + 300*time.Millisecond
	smLateBound = summon.Fresh + summon.TripAfter + 250*time.Millisecond
)

// TestSummonLostFramesTrip: the phone stops sending LIVE and HB while a verb is active. D2 injects exactly one abort
// within TripAfter + 300 ms of the last LIVE sent; its cmd_result is swallowed, and the session goes on.
func TestSummonLostFramesTrip(t *testing.T) {
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	p.hold(smCmdArrow(smForward, 11))
	time.Sleep(300 * time.Millisecond) // a steady hold first

	last := p.stopSending()
	abort := r.waitCarAfterVerb(smForward, smAbort, smWait)
	lat := abort.At.Sub(last)
	t.Logf("D2 trip (lost frames): the injected abort reached the car %v after the last LIVE sent", lat)
	if lat > smTripBound {
		t.Fatalf("the injected abort reached the car %v after the last LIVE sent, want within %v", lat, smTripBound)
	}

	// The car answers the abort. The helper swallows that answer, so the phone never sees it.
	const swallowed = "summon: the injected cmd_abort's cmd_result swallowed"
	smEventually(t, "the car's answer to the injected abort, at the helper or at the phone", func() bool {
		return r.helperLog.count(swallowed) > 0 || p.countResults(smAbort) > 0
	})
	time.Sleep(300 * time.Millisecond) // a forwarded result, or a second abort, would be here by now
	if f := p.results(smAbort); len(f) > 0 {
		t.Fatalf("the injected abort's cmd_result reached the phone: %s", f[0].raw)
	}
	if n := r.helperLog.count(swallowed); n != 1 {
		t.Fatalf("the helper swallowed %d results, want the injected abort's\n%s", n, r.helperLog)
	}
	if n := smCount(r.carAfterVerb(smForward), smAbort); n != 1 {
		t.Fatalf("the car received %d aborts after the verb, want exactly one", n)
	}
	r.wantOneTrip()
	p.mustBeUp()
}

// TestSummonStalledLinkTrips: the delay proxy holds every byte both ways while the phone keeps holding. An abort
// reaches the car within TripAfter + 300 ms of the last LIVE that got through, and it is D2's: one trip logged, no disconnect, the
// session still up. Then the link comes back with the finger still down: the held burst arrives, then about 300 ms of
// fresh LIVE and HB, and no heartbeat is forwarded after the abort, since D2 forwards none until the next forwarded
// verb.
func TestSummonStalledLinkTrips(t *testing.T) {
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	p.hold(smCmdArrow(smForward, 11))
	time.Sleep(300 * time.Millisecond)

	// The stall starts right after a LIVE got through. The sender pauses, and the time its latest LIVE was handed to
	// Send is the reference. The proxy reads phone bytes after that instant (that LIVE), then nothing for 20 ms, so
	// every byte the phone wrote has been forwarded. Then the stall starts, and the sender resumes into it. The pause
	// counts against the bound.
	last := p.pause()
	r.proxy.waitUpQuiet(last, 20*time.Millisecond)
	r.proxy.hold()
	p.resume()

	abort := r.waitCarAfterVerb(smForward, smAbort, smWait)
	lat := abort.At.Sub(last)
	t.Logf("D2 trip (stalled link): the injected abort reached the car %v after the last LIVE that got through", lat)
	smWantTrip(t, lat, smTripBound, "the last LIVE that got through")
	time.Sleep(700 * time.Millisecond) // the stall goes on, and the phone keeps sending into it
	r.wantOneTrip()
	if n := smCount(r.car.Received(), smDisconnect); n != 0 {
		t.Fatalf("the car received %d disconnects during the stall: the abort was not D2's", n)
	}
	p.mustBeUp()

	// The link comes back while the finger is still down: the held burst, then fresh LIVE and HB again.
	r.proxy.flush()
	p.tick(true) // a tick has reached the phone again
	time.Sleep(300 * time.Millisecond)
	if n := smCount(r.carAfter(abort), smHBApp); n != 0 {
		t.Fatalf("%d heartbeats reached the car after the injected abort", n)
	}
	if n := smCount(r.carAfterVerb(smForward), smAbort); n != 1 {
		t.Fatalf("the car received %d aborts after the verb, want exactly one", n)
	}
	p.mustBeUp()
}

// TestSummonLateLinkTrips is the case freshness exists for: the link delays only Pi→phone by 2 s, and the phone keeps
// sending LIVE and HB at their usual rate. They arrive on time, but echo old tick ids, so they are stale. Exactly one
// abort reaches the car within Fresh + TripAfter + 250 ms of the delay starting, no heartbeat is forwarded after it, and a later re-press
// is refused with action_invalid. No stale heartbeat is forwarded before the trip either: none reaches the car later
// than F after the delay started, plus 100 ms for the helper's forwarding.
func TestSummonLateLinkTrips(t *testing.T) {
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	p.hold(smCmdArrow(smForward, 11))
	time.Sleep(300 * time.Millisecond)

	livesBefore, hbsBefore := p.livesSent(), len(p.hbsSent())
	t0 := r.proxy.setDelay(0, 2*time.Second)
	abort := r.waitCarAfterVerb(smForward, smAbort, smWait)
	lat := abort.At.Sub(t0)
	t.Logf("D2 trip (late link): the injected abort reached the car %v after the delay started", lat)
	if lat > smLateBound {
		t.Fatalf("the abort reached the car %v after the delay started, want within %v", lat, smLateBound)
	}
	time.Sleep(600 * time.Millisecond) // the phone keeps sending, on time and stale
	since := time.Since(t0)
	lives, hbs := p.livesSent()-livesBefore, len(p.hbsSent())-hbsBefore
	if lives < int(since/(50*time.Millisecond))*3/4 || hbs < int(since/(100*time.Millisecond))*3/4 {
		t.Fatalf("the phone sent %d LIVEs and %d HBs in the %v since the delay started, want its usual 20 Hz and 10 Hz",
			lives, hbs, since)
	}
	if n := smCount(r.carAfter(abort), smHBApp); n != 0 {
		t.Fatalf("%d heartbeats reached the car after the abort", n)
	}
	if n := smCount(r.carAfterVerb(smForward), smAbort); n != 1 {
		t.Fatalf("the car received %d aborts after the verb, want exactly the injected one", n)
	}
	// From t0 on, the newest tick the phone holds was issued before t0, so every HB the helper gets later than t0 + F
	// is stale: before the trip as after it, none may reach the car.
	cut := t0.Add(summon.Fresh + 100*time.Millisecond)
	for _, rec := range r.car.Received() {
		if smCarKind(rec) == smHBApp && rec.At.After(cut) {
			t.Fatalf("a heartbeat reached the car %v after the delay started: stale, past F = %v",
				rec.At.Sub(t0), summon.Fresh)
		}
	}

	// A later re-press, still behind the late link: the tick it echoes is old, so it is refused.
	p.release()
	p.press(smCmdArrow(smForward, 11), false)
	p.wantRefusal(p.waitResult(smForward, 2, 5*time.Second), smForward, "action_invalid")
	if n := smCountVerbs(r.car.Received()); n != 1 {
		t.Fatalf("the car received %d verbs, want only the first", n)
	}
}

// TestSummonLateLinkJustOverFreshTrips: a Pi→phone late link of Fresh + 70 ms. Every tick the phone echoes is then at
// least that old at the helper, over F = Fresh, so the hold must trip, within Fresh + TripAfter + 250 ms of the delay
// starting as in TestSummonLateLinkTrips. That test's 2 s delay cannot tell F = Fresh from a far larger F; this one
// can.
func TestSummonLateLinkJustOverFreshTrips(t *testing.T) {
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	p.hold(smCmdArrow(smForward, 11))
	time.Sleep(300 * time.Millisecond)

	t0 := r.proxy.setDelay(0, summon.Fresh+70*time.Millisecond)
	abort := r.waitCarAfterVerb(smForward, smAbort, smWait)
	lat := abort.At.Sub(t0)
	t.Logf("D2 trip (late link just over Fresh): the injected abort reached the car %v after the delay started", lat)
	if lat > smLateBound {
		t.Fatalf("the abort reached the car %v after the delay started, want within %v", lat, smLateBound)
	}
	time.Sleep(300 * time.Millisecond) // a second trip, or a second abort, would be here by now
	r.wantOneTrip()
	if n := smCount(r.carAfterVerb(smForward), smAbort); n != 1 {
		t.Fatalf("the car received %d aborts after the verb, want exactly one", n)
	}
}

// TestSummonDelayedLinkRefusesQueuedVerb: with both directions delayed by 2 s, the phone releases and presses again.
// The re-press's verb reaches the helper stale and is refused (action_invalid), and the car never receives a second
// verb.
func TestSummonDelayedLinkRefusesQueuedVerb(t *testing.T) {
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	p.hold(smCmdArrow(smForward, 11))
	time.Sleep(300 * time.Millisecond)

	r.proxy.setDelay(2*time.Second, 2*time.Second)
	p.release()
	time.Sleep(100 * time.Millisecond)
	p.press(smCmdArrow(smForward, 11), false) // queued behind the delay, echoing the newest tick the phone has
	p.wantRefusal(p.waitResult(smForward, 2, 6*time.Second), smForward, "action_invalid")
	if n := r.helperLog.count(smForward + " (press 2) refused: action_invalid"); n != 1 {
		t.Fatalf("the helper logged %d refusals of the re-press, want 1\n%s", n, r.helperLog)
	}
	if n := smCountVerbs(r.car.Received()); n != 1 {
		t.Fatalf("the car received %d verbs, want only the first", n)
	}
	// D2 tripped meanwhile, and the release went through late, after the trip's abort.
	r.waitCar(smAbort, 2)
	if got := smCarKinds(r.carAfterVerb(smForward)); !smOnlyHBsThen(got, smAbort, smAbort) {
		t.Fatalf("after the verb the car received %q, want heartbeats, the trip's abort and the late release", got)
	}
}

// TestSummonStallThenFlushNothingResumes: the link holds every byte for 2 s while the phone keeps holding, then
// flushes. No verb or heartbeat in the burst is forwarded, and the car's last message is the injected abort. The
// burst carries a VERB as well, sent without a release before it, so "no verb" is not vacuous: a release in the burst
// is always forwarded, and would itself be the car's last message.
//
// A native phone sends a LIVE only for each tick it receives (design §5.2), and a stalled link brings none, so the
// bench phone sends no LIVE while the link holds. Its heartbeats go on (Tesla's timer), and LIVE resumes after the
// flush. The burst is then about 21 phone frames, as a real phone's would be, well inside agw's relay queue.
func TestSummonStallThenFlushNothingResumes(t *testing.T) {
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	p.hold(smCmdArrow(smForward, 11))
	time.Sleep(300 * time.Millisecond)

	p.setLive(false)
	lives, hbs := p.livesSent(), len(p.hbsSent())
	held := r.proxy.hold()
	time.Sleep(100 * time.Millisecond)
	p.verb(smCmdArrow(smForward, 11), false) // into the stall
	abort := r.waitCarAfterVerb(smForward, smAbort, smWait)
	t.Logf("D2 trip (stall): the injected abort reached the car %v after the stall started", abort.At.Sub(held))
	time.Sleep(time.Until(held.Add(2 * time.Second)))
	if n := p.livesSent() - lives; n != 0 {
		t.Fatalf("the phone sent %d LIVEs into the stall, want none", n)
	}
	t.Logf("the burst: %d HBs and the VERB, no LIVE", len(p.hbsSent())-hbs)
	r.proxy.flush()
	p.setLive(true)

	p.wantRefusal(p.waitResult(smForward, 2, smWait), smForward, "action_invalid") // the burst's verb
	// The phone still holds: its LIVE and HB are fresh again, and must resume nothing.
	time.Sleep(500 * time.Millisecond)
	if after := r.carAfter(abort); len(after) != 0 {
		t.Fatalf("after the injected abort the car received %q, want nothing", smCarKinds(after))
	}
	if n := smCountVerbs(r.car.Received()); n != 1 {
		t.Fatalf("the car received %d verbs, want only the first", n)
	}
	r.wantOneTrip()
}

// TestSummonVerbWhileActiveRefused: while Forward is active, each verb is refused with action_invalid, and the hold
// goes on.
func TestSummonVerbWhileActiveRefused(t *testing.T) {
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	p.hold(smCmdArrow(smForward, 11))
	for _, js := range []string{smCmdFindMe(11), smCmdArrow(smForward, 11), smCmdArrow(smReverse, 11)} {
		typ := smMsgTypeOf(js)
		n := p.countResults(typ) + 1
		p.verb(js, true)
		p.wantRefusal(p.waitResult(typ, n, smWait), typ, "action_invalid")
	}
	if n := smCountVerbs(r.car.Received()); n != 1 {
		t.Fatalf("the car received %d verbs, want only the first", n)
	}
	if n := smCount(r.carAfterVerb(smForward), smAbort); n != 0 {
		t.Fatalf("the car received %d aborts while the hold went on, want none", n)
	}
	p.release()
	r.waitCar(smAbort, 1)
}

// TestSummonFloor: cmd_find_me, cmd_forward and cmd_reverse from 2 m are each refused with location_params_invalid;
// from 5 m each is forwarded. Go to Target (cmd_find_me, pin_drop) is where the phone and the goal differ, and each
// must clear the floor on its own: the phone at 5 m with the goal at 2 m, and the phone at 2 m with the goal at 5 m,
// are both refused.
func TestSummonFloor(t *testing.T) {
	smWantDistance(t, 2)
	smWantDistance(t, 5)
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	for _, at := range []func(northM float64) string{
		smCmdFindMe,
		func(m float64) string { return smCmdArrow(smForward, m) },
		func(m float64) string { return smCmdArrow(smReverse, m) },
	} {
		typ := smMsgTypeOf(at(0))
		p.verb(at(2), true)
		p.wantRefusal(p.waitResult(typ, 1, smWait), typ, "location_params_invalid")
		if n := smCount(r.car.Received(), typ); n != 0 {
			t.Fatalf("%s from 2 m reached the car", typ)
		}
		p.hold(at(5)) // forwarded, and the car answered it
		if n := smCount(r.car.Received(), typ); n != 1 {
			t.Fatalf("%s from 5 m: the car received %d, want 1", typ, n)
		}
		aborts := smCount(r.car.Received(), smAbort)
		p.release()
		r.waitCar(smAbort, aborts+1)
		p.waitResult(smAbort, aborts+1, smWait) // Tesla's arrows wait for the abort's result
	}
	for _, at := range []struct{ phone, goal float64 }{{5, 2}, {2, 5}} {
		n := p.countResults(smFindMe) + 1
		p.verb(smCmdGoTo(at.phone, at.goal), true)
		p.wantRefusal(p.waitResult(smFindMe, n, smWait), smFindMe, "location_params_invalid")
	}
	if n := smCount(r.car.Received(), smFindMe); n != 1 {
		t.Fatalf("the car received %d cmd_find_me, want only the 5 m Come to Me", n)
	}
	if v := r.car.Violations(); len(v) > 0 {
		t.Fatalf("the mock car saw rules broken: %q", v)
	}
}

// TestSummonStalePosition: mockcar.FreezeAfter(0) during a hold, then a release and a re-press, which is refused with
// invalid_vehicle_location.
//
// Which of D3's staleness rules applies: the first, "no car frame of any kind for more than 1 s of hold time".
// FreezeAfter(0) stops every frame the mock car sends, the heartbeat echoes included, so no echo keeps the stream
// alive; the test checks that nothing reaches the phone after the freeze, and that the helper logs that rule's line.
// So no SetPose is needed: the car stays in P at speed 0, where neither location rule could apply anyway.
//
// The phone uses Come to Me, whose re-press does not wait for the abort's cmd_result: the frozen car never sends one.
func TestSummonStalePosition(t *testing.T) {
	const stale = "summon: D3: the car position is stale: no car frame for"

	// One hold, 1.2 s past the freeze (R16).
	t.Run("one hold", func(t *testing.T) {
		r := startSummonRig(t, summonOpts{})
		p := r.phone()
		p.hold(smCmdFindMe(11))
		p.waitCar(smHBCar, 2)

		r.car.FreezeAfter(0)
		frozen := time.Now()
		time.Sleep(100 * time.Millisecond) // what the car had queued before the freeze arrives
		settled := len(p.frames())
		time.Sleep(time.Until(frozen.Add(1200 * time.Millisecond)))
		if n := len(p.frames()); n != settled {
			t.Fatalf("%d car frames reached the phone after the freeze: the echoes still flow", n-settled)
		}
		p.release()
		p.press(smCmdFindMe(11), true)
		p.wantRefusal(p.waitResult(smFindMe, 2, smWait), smFindMe, "invalid_vehicle_location")
		if n := r.helperLog.count(stale); n != 1 {
			t.Fatalf("D3's first rule logged the staleness %d times, want 1\n%s", n, r.helperLog)
		}
		if n := smCountVerbs(r.car.Received()); n != 1 {
			t.Fatalf("the car received %d verbs, want only the first", n)
		}
	})

	// The quiet time carries across holds (plan call 2): otherwise a frozen car could be pressed again for ever in
	// holds under 1 s. The first hold runs 0.5 s past the freeze, so the re-press is forwarded; the second runs 0.7 s,
	// under 1 s on its own and over it summed with the first, so the third press is refused.
	t.Run("the quiet time carries across holds", func(t *testing.T) {
		r := startSummonRig(t, summonOpts{})
		p := r.phone()
		p.hold(smCmdFindMe(11))
		p.waitCar(smHBCar, 2)

		r.car.FreezeAfter(0)
		frozen := time.Now()
		time.Sleep(time.Until(frozen.Add(500 * time.Millisecond)))
		p.release()
		p.press(smCmdFindMe(11), true)
		r.waitCar(smFindMe, 2) // forwarded: 0.5 s of hold time is not stale
		second := time.Now()
		time.Sleep(time.Until(second.Add(700 * time.Millisecond)))
		p.release()
		p.press(smCmdFindMe(11), true)
		p.wantRefusal(p.waitResult(smFindMe, 2, smWait), smFindMe, "invalid_vehicle_location")
		if n := r.helperLog.count(stale); n != 1 {
			t.Fatalf("D3's first rule logged the staleness %d times, want 1\n%s", n, r.helperLog)
		}
		if n := smCountVerbs(r.car.Received()); n != 2 {
			t.Fatalf("the car received %d verbs, want the first two", n)
		}
	})
}

// TestSummonDeviceLocationPassesThrough: a device_location 1 m from the car reaches the mock car, canonical, between
// holds and during one: D3 never checks it.
func TestSummonDeviceLocationPassesThrough(t *testing.T) {
	smWantDistance(t, 1)
	r := startSummonRig(t, summonOpts{})
	p := r.phone()
	between := smDeviceLocationJSON(1)
	p.msg(between)
	r.waitCar(smDeviceLoc, 1)
	p.hold(smCmdFindMe(11))
	during := smDeviceLocationJSON(1)
	p.msg(during)
	r.waitCar(smDeviceLoc, 2)
	var got []string
	for _, rec := range r.car.Received() {
		if rec.MsgType == smDeviceLoc {
			got = append(got, string(rec.Data))
		}
	}
	want := []string{string(smCanonical(t, summon.KindMsg, between)), string(smCanonical(t, summon.KindMsg, during))}
	if !slices.Equal(got, want) {
		t.Fatalf("the car received device_location %q, want %q", got, want)
	}
	p.release()
}

// TestSummonSchema: a valid info is forwarded; an info with txid "x" is dropped and the session continues; a VERB with
// summon_type "x" ends the inner session (BYE error), and the car records the disconnect.
func TestSummonSchema(t *testing.T) {
	r := startSummonRig(t, summonOpts{})
	p := r.phone()

	good := smInfoJSON("<none>")
	p.msg(good)
	r.waitCar(smInfo, 1)
	rec := r.carLast(smInfo)
	if got, want := string(rec.Data), string(smCanonical(t, summon.KindMsg, good)); got != want {
		t.Fatalf("the car's info is %s, want the canonical %s", got, want)
	}
	if !strings.Contains(string(rec.Data), `"txid":"<none>"`) {
		t.Fatalf("the car's info does not carry txid <none> literally: %s", rec.Data)
	}

	p.msg(smInfoJSON("x"))
	r.waitHelperLog(`summon: a non-conforming MSG "autopark:info"`, 1)
	p.msg(smKeepaliveJSON()) // the session continues: the next message goes through
	r.waitCar(smKeepalive, 1)
	if n := smCount(r.car.Received(), smInfo); n != 1 {
		t.Fatalf("the car received %d info messages, want only the valid one", n)
	}
	p.mustBeUp()

	bad := strings.Replace(smCmdFindMe(11), `"summon_type":"find_me"`, `"summon_type":"x"`, 1)
	p.verb(bad, true)
	if err := p.waitEnd(smWait); err == nil || !strings.Contains(err.Error(), "BYE: error") {
		t.Fatalf("the inner session ended with %v, want BYE error", err)
	}
	r.waitCar(smDisconnect, 1)
	if n := smCountVerbs(r.car.Received()); n != 0 {
		t.Fatal("the malformed verb reached the car")
	}
	r.waitSessions(1)
}

// TestSummonRevocation: when the car stops listing the phone's key as OWNER or DRIVER (removed in the Tesla app or
// with the key card), the authority re-checks end both sessions, the outer (agw's core) and the inner (the helper's),
// and the car gets the abort and the disconnect exactly once. No key list is involved: the phone's key was never
// written anywhere on the gateway.
//   - Removed from the car: both authd views drop the key. agw re-checks at once on the sign-in helper's
//     keys_changed; the helper re-checks too, and whichever core acts first, the other ends for the closed relay.
//   - Only the helper's re-check fails: the inner session's BYE revoked (helper-first order).
//   - Only agw's re-check fails: agw's BYE revoked, after which the helper ends on the relay's EOF.
func TestSummonRevocation(t *testing.T) {
	t.Run("removed from the car", func(t *testing.T) {
		r := startSummonRig(t, summonOpts{})
		p := r.phone()
		p.hold(smCmdArrow(smForward, 11))
		removed := time.Now()
		r.dropAgwRole()
		r.dropHelperRole()
		err := p.waitEnd(3 * time.Second)
		ended := time.Since(removed).Round(time.Millisecond)
		smRevokedOnce(t, r, removed)
		agw, hlp := smWindowReason(r.sessionLogs[0]), smWindowReason(r.sessionLogs[1])
		t.Logf("the inner session ended %v after the removal (%v); the windows closed with agw %q and the helper %q",
			ended, err, agw, hlp)
		if !smRevoked(agw) && !smRevoked(hlp) {
			t.Fatalf("neither core revoked: the windows closed with agw %q and the helper %q", agw, hlp)
		}
	})

	t.Run("the helper's re-check only", func(t *testing.T) {
		r := startSummonRig(t, summonOpts{})
		p := r.phone()
		p.hold(smCmdArrow(smForward, 11))
		removed := time.Now()
		r.dropHelperRole()
		if err := p.waitEnd(3 * time.Second); err == nil || !strings.Contains(err.Error(), "BYE: revoked") {
			t.Fatalf("the inner session ended with %v, want the helper's BYE revoked", err)
		}
		smRevokedOnce(t, r, removed)
		agw, hlp := smWindowReason(r.sessionLogs[0]), smWindowReason(r.sessionLogs[1])
		if !smRevoked(hlp) || smRevoked(agw) {
			t.Fatalf("the windows closed with agw %q and the helper %q, want the helper's revoked and agw's not",
				agw, hlp)
		}
	})

	t.Run("agw's re-check only", func(t *testing.T) {
		r := startSummonRig(t, summonOpts{})
		p := r.phone()
		p.hold(smCmdArrow(smForward, 11))
		removed := time.Now()
		r.dropAgwRole()
		// agw's BYE revoked ends the outer session, and client.InnerConn the inner one with it.
		err := p.waitEnd(3 * time.Second)
		t.Logf("the inner session ended %v after the removal: %v", time.Since(removed).Round(time.Millisecond), err)
		smRevokedOnce(t, r, removed)
		agw, hlp := smWindowReason(r.sessionLogs[0]), smWindowReason(r.sessionLogs[1])
		if !smRevoked(agw) || smRevoked(hlp) {
			t.Fatalf("the windows closed with agw %q and the helper %q, want agw's revoked and the helper's not",
				agw, hlp)
		}
		fp := keys.Fingerprint(r.ph.dev.PublicKey().Bytes())
		if !strings.Contains(r.agwLog.String(), "agw: revoking SUMMON fp="+fp) {
			t.Fatalf("agw did not log the revocation\n%s", r.logs())
		}
	})
}

// smRevoked reports whether a session-log reason is a re-check's revocation: "revoked: authority re-check (...)".
func smRevoked(reason string) bool { return strings.HasPrefix(reason, "revoked: authority re-check (") }

// dropAgwRole makes agw's authd (behind the sign-in helper) list the phone's key as role 0, and tells the helper's
// watchers that keys changed, as authd's KeysUpdated does: agw re-checks at once.
func (r *summonRig) dropAgwRole() {
	r.authd.set(true, nil, row(r.ph.dev.PublicKey().Bytes(), 0, 6))
	r.authSrv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
}

// dropHelperRole makes the summon helper's authd list the phone's key as role 0, and asks its core for a re-check at
// once (on the car it runs every 10 s while no key-change watch is up).
func (r *summonRig) dropHelperRole() {
	r.helperRole.Store(0)
	r.helperCore.Load().KeysChanged("test")
}

// smSwitchOracle is the summon helper's authd: OWNER, with com.tesla.Auth authd's, the role a test can change.
type smSwitchOracle struct{ role *atomic.Int32 }

func (o smSwitchOracle) KeyRole(context.Context, []byte) (int32, error) { return o.role.Load(), nil }
func (o smSwitchOracle) AuthOwnerIsAuthd(context.Context) (bool, error) { return true, nil }

// smRevokedOnce waits until both cores' windows have closed and both pollers have had their turn after the rewrite
// (each polls every 500 ms). Then the car must have received only heartbeats after the verb, then exactly one abort
// and one disconnect, and the marker must be cleared.
func smRevokedOnce(t *testing.T, r *summonRig, rewrite time.Time) {
	t.Helper()
	r.waitSessions(1)
	time.Sleep(time.Until(rewrite.Add(1200 * time.Millisecond)))
	if got := smCarKinds(r.carAfterVerb(smForward)); !smOnlyHBsThen(got, smAbort, smDisconnect) {
		t.Fatalf("after the verb the car received %q, want heartbeats, then exactly one abort and one disconnect", got)
	}
	smEventually(t, "the session marker cleared", func() bool { return !marker.Present(r.markerPath) })
}

// ---- the rig ----

const (
	smWait      = 3 * time.Second     // the bound on anything a test waits for that should happen
	smHelperVIN = "5YJ3E1EA7KF000000" // format-valid: helper.Serve refuses a VIN that fails helper.ValidVIN

	// The car's pose. The phone's positions are metres due north of it.
	smCarLat, smCarLon = 52.51637, 13.37771

	smFindMe    = "autopark:cmd_find_me"
	smForward   = "autopark:cmd_forward"
	smReverse   = "autopark:cmd_reverse"
	smAbort     = "autopark:cmd_abort"
	smCmdResult = "autopark:cmd_result"
	smHBApp     = "autopark:heartbeat_app_webrtc"
	smHBCar     = "autopark:heartbeat_car_webrtc"
	smStyle     = "autopark:style"
	smStatus    = "autopark:status"
	smLocation  = "vehicle_data:location"
	smDeviceLoc = "autopark:device_location"
	smInfo      = "autopark:info"
	smKeepalive = "control:keepalive"

	// What reached the car besides the phone's data (which is named by its msg_type).
	smH1         = "data_channels_ready"
	smDisconnect = "disconnect"
)

type summonOpts struct {
	realBus bool // the helper on carlink.DialBus over a private dbus-daemon (bustest), the car on mockcar.DBusPort
	helloV2 bool // the phone signs in with version 2 hellos, nothing pinned (client.DialV2, client.HandshakeV2)
}

// summonRig is the bench: the fake Pi, agw, the helper, the mock car, and the delay proxy the phones dial.
type summonRig struct {
	t           *testing.T
	helloV2     bool       // summonOpts.helloV2
	ph          phoneSetup // agw's key and VIN, and the phone's key
	authd       *fakeAuthd // agw's authd, behind the sign-in helper
	authSrv     *authsrv.Server
	helperRole  atomic.Int32                  // the role the helper's authd gives the phone's key
	helperCore  atomic.Pointer[server.Server] // the helper's core (helper.Config.OnCore)
	pi          *dialouttest.Pi
	proxy       *smDelayProxy
	car         *mockcar.Car
	helperPub   *ecdsa.PublicKey // the helper's pin, as the phone parsed it
	helperPin   string           // the VIN in the helper's pin
	markerPath  string
	sessionLogs [2]string   // agw's core's session log, then the helper's core's
	agwLog      *syncBuf    // agw's stderr: the standard logger, which both cores' window lines use too
	helperLog   *smLines    // the helper's Logf: its own sink, since helper.Serve never touches the standard logger
	exitLive    atomic.Bool // while set, a call of the helper's Exit fails the test
	lastHello   time.Time
}

// startSummonRig starts the bench and returns once agw is dialling the Pi with SUMMON among its capabilities.
func startSummonRig(t *testing.T, o summonOpts) *summonRig {
	t.Helper()
	if os.Getuid() == 0 {
		// summonrelay.Config.HelperUID 0 means the car's 1337, so the relay would refuse the helper's socket.
		t.Skip("the Summon bench passes this test's uid as the helper's: it cannot run as root")
	}
	r := &summonRig{t: t, helloV2: o.helloV2, ph: setupPhone(t), pi: dialouttest.Start(t), helperLog: &smLines{}}
	r.helperRole.Store(2)

	// agw signs the phone in through camhelper's real sign-in server, over fake authd listing the phone's key as role
	// 2. Never -fake-role: it serves only the bench capabilities, and SUMMON is a real one.
	a := &fakeAuthd{}
	a.set(true, nil, row(r.ph.dev.PublicKey().Bytes(), 2, 6))
	authSock, asrv, _ := startSignIn(t, a)
	r.authd, r.authSrv = a, asrv

	dir := shortDir(t) // not t.TempDir(): darwin caps unix socket paths at 104 bytes
	relaySock := filepath.Join(dir, "relay.sock")
	r.markerPath = filepath.Join(dir, "session")
	r.sessionLogs = [2]string{filepath.Join(r.ph.dir, "agw-sessions"), filepath.Join(dir, "helper-sessions")}
	prev := newSummonRelay
	newSummonRelay = func() capability.Capability {
		// The helper's uid is this test's. HelperUID 0 would mean the car's 1337, which is why the bench never runs as
		// root (it skips above). The default SocketUID (Lstat) sees this test's uid as the socket's owner.
		return summonrelay.NewWithConfig(summonrelay.Config{Socket: relaySock, HelperUID: os.Getuid(),
			RouteCheck: func(context.Context) error { return nil }})
	}
	t.Cleanup(func() { newSummonRelay = prev }) // cleanups run last-in first-out: after serve has returned

	// The car, on its bus.
	var bus carlink.Bus
	var port mockcar.Port
	if o.realBus {
		b := bustest.Start(t)
		hb, err := carlink.DialBus(b.Addr())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { hb.Close() })
		bus, port = hb, mockcar.DBusPort(b.Conn(t))
	} else {
		fb := carlink.NewFakeBus()
		bus, port = fb, mockcar.FakePort(fb)
	}
	r.car = mockcar.New(mockcar.Config{Port: port})
	r.car.SetPose(smCarLat, smCarLon, 90, 0, "off", "P")
	carCtx, stopCar := context.WithCancel(context.Background())
	carDone := make(chan struct{})
	go func() {
		defer close(carDone)
		r.car.Run(carCtx)
	}()
	t.Cleanup(func() {
		stopCar()
		<-carDone
	})

	// The helper, in-process, with its own key and a format-valid VIN. The phone pins them as it would from the
	// helper's pin.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if r.helperPub, r.helperPin, err = client.ParsePin(keys.PinString(&key.PublicKey, smHelperVIN)); err != nil {
		t.Fatal(err)
	}
	listening := make(chan struct{})
	r.exitLive.Store(true)
	cfg := helper.Config{
		Bus: bus, Oracle: smSwitchOracle{&r.helperRole}, Key: key, VIN: smHelperVIN,
		Socket: relaySock, AllowUID: os.Getuid(), OnCore: r.helperCore.Store,
		MarkerPath: r.markerPath, LivePath: filepath.Join(dir, "live"), LogPath: r.sessionLogs[1],
		Logf: r.helperLog.Logf,
		// Never os.Exit on the bench. Any Exit is a defect (an emit overrun's code 3, a Fatal's 4), so it fails the test.
		Exit: func(code int) {
			if r.exitLive.Load() {
				t.Errorf("the helper called Exit(%d)", code)
			}
		},
		OnListening: func() { close(listening) }, // the relay socket exists
	}
	hctx, stopHelper := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- helper.Serve(hctx, cfg) }()
	t.Cleanup(func() {
		stopHelper()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("helper.Serve returned %v after its context ended, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("helper.Serve did not return within 5 s of its context's end\n%s", r.logs())
		}
		r.exitLive.Store(false) // Serve has returned: nothing can call Exit now
		r.checkClean()
	})
	select {
	case <-listening:
	case err := <-served:
		t.Fatalf("helper.Serve returned before listening: %v\n%s", err, r.helperLog)
	case <-time.After(5 * time.Second):
		t.Fatalf("helper.Serve did not listen within 5 s\n%s", r.helperLog)
	}

	// agw: serve -dial to the Pi, with the real sign-in and the relay to the helper's socket.
	_, _, r.agwLog, _ = startServe(t, "-bench", "-dial", r.pi.CarAddr(), "-auth-socket", authSock,
		"-key", r.ph.keyPath, "-vin-file", r.ph.vinPath, "-log", r.sessionLogs[0])
	waitLogCount(t, r.agwLog, "(capabilities ECHO,CAMERA,DASHCAM,SUMMON,VCMD)", 1)
	r.proxy = smStartDelayProxy(t, r.pi.PhoneAddr())
	return r
}

// checkClean runs at the end, once both cores have stopped. The car saw no rule broken. Neither log carries a VIN, a
// coordinate or a phone key (Global Constraints: fingerprints only, and phone and car messages by msg_type and size),
// and neither core's session log carries a VIN or a phone key. The coordinate check leaves the session logs out: their
// RFC3339Nano times can hold a digit run such as "52.516".
func (r *summonRig) checkClean() {
	t := r.t
	if v := r.car.Violations(); len(v) > 0 {
		t.Errorf("the mock car saw rules broken: %q", v)
	}
	phoneKey := hex.EncodeToString(r.ph.dev.PublicKey().Bytes())
	logs := r.logs()
	for _, s := range []string{testVIN, smHelperVIN, fmt.Sprint(smCarLat)[:6], fmt.Sprint(smCarLon)[:6], phoneKey} {
		if strings.Contains(logs, s) {
			t.Errorf("the logs carry %q\n%s", s, logs)
		}
	}
	for _, path := range r.sessionLogs {
		b, _ := os.ReadFile(path) // missing when no window closed
		for _, s := range []string{testVIN, smHelperVIN, phoneKey} {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("the session log %s carries %q:\n%s", filepath.Base(path), s, b)
			}
		}
	}
}

// logs is both logs, for a failure's message.
func (r *summonRig) logs() string {
	agw := "(not started)"
	if r.agwLog != nil {
		agw = r.agwLog.String()
	}
	return "agw log:\n" + agw + "\n" + r.helperLog.String()
}

// waitIdle waits until the Pi holds agw's 3 idle connections, as it does between sessions.
func (r *summonRig) waitIdle() {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.pi.Idle() != 3 {
		if time.Now().After(deadline) {
			r.t.Fatalf("Pi idle = %d, want 3\n%s", r.pi.Idle(), r.logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// phone signs a phone in, as SummonLink does (design §6.3): the outer SUMMON window to agw through the Pi with agw's
// pin, then the inner session to the helper over it with the helper's pin. It returns once the car's answer to H1
// (its style, status and location) has reached the phone.
func (r *summonRig) phone() *smPhone {
	t := r.t
	t.Helper()
	r.waitIdle()
	if d := time.Until(r.lastHello.Add(250 * time.Millisecond)); d > 0 {
		time.Sleep(d) // each core admits 5 hellos a second
	}
	r.lastHello = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var outer, s *client.Session
	var err error
	if r.helloV2 {
		var gk *ecdsa.PublicKey
		if outer, gk, err = client.DialV2(ctx, r.proxy.addr(), proto.CapSummon, nil, nil, r.ph.vin, r.ph.dev); err == nil &&
			!gk.Equal(r.ph.pub) {
			t.Fatal("agw's v2 challenge carried a key other than agw's pin key")
		}
	} else {
		outer, err = client.Dial(ctx, r.proxy.addr(), proto.CapSummon, nil, r.ph.pub, r.ph.vin, r.ph.dev)
	}
	if err != nil {
		t.Fatalf("the outer SUMMON window: %v\n%s", err, r.logs())
	}
	in := client.InnerConn(outer, summonrelay.TypeInner, summonrelay.TypeInnerOut)
	if r.helloV2 {
		var hk *ecdsa.PublicKey
		if s, hk, err = client.HandshakeV2(ctx, in, proto.CapSummon, nil, nil, r.helperPin, r.ph.dev); err == nil &&
			!hk.Equal(r.helperPub) {
			in.Close()
			t.Fatal("the helper's v2 challenge carried a key other than the helper's pin key")
		}
	} else {
		s, err = client.Handshake(ctx, in, proto.CapSummon, nil, r.helperPub, r.helperPin, r.ph.dev)
	}
	if err != nil {
		in.Close()
		t.Fatalf("the inner SUMMON session: %v\n%s", err, r.logs())
	}
	p := &smPhone{r: r, t: t, s: s, ended: make(chan struct{})}
	go p.recv()
	t.Cleanup(p.end)
	p.waitCar(smLocation, 1)
	return p
}

// smCarRec is one input the car received, with its index in Received.
type smCarRec struct {
	mockcar.Received
	idx int
}

// smCarKind names a car input: the msg_type of the phone's data, smH1 or smDisconnect.
func smCarKind(rec mockcar.Received) string {
	if rec.Member == carlink.SignalLocalToRemoteSignaling {
		switch {
		case bytes.Contains(rec.Data, []byte(`"data_channels_ready"`)):
			return smH1
		case rec.MsgType == "autopark:set_webrtc":
			return smDisconnect
		}
		return "signaling " + string(rec.Data)
	}
	return rec.MsgType
}

func smCarKinds(recs []mockcar.Received) []string {
	out := make([]string, len(recs))
	for i, rec := range recs {
		out[i] = smCarKind(rec)
	}
	return out
}

func smCount(recs []mockcar.Received, kind string) int {
	n := 0
	for _, rec := range recs {
		if smCarKind(rec) == kind {
			n++
		}
	}
	return n
}

// smCountVerbs counts the motion verbs among recs.
func smCountVerbs(recs []mockcar.Received) int {
	return smCount(recs, smFindMe) + smCount(recs, smForward) + smCount(recs, smReverse)
}

func (r *summonRig) carKinds() []string { return smCarKinds(r.car.Received()) }

// waitCar waits until the car has received n inputs of kind.
func (r *summonRig) waitCar(kind string, n int) {
	r.t.Helper()
	if !r.car.WaitFor(func(got []mockcar.Received) bool { return smCount(got, kind) >= n }, smWait) {
		r.t.Fatalf("the car did not receive %d %q within %v; it got %q\n%s", n, kind, smWait, r.carKinds(), r.logs())
	}
}

// carAfterVerb is what the car received after its first verbType.
func (r *summonRig) carAfterVerb(verbType string) []mockcar.Received {
	recs := r.car.Received()
	if i := slices.IndexFunc(recs, func(rec mockcar.Received) bool { return smCarKind(rec) == verbType }); i >= 0 {
		return recs[i+1:]
	}
	return nil
}

// waitCarAfterVerb waits up to within for the car's first input of kind after its first verbType.
func (r *summonRig) waitCarAfterVerb(verbType, kind string, within time.Duration) smCarRec {
	r.t.Helper()
	var found smCarRec
	ok := r.car.WaitFor(func(got []mockcar.Received) bool {
		v := slices.IndexFunc(got, func(rec mockcar.Received) bool { return smCarKind(rec) == verbType })
		for i := v + 1; v >= 0 && i < len(got); i++ {
			if smCarKind(got[i]) == kind {
				found = smCarRec{got[i], i}
				return true
			}
		}
		return false
	}, within)
	if !ok {
		r.t.Fatalf("no %q reached the car within %v of waiting, after its %q; it got %q\n%s", kind, within, verbType,
			r.carKinds(), r.logs())
	}
	return found
}

// carAfter is what the car received after c.
func (r *summonRig) carAfter(c smCarRec) []mockcar.Received { return r.car.Received()[c.idx+1:] }

// carLast is the car's latest input of kind.
func (r *summonRig) carLast(kind string) mockcar.Received {
	r.t.Helper()
	recs := r.car.Received()
	for i := len(recs) - 1; i >= 0; i-- {
		if smCarKind(recs[i]) == kind {
			return recs[i]
		}
	}
	r.t.Fatalf("the car received no %q", kind)
	return mockcar.Received{}
}

// waitHelperLog waits until the helper has logged a line containing sub n times.
func (r *summonRig) waitHelperLog(sub string, n int) {
	r.t.Helper()
	smEventually(r.t, fmt.Sprintf("%d helper log lines with %q", n, sub), func() bool {
		return r.helperLog.count(sub) >= n
	})
}

// wantOneTrip checks that D2 tripped exactly once. It waits for the trip's log line first. Tick logs it only after its
// injected abort's emit has returned, and the emit reaches the mock car on other goroutines, so the car can see the
// abort before the line is written. Every caller first lets 300 ms or more pass after the abort, so a second trip
// would show.
func (r *summonRig) wantOneTrip() {
	r.t.Helper()
	const trip = "summon: D2 trip"
	r.waitHelperLog(trip, 1)
	if n := r.helperLog.count(trip); n != 1 {
		r.t.Fatalf("%d D2 trips logged, want 1\n%s", n, r.helperLog)
	}
}

// waitSessions waits until each core's session log holds n closed SUMMON windows: agw's outer one and the helper's
// inner one.
func (r *summonRig) waitSessions(n int) {
	r.t.Helper()
	smEventually(r.t, fmt.Sprintf("%d closed SUMMON windows in each core's session log", n), func() bool {
		return summonWindows(r.sessionLogs[0]) >= n && summonWindows(r.sessionLogs[1]) >= n
	})
}

// summonWindows counts the SUMMON windows in a core's session log.
func summonWindows(path string) int { return len(smWindowReasons(path)) }

// smWindowReason is the reason the first SUMMON window in a core's session log closed with, or "".
func smWindowReason(path string) string {
	if rs := smWindowReasons(path); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// smWindowReasons is the closing reason of each SUMMON window in a core's session log, in order.
func smWindowReasons(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		var e struct {
			Cap    string `json:"cap"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Cap == "SUMMON" {
			out = append(out, e.Reason)
		}
	}
	return out
}

// smWantTrip checks a D2 abort's latency from a reference LIVE, as the phone stamped it before Send: within bound, and
// over TripAfter. D2 trips only once more than TripAfter has passed since the latest liveness instant. An abort that
// soon after the reference means the reference never reached the helper (the measure would then count from a later
// LIVE than the last one that got through), or that D2 tripped early.
func smWantTrip(t *testing.T, lat, bound time.Duration, ref string) {
	t.Helper()
	if lat <= summon.TripAfter {
		t.Fatalf("the abort reached the car %v after %s, not over %v: that LIVE never reached the helper, or D2 "+
			"tripped early", lat, ref, summon.TripAfter)
	}
	if lat > bound {
		t.Fatalf("the abort reached the car %v after %s, want within %v", lat, ref, bound)
	}
}

func smEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(smWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: not within %v", what, smWait)
		}
		time.Sleep(time.Millisecond)
	}
}

// smLines is the helper's log: every Logf line, kept in memory. It never blocks for long, as Config.Logf must not.
type smLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *smLines) Logf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (l *smLines) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (l *smLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return "helper log:\n  " + strings.Join(l.lines, "\n  ")
}

// ---- the phone ----

// smPhone is the bench phone. Its Recv loop takes the inner session's TICKs in and records the CAR frames. While a
// control is held, its sender sends LIVE at 20 Hz and, once the car has answered the press, Tesla's heartbeat every
// 100 ms.
type smPhone struct {
	r       *summonRig
	t       *testing.T
	s       *client.Session // the inner session; client.InnerConn carries it in the outer one
	presses uint32          // the latest press id

	mu         sync.Mutex
	cars       []smFrame   // the CAR frames, in order
	unexpected []byte      // frame types other than CAR
	carTS      json.Number // car_timestamp of the latest heartbeat echo
	err        error       // what ended the inner session
	ended      chan struct{}

	// The sender. smu is held across each round of sends, so pause and stopSending return between rounds.
	smu       sync.Mutex
	stop      chan struct{}
	stopped   chan struct{}
	sendPress uint32
	hbOn      bool
	paused    bool
	liveOff   bool      // the rounds send no LIVE (setLive); their HBs go on
	lastLive  time.Time // when the latest LIVE was handed to Send
	lives     int
	hbs       []string // the HB JSON sent, in order
}

// smFrame is one CAR frame: the channel byte and the car's JSON.
type smFrame struct {
	idx int // its position among the phone's CAR frames
	ch  byte
	raw []byte
	obj map[string]any // numbers as json.Number; nil if raw is not a JSON object
}

func (f smFrame) msgType() string { return f.str("msg_type") }

func (f smFrame) str(key string) string {
	s, _ := f.obj[key].(string)
	return s
}

func (f smFrame) num(key string) string {
	n, _ := f.obj[key].(json.Number)
	return n.String()
}

func (p *smPhone) recv() {
	defer close(p.ended)
	for {
		typ, body, err := p.s.Recv()
		if err != nil {
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		if typ != summon.TypeCar || len(body) == 0 {
			p.unexpected = append(p.unexpected, typ)
		} else {
			f := smFrame{idx: len(p.cars), ch: body[0], raw: slices.Clone(body[1:])}
			f.obj = smDecodeObj(f.raw)
			p.cars = append(p.cars, f)
			if n, ok := f.obj["car_timestamp"].(json.Number); ok && f.msgType() == smHBCar {
				p.carTS = n
			}
		}
		p.mu.Unlock()
	}
}

func (p *smPhone) frames() []smFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.cars)
}

func smFrameTypes(fs []smFrame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.msgType()
	}
	return out
}

// waitCar waits until the phone has n CAR frames of msgType.
func (p *smPhone) waitCar(msgType string, n int) {
	p.t.Helper()
	smEventually(p.t, fmt.Sprintf("%d CAR frames %q at the phone", n, msgType), func() bool {
		c := 0
		for _, f := range p.frames() {
			if f.msgType() == msgType {
				c++
			}
		}
		return c >= n
	})
}

// results is the phone's cmd_results for cmdType, in order.
func (p *smPhone) results(cmdType string) []smFrame {
	var out []smFrame
	for _, f := range p.frames() {
		if f.msgType() == smCmdResult && f.str("cmd_type") == cmdType {
			out = append(out, f)
		}
	}
	return out
}

func (p *smPhone) countResults(cmdType string) int { return len(p.results(cmdType)) }

// waitResult waits up to within for the phone's n-th cmd_result for cmdType, and returns it.
func (p *smPhone) waitResult(cmdType string, n int, within time.Duration) smFrame {
	p.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := p.results(cmdType); len(got) >= n {
			return got[n-1]
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("the phone has %d cmd_results for %s after %v, want %d; its CAR frames: %q\n%s",
				p.countResults(cmdType), cmdType, within, n, smFrameTypes(p.frames()), p.r.logs())
		}
		time.Sleep(time.Millisecond)
	}
}

// wantRefusal checks that f is the helper's refusal of cmdType for reason, on dcTcp, its keys in Go's sorted order as
// QtCarServer's are, and that its timestamp is at or above every cmd_result timestamp the phone got before it.
func (p *smPhone) wantRefusal(f smFrame, cmdType, reason string) {
	p.t.Helper()
	prefix := fmt.Sprintf(`{"cmd_type":%q,"msg_type":%q,"reason":%q,"result":false,"timestamp":`,
		cmdType, smCmdResult, reason)
	if f.ch != summon.ChanTCP || !strings.HasPrefix(string(f.raw), prefix) || len(f.obj) != 5 {
		p.t.Fatalf("got the CAR frame %s on channel %d, want the refusal %s…} on dcTcp", f.raw, f.ch, prefix)
	}
	ts, err := json.Number(f.num("timestamp")).Int64()
	if err != nil {
		p.t.Fatalf("the refusal's timestamp: %v", err)
	}
	for _, e := range p.frames()[:f.idx] {
		if e.msgType() != smCmdResult {
			continue
		}
		if et, err := json.Number(e.num("timestamp")).Int64(); err == nil && et > ts {
			p.t.Fatalf("the refusal's timestamp %d is below an earlier cmd_result's %d", ts, et)
		}
	}
}

func (p *smPhone) endErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// mustBeUp fails if the inner session has ended.
func (p *smPhone) mustBeUp() {
	p.t.Helper()
	select {
	case <-p.ended:
		p.t.Fatalf("the inner session ended: %v\n%s", p.endErr(), p.r.logs())
	default:
	}
}

// waitEnd waits up to d for the inner session's end, and returns the error that ended it.
func (p *smPhone) waitEnd(d time.Duration) error {
	p.t.Helper()
	select {
	case <-p.ended:
	case <-time.After(d):
		p.t.Fatalf("the inner session did not end within %v\n%s", d, p.r.logs())
	}
	return p.endErr()
}

// end is the phone leaving: the sender stops, and the inner session closes, which closes the outer one with it
// (client.InnerConn). It is idempotent.
func (p *smPhone) end() {
	p.stopSending()
	p.s.Close()
	select {
	case <-p.ended:
	case <-time.After(smWait):
		p.t.Errorf("the phone's Recv loop did not end within %v of its close", smWait)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.unexpected) > 0 {
		p.t.Errorf("the phone got frame types %x besides CAR", p.unexpected)
	}
}

// tick is the newest tick's id. fresh first waits for a tick read in the last 25 ms, as a press on a healthy link
// echoes; without it the phone echoes whatever tick it has, however old (a delayed or stalled link).
func (p *smPhone) tick(fresh bool) [8]byte {
	p.t.Helper()
	deadline := time.Now().Add(smWait)
	for {
		id, age, ok := p.s.NewestTick()
		if ok && (!fresh || age < 25*time.Millisecond) {
			return id
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("no fresh TICK within %v", smWait)
		}
		time.Sleep(time.Millisecond)
	}
}

// press is a press on a motion control: native assigns a press id and starts LIVE, and Tesla's press logic sends
// the verb.
func (p *smPhone) press(js string, fresh bool) {
	p.t.Helper()
	p.presses++
	p.startSending(p.presses)
	p.sendVerb(js, p.presses, fresh)
}

// verb sends a VERB under a new press id, and starts nothing.
func (p *smPhone) verb(js string, fresh bool) {
	p.t.Helper()
	p.presses++
	p.sendVerb(js, p.presses, fresh)
}

func (p *smPhone) sendVerb(js string, press uint32, fresh bool) {
	p.t.Helper()
	if err := p.s.Send(summon.TypeVerb, smMotionBody(p.tick(fresh), press, js)); err != nil {
		p.t.Fatalf("sending the verb: %v", err)
	}
}

// hold presses with a fresh tick, waits until the car has the verb and the phone the car's cmd_result for it, and
// then turns the heartbeats on: Tesla's app starts them only after the car's cmd_result (Appendix A §2.4).
func (p *smPhone) hold(js string) {
	p.t.Helper()
	typ := smMsgTypeOf(js)
	verbs, results := smCount(p.r.car.Received(), typ), p.countResults(typ)
	p.press(js, true)
	p.r.waitCar(typ, verbs+1)
	if f := p.waitResult(typ, results+1, smWait); f.obj["result"] != true {
		p.t.Fatalf("the car refused %s: %s", typ, f.raw)
	}
	p.smu.Lock()
	p.hbOn = true
	p.smu.Unlock()
}

// release is Tesla's release on this car: native stops LIVE and sends cmd_abort, and the heartbeats stop.
func (p *smPhone) release() {
	p.t.Helper()
	p.stopSending()
	p.msg(smAbortMsgJSON())
}

// msg sends a MSG.
func (p *smPhone) msg(js string) {
	p.t.Helper()
	if err := p.s.Send(summon.TypeMsg, []byte(js)); err != nil {
		p.t.Fatalf("sending %s: %v", smMsgTypeOf(js), err)
	}
}

// startSending starts the sender for press: a LIVE every 50 ms (unless setLive turns it off), and an HB every second
// round once hbOn is set, each echoing the newest tick the phone has read, however old. The late link's old tick ids
// are the point of TestSummonLateLinkTrips: the phone keeps its usual rate whatever the link does.
func (p *smPhone) startSending(press uint32) {
	p.stopSending()
	p.smu.Lock()
	defer p.smu.Unlock()
	p.stop, p.stopped = make(chan struct{}), make(chan struct{})
	p.sendPress, p.hbOn, p.paused, p.liveOff = press, false, false, false
	go p.sender(p.stop, p.stopped)
}

func (p *smPhone) sender(stop, stopped chan struct{}) {
	defer close(stopped)
	tk := time.NewTicker(50 * time.Millisecond)
	defer tk.Stop()
	for round := 0; ; round++ {
		select {
		case <-stop:
			return
		case <-p.ended:
			return
		case <-tk.C:
		}
		p.smu.Lock()
		if !p.paused {
			p.sendRound(round)
		}
		p.smu.Unlock()
	}
}

// sendRound sends a LIVE, and every second round an HB once the heartbeats are on. p.smu is held.
func (p *smPhone) sendRound(round int) {
	id, _, ok := p.s.NewestTick()
	if !ok {
		return
	}
	at := time.Now() // taken before Send: the latency bounds count from here
	if !p.liveOff && p.s.Send(summon.TypeLive, id[:]) == nil {
		p.lastLive, p.lives = at, p.lives+1
	}
	if p.hbOn && round%2 == 1 {
		p.mu.Lock()
		carTS := p.carTS
		p.mu.Unlock()
		js := smHeartbeatJSON(carTS)
		if p.s.Send(summon.TypeHB, smMotionBody(id, p.sendPress, js)) == nil {
			p.hbs = append(p.hbs, js)
		}
	}
}

// stopSending stops the sender, if it runs, and returns when the latest LIVE was sent. Nothing is sent after it
// returns.
func (p *smPhone) stopSending() time.Time {
	p.smu.Lock()
	stop, stopped := p.stop, p.stopped
	p.stop, p.stopped = nil, nil
	p.smu.Unlock()
	if stop != nil {
		close(stop)
		<-stopped
	}
	p.smu.Lock()
	defer p.smu.Unlock()
	return p.lastLive
}

// pause holds the sender between rounds, and returns when the latest LIVE was sent.
func (p *smPhone) pause() time.Time {
	p.smu.Lock()
	defer p.smu.Unlock()
	p.paused = true
	return p.lastLive
}

// setLive turns the sender's LIVE on or off, between rounds, as a native phone's LIVE stops when no tick arrives. The
// heartbeats go on either way.
func (p *smPhone) setLive(on bool) {
	p.smu.Lock()
	p.liveOff = !on
	p.smu.Unlock()
}

func (p *smPhone) resume() {
	p.smu.Lock()
	p.paused = false
	p.smu.Unlock()
}

func (p *smPhone) livesSent() int {
	p.smu.Lock()
	defer p.smu.Unlock()
	return p.lives
}

func (p *smPhone) hbsSent() []string {
	p.smu.Lock()
	defer p.smu.Unlock()
	return slices.Clone(p.hbs)
}

// smMotionBody is a VERB or HB body: the tick id ‖ the press id (u32 BE) ‖ Tesla's JSON.
func smMotionBody(tick [8]byte, press uint32, js string) []byte {
	b := make([]byte, 12, 12+len(js))
	copy(b, tick[:])
	binary.BigEndian.PutUint32(b[8:], press)
	return append(b, js...)
}

// ---- Tesla's messages (Appendix A §2.1): {msg_type, ...payload, "created:timestamp": Date.now()} ----

// smNorthLat is the latitude m metres due north of the car. D3 measures with the haversine at R = 6371 km, along which
// a pure latitude offset of m/(R·π/180) degrees is m metres.
func smNorthLat(m float64) float64 { return smCarLat + m/(6371000*math.Pi/180) }

// smCmdFindMe is Come to Me from m metres north of the car: the goal is the phone's own fix.
func smCmdFindMe(m float64) string {
	lat := smNorthLat(m)
	return fmt.Sprintf(`{"msg_type":%q,"latitude":%v,"longitude":%v,"accuracy":5,"goal_latitude":%v,`+
		`"goal_longitude":%v,"client_version":2,"summon_type":"find_me","created:timestamp":%d}`,
		smFindMe, lat, smCarLon, lat, smCarLon, time.Now().UnixMilli())
}

// smCmdGoTo is Go to Target: cmd_find_me from phoneM metres north of the car, with the pin, the goal, goalM metres
// north of it (Appendix A §2.2: lat/lon/accuracy stay the phone's fix).
func smCmdGoTo(phoneM, goalM float64) string {
	return fmt.Sprintf(`{"msg_type":%q,"latitude":%v,"longitude":%v,"accuracy":5,"goal_latitude":%v,`+
		`"goal_longitude":%v,"client_version":2,"summon_type":"pin_drop","created:timestamp":%d}`,
		smFindMe, smNorthLat(phoneM), smCarLon, smNorthLat(goalM), smCarLon, time.Now().UnixMilli())
}

// smCmdArrow is cmd_forward or cmd_reverse from m metres north of the car.
func smCmdArrow(typ string, m float64) string {
	return fmt.Sprintf(`{"msg_type":%q,"latitude":%v,"longitude":%v,"client_version":2,"created:timestamp":%d}`,
		typ, smNorthLat(m), smCarLon, time.Now().UnixMilli())
}

// smHeartbeatJSON is heartbeat_app_webrtc: car_timestamp is the latest echo's, omitted before the first.
func smHeartbeatJSON(carTS json.Number) string {
	now := time.Now().UnixMilli()
	if carTS == "" || carTS == "0" {
		return fmt.Sprintf(`{"msg_type":%q,"timestamp":%d,"created:timestamp":%d}`, smHBApp, now, now)
	}
	return fmt.Sprintf(`{"msg_type":%q,"timestamp":%d,"car_timestamp":%s,"created:timestamp":%d}`, smHBApp, now, carTS,
		now)
}

// smDeviceLocationJSON is Come to Me's device_location from m metres north of the car (no altitude: -1).
func smDeviceLocationJSON(m float64) string {
	lat := smNorthLat(m)
	return fmt.Sprintf(`{"msg_type":%q,"latitude":%v,"longitude":%v,"accuracy":5,"altitude":-1,"altitude_accuracy":-1,`+
		`"goal_latitude":%v,"goal_longitude":%v,"summon_type":"find_me","created:timestamp":%d}`,
		smDeviceLoc, lat, smCarLon, lat, smCarLon, time.Now().UnixMilli())
}

// smInfoJSON is the info Tesla's app sends for each autopark:style; its txid is always "<none>".
func smInfoJSON(txid string) string {
	return fmt.Sprintf(`{"msg_type":%q,"txid":%q,"x_tesla_user_agent":"TeslaApp/4.57.5","model":"iPhone",`+
		`"name":"bench","tag":"TeslaApp/4.57.5","public_key_hash":"cce6a2d2","created:timestamp":%d}`,
		smInfo, txid, time.Now().UnixMilli())
}

func smKeepaliveJSON() string {
	return fmt.Sprintf(`{"msg_type":%q,"created:timestamp":%d}`, smKeepalive, time.Now().UnixMilli())
}

// smAbortMsgJSON is Tesla's release, payload-less but stamped; the helper sends the car summon.AbortJSON for it.
func smAbortMsgJSON() string {
	return fmt.Sprintf(`{"msg_type":%q,"created:timestamp":%d}`, smAbort, time.Now().UnixMilli())
}

// smCanonical is what the helper sends the car for js: Parse's re-encoding.
func smCanonical(t *testing.T, k summon.Kind, js string) []byte {
	t.Helper()
	m, err := summon.Parse(k, []byte(js))
	if err != nil {
		t.Fatalf("the bench built a non-conforming message %s: %v", js, err)
	}
	return m.Canonical
}

func smMsgTypeOf(js string) string {
	var head struct {
		MsgType string `json:"msg_type"`
	}
	json.Unmarshal([]byte(js), &head)
	return head.MsgType
}

func smDecodeObj(b []byte) map[string]any {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil {
		return nil
	}
	return m
}

func smRepeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// smOnlyHBsThen reports whether got is heartbeats, then exactly tail.
func smOnlyHBsThen(got []string, tail ...string) bool {
	i := 0
	for i < len(got) && got[i] == smHBApp {
		i++
	}
	return slices.Equal(got[i:], tail)
}

// smWantDistance checks the bench's geometry against D3's own measure: smNorthLat(m) is m metres from the car.
func smWantDistance(t *testing.T, m float64) {
	t.Helper()
	if d := guards.DistanceM(smCarLat, smCarLon, smNorthLat(m), smCarLon); math.Abs(d-m) > 0.01 {
		t.Fatalf("smNorthLat(%v) is %v m from the car by D3's measure", m, d)
	}
}

// ---- the delay proxy ----

// smDelayProxy sits between the phones and the Pi's phone port. Each direction can be delayed by its own D (the bytes
// read are written D later, in order), and hold keeps every byte, both ways, until flush. It always reads at once in
// both directions, so neither end ever blocks on it.
type smDelayProxy struct {
	t      *testing.T
	ln     net.Listener
	target string
	wg     sync.WaitGroup

	mu       sync.Mutex
	up, down time.Duration // the delays: phone→Pi, Pi→phone
	holding  bool
	closed   bool
	conns    []net.Conn
	pumps    []*smPump
	upLast   time.Time // when bytes from a phone were last read
	upBusy   int       // phone→Pi chunks taken from a queue and not yet written
}

// smPump is one direction of one connection.
type smPump struct {
	p        *smDelayProxy
	up       bool // phone→Pi
	src, dst net.Conn
	kick     chan struct{} // capacity 1: the queue or the proxy's mode changed
	q        []smChunk     // guarded by p.mu
}

type smChunk struct {
	b   []byte    // nil: the source ended
	due time.Time // when it was read, plus the direction's delay then
}

func smStartDelayProxy(t *testing.T, target string) *smDelayProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &smDelayProxy{t: t, ln: ln, target: target}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

func (p *smDelayProxy) addr() string { return p.ln.Addr().String() }

func (p *smDelayProxy) accept() {
	defer p.wg.Done()
	for {
		phone, err := p.ln.Accept()
		if err != nil {
			return
		}
		pi, err := net.DialTimeout("tcp", p.target, smWait)
		if err != nil {
			phone.Close()
			continue
		}
		up := &smPump{p: p, up: true, src: phone, dst: pi, kick: make(chan struct{}, 1)}
		down := &smPump{p: p, src: pi, dst: phone, kick: make(chan struct{}, 1)}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			phone.Close()
			pi.Close()
			return
		}
		p.conns = append(p.conns, phone, pi)
		p.pumps = append(p.pumps, up, down)
		p.wg.Add(4)
		p.mu.Unlock()
		go up.read()
		go up.write()
		go down.read()
		go down.write()
	}
}

// setDelay delays the bytes read from now on: phone→Pi by up, Pi→phone by down. It returns when the delays started.
func (p *smDelayProxy) setDelay(up, down time.Duration) time.Time {
	p.mu.Lock()
	at := time.Now()
	p.up, p.down = up, down
	pumps := p.pumps
	p.mu.Unlock()
	for _, u := range pumps {
		u.wake()
	}
	return at
}

// hold keeps every byte, both ways, from now until flush. It returns when the hold started.
func (p *smDelayProxy) hold() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holding = true
	return time.Now()
}

// flush ends the hold: every byte held is written at once, in order.
func (p *smDelayProxy) flush() {
	p.mu.Lock()
	p.holding = false
	pumps := p.pumps
	p.mu.Unlock()
	for _, u := range pumps {
		u.wake()
	}
}

// waitUpQuiet waits until the proxy has read phone bytes after the instant after, then nothing from a phone for d,
// with nothing queued or in flight toward the Pi. The caller's last frame was handed to Send at after, so its bytes
// reach the proxy only later. Once this returns, that frame has all but certainly been forwarded, but it is not
// proven. Bytes of an earlier frame still in flight at after also count as read after it. If the frame's own bytes
// then came more than d behind them, the stall that follows would catch the frame. StalledLinkTrips' lower bound
// (smWantTrip) makes that case fail loudly instead of measuring from the wrong LIVE.
func (p *smDelayProxy) waitUpQuiet(after time.Time, d time.Duration) {
	p.t.Helper()
	smEventually(p.t, "the phone's last frame through the proxy, then quiet", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, u := range p.pumps {
			if u.up && len(u.q) > 0 {
				return false
			}
		}
		return p.upLast.After(after) && p.upBusy == 0 && time.Since(p.upLast) >= d
	})
}

func (p *smDelayProxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns, pumps := p.conns, p.pumps
	p.mu.Unlock()
	p.ln.Close()
	for _, c := range conns {
		c.Close()
	}
	for _, u := range pumps {
		u.wake()
	}
	p.wg.Wait()
}

func (u *smPump) wake() {
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// read queues what src sends, stamped with when it is due, and the end of src.
func (u *smPump) read() {
	defer u.p.wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := u.src.Read(buf)
		if n > 0 {
			u.push(slices.Clone(buf[:n]))
		}
		if err != nil {
			u.push(nil)
			return
		}
	}
}

func (u *smPump) push(b []byte) {
	p := u.p
	p.mu.Lock()
	now := time.Now()
	d := p.down
	if u.up {
		d, p.upLast = p.up, now
	}
	u.q = append(u.q, smChunk{b: b, due: now.Add(d)})
	p.mu.Unlock()
	u.wake()
}

// write writes the queue to dst as each chunk falls due, and passes src's end on as a half-close.
func (u *smPump) write() {
	defer u.p.wg.Done()
	for {
		c, ok := u.next()
		if !ok {
			return
		}
		if c.b == nil {
			if cw, ok := u.dst.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			} else {
				u.dst.Close()
			}
			return
		}
		_, err := u.dst.Write(c.b)
		if u.up {
			u.p.mu.Lock()
			u.p.upBusy--
			u.p.mu.Unlock()
		}
		if err != nil {
			u.src.Close()
			u.dst.Close()
			return
		}
	}
}

// next waits until the queue's head is due and the proxy is not holding, and takes it; false once the proxy closed.
func (u *smPump) next() (smChunk, bool) {
	p := u.p
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return smChunk{}, false
		}
		var due <-chan time.Time
		if !p.holding && len(u.q) > 0 {
			wait := time.Until(u.q[0].due)
			if wait <= 0 {
				c := u.q[0]
				u.q = u.q[1:]
				if u.up && c.b != nil {
					p.upBusy++
				}
				p.mu.Unlock()
				return c, true
			}
			due = time.After(wait)
		}
		p.mu.Unlock()
		select {
		case <-u.kick:
		case <-due:
		}
	}
}
