package camhelper

import (
	"strings"
	"time"

	"airgapp/gateway/camhelper/proto"
)

type readings struct{ gear, sentry, climate, camAccess string }

type gateCounters struct{ nonP, unknown, sentry, camera, summonSel int }

// note is an event for a session, sent after s.mu is released.
type note struct {
	sess *session
	ev   proto.Event
}

type talkState struct {
	owner     *session
	armedAt   time.Time
	writer    Child
	pumpStop  chan struct{}
	restartAt time.Time
	restarts  int // consecutive failed writer starts
	routeOK   bool
	missing   int
	edges     int
	nextEdge  time.Time
	boost     bool
	gain      *int
}

// read takes a fresh reading of the four checkWebcamAllowed inputs. It runs
// without s.mu (four subprocesses).
func (s *Server) read() readings {
	get := func(r DVRead) string {
		raw, err := s.cfg.Car.Read(r)
		if err != nil {
			return ""
		}
		v, ok := parseDV(raw)
		if !ok {
			return ""
		}
		return v
	}
	rd := readings{sentry: get(ReadSentry), climate: get(ReadClimate), camAccess: get(ReadCamAccess)}
	if raw, err := s.cfg.Car.Read(ReadGear); err == nil {
		rd.gear = gearOf(raw)
	}
	return rd
}

func driving(gear string) bool { return gear == "R" || gear == "N" || gear == "D" }

func sentryBlocks(rd readings) bool {
	return rd.sentry == "Off" && !strings.Contains(rd.climate, "Dog")
}

// gateOf is the firmware's checkWebcamAllowed on known values: a hold is
// refused only on a known failure (an invalid gear is allowed).
func gateOf(rd readings) string {
	switch {
	case driving(rd.gear):
		return proto.ErrGateGear
	case rd.camAccess != "" && rd.camAccess != "true":
		return proto.ErrGateCamera
	case sentryBlocks(rd):
		return proto.ErrGateSentry
	}
	return ""
}

// talkGate is stricter: every input must be known and allow the webcam.
func talkGate(rd readings) string {
	switch {
	case rd.gear != "P":
		return proto.ErrGateGear
	case rd.camAccess != "true":
		return proto.ErrGateCamera
	case rd.sentry == "" || sentryBlocks(rd):
		return proto.ErrGateSentry
	}
	return ""
}

// ---- the ordered release (PLAN §4.9) ----

type change struct {
	talk   bool       // end the talk of the current owner
	holds  []*session // drop these holders
	sel    bool       // end the selectable hold
	noEdge bool       // talk ends without the 0 -> 1 edge (the hold is released next: SUMMON)
}

// applyLocked changes the in-memory state and queues the car side in the
// §4.9 order: requestAudio 0 -> sessionActive 0 (only when no holder is left;
// an edge instead when talk ended and others still hold) -> selectable 0 ->
// gain -200 (only with our marker). Each 0 is confirmed by a readback before
// its marker goes (qClear). It returns the children to stop; the caller waits
// for s.car.barrier() first, then stops them, then removes temp files.
func (s *Server) applyLocked(c change) []Child {
	var stop []Child
	talkEnded := c.talk && s.talk.owner != nil
	if talkEnded {
		s.talk.owner = nil // the clears below see the new state
		s.qClear(markRequestAudio)
	}
	dropped := false
	for _, h := range c.holds {
		if s.holders[h] {
			delete(s.holders, h)
			dropped = true
		}
	}
	if dropped || talkEnded {
		if len(s.holders) == 0 {
			s.qClear(markSessionAct)
		} else if talkEnded && !c.noEdge {
			s.qWrite(SessionActive0) // others still hold: a falling edge un-routes the exterior stream
			s.qSleep(s.cfg.EdgeDip)
			s.qWrite(SessionActive1)
		}
	}
	if c.sel && s.selOwner != nil {
		s.log.Printf("selectable hold %d released", s.selVal)
		owner := s.selOwner
		s.selOwner, s.selVal = nil, 0
		s.qClear(markSelectable)
		stop = append(stop, s.unsubscribeLocked(owner, SelectablePort)...)
	}
	if talkEnded {
		if s.mk.has(markGain) {
			s.qClear(markGain)
		}
		if w := s.stopWriterLocked(); w != nil {
			stop = append(stop, w)
		}
		s.talk = talkState{}
	}
	return stop
}

