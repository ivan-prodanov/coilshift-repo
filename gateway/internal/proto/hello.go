// Package proto holds the gateway's plaintext handshake messages, signature
// and transcript rules, and message type constants.
package proto

import (
	"bytes"
	"crypto/ecdh"
	"errors"
)

// Capability ids.
const (
	CapSummon  byte = 0x01
	CapCamera  byte = 0x02
	CapDashcam byte = 0x03
	CapVcmd    byte = 0x04 // Direct commands (capability/vcmd)
	CapEcho    byte = 0x7F // test only
)

// Plaintext message types (first byte of the HPKE plaintext).
const (
	TypeArm       byte = 0x01
	TypeHeartbeat byte = 0x02
	TypeClose     byte = 0x03
	TypeReady     byte = 0x80
	TypeBye       byte = 0x81
	TypeTick      byte = 0x82 // reserved for the core (SUMMON plan); never in a Spec.Out
)

// HelloLen is the length of a hello message.
const HelloLen = 71

// Hello versions. The version byte picks the CHALLENGE the car answers with:
// HelloV1 a v1 CHALLENGE (ChallengeLen, signed by the key the phone pinned),
// HelloV2 a v2 CHALLENGE that also carries the signing key itself
// (ChallengeV2Len), so a phone can trust the car's identity on first connect.
const (
	HelloV1 byte = 0x01
	HelloV2 byte = 0x02
)

var helloMagic = []byte("AGW1")

// Hello is the phone's opening message. A zero Version encodes as HelloV1.
type Hello struct {
	Version byte
	Cap     byte
	PkS     [65]byte
}

// ParseHello validates and decodes a hello.
func ParseHello(b []byte) (Hello, error) {
	var h Hello
	if len(b) != HelloLen {
		return h, errors.New("proto: bad hello length")
	}
	if !bytes.Equal(b[:4], helloMagic) {
		return h, errors.New("proto: bad hello magic")
	}
	if b[4] != HelloV1 && b[4] != HelloV2 {
		return h, errors.New("proto: bad hello version")
	}
	switch b[5] {
	case CapSummon, CapCamera, CapDashcam, CapVcmd, CapEcho:
	default:
		return h, errors.New("proto: unknown capability")
	}
	if b[6] != 0x04 {
		return h, errors.New("proto: pkS not uncompressed")
	}
	if _, err := ecdh.P256().NewPublicKey(b[6:]); err != nil {
		return h, errors.New("proto: pkS not on curve")
	}
	h.Version = b[4]
	h.Cap = b[5]
	copy(h.PkS[:], b[6:])
	return h, nil
}

// Bytes encodes the hello.
func (h Hello) Bytes() []byte {
	b := make([]byte, 0, HelloLen)
	b = append(b, helloMagic...)
	v := h.Version
	if v == 0 {
		v = HelloV1
	}
	b = append(b, v, h.Cap)
	return append(b, h.PkS[:]...)
}
