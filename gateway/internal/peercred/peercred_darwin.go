// Package peercred reports the uid at the other end of a unix socket.
package peercred

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID returns the uid of the process at the other end of a unix socket (LOCAL_PEERCRED; desk tests only): for an
// accepted connection, the uid that called connect(); for a dialled one, the uid that called listen().
func PeerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("peercred: not a unix socket")
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
