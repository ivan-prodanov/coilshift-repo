// Package testkit provides a FakeWindow for unit-testing capabilities without the server.
package testkit

import (
	"crypto/rand"
	"sync"
	"sync/atomic"
	"time"

	"airgapp/gateway/capability"
)

// FakeWindow implements capability.Window. Safe for concurrent use. It records
// sends per type and does not enforce Spec sizes or lane limits.
type FakeWindow struct {
	mu        sync.Mutex
	sent      map[byte][][]byte
	detail    string
	events    []string
	reason    string
	failTry   bool
	closed    bool
	queued    atomic.Int64
	closeOnce sync.Once
	done      chan struct{}
	ticks     map[[8]byte]tickAge
	host      capability.Host
	pk        []byte
}

type tickAge struct {
	age time.Duration
	ok  bool
}

func NewFakeWindow() *FakeWindow {
	return &FakeWindow{
		sent: map[byte][][]byte{}, done: make(chan struct{}),
		ticks: map[[8]byte]tickAge{}, host: NewFakeHost(),
	}
}

// IssueTick returns a fresh random tick id that TickAge reports as ok with
// age 0 until SetTickAge changes it. Nothing is recorded in Sent.
func (w *FakeWindow) IssueTick() [8]byte {
	var id [8]byte
	rand.Read(id[:])
	w.SetTickAge(id, 0, true)
	return id
}

// SetTickAge sets what TickAge returns for id (issued or not).
func (w *FakeWindow) SetTickAge(id [8]byte, age time.Duration, ok bool) {
	w.mu.Lock()
	w.ticks[id] = tickAge{age, ok}
	w.mu.Unlock()
}

// TickAge returns what IssueTick/SetTickAge set for id; unknown ids are !ok.
func (w *FakeWindow) TickAge(id [8]byte) (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, found := w.ticks[id]
	if !found || !t.ok {
		return 0, false
	}
	return t.age, true
}

// Host returns the window's Host: a fresh *FakeHost unless SetHost replaced it.
func (w *FakeWindow) Host() capability.Host {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.host
}

// SetHost replaces the Host (share one *FakeHost between windows to model the
// gateway-wide state).
func (w *FakeWindow) SetHost(h capability.Host) {
	w.mu.Lock()
	w.host = h
	w.mu.Unlock()
}

// SetPublicKey sets what PublicKey returns (copied). Until it is called
// PublicKey returns nil; the real window always returns 65 bytes.
func (w *FakeWindow) SetPublicKey(pk []byte) {
	w.mu.Lock()
	w.pk = append([]byte(nil), pk...)
	w.mu.Unlock()
}

// PublicKey returns a copy of the SetPublicKey value.
func (w *FakeWindow) PublicKey() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pk == nil {
		return nil
	}
	return append([]byte(nil), w.pk...)
}

// FakeHost is a settable capability.Host. Unlike the real one it honours
// SetSummonLinkDegraded from any caller and never resets it by itself.
type FakeHost struct {
	mu       sync.Mutex
	open     bool
	degraded bool
}

var _ capability.Host = (*FakeHost)(nil)

func NewFakeHost() *FakeHost { return &FakeHost{} }

func (h *FakeHost) SetSummonOpen(v bool) {
	h.mu.Lock()
	h.open = v
	h.mu.Unlock()
}

func (h *FakeHost) SummonOpen() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.open
}

func (h *FakeHost) SummonLinkDegraded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.degraded
}

func (h *FakeHost) SetSummonLinkDegraded(v bool) {
	h.mu.Lock()
	h.degraded = v
	h.mu.Unlock()
}

func (w *FakeWindow) record(t byte, body []byte) {
	w.sent[t] = append(w.sent[t], append([]byte(nil), body...))
}

func (w *FakeWindow) Send(t byte, body []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return capability.ErrClosed
	}
	w.record(t, body)
	return nil
}

func (w *FakeWindow) TrySend(t byte, body []byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.failTry {
		return false
	}
	w.record(t, body)
	return true
}

func (w *FakeWindow) QueuedBytes() int     { return int(w.queued.Load()) }
func (w *FakeWindow) SetQueuedBytes(n int) { w.queued.Store(int64(n)) }

func (w *FakeWindow) FailTrySend(v bool) {
	w.mu.Lock()
	w.failTry = v
	w.mu.Unlock()
}

func (w *FakeWindow) SetDetail(d string) {
	w.mu.Lock()
	w.detail = d
	w.mu.Unlock()
}

// LogEvent records event (no bounds are enforced).
func (w *FakeWindow) LogEvent(event string) {
	w.mu.Lock()
	w.events = append(w.events, event)
	w.mu.Unlock()
}

// Events returns the LogEvent values, in order.
func (w *FakeWindow) Events() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.events...)
}

// Detail returns the last SetDetail value.
func (w *FakeWindow) Detail() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.detail
}

// Close is idempotent; the first reason is kept and Done is closed once.
func (w *FakeWindow) Close(reason string) {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.reason = reason
		w.closed = true
		w.mu.Unlock()
		close(w.done)
	})
}

func (w *FakeWindow) CloseReason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

func (w *FakeWindow) Done() <-chan struct{}  { return w.done }
func (w *FakeWindow) KeyFingerprint() string { return "fake-fingerprint" }

// Sent returns the bodies sent with type t, in order.
func (w *FakeWindow) Sent(t byte) [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.sent[t]...)
}
