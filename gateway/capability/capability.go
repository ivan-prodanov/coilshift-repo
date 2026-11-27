// Package capability is the public API a gateway capability (CAMERA, DASHCAM,
// SUMMON, ECHO) implements. It is a leaf package: it imports no internal code.
package capability

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrClosed is returned by Window.Send once the window is closed.
var ErrClosed = errors.New("capability: window closed")

type Spec struct {
	Name    string        // "CAMERA", "DASHCAM", "SUMMON", "ECHO"
	ID      byte          // the hello cap byte
	In      map[byte]int  // allowed inbound types (besides ARM/HEARTBEAT/CLOSE) → max body bytes
	Out     map[byte]int  // allowed outbound types (besides READY/BYE) → max body bytes
	LeaseMS int           // 0 = no lease; CAMERA and DASHCAM use 20000
	TickHz  int           // 0 = no TICKs; 1..50 = the core sends TICK (0x82) at this rate (SUMMON: 20)
	Bulk    map[byte]bool // outbound types that go on the bulk lane (H264 AUs, DASHCAM chunks);
	// every other outbound type, and BYE, uses the priority lane and is
	// written before any queued bulk frame
}

// Window is the capability's handle to the session. After Close (or any
// teardown) Send returns ErrClosed and TrySend returns false; nothing is sent.
// Send/TrySend take ownership of body: the caller must not modify it after the
// call. The body passed to Handler.Frame is valid only until Frame returns;
// copy it to keep it.
type Window interface {
	Send(t byte, body []byte) error   // blocks while that lane is full
	TrySend(t byte, body []byte) bool // false if that lane is full (media drops)
	QueuedBytes() int                 // bulk-lane depth in bytes; a lock-free atomic read; bulk cap 4 MiB
	SetDetail(detail string)          // session-log detail, e.g. "front,rear"; no media
	LogEvent(event string)            // appends {"t": ms since window start, "e": event} to this window's session-log record; ≤ 64 per window, each ≤ 128 bytes (longer is truncated, extra dropped); never media
	Close(reason string)              // starts teardown; idempotent. Only fixed reasons reach the wire; others are sent as "error"
	Done() <-chan struct{}
	KeyFingerprint() string
	PublicKey() []byte // a copy of the authenticated phone key pkS (65 bytes, uncompressed P-256)
	// TickAge reports how long ago (on the core's BOOTTIME clock) the TICK
	// with this id was issued. ok is true only for one of the last 64 ticks
	// issued in this window; an unknown, expired or other window's id is !ok.
	TickAge(id [8]byte) (age time.Duration, ok bool)
	Host() Host // cross-capability state shared by every window
}

// Host is gateway-wide state shared between capabilities. Every method is
// safe for concurrent use and never blocks on I/O.
type Host interface {
	// SummonOpen is true while a SUMMON (ID 0x01) window holds its slot.
	SummonOpen() bool
	// SummonLinkDegraded is SUMMON's report that its link is degraded. It
	// resets to false when the SUMMON window frees its slot.
	SummonLinkDegraded() bool
	// SetSummonLinkDegraded is honoured only when called through the SUMMON
	// window that holds the slot; from any other window it does nothing.
	SetSummonLinkDegraded(bool)
}

type Handler interface {
	Frame(t byte, body []byte) error // non-nil → teardown with BYE reason "error" (the error text is only logged on the car)
	Teardown(reason string)          // called exactly once, after reads stop and before BYE
}

// Capability is what a capability implements. The core calls PreArm and Open
// on the connection goroutine with a context bounded by a fixed budget
// (PreArm 1 s, Open 2 s; also cancelled by shutdown). Neither may block past
// its context: a capability that is slow to become ready returns from Open at
// once and reports readiness with its own STATUS frame. An error, a panic or
// an overrun closes the connection without a message and frees the slot.
type Capability interface {
	Spec() Spec
	PreArm(ctx context.Context, pkS []byte) error // extra gates; SUMMON adds R1/proximity; CAMERA returns nil
	Open(ctx context.Context, w Window, params json.RawMessage) (Handler, error)
}
