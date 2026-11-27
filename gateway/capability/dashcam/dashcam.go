// Package dashcam is the DASHCAM capability (id 0x03): read-only listing and
// chunked, resumable reads of the recorded clips under /mnt/usb/TeslaCam,
// through the camera root helper (its own helper session per window).
// Wire protocol: docs/CAMERA-PROTOCOL.md.
package dashcam

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"airgapp/gateway/camhelper/client"
	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability"
)

// Capability id and frame types.
const (
	ID byte = 0x03

	TList   byte = 0x40
	TGet    byte = 0x41
	TCancel byte = 0x42

	TListResult byte = 0xC0
	TChunk      byte = 0xC1
	TError      byte = 0xC2
)

// Limits.
const (
	MaxPart     = 65536
	ChunkHeader = 20
	MaxChunk    = MaxPart - ChunkHeader
	MaxCutT     = 120
	maxQueued   = 8
)

// Error codes.
const (
	ErrNotFound  = "not_found"
	ErrForbidden = "forbidden_path"
	ErrCutFailed = "cut_failed"
	ErrBusy      = "busy"
	ErrIO        = "io"
)

// Helper is the part of the camhelper client DASHCAM uses.
type Helper interface {
	DashList(context.Context) (proto.ListPage, error)
	DashRead(ctx context.Context, path, cut string, offset int64, length int) ([]byte, int64, error)
	DashCut(ctx context.Context, path string, ss, t float64) (string, int64, error)
	Done() <-chan struct{}
	Close() error
}

// DialFunc opens one helper session.
type DialFunc func() (Helper, error)

