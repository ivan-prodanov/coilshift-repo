package camera

import (
	"sort"
	"sync"
	"time"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability"
)

// ---- video ----

// FlagCatchup (VIDEO flags bit2) marks a catch-up AU: decode, do not display.
const FlagCatchup byte = 0x04

// GOPCacheMax bounds the per-port GOP cache (AUs from the last IDR on).
const GOPCacheMax = 1536 << 10

// video routes helper AUs to VIDEO frames. It runs on the helper client's
// reader goroutine and never blocks: TrySend only, dropping to the next IDR.
// It also keeps, per captured port, the current GOP (every AU since the last
// IDR), so a stream that starts on a warm port is sent that GOP at once
// (catch-up) instead of waiting up to 0.75 s for the next IDR.
type video struct {
	w capability.Window

	mu       sync.Mutex
	byPort   map[uint16]*vstream
	last     map[uint16]time.Time // last AU per port (any, sent or not)
	gops     map[uint16]*gop
	closed   map[uint16]bool // AUs dropped (not cached, sent or counted) until open
	dropping bool
}

type vstream struct {
	spec    streamSpec
	needIDR bool
	pending bool // installed, STREAMS not yet sent: live AUs wait, start() catches up
}

// gop is one port's cached group of pictures. Entries are helper AU bodies
// (12-byte header + AU); they are never sent or modified, only copied.
type gop struct {
	aus   [][]byte
	bytes int
	ok    bool // started at an IDR with SPS/PPS and within GOPCacheMax
}

func newVideo(w capability.Window) *video {
	return &video{w: w, byPort: map[uint16]*vstream{}, last: map[uint16]time.Time{}, gops: map[uint16]*gop{}, closed: map[uint16]bool{}}
}

// install puts a new stream table in place. A stream whose index, port or
// view changed, or that switches to full rate, is pending: no live AU is
// sent for it until start() (called after STREAMS went out) has sent its
// catch-up or decided it waits for the next IDR.
func (v *video) install(specs []streamSpec) {
	v.mu.Lock()
	defer v.mu.Unlock()
	next := make(map[uint16]*vstream, len(specs))
	for _, s := range specs {
		old := v.byPort[uint16(s.port)]
		ns := &vstream{spec: s, needIDR: true, pending: true}
		if old != nil && old.spec.index == s.index && old.spec.view == s.view && old.spec.dewarp == s.dewarp && (old.spec.full || !s.full) {
			ns.needIDR, ns.pending = old.needIDR, old.pending
		}
		next[uint16(s.port)] = ns
	}
	v.byPort = next
}

// start sends the catch-up of every pending stream, full-rate streams first.
func (v *video) start() {
	v.mu.Lock()
	defer v.mu.Unlock()
	var pend []*vstream
	for _, s := range v.byPort {
		if s.pending {
			pend = append(pend, s)
		}
	}
	sort.Slice(pend, func(i, j int) bool {
		if pend[i].spec.full != pend[j].spec.full {
			return pend[i].spec.full
		}
		return pend[i].spec.index < pend[j].spec.index
	})
	for _, s := range pend {
		v.catchUpLocked(s)
	}
}

// catchUpLocked sends a starting stream its port's cached GOP: every AU but
// the newest flagged CATCHUP, the newest displayed (a keyframe-only stream
// gets the IDR alone). With the bulk lane above the high watermark, no cache,
// or a failed send, the stream waits for the next live IDR instead.
func (v *video) catchUpLocked(s *vstream) {
	s.pending, s.needIDR = false, true
	g := v.gops[uint16(s.spec.port)]
	if v.dropping || v.w.QueuedBytes() > DropAbove || g == nil || !g.ok || len(g.aus) == 0 {
		return
	}
	list := g.aus
	if !s.spec.full {
		list = list[:1]
	}
	for i, b := range list {
		if v.w.QueuedBytes() > DropAbove {
			return
		}
		out := append([]byte(nil), b...)
		out[0], out[1], out[3] = 1, s.spec.index, 0
		out[2] &= proto.FlagIDR | proto.FlagParamSets
		if i < len(list)-1 {
			out[2] |= FlagCatchup
		}
		if !v.w.TrySend(TVideo, out) {
			return
		}
	}
	s.needIDR = false
}

// markAll makes every stream resync at an IDR and forgets the caches (helper
// session lost: every capture restarts).
func (v *video) markAll() {
	v.mu.Lock()
	for _, s := range v.byPort {
		s.needIDR = true
	}
	v.last = map[uint16]time.Time{}
	v.gops = map[uint16]*gop{}
	v.mu.Unlock()
}

// clear forgets one port's cache (its capture stopped or restarted).
func (v *video) clear(port int) {
	v.mu.Lock()
	delete(v.gops, uint16(port))
	v.mu.Unlock()
}

// shut forgets port's cache and drops its AUs until open: the AUs of a
// stopped capture may still be in flight (the helper sends replies ahead of
// queued media), and on 9895 they may be of the previous selectable camera.
func (v *video) shut(port int) {
	v.mu.Lock()
	delete(v.gops, uint16(port))
	delete(v.last, uint16(port)) // streams_ok waits for the new capture
	v.closed[uint16(port)] = true
	v.mu.Unlock()
}

// open starts accepting port's AUs again, from an empty cache (called right
// before its capture starts).
func (v *video) open(port int) {
	v.mu.Lock()
	delete(v.gops, uint16(port))
	delete(v.closed, uint16(port))
	v.mu.Unlock()
}

func (v *video) lastAU(port int) time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.last[uint16(port)]
}

