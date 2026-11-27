// Package clock provides the boot-relative millisecond clock used by the
// gateway, with a controllable fake for tests.
package clock

import "sync"

// Clock reports milliseconds since boot.
type Clock interface {
	BootMS() uint64
}

// Fake is a manually advanced Clock, safe for concurrent use.
type Fake struct {
	mu sync.Mutex
	ms uint64
}

// fakeStartMS is the fixed initial BootMS of a Fake.
const fakeStartMS = 1_000_000

// NewFake returns a Fake starting at a fixed BootMS.
func NewFake() *Fake { return &Fake{ms: fakeStartMS} }

// BootMS returns the current fake time.
func (f *Fake) BootMS() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ms
}

// Advance moves the fake time forward by ms milliseconds.
func (f *Fake) Advance(ms uint64) {
	f.mu.Lock()
	f.ms += ms
	f.mu.Unlock()
}
