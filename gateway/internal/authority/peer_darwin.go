package authority

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the uid at the other end of a unix connection (LOCAL_PEERCRED;
// desk tests only): for the dialling side, the uid that called listen().
func peerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("not a unix socket")
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
