package camhelper

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Darwin is for desk tests only: no Pdeathsig.
func sysProcAttr(cred *syscall.Credential) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: cred}
}

// PeerUID returns the uid of the process on the other end of a unix socket
// (LOCAL_PEERCRED).
func PeerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("camhelper: not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var x *unix.Xucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		x, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(x.Uid), nil
}

func killGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }
