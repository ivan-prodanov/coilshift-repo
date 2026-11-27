package server

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"airgapp/gateway/capability"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/session"
	"airgapp/gateway/internal/wire"
)

const (
	prioLaneCap   = 64 << 10 // READY, BYE and every non-bulk type
	bulkLaneCap   = 4 << 20  // Spec.Bulk types
	coreMaxFrame  = 1024     // inbound ciphertext limit floor (core types)
	aeadOverhead  = 16
	leaseTick     = 100 * time.Millisecond
	byeTimeout    = 200 * time.Millisecond // BYE (and draining priority frames) best-effort
	lingerTimeout = 50 * time.Millisecond  // after FIN, wait this long for the peer's FIN
	lingerMax     = 256 << 10              // and discard at most this much input
	maxReasonLen  = 256                    // local (log) copy of a Close reason
	maxDetailLen  = 256
	maxEvents     = 64  // Window.LogEvent entries per window
	maxEventLen   = 128 // bytes per entry
	tickKeep      = 64  // TICKs remembered per window for TickAge
	tickBodyLen   = 16  // id (8) || issued BOOTTIME ms (u64 BE)
)

// Teardown reasons: the only strings that go on the wire in BYE.
const (
	reasonClose    = "close"
	reasonLease    = "lease"
	reasonGrammar  = "grammar"
	reasonOversize = "oversize"
	reasonDecrypt  = "decrypt"
	reasonRevoked  = "revoked"
	reasonError    = "error" // Handler.Frame error or panic (details logged locally)
	reasonDrop     = "drop"  // connection lost (read/write error); BYE rarely arrives
	reasonSigterm  = "sigterm"
)

// wireReason is the BYE body for a teardown reason: one of the fixed
// reasons verbatim, anything else (a capability's own Close text, an ad-hoc
// Shutdown reason) as "error". The original text stays in the local logs.
func wireReason(r string) string {
	switch r {
	case reasonClose, reasonLease, reasonGrammar, reasonOversize, reasonDecrypt,
		reasonRevoked, reasonSigterm, reasonError, reasonDrop:
		return r
	}
	return reasonError
}

var (
	// errOutbound is returned by Send for a type outside Spec.Out (or a core
	// type) or a body above its Spec.Out limit. It is a capability bug: the
	// frame is refused and the window stays open.
	errOutbound = errors.New("server: outbound type not allowed or body too large")
	errLaneFull = errors.New("server: lane full")
	// errLaneFullOpening is returned by Send while Open runs: the writer
	// starts only after Open returns (READY must be first), so a full lane
	// cannot drain and Send fails fast instead of blocking.
	errLaneFullOpening = errors.New("server: lane full while Open runs (Send does not block until Open returns)")
)

type frame struct {
	t    byte
	body []byte
}

type lane struct {
	q     []frame
	bytes int
	cap   int
}

// fits allows any frame into an empty lane, so a frame larger than the cap
// (a 64 KiB priority frame) cannot deadlock.
func (l *lane) fits(n int) bool { return l.bytes == 0 || l.bytes+n <= l.cap }

func (l *lane) push(f frame) {
	l.q = append(l.q, f)
	l.bytes += len(f.body)
}

func (l *lane) pop() frame {
	f := l.q[0]
	l.q[0] = frame{}
	l.q = l.q[1:]
	l.bytes -= len(f.body)
	return f
}

// drop removes every queued frame of type t.
func (l *lane) drop(t byte) {
	q := l.q[:0]
	for _, f := range l.q {
		if f.t == t {
			l.bytes -= len(f.body)
			continue
		}
		q = append(q, f)
	}
	clear(l.q[len(q):])
	l.q = q
}

// tickRing remembers the last tickKeep TICKs a window issued.
type tickRing struct {
	ids  [tickKeep][8]byte
	ms   [tickKeep]uint64
	n    int // filled entries
	next int // slot the next add overwrites
}

func (r *tickRing) add(id [8]byte, ms uint64) {
	r.ids[r.next], r.ms[r.next] = id, ms
	r.next = (r.next + 1) % tickKeep
	r.n = min(r.n+1, tickKeep)
}

// issued returns the BOOTTIME ms at which tick id was issued.
func (r *tickRing) issued(id [8]byte) (uint64, bool) {
	for i := 0; i < r.n; i++ {
		if r.ids[i] == id {
			return r.ms[i], true
		}
	}
	return 0, false
}