// finish waits for the queued car operations, then stops the children.
func (s *Server) finish(stop []Child) {
	b := s.car.barrier()
	s.mu.Unlock()
	<-b
	stopAll(stop)
}

// ---- requests: hold, release, talk, gain, sel, status ----

// arbiter handles the camera-state ops. It returns the session's status and
// an error code ("" = ok).
func (s *Server) arbiter(sess *session, req proto.Request) (*proto.Status, string) {
	status := func() *proto.Status {
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.statusLocked(sess)
		return &st
	}
	switch req.Op {
	case proto.OpStatus:
		rd := s.read()
		s.mu.Lock()
		s.rd = rd
		s.mu.Unlock()
		return status(), ""

	case proto.OpHold:
		rd := s.read()
		summonOK, _ := s.cfg.SummonOK()
		s.mu.Lock()
		s.rd = rd
		if !s.holders[sess] {
			// SUMMON mode waives the gates, as Tesla's webcam handler skips
			// checkWebcamAllowed for a summon session; the live file is checked
			// again here, not only by the watcher.
			if g := gateOf(rd); g != "" && !(s.summon == sess && summonOK) {
				st := s.statusLocked(sess)
				s.mu.Unlock()
				return &st, g
			}
			if err := s.mk.set(markSessionAct); err != nil { // M1: never hold what the sweep could not undo
				s.mu.Unlock()
				s.log.Printf("marker %s: %v (hold refused)", markSessionAct, err)
				return status(), proto.ErrInternal
			}
			s.holders[sess] = true
			if len(s.holders) == 1 {
				s.qWrite(SessionActive1)
			}
		}
		s.finish(nil)
		return status(), ""

	case proto.OpRelease:
		s.mu.Lock()
		var stop []Child
		if s.holders[sess] {
			stop = s.applyLocked(change{talk: s.talk.owner == sess, holds: []*session{sess}})
		}
		s.finish(stop)
		return status(), ""

	case proto.OpTalkArm:
		return s.talkArm(sess)

	case proto.OpTalkDisarm:
		s.mu.Lock()
		var stop []Child
		if s.talk.owner == sess {
			stop = s.applyLocked(change{talk: true, noEdge: req.NoEdge})
		}
		s.finish(stop)
		return status(), ""

	case proto.OpGain:
		s.mu.Lock()
		if s.talk.owner != sess {
			s.mu.Unlock()
			return status(), proto.ErrNotArmed
		}
		if req.Boost {
			if err := s.mk.set(markGain); err != nil { // dirty BEFORE the write; refuse if impossible
				s.mu.Unlock()
				s.log.Printf("marker %s: %v (boost refused)", markGain, err)
				return status(), proto.ErrInternal
			}
			s.qGain(GainBoost)
			s.talk.boost = true
			g := GainBoost.DB()
			s.talk.gain = &g
		} else {
			s.talk.boost = false
			if s.mk.has(markGain) {
				s.qGain(GainNative)
				g := GainNative.DB()
				s.talk.gain = &g
			}
		}
		s.finish(nil)
		return status(), ""

	case proto.OpSel:
		return s.sel(sess, req.Sel)

	case proto.OpSummon:
		return s.summonOp(sess, req.On)
	}
	return nil, proto.ErrBadRequest
}

// summonOp turns this session's SUMMON mode on (only while the car's Summon
// session checks out, and for one session at a time) or off. On ends the
// session's talk first: talk never runs during SUMMON. Off releases the
// session's selectable, whatever it holds: "back to 0 when SUMMON closes"
// (design §6.5); the session re-acquires its parked default itself.
func (s *Server) summonOp(sess *session, on bool) (*proto.Status, string) {
	var ok bool
	var who string
	if on {
		ok, who = s.cfg.SummonOK()
	}
	s.mu.Lock()
	refuse := func(code string) (*proto.Status, string) {
		st := s.statusLocked(sess)
		s.mu.Unlock()
		return &st, code
	}
	var stop []Child
	var notes []note
	switch {
	case on && s.summon == sess:
	case on && s.summon != nil:
		return refuse(proto.ErrBusy)
	case on && !ok:
		return refuse(proto.ErrNoSummon)
	case on:
		if s.talk.owner == sess {
			notes = append(notes, note{sess, proto.Event{Ev: proto.EvTalkEnd, Reason: proto.ErrSummon}})
			stop = s.applyLocked(change{talk: true})
		}
		s.summon = sess
		s.log.Printf("SUMMON mode on (summonhelper pid %s): gear, Sentry and camera-access gates waived for this session (Tesla's summon webcam)", who)
	case s.summon == sess:
		s.summon = nil
		if s.selOwner == sess {
			stop = s.applyLocked(change{sel: true})
		}
		s.log.Printf("SUMMON mode off: the normal gates again")
	}
	st := s.statusLocked(sess)
	s.finish(stop)
	for _, n := range notes {
		n.sess.event(n.ev)
	}
	return &st, ""
}

