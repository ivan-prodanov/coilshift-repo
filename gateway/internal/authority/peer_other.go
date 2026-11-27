//go:build !linux && !darwin

package authority

import (
	"errors"
	"net"
)

// peerUID has no peer credentials here, so every helper is refused.
func peerUID(net.Conn) (int, error) {
	return -1, errors.New("peer credentials are not supported on this OS")
}