// window is the server side of one armed session; it implements
// capability.Window.
type window struct {
	s     *Server
	c     net.Conn
	ce    *capEntry
	spec  capability.Spec
	pk    []byte
	fp    string
	in    *session.Inbound
	out   *session.Outbound
	h     capability.Handler
	start time.Time

	mu         sync.Mutex
	cond       *sync.Cond // any lane change, Close, writer stop
	prio, bulk lane
	closed     bool   // Close called: sends refused, bulk dropped
	opening    bool   // Open has not returned: a full lane fails Send fast
	stopWriter bool   // teardown: writer drains priority frames, then exits
	reason     string // Handler.Teardown's; BYE carries wireReason(reason)
	logReason  string // the session log's and stderr's: reason, and why it closed when known
	detail     string
	events     []logEvent
	ticks      tickRing

	queued      atomic.Int64  // bulk lane bytes
	lease       atomic.Uint64 // BOOTTIME ms deadline (LeaseMS > 0)
	writeFailed atomic.Bool

	closeOnce  sync.Once
	done       chan struct{}
	readerDone chan struct{}
	writerDone chan struct{}
}

var _ capability.Window = (*window)(nil)

func newWindow(s *Server, c net.Conn, ce *capEntry, pk []byte, in *session.Inbound, out *session.Outbound) *window {
	w := &window{
		s: s, c: c, ce: ce, spec: ce.spec, pk: pk, fp: keys.Fingerprint(pk),
		in: in, out: out, start: time.Now(), opening: true,
		prio:       lane{cap: prioLaneCap},
		bulk:       lane{cap: bulkLaneCap},
		done:       make(chan struct{}),
		readerDone: make(chan struct{}),
		writerDone: make(chan struct{}),
	}
	w.cond = sync.NewCond(&w.mu)
	w.renew()
	return w
}

// queueReady puts READY on the priority lane before Open runs.
func (w *window) queueReady() {
	body, _ := json.Marshal(struct {
		LeaseMS int `json:"lease_ms"`
	}{w.spec.LeaseMS})
	w.mu.Lock()
	w.prio.push(frame{proto.TypeReady, body})
	w.mu.Unlock()
}

// opened marks Open as returned: from here Send blocks on a full lane.
func (w *window) opened() {
	w.mu.Lock()
	w.opening = false
	w.mu.Unlock()
}

// --- capability.Window ---

func (w *window) Send(t byte, body []byte) error { return w.enqueue(t, body, true) }

func (w *window) TrySend(t byte, body []byte) bool { return w.enqueue(t, body, false) == nil }

func (w *window) enqueue(t byte, body []byte, block bool) error {
	lim, ok := w.spec.Out[t]
	if !ok || t == proto.TypeReady || t == proto.TypeBye || t == proto.TypeTick || len(body) > lim {
		return errOutbound
	}
	isBulk := w.spec.Bulk[t]
	w.mu.Lock()
	defer w.mu.Unlock()
	l := &w.prio
	if isBulk {
		l = &w.bulk
	}
	for {
		if w.closed {
			return capability.ErrClosed
		}
		if l.fits(len(body)) {
			break
		}
		if !block {
			return errLaneFull
		}
		if w.opening {
			return errLaneFullOpening
		}
		w.cond.Wait()
	}
	l.push(frame{t, body})
	if isBulk {
		w.queued.Store(int64(w.bulk.bytes))
	}
	w.cond.Broadcast()
	return nil
}

func (w *window) QueuedBytes() int { return int(w.queued.Load()) }

func (w *window) SetDetail(d string) {
	if len(d) > maxDetailLen {
		d = d[:maxDetailLen]
	}
	w.mu.Lock()
	w.detail = d
	w.mu.Unlock()
}

// LogEvent appends a bounded event to the window's session-log record.
func (w *window) LogEvent(e string) {
	t := time.Since(w.start).Milliseconds()
	e = truncUTF8(strings.ToValidUTF8(e, "?"), maxEventLen)
	w.mu.Lock()
	if len(w.events) < maxEvents {
		w.events = append(w.events, logEvent{T: t, E: e})
	}
	w.mu.Unlock()
}

// truncUTF8 cuts valid UTF-8 s to at most n bytes on a rune boundary.
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Close starts teardown; idempotent, never blocks. From here Send returns
// ErrClosed, queued bulk frames are dropped and Done is closed; the window's
// run goroutine does the ordered teardown.
func (w *window) Close(reason string) { w.close(reason, "") }

// revoke closes w for a failed authority re-check: Handler.Teardown and BYE
// get exactly "revoked" (SUMMON maps its fixed reasons exactly). why (no key
// material) stays local: the session log's reason and the stderr log get
// "revoked: " + why.
func (w *window) revoke(why string) { w.close(reasonRevoked, why) }