// endSummonLocked ends sess's SUMMON mode because the live file is gone:
// summon_end, and its selectable released (sel_end, cap_end for 9895). It
// returns the events to send once s.mu is released and the children to stop
// after the car barrier.
func (s *Server) endSummonLocked(sess *session) ([]note, []Child) {
	s.summon = nil
	s.log.Printf("SUMMON mode ended: the car's Summon session is over (live file)")
	notes := []note{{sess, proto.Event{Ev: proto.EvSummonEnd, Reason: "live"}}}
	var stop []Child
	if s.selOwner == sess {
		notes = append(notes, note{sess, proto.Event{Ev: proto.EvSelEnd, Reason: proto.EvSummonEnd}},
			note{sess, proto.Event{Ev: proto.EvCapEnd, Port: SelectablePort, Reason: "sel_end"}})
		stop = s.applyLocked(change{sel: true})
	}
	return notes, stop
}

// summonWatch ends SUMMON mode within SummonPoll of the live file going,
// independently of the tick, whose car reads can take seconds (review
// live-3). It waits for the car barrier outside the lock, as finish does.
func (s *Server) summonWatch() {
	defer close(s.watchDone)
	t := time.NewTicker(s.cfg.SummonPoll)
	defer t.Stop()
	for {
		select {
		case <-s.tickStop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		sess := s.summon
		s.mu.Unlock()
		if sess == nil {
			continue
		}
		if ok, _ := s.cfg.SummonOK(); ok {
			continue
		}
		s.mu.Lock()
		if s.closing || s.summon != sess {
			s.mu.Unlock()
			continue
		}
		notes, stop := s.endSummonLocked(sess)
		b := s.car.barrier()
		s.mu.Unlock()
		for _, n := range notes {
			n.sess.event(n.ev)
		}
		<-b
		stopAll(stop)
	}
}

func (s *Server) talkArm(sess *session) (*proto.Status, string) {
	rd := s.read()
	links, lerr := s.cfg.Car.Links()
	s.mu.Lock()
	s.rd = rd
	refuse := func(code string) (*proto.Status, string) {
		st := s.statusLocked(sess)
		s.mu.Unlock()
		return &st, code
	}
	switch {
	case !s.holders[sess]:
		return refuse(proto.ErrNotHeld)
	case s.summon == sess: // talk never runs during SUMMON (review live-4)
		return refuse(proto.ErrSummon)
	case s.talk.owner == sess:
		st := s.statusLocked(sess)
		s.mu.Unlock()
		return &st, ""
	case s.talk.owner != nil:
		return refuse(proto.ErrBusy)
	}
	if lerr != nil {
		s.log.Printf("pw-link: %v (exterior check skipped)", lerr)
		links = ""
	}
	if g := talkGate(rd); g != "" {
		return refuse(g)
	}
	if _, foreign := routeState(links); foreign {
		return refuse(proto.ErrExterior) // a Boombox or the megaphone owns the exterior speaker
	}
	for _, m := range []string{markRequestAudio, markSessionAct} {
		if err := s.mk.set(m); err != nil {
			s.log.Printf("marker %s: %v (talk refused)", m, err)
			return refuse(proto.ErrInternal)
		}
	}
	s.talk = talkState{owner: sess, armedAt: time.Now(), restartAt: time.Now().Add(time.Second)}
	s.qEdge()
	b := s.car.barrier()
	s.mu.Unlock()
	<-b // attach the writer right after the edge: an idle route is reaped in ~4 s

	s.mu.Lock()
	if s.talk.owner != sess { // a gate ended it during the edge
		return refuse(proto.ErrGateGear)
	}
	if err := s.startWriterLocked(); err != nil {
		s.log.Printf("talk writer: %v", err)
		stop := s.applyLocked(change{talk: true})
		st := s.statusLocked(sess)
		s.finish(stop)
		return &st, proto.ErrSpawn
	}
	st := s.statusLocked(sess)
	s.mu.Unlock()
	return &st, ""
}

func (s *Server) sel(sess *session, v int) (*proto.Status, string) {
	if v != 0 && v != 1 && v != 3 && !summonOnlySel(v) {
		return nil, proto.ErrBadRequest
	}
	if v == 0 {
		s.mu.Lock()
		var stop []Child
		if s.selOwner == sess {
			stop = s.applyLocked(change{sel: true})
		}
		st := s.statusLocked(sess)
		s.finish(stop)
		return &st, ""
	}
	rd := s.read()
	summonOK, _ := s.cfg.SummonOK()
	s.mu.Lock()
	s.rd = rd
	refuse := func(code string) (*proto.Status, string) {
		st := s.statusLocked(sess)
		s.mu.Unlock()
		return &st, code
	}
	// SUMMON mode copies Tesla's summon webcam: exactly its camera for the
	// gear (summonSel: Grid 11, Backup 9 in R, the front fisheye 3 in D), in
	// any gear; any of the three while the gear is unknown. Otherwise parked
	// only, and never 9 or 11.
	summon := s.summon == sess && v != 1 && summonOK
	if want, known := summonSel(rd.gear); summon && known && v != want {
		return refuse(proto.ErrSelRefused)
	}
	switch {
	case summonOnlySel(v) && !summon:
		return refuse(proto.ErrSelRefused)
	case rd.gear != "P" && !summon:
		return refuse(proto.ErrSelRefused)
	case s.selOwner != nil && s.selOwner != sess:
		return refuse(proto.ErrBusy)
	}
	if s.selOwner != sess || s.selVal != v {
		if err := s.mk.set(markSelectable); err != nil {
			s.log.Printf("marker %s: %v (selectable refused)", markSelectable, err)
			return refuse(proto.ErrInternal)
		}
		s.qWrite(selWrite(v))
		s.log.Printf("selectable hold %d (diverts the dashcam wide recording)", v)
	}
	s.selOwner, s.selVal = sess, v
	st := s.statusLocked(sess)
	s.finish(nil)
	return &st, ""
}

func selWrite(v int) DVWrite {
	switch v {
	case 3:
		return Selectable3
	case 9:
		return Selectable9
	case 11:
		return Selectable11
	}
	return Selectable1
}

func (s *Server) statusLocked(sess *session) proto.Status {
	st := proto.Status{
		Gear: s.rd.gear, Driving: driving(s.rd.gear), Sentry: s.rd.sentry, Climate: s.rd.climate,
		CamAccess: s.rd.camAccess, Gate: gateOf(s.rd), Held: s.holders[sess], Route: "off",
		Summon: s.summon == sess,
	}
	if st.Summon {
		st.Gate = "" // waived
	}
	if s.talk.owner == sess {
		st.Talk, st.Boost, st.Route = true, s.talk.boost, "down"
		if s.talk.routeOK {
			st.Route = "ok"
		}
		if s.talk.gain != nil {
			g := *s.talk.gain
			st.GainDB = &g
		}
	}
	if s.selOwner == sess {
		st.Sel = s.selVal
	}
	return st
}

// ---- talk writer and PCM ----

// startWriterLocked attaches the fixed talk writer and its PCM pump.
func (s *Server) startWriterLocked() error {
	c, err := s.startChildLocked(ChildSpec{Argv: writerArgv[s.cfg.Writer], Env: writerEnv, Stdin: true})
	if err != nil {
		return err
	}
	pcm := make(chan []byte, 64) // ~1.3 s of 20 ms chunks
	stop := make(chan struct{})
	s.talk.writer, s.talk.pumpStop = c, stop
	s.pcmMu.Lock()
	s.pcmOwner, s.pcmCh = s.talk.owner, pcm
	s.pcmMu.Unlock()
	go func() {
		in := c.Stdin()
		for {
			select {
			case <-stop:
				return
			case b := <-pcm:
				if _, err := in.Write(b); err != nil {
					<-stop // the writer died: the tick loop restarts it
					return
				}
			}
		}
	}()
	return nil
}

// stopWriterLocked detaches the writer (if any) and returns it to be stopped.
func (s *Server) stopWriterLocked() Child {
	s.pcmMu.Lock()
	s.pcmOwner, s.pcmCh = nil, nil
	s.pcmMu.Unlock()
	w := s.talk.writer
	if w != nil {
		close(s.talk.pumpStop)
		s.talk.writer, s.talk.pumpStop = nil, nil
	}
	return w
}

// pcm hands a PCM chunk to the talk writer of sess (dropped otherwise). It
// takes only pcmMu: a slow car call never stalls PCM.
func (s *Server) pcm(sess *session, b []byte) {
	s.pcmMu.Lock()
	defer s.pcmMu.Unlock()
	if s.pcmOwner != sess || s.pcmCh == nil {
		return
	}
	select {
	case s.pcmCh <- b:
	default: // the writer is not keeping up: drop
	}
}

// ---- the 1 Hz loop ----

func (s *Server) tickLoop() {
	defer close(s.tickDone)
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-s.tickStop:
			return
		case <-t.C:
			s.tick()
		}
	}
}

