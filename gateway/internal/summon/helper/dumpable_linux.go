//go:build linux

package helper

import "golang.org/x/sys/unix"

// DisableDumps clears the process's dumpable flag (prctl PR_SET_DUMPABLE 0): the kernel then writes no core dump of
// it, and other processes of its uid can neither ptrace it nor read its /proc/<pid>/mem. The helper holds its
// long-term key. It also sets RLIMIT_CORE to 0, as agw does, because a non-dumpable process is still dumped when
// fs.suid_dumpable is 2. It is idempotent: summonhelper's serve calls it before it loads the key, and Serve again.
func DisableDumps() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return err
	}
	return unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0})
}