// close is Close with why, an explanation for the local logs only.
func (w *window) close(reason, why string) {
	w.closeOnce.Do(func() {
		reason = strings.ToValidUTF8(reason, "?")
		if len(reason) > maxReasonLen {
			reason = reason[:maxReasonLen] // local logs only; the wire gets wireReason
		}
		if reason == "" {
			reason = reasonError
		}
		logReason := reason
		if why != "" {
			logReason = truncUTF8(strings.ToValidUTF8(reason+": "+why, "?"), maxReasonLen)
		}
		w.mu.Lock()
		w.closed = true
		w.reason, w.logReason = reason, logReason
		w.bulk = lane{cap: bulkLaneCap}
		w.queued.Store(0)
		w.prio.drop(proto.TypeTick) // a TICK is a freshness proof: none after Close
		w.cond.Broadcast()
		w.mu.Unlock()
		close(w.done)
	})
}

func (w *window) Done() <-chan struct{} { return w.done }

func (w *window) KeyFingerprint() string { return w.fp }

func (w *window) PublicKey() []byte { return append([]byte(nil), w.pk...) }

func (w *window) Host() capability.Host { return windowHost{w} }

// TickAge measures from the injected Clock; only this window's last
// tickKeep ticks are known.
func (w *window) TickAge(id [8]byte) (time.Duration, bool) {
	w.mu.Lock()
	ms, ok := w.ticks.issued(id)
	w.mu.Unlock()
	if !ok {
		return 0, false
	}
	now := w.s.clk.BootMS()
	if now < ms {
		return 0, true
	}
	return time.Duration(now-ms) * time.Millisecond, true
}

// sendTick queues one TICK on the priority lane and remembers it. If the
// lane is full, or the window is closed, the tick is skipped: it never blocks.
func (w *window) sendTick() {
	var id [8]byte
	rand.Read(id[:]) // never fails (crypto/rand, Go >= 1.24)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || !w.prio.fits(tickBodyLen) {
		return
	}
	ms := w.s.clk.BootMS()
	body := make([]byte, tickBodyLen)
	copy(body, id[:])
	binary.BigEndian.PutUint64(body[8:], ms)
	w.prio.push(frame{proto.TypeTick, body})
	w.ticks.add(id, ms)
	w.cond.Broadcast()
}

// --- lifecycle ---

func (w *window) isClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func (w *window) renew() {
	if w.spec.LeaseMS > 0 {
		w.lease.Store(w.s.clk.BootMS() + uint64(w.spec.LeaseMS))
	}
}

// abort undoes a reserved window whose Open failed: no reader/writer ran,
// no Handler exists, nothing is sent. The caller closes the socket.
func (w *window) abort() {
	w.Close(reasonError)
	w.s.free(w)
}

// run serves the window until Close, then tears down. TICKs (Spec.TickHz)
// start here, after READY is queued, and stop before teardown, so none is
// queued after Close and none follows BYE.
func (w *window) run() {
	log.Printf("agw: %s open fp=%s", w.spec.Name, w.fp)
	w.renew()
	go w.reader()
	go w.writer()
	var leaseC, tickC <-chan time.Time
	if w.spec.LeaseMS > 0 {
		t := time.NewTicker(leaseTick)
		defer t.Stop()
		leaseC = t.C
	}
	var ticker *time.Ticker
	if w.spec.TickHz > 0 {
		ticker = time.NewTicker(time.Second / time.Duration(w.spec.TickHz))
		tickC = ticker.C
	}
	for !w.isClosed() {
		select {
		case <-w.done:
		case <-leaseC:
			if w.s.clk.BootMS() >= w.lease.Load() {
				w.Close(reasonLease)
			}
		case <-tickC:
			w.sendTick()
		}
	}
	if ticker != nil {
		ticker.Stop()
	}
	w.teardown()
}

// teardown is the ordered shutdown: stop reading -> Handler.Teardown -> BYE
// (best-effort, 200 ms) -> close the socket -> free the slot -> session log.
// It runs once, on the run goroutine.
func (w *window) teardown() {
	w.mu.Lock()
	reason, logReason := w.reason, w.logReason
	w.mu.Unlock()
	bye := wireReason(reason)

	w.c.SetReadDeadline(time.Unix(1, 0))
	<-w.readerDone

	w.handlerTeardown(reason)

	w.c.SetWriteDeadline(time.Now().Add(byeTimeout))
	w.mu.Lock()
	w.stopWriter = true
	w.cond.Broadcast()
	w.mu.Unlock()
	<-w.writerDone
	if !w.writeFailed.Load() {
		w.writeFrame(proto.TypeBye, []byte(bye)) // best-effort
	}

	w.closeConn()
	w.s.free(w)

	w.mu.Lock()
	detail, events := w.detail, w.events
	w.mu.Unlock()
	w.s.slog.write(logEntry{
		Start: w.start.UTC().Format(time.RFC3339Nano), End: time.Now().UTC().Format(time.RFC3339Nano),
		FP: w.fp, Cap: w.spec.Name, Detail: detail, Reason: logReason, Events: events,
	})
	log.Printf("agw: %s closed fp=%s reason=%q bye=%s", w.spec.Name, w.fp, logReason, bye)
}