// SocketDialer dials the helper at path.
func SocketDialer(path string) DialFunc {
	return func() (Helper, error) {
		c, err := client.Dial(path, nil)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// Config holds the dialer and limits (zero = car defaults).
type Config struct {
	Dial        DialFunc
	Root        string        // clip root for event paths and the GET prefix check
	ReadTimeout time.Duration // per dash_read: 5 s
	ListTimeout time.Duration // dash_list: 30 s
	CutTimeout  time.Duration // dash_cut: 40 s
	RateBPS     int           // CHUNK bytes per second: 4 MiB/s
	QueueBytes  int           // keep the bulk lane below this before reading on: 1 MiB
	Log         *log.Logger
}

func (c *Config) defaults() {
	if c.Dial == nil {
		c.Dial = SocketDialer(proto.DefaultSocket)
	}
	if c.Root == "" {
		c.Root = TeslaCam
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 5 * time.Second
	}
	if c.ListTimeout == 0 {
		c.ListTimeout = 30 * time.Second
	}
	if c.CutTimeout == 0 {
		c.CutTimeout = 40 * time.Second
	}
	if c.RateBPS == 0 {
		c.RateBPS = 4 << 20
	}
	if c.QueueBytes == 0 {
		c.QueueBytes = 1 << 20
	}
	if c.Log == nil {
		c.Log = log.Default()
	}
}

type dashCap struct{ cfg Config }

// New returns DASHCAM using the helper at its default socket.
func New() capability.Capability { return NewWithConfig(Config{}) }

// NewWithConfig returns DASHCAM with a custom dialer or limits.
func NewWithConfig(cfg Config) capability.Capability {
	cfg.defaults()
	return &dashCap{cfg: cfg}
}

func (*dashCap) Spec() capability.Spec {
	return capability.Spec{
		Name: "DASHCAM", ID: ID, LeaseMS: 20000,
		In:   map[byte]int{TList: 256, TGet: 512, TCancel: 64},
		Out:  map[byte]int{TListResult: MaxPart, TChunk: MaxPart, TError: 256},
		Bulk: map[byte]bool{TChunk: true},
	}
}

func (*dashCap) PreArm(context.Context, []byte) error { return nil }

// Open returns at once; the helper is dialed by the workers on first use.
func (c *dashCap) Open(_ context.Context, w capability.Window, params json.RawMessage) (capability.Handler, error) {
	var p struct{}
	if err := strictJSON(params, &p); err != nil {
		return nil, fmt.Errorf("dashcam: ARM params: %w", err)
	}
	h := &handler{
		cfg: c.cfg, w: w, stop: make(chan struct{}),
		lists: make(chan uint32, 2), gets: make(chan *getReq, maxQueued),
		active: map[uint32]*getReq{},
	}
	h.wg.Add(2)
	go h.listWorker()
	go h.getWorker()
	return h, nil
}

func strictJSON(b []byte, v any) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

type getReq struct {
	ID     uint32   `json:"id"`
	Path   string   `json:"path"`
	Offset int64    `json:"offset"`
	Length int64    `json:"length"`
	SS     *float64 `json:"ss"`
	T      *float64 `json:"t"`

	cancelled bool // guarded by handler.mu
}

type handler struct {
	cfg  Config
	w    capability.Window
	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup

	lists chan uint32
	gets  chan *getReq

	mu      sync.Mutex
	active  map[uint32]*getReq // queued or running GETs by id
	h       Helper
	stopped bool
}

func (h *handler) Frame(t byte, body []byte) error {
	switch t {
	case TList:
		var r struct {
			ID *uint32 `json:"id"`
		}
		if err := strictJSON(body, &r); err != nil || r.ID == nil {
			return fmt.Errorf("dashcam: LIST: bad body")
		}
		select {
		case h.lists <- *r.ID:
		default:
			h.sendErr(*r.ID, ErrBusy)
		}
		return nil
	case TGet:
		r := &getReq{}
		if err := strictJSON(body, r); err != nil || len(bytes.TrimSpace(body)) == 0 {
			return fmt.Errorf("dashcam: GET: bad body")
		}
		if r.Offset < 0 || r.Length < 0 || (r.SS == nil) != (r.T == nil) ||
			(r.SS != nil && (!finite(*r.SS) || *r.SS < 0 || !finite(*r.T) || *r.T <= 0 || *r.T > MaxCutT)) {
			return fmt.Errorf("dashcam: GET: bad range")
		}
		if !strings.HasPrefix(r.Path, h.cfg.Root+"/") || strings.Contains(r.Path, "/../") || strings.HasSuffix(r.Path, "/..") {
			h.sendErr(r.ID, ErrForbidden)
			return nil
		}
		h.mu.Lock()
		if _, dup := h.active[r.ID]; dup {
			h.mu.Unlock()
			h.sendErr(r.ID, ErrBusy)
			return nil
		}
		select {
		case h.gets <- r:
			h.active[r.ID] = r
		default:
			h.mu.Unlock()
			h.sendErr(r.ID, ErrBusy)
			return nil
		}
		h.mu.Unlock()
		return nil
	case TCancel:
		var r struct {
			ID *uint32 `json:"id"`
		}
		if err := strictJSON(body, &r); err != nil || r.ID == nil {
			return fmt.Errorf("dashcam: CANCEL: bad body")
		}
		h.mu.Lock()
		if g := h.active[*r.ID]; g != nil {
			g.cancelled = true
		}
		h.mu.Unlock()
		return nil
	}
	return fmt.Errorf("dashcam: unexpected type 0x%02x", t)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// Teardown stops the workers, closes this window's helper session (its cut
// files are removed by the helper) and waits at most 300 ms. It never waits
// on Done.
func (h *handler) Teardown(reason string) {
	h.once.Do(func() {
		close(h.stop)
		h.mu.Lock()
		h.stopped = true
		hc := h.h
		h.h = nil
		for _, g := range h.active {
			g.cancelled = true
		}
		h.mu.Unlock()
		if hc != nil {
			hc.Close()
		}
		done := make(chan struct{})
		go func() { h.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			h.cfg.Log.Printf("dashcam: teardown (%s): workers still finishing", reason)
		}
	})
}

func (h *handler) stopping() bool {
	select {
	case <-h.stop:
		return true
	case <-h.w.Done():
		return true
	default:
		return false
	}
}

func (h *handler) recoverGo(name string) {
	if r := recover(); r != nil {
		h.cfg.Log.Printf("dashcam: %s panic: %v", name, r)
		h.w.Close("error: dashcam " + name + " panic")
	}
}

// helper returns the session, dialing it if needed.
func (h *handler) helper() (Helper, error) {
	h.mu.Lock()
	hc := h.h
	h.mu.Unlock()
	if hc != nil {
		select {
		case <-hc.Done():
			h.drop(hc)
		default:
			return hc, nil
		}
	}
	nc, err := h.cfg.Dial()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		nc.Close()
		return nil, errors.New("dashcam: stopped")
	}
	if h.h != nil {
		nc.Close()
		return h.h, nil
	}
	h.h = nc
	return nc, nil
}

func (h *handler) drop(hc Helper) {
	h.mu.Lock()
	if h.h == hc {
		h.h = nil
	}
	h.mu.Unlock()
	hc.Close()
}

func (h *handler) sendErr(id uint32, code string) {
	b, _ := json.Marshal(struct {
		ID    uint32 `json:"id"`
		Error string `json:"error"`
	}{id, code})
	h.w.Send(TError, b)
}

// code maps a helper failure to a DASHCAM error code; a dead session is
// dropped so the next request redials.
func (h *handler) code(hc Helper, err error) string {
	switch client.Code(err) {
	case proto.ErrNotFound:
		return ErrNotFound
	case proto.ErrForbidden:
		return ErrForbidden
	case proto.ErrCutFailed:
		return ErrCutFailed
	case proto.ErrBusy:
		return ErrBusy
	case "":
		if hc != nil {
			h.drop(hc)
		}
	}
	return ErrIO
}

func (h *handler) listWorker() {
	defer h.wg.Done()
	defer h.recoverGo("list")
	for {
		var id uint32
		select {
		case <-h.stop:
			return
		case <-h.w.Done():
			return
		case id = <-h.lists:
		}
		hc, err := h.helper()
		if err != nil {
			h.sendErr(id, ErrIO)
			continue
		}
		ctx, cancel := h.ctx(h.cfg.ListTimeout)
		inv, err := hc.DashList(ctx)
		cancel()
		if err != nil {
			h.sendErr(id, h.code(hc, err))
			continue
		}
		for _, part := range packParts(id, buildEvents(inv)) {
			if h.w.Send(TListResult, part) != nil {
				return
			}
		}
	}
}

// ctx is bounded by d and cancelled by Teardown.
func (h *handler) ctx(d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	go func() {
		select {
		case <-h.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (h *handler) getWorker() {
	defer h.wg.Done()
	defer h.recoverGo("get")
	for {
		var g *getReq
		select {
		case <-h.stop:
			return
		case <-h.w.Done():
			return
		case g = <-h.gets:
		}
		h.serve(g)
		h.mu.Lock()
		delete(h.active, g.ID)
		h.mu.Unlock()
	}
}

func (h *handler) cancelled(g *getReq) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return g.cancelled
}

func (h *handler) serve(g *getReq) {
	if h.cancelled(g) {
		return
	}
	hc, err := h.helper()
	if err != nil {
		h.sendErr(g.ID, ErrIO)
		return
	}
	path, cut := g.Path, ""
	if g.SS != nil {
		ctx, cancel := h.ctx(h.cfg.CutTimeout)
		tok, _, err := hc.DashCut(ctx, g.Path, *g.SS, *g.T)
		cancel()
		if err != nil {
			if !h.stopping() {
				h.sendErr(g.ID, h.code(hc, err))
			}
			return
		}
		path, cut = "", tok
	}
	off, end := g.Offset, int64(math.MaxInt64)
	if g.Length > 0 && off <= math.MaxInt64-g.Length {
		end = off + g.Length
	}
	start := time.Now()
	var sent int64
	first := true
	for {
		if h.cancelled(g) || h.stopping() {
			return
		}
		// Flow control: keep the bulk lane short (CANCEL stays responsive,
		// live CAMERA on its own connection is not starved), and rate-limit.
		for h.w.QueuedBytes() > h.cfg.QueueBytes {
			if h.cancelled(g) || h.stopping() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		if ahead := time.Duration(float64(sent)/float64(h.cfg.RateBPS)*float64(time.Second)) - time.Since(start); ahead > 0 {
			select {
			case <-time.After(ahead):
			case <-h.stop:
				return
			}
			if h.cancelled(g) {
				return
			}
		}
		n := int64(MaxChunk)
		if end-off < n {
			n = end - off
		}
		ctx, cancel := h.ctx(h.cfg.ReadTimeout)
		data, total, err := hc.DashRead(ctx, path, cut, off, int(n))
		cancel()
		if err != nil {
			if !h.stopping() {
				h.sendErr(g.ID, h.code(hc, err))
			}
			return
		}
		if first {
			first = false
			if end > total {
				end = total
			}
			if int64(len(data)) > end-off {
				data = data[:max(0, end-off)]
			}
		}
		body := make([]byte, ChunkHeader+len(data))
		binary.BigEndian.PutUint32(body[0:4], g.ID)
		binary.BigEndian.PutUint64(body[4:12], uint64(off))
		binary.BigEndian.PutUint64(body[12:20], uint64(total))
		copy(body[ChunkHeader:], data)
		if h.w.Send(TChunk, body) != nil {
			return
		}
		off += int64(len(data))
		sent += int64(len(data))
		if len(data) == 0 || off >= end {
			return
		}
	}
}

// ---- LIST_RESULT packing ----

const partOverhead = len(`{"id":4294967295,"part":2147483647,"last":false,"events":[]}`)

// packParts splits the events over LIST_RESULT frames of at most MaxPart
// bytes. An event too large for one frame on its own (only a very long
// RecentClips buffer) loses its oldest minutes until it fits.
func packParts(id uint32, events []*event) [][]byte {
	var parts [][]byte
	var cur [][]byte
	curLen := 0
	flush := func(last bool) {
		var b bytes.Buffer
		fmt.Fprintf(&b, `{"id":%d,"part":%d,"last":%t,"events":[`, id, len(parts), last)
		for i, e := range cur {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(e)
		}
		b.WriteString("]}")
		parts = append(parts, b.Bytes())
		cur, curLen = nil, 0
	}
	for _, e := range events {
		eb := fitEvent(e, MaxPart-partOverhead)
		if eb == nil {
			continue
		}
		if curLen+len(eb)+1 > MaxPart-partOverhead && len(cur) > 0 {
			flush(false)
		}
		cur = append(cur, eb)
		curLen += len(eb) + 1
	}
	flush(true)
	return parts
}

func fitEvent(e *event, limit int) []byte {
	for {
		b, _ := e.MarshalJSON()
		if len(b) <= limit {
			return b
		}
		if len(e.minuteList) <= 1 {
			return nil
		}
		// Drop the oldest minute: the last of the newest-first RecentClips
		// list, the first of an event's oldest-first list.
		var drop string
		if e.recent {
			drop = e.minuteList[len(e.minuteList)-1]
			e.minuteList = e.minuteList[:len(e.minuteList)-1]
		} else {
			drop = e.minuteList[0]
			e.minuteList = e.minuteList[1:]
		}
		delete(e.minutes, drop)
	}
}