func (s *Server) busyLocked() bool {
	return len(s.holders) > 0 || s.talk.owner != nil || s.selOwner != nil || s.summon != nil
}

func edgeBackoff(n int) time.Duration {
	switch {
	case n <= 1:
		return 3 * time.Second
	case n == 2:
		return 5 * time.Second
	case n == 3:
		return 10 * time.Second
	case n == 4:
		return 20 * time.Second
	}
	return 30 * time.Second
}

func (s *Server) tick() {
	s.mu.Lock()
	busy := s.busyLocked()
	if !busy {
		s.gate = gateCounters{}
	}
	s.orphansLocked(time.Now()) // H1: retry the clears nobody owns any more, with backoff
	talk := s.talk.owner != nil
	inSummon := s.summon // the mode this tick judges; one granted during the reads below was checked at its grant
	s.mu.Unlock()
	if !busy {
		return
	}
	rd := s.read()
	summonOK := false
	if inSummon != nil {
		summonOK, _ = s.cfg.SummonOK()
	}
	links, linkErr := "", error(nil)
	if talk {
		links, linkErr = s.cfg.Car.Links()
	}

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.rd = rd
	var stop []Child
	var notes []note
	notify := func(sess *session, ev, reason string) {
		if sess != nil {
			notes = append(notes, note{sess, proto.Event{Ev: ev, Reason: reason}})
		}
	}
	endSel := func(reason string) {
		if s.selOwner == nil {
			return
		}
		notify(s.selOwner, proto.EvSelEnd, reason)
		notes = append(notes, note{s.selOwner, proto.Event{Ev: proto.EvCapEnd, Port: SelectablePort, Reason: "sel_end"}})
		stop = append(stop, s.applyLocked(change{sel: true})...)
	}

	// SUMMON mode lasts only while the car's Summon session does: once
	// summonhelper's live file is gone (or cannot be checked) the mode ends,
	// its selectable is released, and the normal gates below apply again in
	// this same tick. Only the mode this tick checked is judged: a session
	// granted it while the car was being read passed the check at its grant.
	if s.summon != nil && s.summon == inSummon && !summonOK {
		n, st := s.endSummonLocked(s.summon)
		notes, stop = append(notes, n...), append(stop, st...)
	}
	summon := s.summon

	g := &s.gate
	switch {
	case rd.gear == "P":
		g.nonP, g.unknown = 0, 0
	case driving(rd.gear):
		g.nonP++
		g.unknown = 0
	default:
		g.unknown++
	}
	if sentryBlocks(rd) {
		g.sentry++
	} else {
		g.sentry = 0
	}
	if rd.camAccess != "" && rd.camAccess != "true" {
		g.camera++
	} else {
		g.camera = 0
	}
	// Never fight checkWebcamAllowed: talk and the selectable stop at the
	// first non-P reading (and after 3 unknown ones), holds at the second; a
	// Sentry or camera-access failure seen twice ends everything. SUMMON mode
	// is the exception, as Tesla skips checkWebcamAllowed for a summon
	// session, but only for that session's own hold and selectable: talk
	// keeps its strict gate, and every other session's hold and selectable end
	// as usual. The counters keep counting, so they apply at once when the
	// mode ends.
	waived := func(sess *session) bool { return summon != nil && sess == summon }
	endHold := ""
	switch {
	case g.nonP >= 2:
		endHold = proto.ErrGateGear
	case g.sentry >= 2:
		endHold = proto.ErrGateSentry
	case g.camera >= 2:
		endHold = proto.ErrGateCamera
	}
	if endHold != "" {
		var hs []*session
		for h := range s.holders {
			if !waived(h) {
				hs = append(hs, h)
			}
		}
		talkEnd := s.talk.owner != nil
		if talkEnd {
			notify(s.talk.owner, proto.EvTalkEnd, endHold)
		}
		if !waived(s.selOwner) {
			endSel(endHold)
		}
		if talkEnd || len(hs) > 0 {
			stop = append(stop, s.applyLocked(change{talk: talkEnd, holds: hs})...)
		}
		for _, h := range hs {
			notify(h, proto.EvHoldEnd, endHold)
		}
	}
	if s.talk.owner != nil && (g.nonP >= 1 || g.unknown >= 3) {
		notify(s.talk.owner, proto.EvTalkEnd, proto.ErrGateGear)
		stop = append(stop, s.applyLocked(change{talk: true})...)
	}
	if (g.nonP >= 1 || g.unknown >= 3) && !waived(s.selOwner) {
		endSel(proto.ErrSelRefused)
	}

	// SUMMON mode holds only Tesla's camera for the gear. CAMERA follows a
	// gear change within one status event, so a summon selectable that still
	// does not match on two consecutive ticks is ended (review live-5).
	if want, known := summonSel(rd.gear); summon != nil && s.selOwner == summon && known && s.selVal != want {
		g.summonSel++
	} else {
		g.summonSel = 0
	}
	if g.summonSel >= 2 {
		g.summonSel = 0
		endSel(proto.ErrSelRefused)
	}

	// Re-assert what is held, requestAudio before sessionActive; not while a
	// non-P gear was just seen (the firmware is clearing it), except in SUMMON
	// mode (Tesla holds the webcam session through the summon). The tick waits
	// for its car operations before the next one, so slow car tools slow the
	// loop down instead of piling writes up.
	if s.talk.owner != nil {
		s.qWrite(RequestAudio1)
	}
	if len(s.holders) > 0 && (g.nonP == 0 || summon != nil && s.holders[summon]) {
		s.qWrite(SessionActive1)
	}
	if s.selOwner != nil {
		s.qWrite(selWrite(s.selVal))
	}
	if s.talk.owner != nil && s.talk.boost {
		s.qGain(GainBoost)
	}

	// Talk route watchdog and writer supervision.
	if t := &s.talk; t.owner != nil {
		now := time.Now()
		if talk && linkErr == nil {
			ok, _ := routeState(links)
			switch {
			case ok:
				if !t.routeOK {
					notes = append(notes, note{t.owner, proto.Event{Ev: proto.EvRoute, Reason: "ok"}})
				}
				t.routeOK, t.missing, t.edges = true, 0, 0
			default:
				if t.routeOK {
					notes = append(notes, note{t.owner, proto.Event{Ev: proto.EvRoute, Reason: "down"}})
				}
				t.routeOK = false
				if now.Sub(t.armedAt) >= s.cfg.RouteGrace {
					t.missing++
					if t.missing >= 2 && !now.Before(t.nextEdge) {
						s.log.Printf("talk route lost: re-firing the edge (attempt %d)", t.edges+1)
						s.qEdge()
						t.armedAt, t.missing = now, 0
						t.edges++
						t.nextEdge = now.Add(edgeBackoff(t.edges))
					}
				}
			}
		}
		if t.writer != nil {
			select {
			case <-t.writer.Done():
				s.log.Printf("talk writer exited (%v)", t.writer.ExitErr())
				s.stopWriterLocked()
			default:
			}
		}
		if t.writer == nil && !now.Before(t.restartAt) { // M3: keep retrying, with backoff
			if err := s.startWriterLocked(); err != nil {
				t.restarts++
				t.restartAt = now.Add(s.cfg.Backoff(t.restarts))
				s.log.Printf("talk writer restart failed (%d/%d): %v", t.restarts, WriterRestarts, err)
				if t.restarts >= WriterRestarts {
					notify(t.owner, proto.EvTalkEnd, proto.ErrSpawn)
					stop = append(stop, s.applyLocked(change{talk: true})...)
				}
			} else {
				t.restarts = 0
				t.restartAt = now.Add(time.Second)
			}
		}
	}

	// Status to every session that holds or owns something.
	for sess := range s.sessions {
		if s.holders[sess] || s.talk.owner == sess || s.selOwner == sess {
			st := s.statusLocked(sess)
			notes = append(notes, note{sess, proto.Event{Ev: proto.EvStatus, Status: &st}})
		}
	}
	b := s.car.barrier()
	s.mu.Unlock()
	for _, n := range notes {
		n.sess.event(n.ev)
	}
	<-b
	stopAll(stop)
}
