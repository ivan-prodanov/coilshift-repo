//go:build linux

package helper

import (
	"testing"

	"golang.org/x/sys/unix"
)

// On Linux, after DisableDumps the process is not dumpable, and its core size limit is 0.
func TestDisableDumps(t *testing.T) {
	if err := DisableDumps(); err != nil {
		t.Fatalf("DisableDumps: %v", err)
	}
	d, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || d != 0 {
		t.Fatalf("PR_GET_DUMPABLE = %d, %v; want 0", d, err)
	}
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &rl); err != nil || rl.Cur != 0 || rl.Max != 0 {
		t.Fatalf("RLIMIT_CORE = %+v, %v; want 0, 0", rl, err)
	}
}
