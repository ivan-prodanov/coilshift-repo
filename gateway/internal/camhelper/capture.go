package camhelper

import (
	"sync"
	"time"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/annexb"
)

// capture is ONE capture child per port, shared by every session that
// subscribed to it: overlapping CAMERA windows (a late Teardown of an
// abandoned one and a new one) never double the gst children. Its access
// units are fanned out to every subscriber.
type capture struct {
	port  int
	child Child

	mu      sync.Mutex
	subs    map[*session]bool
	stopped bool // stopped by us (last subscriber gone): no cap_end event
}

func (c *capture) subscribers() []*session {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*session, 0, len(c.subs))
	for s := range c.subs {
		out = append(out, s)
	}
	return out
}

// capStart subscribes sess to port, starting the shared capture if needed.
func (s *Server) capStart(sess *session, port int) string {
	argv := captureArgv(port)
	if argv == nil {
		return proto.ErrPort
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if port == SelectablePort && s.selOwner != sess {
		return proto.ErrPort // 9895 only during this session's selectable hold
	}
	if sess.caps[port] {
		return ""
	}
	if len(sess.caps) >= MaxCapturesPerSession {
		return proto.ErrBusy
	}
	c := s.caps[port]
	if c == nil {
		child, err := s.startChildLocked(ChildSpec{Argv: argv, Env: gstEnv, Stdout: true})
		if err == errTooManyChildren {
			return proto.ErrBusy
		}
		if err != nil {
			s.log.Printf("capture %d: %v", port, err)
			return proto.ErrSpawn
		}
		c = &capture{port: port, child: child, subs: map[*session]bool{}}
		s.caps[port] = c
		go s.runCapture(c)
	}
	c.mu.Lock()
	c.subs[sess] = true
	c.mu.Unlock()
	sess.caps[port] = true
	return ""
}

// capStop unsubscribes sess; the last subscriber stops the child.
func (s *Server) capStop(sess *session, port int) {
	s.mu.Lock()
	stop := s.unsubscribeLocked(sess, port)
	s.mu.Unlock()
	stopAll(stop)
}

// unsubscribeLocked removes sess from port's capture and returns the child
// to stop when nobody is left.
func (s *Server) unsubscribeLocked(sess *session, port int) []Child {
	if !sess.caps[port] {
		return nil
	}
	delete(sess.caps, port)
	c := s.caps[port]
	if c == nil {
		return nil
	}
	c.mu.Lock()
	delete(c.subs, sess)
	last := len(c.subs) == 0
	if last {
		c.stopped = true
	}
	c.mu.Unlock()
	if !last {
		return nil
	}
	delete(s.caps, port)
	return []Child{c.child}
}

func (s *Server) unsubscribeAllLocked(sess *session) []Child {
	var stop []Child
	for port := range sess.caps {
		stop = append(stop, s.unsubscribeLocked(sess, port)...)
	}
	return stop
}

// runCapture splits the child's Annex-B output into access units and fans
// each one out to the current subscribers.
func (s *Server) runCapture(c *capture) {
	sp := annexb.NewSplitter(proto.MaxAU)
	emit := func(au annexb.AU) {
		f := encodeAU(uint16(c.port), au, s.epoch)
		for _, sess := range c.subscribers() {
			sess.out.pushAU(uint16(c.port), au.IDR, au.AfterDrop, f)
		}
	}
	buf := make([]byte, 64<<10)
	out := c.child.Stdout()
	for {
		n, err := out.Read(buf)
		if n > 0 {
			sp.Write(buf[:n], time.Now(), emit)
		}
		if err != nil {
			break
		}
	}
	sp.Flush(emit)
	select {
	case <-c.child.Done():
	case <-time.After(5 * time.Second):
		c.child.Stop()
	}
	s.mu.Lock()
	if s.caps[c.port] == c {
		delete(s.caps, c.port)
	}
	c.mu.Lock()
	stopped := c.stopped
	subs := make([]*session, 0, len(c.subs))
	for sess := range c.subs {
		subs = append(subs, sess)
		delete(sess.caps, c.port)
	}
	c.subs = map[*session]bool{}
	c.mu.Unlock()
	s.mu.Unlock()
	if !stopped {
		s.log.Printf("capture %d ended: %v", c.port, c.child.ExitErr())
		for _, sess := range subs {
			sess.event(proto.Event{Ev: proto.EvCapEnd, Port: c.port, Reason: "exit"})
		}
	}
}

// encodeAU builds one complete AU frame (length prefix included). The frame
// is shared read-only by every subscriber's queue.
func encodeAU(port uint16, au annexb.AU, epoch time.Time) []byte {
	n := 5 + proto.AUHeader + len(au.Data)
	var flags byte
	if au.IDR {
		flags |= proto.FlagIDR
	}
	if au.ParamSets {
		flags |= proto.FlagParamSets
	}
	f := make([]byte, n)
	f[0], f[1], f[2], f[3] = byte((n-4)>>24), byte((n-4)>>16), byte((n-4)>>8), byte(n-4)
	f[4] = proto.TAU
	us := au.Time.Sub(epoch).Microseconds()
	if us < 0 {
		us = 0
	}
	proto.PutAUHeader(f[5:], port, flags, uint64(us))
	copy(f[5+proto.AUHeader:], au.Data)
	return f
}
