package client

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"

	"airgapp/gateway/internal/wire"
)

// The inbound cap matches the phone's: the largest Spec.Out body (1 MiB)
// plus the type byte and the AES-GCM tag.
func TestInboundFrameCap(t *testing.T) {
	for _, tc := range []struct {
		n        uint32
		tooLarge bool
	}{{1<<20 + 1 + 16, false}, {1<<20 + 1 + 16 + 1, true}} {
		a, b := net.Pipe()
		s := &Session{conn: a, done: make(chan struct{})}
		go func() {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], tc.n)
			b.Write(hdr[:])
			b.Close() // no body: an accepted length then hits EOF
		}()
		_, _, err := s.recv()
		if got := errors.Is(err, wire.ErrTooLarge); got != tc.tooLarge {
			t.Fatalf("length %d: err %v", tc.n, err)
		}
		a.Close()
	}
}
