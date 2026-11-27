//go:build linux

package clock

import "golang.org/x/sys/unix"

type bootClock struct{}

// Boot returns the real clock: CLOCK_BOOTTIME, which keeps counting across
// suspend.
func Boot() Clock { return bootClock{} }

func (bootClock) BootMS() uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		panic("clock: CLOCK_BOOTTIME unavailable: " + err.Error())
	}
	return uint64(ts.Sec)*1000 + uint64(ts.Nsec)/1_000_000
}