func (w *window) handlerTeardown(reason string) { teardownHandler(w.spec.Name, w.h, reason) }

// teardownHandler calls h.Teardown, recovering a panic.
func teardownHandler(name string, h capability.Handler, reason string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("agw: %s Teardown panic: %v", name, r)
		}
	}()
	h.Teardown(reason)
}

// closeConn half-closes, briefly discards late input so the close does not
// turn into an RST that could destroy the BYE in flight, then closes.
func (w *window) closeConn() {
	if hc, ok := w.c.(interface{ CloseWrite() error }); ok && hc.CloseWrite() == nil {
		w.c.SetReadDeadline(time.Now().Add(lingerTimeout))
		io.CopyN(io.Discard, w.c, lingerMax)
	}
	w.c.Close()
}

// reader opens inbound frames in order and dispatches them. It is the only
// caller of Handler.Frame.
func (w *window) reader() {
	defer close(w.readerDone)
	readMax := max(w.ce.maxIn+1+aeadOverhead, coreMaxFrame)
	for {
		ct, err := wire.ReadFrame(w.c, readMax)
		if err != nil {
			if errors.Is(err, wire.ErrTooLarge) {
				w.Close(reasonOversize)
			} else {
				w.Close(reasonDrop)
			}
			return
		}
		if w.isClosed() {
			return
		}
		pt, err := w.in.Open(ct)
		if err != nil {
			w.Close(reasonDecrypt)
			return
		}
		if reason := w.dispatch(pt); reason != "" {
			w.Close(reason)
			return
		}
	}
}

// dispatch handles one plaintext; a non-empty result is a teardown reason.
func (w *window) dispatch(pt []byte) string {
	if len(pt) == 0 {
		return reasonGrammar
	}
	t, body := pt[0], pt[1:]
	switch t {
	case proto.TypeHeartbeat:
		if len(body) != 0 {
			return reasonGrammar
		}
		w.renew()
		return ""
	case proto.TypeClose:
		if len(body) != 0 {
			return reasonGrammar
		}
		return reasonClose
	case proto.TypeArm:
		return reasonGrammar
	}
	lim, ok := w.spec.In[t]
	if !ok {
		return reasonGrammar
	}
	if len(body) > lim {
		return reasonOversize
	}
	w.renew()
	if err := w.frame(t, body); err != nil {
		log.Printf("agw: %s fp=%s frame 0x%02x: %v", w.spec.Name, w.fp, t, err)
		return reasonError
	}
	return ""
}

func (w *window) frame(t byte, body []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return w.h.Frame(t, body)
}

// writer is the window's single sealer/writer: priority lane first, always.
// After Close it drops bulk; after stopWriter it drains priority and exits.
func (w *window) writer() {
	defer close(w.writerDone)
	for {
		w.mu.Lock()
		for len(w.prio.q) == 0 && len(w.bulk.q) == 0 && !w.stopWriter {
			w.cond.Wait()
		}
		var f frame
		switch {
		case len(w.prio.q) > 0:
			f = w.prio.pop()
		case len(w.bulk.q) > 0:
			f = w.bulk.pop()
			w.queued.Store(int64(w.bulk.bytes))
		default: // stopWriter and nothing left
			w.mu.Unlock()
			return
		}
		w.cond.Broadcast()
		w.mu.Unlock()
		if err := w.writeFrame(f.t, f.body); err != nil {
			w.writeFailed.Store(true)
			w.Close(reasonDrop)
			return
		}
	}
}

// writeFrame seals type||body and writes one frame. Only the writer, then
// (after the writer exits) teardown, call it, so seal order = wire order.
func (w *window) writeFrame(t byte, body []byte) error {
	pt := make([]byte, 1+len(body))
	pt[0] = t
	copy(pt[1:], body)
	ct := w.out.Seal(pt) // bodies are bounded by Spec.Out (well under 4 GiB)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(ct)))
	bufs := net.Buffers{hdr[:], ct}
	_, err := bufs.WriteTo(w.c)
	return err
}
