//go:build linux

package supervise

import "syscall"

// childSysProcAttr puts the child in its own process group (so no terminal or
// group-wide signal reaches it past the supervisor) and asks the kernel to
// SIGKILL it if the supervisor itself dies abruptly (SIGKILL, OOM). It must be
// SIGKILL, not SIGTERM: a hung or SIGSTOPped child keeps a SIGTERM pending
// forever, so the orphan would keep com.tesla.Webrtc and no later finish
// (runit's, or the restarted supervisor's) could Acquire it to send the abort.
// SIGKILL kills even a stopped process, and the bus daemon then releases the
// name.
//
// Pdeathsig fires when the OS thread that forked the child exits. Go threads
// exit only when a goroutine that called runtime.LockOSThread ends locked;
// the supervisor never does that.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
