package server

import (
	"log"
	"time"

	"airgapp/gateway/internal/authority"
)

// Open windows are re-checked with authority.Check, the sign-in's own check:
// at once on keys_changed, every 30 s while the helper's key-change watch is
// up, and every 10 s while it is down (changes cannot be pushed then). A
// failed or unreachable check revokes the window (BYE "revoked"; the session
// log keeps why).
//
// keys_changed passes start at least recheckKickGap apart: one held back by
// the gap runs at the first tick after it and covers every change before it
// starts. So a burst of notices costs the helper's rate limit, which the
// sign-ins share, one key request per open key a second, not one per notice.
const (
	recheckEvery     = 30 * time.Second
	recheckDownEvery = 10 * time.Second
	recheckTickEvery = time.Second // how often the loop looks whether a re-check is due
	recheckKickGap   = time.Second // between the starts of keys_changed passes
)

// KeysChanged re-checks every open window (the sign-in helper's
// keys_changed; reason is its reason): at once, or within about a second
// after the last keys_changed pass. It never blocks: triggers that arrive
// while a re-check is pending are folded into it.
func (s *Server) KeysChanged(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keysGen++
	s.kickLocked("keys_changed " + reason)
}

// SetWatchUp records whether the helper's key-change watch is live. While it
// is not (watch_down, or the watch connection lost), open windows are
// re-checked every 10 s instead of every 30 s. It never blocks.
func (s *Server) SetWatchUp(up bool) {
	s.mu.Lock()
	s.watchUp = up
	s.mu.Unlock()
}

// kickLocked asks the loop for a re-check of every window, unless one is
// already pending. Caller holds s.mu.
func (s *Server) kickLocked(trigger string) {
	select {
	case s.kick <- trigger:
	default:
	}
}

// startRecheck starts the loop once, unless the server is shutting down.
func (s *Server) startRecheck() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.recheckStarted {
		return
	}
	s.recheckStarted = true
	tick := s.recheckTick
	var t *time.Ticker
	if tick == nil {
		t = time.NewTicker(recheckTickEvery)
		tick = t.C
	}
	s.wg.Add(1) // Shutdown waits for it with the connections
	go func() {
		defer s.wg.Done()
		if t != nil {
			defer t.Stop()
		}
		s.recheckLoop(tick)
	}()
}

func (s *Server) clockNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// recheckLoop runs re-checks one at a time until Shutdown: on a kick (at
// once, or at the first tick recheckKickGap after the last pass started),
// and on a tick once the period has passed since the last pass. With no
// window open there is nothing to check and the period restarts, so a new
// window's first periodic re-check comes one period after it opened.
func (s *Server) recheckLoop(tick <-chan time.Time) {
	last := s.clockNow()   // the last pass, or when the period restarted
	var lastPass time.Time // when the last pass started; zero before the first
	pending := ""          // a kick's trigger whose pass has not started
	for {
		select {
		case <-s.ctx.Done():
			return
		case k := <-s.kick:
			if pending == "" {
				pending = k
			}
		case <-tick:
		}
		now := s.clockNow()
		s.mu.Lock()
		open, up := len(s.windows) > 0, s.watchUp
		s.mu.Unlock()
		if !open {
			last, pending = now, ""
			continue
		}
		every, why := recheckEvery, "every 30 s"
		if !up {
			every, why = recheckDownEvery, "every 10 s, watch down"
		}
		var trigger string
		switch {
		case pending != "" && (lastPass.IsZero() || now.Sub(lastPass) >= recheckKickGap):
			trigger = pending
		case now.Sub(last) >= every:
			trigger = why
		default:
			continue
		}
		pending = "" // this pass covers every change before it starts
		last, lastPass = now, now
		s.recheckAll(trigger)
	}
}

// recheckAll checks every open window's key, each key once, sequentially
// (the sign-in helper answers one request at a time anyway), and revokes the
// windows whose key fails. A check cut short by Shutdown is not a verdict.
func (s *Server) recheckAll(trigger string) {
	s.mu.Lock()
	ws := make([]*window, 0, len(s.windows))
	for _, w := range s.windows {
		ws = append(ws, w)
	}
	s.mu.Unlock()
	verdict := map[string]error{}
	for _, w := range ws {
		if w.isClosed() {
			continue
		}
		err, done := verdict[string(w.pk)]
		if !done {
			err = authority.Check(s.ctx, s.cfg.RecheckOracle, w.pk)
			if s.ctx.Err() != nil {
				return
			}
			verdict[string(w.pk)] = err
		}
		if err != nil {
			why := "authority re-check (" + trigger + "): " + err.Error() // authority errors carry no key material
			log.Printf("agw: revoking %s fp=%s: %s", w.spec.Name, w.fp, why)
			w.revoke(why)
		}
	}
}
