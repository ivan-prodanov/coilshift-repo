//go:build !linux

package supervise

import "syscall"

// childSysProcAttr puts the child in its own process group. There is no
// parent-death signal off Linux; the child's EPIPE on its liveness write is
// the only signal that the supervisor is gone.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
