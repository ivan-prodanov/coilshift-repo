package client

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"net"
	"sync"

	"airgapp/gateway/internal/proto"
)

// maxNestChunk is the largest body InnerConn puts in one outer frame (the
// gateway core limits inbound bodies to 16384 bytes).
const maxNestChunk = 16384

// Handshake runs the full sign-in over conn (hello, challenge, HPKE ARM, READY)
// and returns the session. On error conn is left for the caller to close.
func Handshake(ctx context.Context, conn net.Conn, cap byte, params any, pinned *ecdsa.PublicKey, vin string,
	device *ecdh.PrivateKey) (*Session, error) {
	s, _, err := handshake(ctx, conn, proto.HelloV1, cap, params, pinned, vin, device)
	return s, err
}

// HandshakeV2 is Handshake with a version 2 hello (DialV2's rules for pinned):
// it returns the key the v2 challenge carried, for the caller to pin.
func HandshakeV2(ctx context.Context, conn net.Conn, cap byte, params any, pinned *ecdsa.PublicKey, vin string,
	device *ecdh.PrivateKey) (*Session, *ecdsa.PublicKey, error) {
	return handshake(ctx, conn, proto.HelloV2, cap, params, pinned, vin, device)
}

// InnerConn turns an open outer session into a byte stream: writes go out as
// frames of type in (split at 16384 bytes), frames of type out are read back,
// and other types are dropped. It owns outer.Recv from now on. Closing the
// conn closes outer.
func InnerConn(outer *Session, in, out byte) net.Conn {
	a, b := net.Pipe()
	c := &nestConn{Conn: a, b: b, outer: outer}

	// phone -> outer: whatever the inner session writes, framed.
	go func() {
		defer c.shutdown()
		buf := make([]byte, maxNestChunk)
		for {
			n, err := b.Read(buf)
			if n > 0 {
				if serr := outer.Send(in, buf[:n]); serr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// outer -> phone.
	go func() {
		defer c.shutdown()
		for {
			t, body, err := outer.Recv()
			if err != nil {
				return
			}
			if t != out {
				continue
			}
			if _, err := b.Write(body); err != nil {
				return
			}
		}
	}()
	return c
}

type nestConn struct {
	net.Conn          // the phone-facing pipe end
	b        net.Conn // the pump-facing end
	outer    *Session
	once     sync.Once
}

// shutdown ends both pipe ends and the outer session; either pump or Close
// may call it, the first wins.
func (c *nestConn) shutdown() {
	c.once.Do(func() {
		c.b.Close()
		c.Conn.Close()
		c.outer.Close()
	})
}

func (c *nestConn) Close() error {
	c.shutdown()
	return nil
}