// cacheLocked adds one AU to its port's GOP: an IDR (with SPS/PPS) starts a
// new GOP; a GOP over GOPCacheMax is dropped until the next IDR.
func (v *video) cacheLocked(port uint16, flags byte, body []byte) {
	g := v.gops[port]
	if g == nil {
		g = &gop{}
		v.gops[port] = g
	}
	if flags&proto.FlagIDR != 0 {
		g.aus, g.bytes, g.ok = nil, 0, flags&proto.FlagParamSets != 0
	}
	if !g.ok {
		return
	}
	if g.bytes+len(body) > GOPCacheMax {
		g.aus, g.bytes, g.ok = nil, 0, false
		return
	}
	g.aus = append(g.aus, body)
	g.bytes += len(body)
}

// onAU caches one helper AU (12-byte header + Annex-B AU) and forwards a copy
// to the stream showing its port, if any.
func (v *video) onAU(port uint16, flags byte, body []byte) {
	defer func() { recover() }() // never take the agw process down from the helper reader
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed[port] {
		return
	}
	v.last[port] = time.Now()
	if len(body) < proto.AUHeader || len(body) > 1<<20 {
		return
	}
	v.cacheLocked(port, flags, body)
	s := v.byPort[port]
	if s == nil || s.pending {
		return
	}
	q := v.w.QueuedBytes()
	if v.dropping && q < ResumeBelow {
		v.dropping = false
	}
	if !v.dropping && q > DropAbove {
		v.dropping = true
	}
	if v.dropping {
		s.needIDR = true
		return
	}
	idr := flags&proto.FlagIDR != 0
	if !s.spec.full && !idr {
		return // keyframe-only stream
	}
	if s.needIDR && (!idr || flags&proto.FlagParamSets == 0) {
		return
	}
	out := append([]byte(nil), body...)
	out[0] = 1 // version
	out[1] = s.spec.index
	out[2] = flags & (proto.FlagIDR | proto.FlagParamSets)
	out[3] = 0
	if !v.w.TrySend(TVideo, out) {
		s.needIDR = true
		return
	}
	s.needIDR = false
}

// ---- talk ----

// talkQueue holds phone PCM (s16le mono 48 kHz) until the pacer plays it,
// at most max bytes (45 s); the excess is dropped.
type talkQueue struct {
	mu       sync.Mutex
	max      int
	buf      []byte
	head     int
	overflow bool
}

func (q *talkQueue) push(b []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	room := q.max - (len(q.buf) - q.head)
	if len(b) > room {
		b = b[:room&^1]
		q.overflow = true
	}
	if q.head > 1<<20 && q.head > len(q.buf)/2 {
		n := copy(q.buf, q.buf[q.head:])
		q.buf, q.head = q.buf[:n], 0
	}
	q.buf = append(q.buf, b...)
}

// pop returns the next n bytes (zero-padded at the end of the queue), or nil
// when the queue is empty; empty reports whether this pop drained it.
func (q *talkQueue) pop(n int) (out []byte, empty bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	avail := len(q.buf) - q.head
	if avail == 0 {
		return nil, false
	}
	out = make([]byte, n)
	k := copy(out, q.buf[q.head:])
	q.head += k
	if q.head == len(q.buf) {
		q.buf, q.head = q.buf[:0], 0
		return out, true
	}
	return out, false
}

// reset drops everything queued (SUMMON opened: never play it later).
func (q *talkQueue) reset() {
	q.mu.Lock()
	q.buf, q.head = nil, 0
	q.mu.Unlock()
}

func (q *talkQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.buf) - q.head
}

// overflowed reports (and clears) an overflow since the last call.
func (q *talkQueue) overflowed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	o := q.overflow
	q.overflow = false
	return o
}

// pcmTarget returns the helper to feed while its talk writer is attached,
// and whether queued audio may play (the route is up).
func (h *handler) pcmTarget() (Helper, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.talkLive || h.h == nil {
		return nil, false
	}
	return h.h, h.routeOK
}

// pacer feeds the talk writer in real time for as long as talk is armed:
// queued PCM while the route is up, silence otherwise (an idle route is
// reaped by audiod about 4 s after the last client data). It keeps about
// 100 ms queued at the writer and never bursts after a stall.
func (h *handler) pacer() {
	defer h.wg.Done()
	defer h.recoverGo("pacer")
	chunk := int(h.cfg.Chunk.Milliseconds()) * bytesPerMS
	if chunk <= 0 || chunk > proto.MaxPCM {
		chunk = chunkBytes
	}
	silence := make([]byte, chunk)
	t := time.NewTicker(h.cfg.Chunk)
	defer t.Stop()
	var start time.Time
	sent := 0
	for {
		select {
		case <-h.stop:
			return
		case <-h.w.Done():
			return
		case <-t.C:
		}
		hc, play := h.pcmTarget()
		if hc == nil {
			start = time.Time{}
			continue
		}
		now := time.Now()
		if start.IsZero() {
			start, sent = now, 0
		}
		due := int(now.Sub(start)/h.cfg.Chunk) + pcmLeadChunks
		if due-sent > 3*pcmLeadChunks { // stalled: do not burst
			sent = due - pcmLeadChunks
		}
		for sent < due {
			b := silence
			if play {
				if p, drained := h.tq.pop(chunk); p != nil {
					b = p
					if drained {
						select {
						case h.drained <- struct{}{}:
						default:
						}
					}
				}
			}
			if err := hc.WritePCM(b); err != nil {
				break
			}
			sent++
		}
	}
}
