//go:build !linux

package clock

import "time"

type bootClock struct{ start time.Time }

// Boot returns a monotonic fallback clock counting from process start; it is
// for development hosts only (the car build is linux and uses CLOCK_BOOTTIME).
func Boot() Clock { return bootClock{start: time.Now()} }

func (b bootClock) BootMS() uint64 { return uint64(time.Since(b.start).Milliseconds()) }
