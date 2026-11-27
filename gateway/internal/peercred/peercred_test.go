//go:build linux || darwin

package peercred

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestPeerUIDSelf: over a socketpair this process made, each end's peer is this process's uid.
func TestPeerUIDSelf(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f) // a dup: f is closed below either way
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		uid, err := PeerUID(c)
		if err != nil {
			t.Fatalf("end %d: PeerUID: %v", i, err)
		}
		if uid != os.Getuid() {
			t.Fatalf("end %d: PeerUID = %d, want %d", i, uid, os.Getuid())
		}
	}
}

// TestPeerUIDNotUnix: a connection that is not a unix socket has no peer uid.
func TestPeerUIDNotUnix(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if uid, err := PeerUID(a); err == nil {
		t.Fatalf("PeerUID on a pipe = %d, want an error", uid)
	}
}
