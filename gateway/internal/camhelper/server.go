// Package camhelper is the camera root helper: the only process that writes
// the camera DataValues, sets the exterior-speaker gain and spawns the
// capture, talk-writer and cut children. It serves the gateway uid over a
// unix socket (camhelper/proto); each connection is a session whose
// resources are released, in the PLAN §4.9 order, when it closes.
//
// It has a compile-time allowlist and nothing else: the DV writes in car.go,
// three gain values, one fixed capture pipeline per allowlisted port, one of
// two fixed talk writers, one fixed ffmpeg stream copy. No shell, no generic
// setter, no kill by name.
//
// Locking: s.mu guards the arbiter state (holders, talk, selectable, gates),
// sessions, captures, children and cut accounting. It is never held across
// car I/O: every DataValue/gain write goes through one ordered car queue
// (carq.go) whose goroutine runs the tools. PCM has its own lock (pcmMu).
package camhelper

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"airgapp/gateway/camhelper/proto"
)

// Config configures a Server.
type Config struct {
	Car      Car
	Spawner  Spawner
	StateDir string // root-only markers; the cut subdir is owned by the child uid
	TeslaCam string // default /mnt/usb/TeslaCam
	AllowUID int    // the only peer uid accepted (4950 on the car)
	Writer   WriterKind
	Log      *log.Logger

	// Timing (zero = car defaults). Tests shrink them.
	Tick       time.Duration                   // gate checks, re-asserts, route watchdog, latch retries: 1 s
	EdgeDip    time.Duration                   // sessionActive 0 -> 1 dip: 400 ms
	RouteGrace time.Duration                   // after an edge, before the watchdog judges: 3 s
	CutTimeout time.Duration                   // ffmpeg bound: 30 s
	Backoff    func(attempt int) time.Duration // latch-clear and writer-restart retries

	// SUMMON mode: SummonOK reports a live Summon session on the car, and
	// the pid summonhelper wrote (for the log). The default checks the live
	// file at SummonLive (default DefaultSummonLive), owned by SummonUID
	// (default DefaultSummonUID) and locked by summonhelper; tests inject
	// their own.
	SummonLive string
	SummonUID  int
	SummonOK   func() (bool, string)
	SummonPoll time.Duration // the SUMMON-mode watcher: 200 ms

	// Shutdown's sweep (zero = /proc and syscall.Kill). Tests point them at
	// fakes, so a test never signals a real process.
	SweepProcDir string
	SweepKill    func(pid int, sig syscall.Signal) error

	PeerUID     func(net.Conn) (int, error)     // default PeerUID
	MaxSessions int                             // default 4
	MaxCuts     int                             // per session, default 4
	CutBudget   int64                           // all cuts, bytes, default 128 MiB
	FreeBytes   func(dir string) (int64, error) // default statfs
}

// Limits.
const (
	// MaxCapturesPerSession bounds a session's capture subscriptions. CAMERA
	// keeps the five direct views and the bumper camera 9901 warm while parked
	// with its hold, plus 9895 during a selectable hold (front's 3 by
	// default): 7.
	MaxCapturesPerSession = 8
	// MaxChildren bounds every child of the helper together: captures are
	// shared between sessions (at most 8 ports), plus the talk writer and one
	// cut.
	MaxChildren = 10
	// WriterRestarts is how many failed talk-writer restarts end talk.
	WriterRestarts = 5
)

var errTooManyChildren = errors.New("camhelper: child limit reached")

// Server is the helper.
type Server struct {
	cfg   Config
	log   *log.Logger
	mk    markers
	epoch time.Time
	car   *carQueue

	mu       sync.Mutex
	closing  bool
	ln       net.Listener
	sessions map[*session]struct{}
	holders  map[*session]bool
	talk     talkState
	selOwner *session
	selVal   int
	summon   *session // the session in SUMMON mode (at most one)
	rd       readings
	gate     gateCounters
	children map[int][]string
	caps     map[int]*capture // shared captures by port
	cutBytes int64
	latch    map[string]*latchRetry // orphan markers being cleared

	pcmMu    sync.Mutex
	pcmOwner *session
	pcmCh    chan []byte

	tickStop  chan struct{}
	tickDone  chan struct{}
	watchDone chan struct{} // summonWatch ended
	wg        sync.WaitGroup
}

// New builds a server; call Serve.
func New(cfg Config) *Server {
	if cfg.TeslaCam == "" {
		cfg.TeslaCam = "/mnt/usb/TeslaCam"
	}
	if cfg.Writer == "" {
		cfg.Writer = WriterPWCat
	}
	if cfg.Log == nil {
		cfg.Log = log.New(os.Stderr, "camhelper: ", log.LstdFlags)
	}
	set := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	set(&cfg.Tick, time.Second)
	set(&cfg.EdgeDip, 400*time.Millisecond)
	set(&cfg.RouteGrace, 3*time.Second)
	set(&cfg.CutTimeout, 30*time.Second)
	set(&cfg.SummonPoll, 200*time.Millisecond)
	if cfg.Backoff == nil {
		cfg.Backoff = backoff
	}
	if cfg.PeerUID == nil {
		cfg.PeerUID = PeerUID
	}
	if cfg.SummonLive == "" {
		cfg.SummonLive = DefaultSummonLive
	}
	if cfg.SummonUID == 0 {
		cfg.SummonUID = DefaultSummonUID
	}
	if cfg.SummonOK == nil {
		live, uid := cfg.SummonLive, cfg.SummonUID
		cfg.SummonOK = func() (bool, string) { return summonLive(live, uid) }
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 4
	}
	if cfg.MaxCuts == 0 {
		cfg.MaxCuts = 4
	}
	if cfg.CutBudget == 0 {
		cfg.CutBudget = 128 << 20
	}
	if cfg.FreeBytes == nil {
		cfg.FreeBytes = freeBytes
	}
	if _, ok := writerArgv[cfg.Writer]; !ok {
		panic("camhelper: unknown writer " + string(cfg.Writer))
	}
	s := &Server{
		cfg: cfg, log: cfg.Log, mk: markers{cfg.StateDir}, epoch: time.Now(),
		sessions: map[*session]struct{}{}, holders: map[*session]bool{},
		children: map[int][]string{}, caps: map[int]*capture{}, latch: map[string]*latchRetry{},
		tickStop: make(chan struct{}), tickDone: make(chan struct{}), watchDone: make(chan struct{}),
	}
	s.car = newCarQueue(s.log)
	return s
}

// backoff is 1, 2, 4, 8, 16, then 30 s.
func backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > 5 {
		return 30 * time.Second
	}
	return time.Second << (n - 1)
}

// Serve accepts sessions on ln until Shutdown. It also runs the 1 Hz loop.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.ln = ln
	s.mu.Unlock()
	go s.tickLoop()
	go s.summonWatch()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		uid, err := s.cfg.PeerUID(c)
		if err != nil || uid != s.cfg.AllowUID {
			s.log.Printf("refused peer uid=%d err=%v", uid, err)
			c.Close()
			continue
		}
		s.mu.Lock()
		if s.closing || len(s.sessions) >= s.cfg.MaxSessions {
			s.mu.Unlock()
			s.log.Printf("refused session: closing or %d sessions open", s.cfg.MaxSessions)
			c.Close()
			continue
		}
		sess := newSession(s, c)
		s.sessions[sess] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go sess.run()
	}
}

// Shutdown (SIGTERM) closes every session, which releases everything in the
// §4.9 order, waits for the car queue, then sweeps any marker left. It
// returns when done.
func (s *Server) Shutdown() {
	s.mu.Lock()
	already := s.closing
	s.closing = true
	ln := s.ln
	var all []*session
	for sess := range s.sessions {
		all = append(all, sess)
	}
	s.mu.Unlock()
	if already {
		return
	}
	if ln != nil {
		ln.Close()
		close(s.tickStop)
		<-s.tickDone
		<-s.watchDone
	}
	for _, sess := range all {
		sess.conn.Close()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		s.log.Printf("shutdown: sessions did not finish releasing; sweeping")
	}
	select {
	case <-s.car.barrier():
	case <-time.After(60 * time.Second):
		s.log.Printf("shutdown: car queue still busy; sweeping")
	}
	s.car.stop()
	Sweep(SweepConfig{Car: s.cfg.Car, StateDir: s.cfg.StateDir, ProcDir: s.cfg.SweepProcDir, Kill: s.cfg.SweepKill, Log: s.log})
}

// ---- children ----

// startChildLocked spawns spec (within MaxChildren) and records it for
// Sweep. Caller holds s.mu.
func (s *Server) startChildLocked(spec ChildSpec) (Child, error) {
	if len(s.children) >= MaxChildren {
		return nil, errTooManyChildren
	}
	c, err := s.cfg.Spawner.Start(spec)
	if err != nil {
		return nil, err
	}
	s.children[c.Pid()] = spec.Argv
	s.saveChildrenLocked()
	go func() {
		<-c.Done()
		s.mu.Lock()
		delete(s.children, c.Pid())
		s.saveChildrenLocked()
		s.mu.Unlock()
	}()
	return c, nil
}

func (s *Server) saveChildrenLocked() {
	recs := make([]childRec, 0, len(s.children))
	for pid, argv := range s.children {
		recs = append(recs, childRec{Pid: pid, Argv: argv})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Pid < recs[j].Pid })
	if err := s.mk.saveChildren(recs); err != nil {
		s.log.Printf("children marker: %v", err)
	}
}

func stopAll(cs []Child) {
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func() { defer wg.Done(); c.Stop() }()
	}
	wg.Wait()
}

// release frees everything a closing session holds. It runs after the
// session's worker has exited, so no request can re-acquire anything. The
// DataValue clears are queued first and waited for, then the children are
// stopped, then the cut files removed (§4.9).
func (s *Server) release(sess *session) {
	s.mu.Lock()
	c := change{talk: s.talk.owner == sess, sel: s.selOwner == sess}
	if s.holders[sess] {
		c.holds = []*session{sess}
	}
	stop := s.applyLocked(c)
	if s.summon == sess {
		s.summon = nil
		s.log.Printf("SUMMON mode off (session closed)")
	}
	stop = append(stop, s.unsubscribeAllLocked(sess)...)
	stop = append(stop, sess.cutChildren...)
	cuts := sess.cuts
	sess.cuts = nil
	for _, ct := range cuts {
		s.cutBytes -= ct.size
	}
	delete(s.sessions, sess)
	b := s.car.barrier()
	s.mu.Unlock()
	<-b
	stopAll(stop)
	for _, ct := range cuts {
		os.Remove(ct.path)
	}
}

// ---- sessions ----

type session struct {
	srv  *Server
	conn net.Conn
	out  *outQueue
	reqs chan proto.Request
	quit chan struct{} // closed when the reader stops

	// guarded by srv.mu
	caps        map[int]bool // capture subscriptions
	cuts        []*cutFile
	cutChildren []Child
}

func newSession(s *Server, c net.Conn) *session {
	return &session{
		srv: s, conn: c, out: newOutQueue(),
		reqs: make(chan proto.Request, 32), quit: make(chan struct{}),
		caps: map[int]bool{},
	}
}

func (sess *session) event(ev proto.Event) {
	b, _ := json.Marshal(ev)
	sess.out.push(proto.Encode(proto.TEvent, b))
}

func (sess *session) run() {
	s := sess.srv
	defer s.wg.Done()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		sess.writer()
	}()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for {
			select {
			case <-sess.quit:
				return
			case req := <-sess.reqs:
				select {
				case <-sess.quit: // the session ended while this request was queued
					return
				default:
				}
				s.handle(sess, req)
			}
		}
	}()
	sess.read()
	close(sess.quit)
	<-workerDone    // no request can acquire anything after this
	s.release(sess) // §4.9 order
	sess.out.close()
	<-writerDone
	sess.conn.Close()
}

func (sess *session) read() {
	r := bufio.NewReaderSize(sess.conn, 64<<10)
	for {
		t, body, err := proto.ReadFrame(r, proto.MaxClientFrame)
		if err != nil {
			return
		}
		switch t {
		case proto.TReq:
			if len(body) > proto.MaxReqJSON {
				return
			}
			var req proto.Request
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				return
			}
			select {
			case sess.reqs <- req:
			default:
				sess.srv.log.Printf("session: too many outstanding requests")
				return
			}
		case proto.TPCM:
			if len(body) > proto.MaxPCM || len(body)%2 != 0 {
				return
			}
			sess.srv.pcm(sess, body)
		default:
			return
		}
	}
}

func (sess *session) writer() {
	for {
		f, ok := sess.out.next()
		if !ok {
			sess.conn.Close() // ends the reader if the queue was cut off
			return
		}
		sess.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := sess.conn.Write(f); err != nil {
			sess.conn.Close() // the reader ends; the session releases
			sess.out.close()
			return
		}
	}
}

// ---- requests ----

