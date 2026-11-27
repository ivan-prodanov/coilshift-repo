package camhelper

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func sysProcAttr(cred *syscall.Credential) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: cred, Pdeathsig: syscall.SIGKILL}
}

// PeerUID returns the uid of the process on the other end of a unix socket
// (SO_PEERCRED).
func PeerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("camhelper: not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}

func killGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }
