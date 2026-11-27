//go:build !linux && !darwin

// Package peercred reports the uid at the other end of a unix socket.
package peercred

import (
	"errors"
	"net"
)

// PeerUID has no peer credentials on this system, so every peer is refused.
func PeerUID(net.Conn) (int, error) {
	return -1, errors.New("peercred: peer credentials are not supported on this OS")
}