func (s *Server) handle(sess *session, req proto.Request) {
	reply := func(r proto.Response) {
		r.ID = req.ID
		b, _ := json.Marshal(r)
		sess.out.push(proto.Encode(proto.TResp, b))
	}
	fail := func(code string) { reply(proto.Response{Err: code}) }
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		fail(proto.ErrShuttingDown)
		return
	}
	switch req.Op {
	case proto.OpHold, proto.OpRelease, proto.OpTalkArm, proto.OpTalkDisarm, proto.OpGain, proto.OpSel, proto.OpSummon, proto.OpStatus:
		st, code := s.arbiter(sess, req)
		if code != "" {
			reply(proto.Response{Err: code, Status: st})
			return
		}
		reply(proto.Response{OK: true, Status: st})

	case proto.OpCapStart:
		if code := s.capStart(sess, req.Port); code != "" {
			fail(code)
			return
		}
		reply(proto.Response{OK: true})

	case proto.OpCapStop:
		s.capStop(sess, req.Port)
		reply(proto.Response{OK: true})

	case proto.OpDashList:
		err := listTeslaCam(s.cfg.TeslaCam, func(p proto.ListPage) error {
			b, err := json.Marshal(p)
			if err != nil {
				return err
			}
			if !sess.out.pushWait(proto.Encode(proto.TList, proto.ListBody(req.ID, b)), sess.quit) {
				return errSessionGone
			}
			return nil
		})
		if err != nil {
			fail(proto.ErrIO)
			return
		}
		reply(proto.Response{OK: true})

	case proto.OpDashRead:
		data, total, code := s.dashRead(sess, req)
		if code != "" {
			fail(code)
			return
		}
		body := make([]byte, proto.DataHeader+len(data))
		proto.PutDataHeader(body, req.ID, uint64(total))
		copy(body[proto.DataHeader:], data)
		sess.out.pushWait(proto.Encode(proto.TData, body), sess.quit)

	case proto.OpDashCut:
		token, size, code := s.dashCut(sess, req)
		if code != "" {
			fail(code)
			return
		}
		reply(proto.Response{OK: true, Cut: token, Size: size})

	default:
		fail(proto.ErrBadRequest)
	}
}

var errSessionGone = errors.New("camhelper: session gone")

// ---- outbound queue ----

const (
	mediaCap = 4 << 20  // queued AUs per session before dropping to the next IDR
	prioCap  = 16 << 20 // control/data frames: a client that stops reading is cut off
)

type outQueue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	prio       [][]byte
	prioBytes  int
	media      [][]byte
	mediaBytes int
	needIDR    map[uint16]bool
	closed     bool
}

func newOutQueue() *outQueue {
	q := &outQueue{needIDR: map[uint16]bool{}}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *outQueue) push(f []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if q.prioBytes+len(f) > prioCap {
		q.closed = true // the writer drains nothing more; the session ends
		q.prio, q.media = nil, nil
		q.cond.Broadcast()
		return
	}
	q.prio = append(q.prio, f)
	q.prioBytes += len(f)
	q.cond.Broadcast()
}

// pushWait queues a large reply frame, waiting while more than 1 MiB of
// control data is queued (dash_list pages, dash_read data). False if the
// session ended.
func (q *outQueue) pushWait(f []byte, quit <-chan struct{}) bool {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return false
		}
		if q.prioBytes < 1<<20 {
			q.prio = append(q.prio, f)
			q.prioBytes += len(f)
			q.cond.Broadcast()
			q.mu.Unlock()
			return true
		}
		q.mu.Unlock()
		select {
		case <-quit:
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// pushAU queues one encoded AU frame (see encodeAU); under pressure it drops
// to the port's next IDR.
func (q *outQueue) pushAU(port uint16, idr, afterDrop bool, f []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if afterDrop {
		q.needIDR[port] = true
	}
	if q.needIDR[port] && !idr {
		return
	}
	if q.mediaBytes+len(f) > mediaCap {
		q.needIDR[port] = true
		return
	}
	q.needIDR[port] = false
	q.media = append(q.media, f)
	q.mediaBytes += len(f)
	q.cond.Broadcast()
}

// next returns the next frame (control first) or false once closed.
func (q *outQueue) next() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.prio) == 0 && len(q.media) == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return nil, false
	}
	if len(q.prio) > 0 {
		f := q.prio[0]
		q.prio[0] = nil
		q.prio = q.prio[1:]
		q.prioBytes -= len(f)
		return f, true
	}
	f := q.media[0]
	q.media[0] = nil
	q.media = q.media[1:]
	q.mediaBytes -= len(f)
	return f, true
}

func (q *outQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.prio, q.media = nil, nil
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Describe returns a one-line summary of the fixed argv tables (for logs).
func Describe() string {
	ports := make([]int, 0, len(capturePorts))
	for p := range capturePorts {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return fmt.Sprintf("capture ports %v (+%d during a selectable hold); writers pw-cat|aplay", ports, SelectablePort)
}
