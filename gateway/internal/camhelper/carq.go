package camhelper

import (
	"log"
	"sync"
	"time"
)

// carQueue runs car operations (DataValue writes, gain, readbacks, the edge's
// sleep) one at a time, in the order they were queued, on its own goroutine.
// The arbiter queues them while holding s.mu, so the car sees the writes in
// the order of the state changes, but s.mu is never held while a (possibly
// slow) car tool runs.
type carQueue struct {
	log *log.Logger

	mu      sync.Mutex
	cond    *sync.Cond
	q       []func()
	stopped bool
	idle    bool
}

func newCarQueue(lg *log.Logger) *carQueue {
	c := &carQueue{log: lg, idle: true}
	c.cond = sync.NewCond(&c.mu)
	go c.loop()
	return c
}

func (c *carQueue) do(f func()) {
	c.mu.Lock()
	if !c.stopped {
		c.q = append(c.q, f)
		c.idle = false
		c.cond.Broadcast()
	}
	c.mu.Unlock()
}

// barrier returns a channel closed once everything queued before it ran.
func (c *carQueue) barrier() <-chan struct{} {
	ch := make(chan struct{})
	c.mu.Lock()
	if c.stopped {
		close(ch)
	} else {
		c.q = append(c.q, func() { close(ch) })
		c.idle = false
		c.cond.Broadcast()
	}
	c.mu.Unlock()
	return ch
}

// pending is the number of queued operations.
func (c *carQueue) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.q)
}

func (c *carQueue) stop() {
	c.mu.Lock()
	c.stopped = true
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *carQueue) loop() {
	for {
		c.mu.Lock()
		for len(c.q) == 0 && !c.stopped {
			c.idle = true
			c.cond.Broadcast()
			c.cond.Wait()
		}
		if len(c.q) == 0 && c.stopped {
			c.mu.Unlock()
			return
		}
		f := c.q[0]
		c.q[0] = nil
		c.q = c.q[1:]
		c.mu.Unlock()
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.log.Printf("car op panic: %v", r)
				}
			}()
			f()
		}()
	}
}

// ---- queued operations (callers hold s.mu, which keeps the order) ----

func (s *Server) qWrite(w DVWrite) {
	s.car.do(func() {
		if err := s.cfg.Car.Write(w); err != nil {
			s.log.Printf("%s: %v", w, err)
		}
	})
}

func (s *Server) qGain(g Gain) {
	s.car.do(func() {
		if err := s.cfg.Car.SetGain(g); err != nil {
			s.log.Printf("pws-media-gain %d: %v", g.DB(), err)
		}
	})
}

func (s *Server) qSleep(d time.Duration) { s.car.do(func() { time.Sleep(d) }) }

// qEdge queues the talk-route edge: requestAudio 1 BEFORE a sessionActive
// 0 -> 1 edge. The markers must already exist.
func (s *Server) qEdge() {
	s.qWrite(RequestAudio1)
	s.qWrite(SessionActive0)
	s.qSleep(s.cfg.EdgeDip)
	s.qWrite(SessionActive1)
}

// latchRetry tracks an orphan marker (no owner) whose clear failed.
type latchRetry struct {
	attempts int
	next     time.Time
	queued   bool
}

// clearOps maps a marker to its zero write and readback.
var clearOps = map[string]struct {
	write DVWrite
	read  DVRead
}{
	markRequestAudio: {RequestAudio0, ReadRequestAudio},
	markSessionAct:   {SessionActive0, ReadSessionActive},
	markSelectable:   {Selectable0, ReadSelectable},
}

// qClear queues the release of what a marker records: the 0 write (gain -200
// for the gain marker) and, for a DataValue, a get-local-dv readback. The
// marker is removed only when both succeeded and nobody owns it again by
// then; otherwise it stays and the tick retries it with backoff (H1).
func (s *Server) qClear(name string) {
	r := s.latch[name]
	if r == nil {
		r = &latchRetry{}
		s.latch[name] = r
	}
	r.queued = true
	s.car.do(func() {
		ok := clearCar(s.cfg.Car, name, s.log)
		s.mu.Lock()
		defer s.mu.Unlock()
		r := s.latch[name]
		if r == nil {
			r = &latchRetry{}
			s.latch[name] = r
		}
		r.queued = false
		if s.ownedLocked(name) {
			return // re-acquired meanwhile: its own write follows; the marker stays
		}
		if ok {
			s.mk.clear(name)
			delete(s.latch, name)
			return
		}
		r.attempts++
		r.next = time.Now().Add(s.cfg.Backoff(r.attempts))
		s.log.Printf("LATCH NOT CLEARED: %s (attempt %d), the marker stays; retrying in %v",
			markerWhat(name), r.attempts, s.cfg.Backoff(r.attempts))
	})
}

// clearCar performs one marker's release on the car and reports success.
func clearCar(car Car, name string, lg *log.Logger) bool {
	if name == markGain {
		if err := car.SetGain(GainMute); err != nil {
			lg.Printf("pws-media-gain -200: %v", err)
			return false
		}
		return true
	}
	op, ok := clearOps[name]
	if !ok {
		return false
	}
	if err := car.Write(op.write); err != nil {
		lg.Printf("%s: %v", op.write, err)
		return false
	}
	raw, err := car.Read(op.read)
	if err != nil {
		lg.Printf("readback after %s: %v", op.write, err)
		return false
	}
	if !isCleared(raw) {
		lg.Printf("readback after %s: still %q", op.write, raw)
		return false
	}
	return true
}

func markerWhat(name string) string {
	switch name {
	case markRequestAudio:
		return "WEBCAM_webcamSessionRequestAudio"
	case markSessionAct:
		return "WEBCAM_webcamSessionActive"
	case markSelectable:
		return "GUI_selectableCameraRequest"
	case markGain:
		return "pws-media-gain"
	}
	return name
}

var latchMarkers = []string{markRequestAudio, markSessionAct, markSelectable, markGain}

// ownedLocked reports whether the in-memory state still owns what a marker
// records. Release decisions come from this state; markers drive only the
// crash sweep and the orphan retries.
func (s *Server) ownedLocked(name string) bool {
	switch name {
	case markRequestAudio, markGain:
		return s.talk.owner != nil
	case markSessionAct:
		return len(s.holders) > 0
	case markSelectable:
		return s.selOwner != nil
	}
	return false
}

// orphansLocked queues a clear for every marker nobody owns whose retry is
// due. The tick calls it every second, busy or not.
func (s *Server) orphansLocked(now time.Time) {
	for _, name := range latchMarkers {
		if !s.mk.has(name) || s.ownedLocked(name) {
			continue
		}
		r := s.latch[name]
		if r != nil && (r.queued || now.Before(r.next)) {
			continue
		}
		s.qClear(name)
	}
}
