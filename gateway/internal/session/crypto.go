// Package session implements the gateway's HPKE session crypto: the
// phone->car receiver context (Auth mode) and the car->phone AEAD stream
// keyed from the HPKE exporter.
package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"encoding/binary"
	"errors"
	"sync"

	"github.com/cloudflare/circl/hpke"
)

// Suite is HPKE Auth mode with DHKEM(P-256, HKDF-SHA256), HKDF-SHA256 and
// AES-256-GCM. aad is empty everywhere.
var Suite = hpke.NewSuite(hpke.KEM_P256_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES256GCM)

const (
	encLen = 65
	tagLen = 16

	labelS2CKey   = "airgapp-gw/1 s2c key"
	labelS2CNonce = "airgapp-gw/1 s2c nonce"
)

var errOpen = errors.New("session: open failed")

var kemScheme = hpke.KEM_P256_HKDF_SHA256.Scheme()

// Inbound is the phone->car receiver context. Frames must be opened strictly
// in order; any failure is permanent.
type Inbound struct {
	mu       sync.Mutex
	opener   hpke.Opener
	failed   bool
	outTaken bool // NewOutbound already called
}

// OpenArm sets up the receiver from the first frame (enc || ciphertext) and
// returns the context and the first plaintext.
func OpenArm(skE *ecdh.PrivateKey, pkS []byte, info, firstFrame []byte) (*Inbound, []byte, error) {
	if len(firstFrame) < encLen+tagLen {
		return nil, nil, errOpen
	}
	sk, err := kemScheme.UnmarshalBinaryPrivateKey(skE.Bytes())
	if err != nil {
		return nil, nil, errOpen
	}
	pk, err := kemScheme.UnmarshalBinaryPublicKey(pkS)
	if err != nil {
		return nil, nil, errOpen
	}
	r, err := Suite.NewReceiver(sk, info)
	if err != nil {
		return nil, nil, errOpen
	}
	op, err := r.SetupAuth(firstFrame[:encLen], pk)
	if err != nil {
		return nil, nil, errOpen
	}
	in := &Inbound{opener: op}
	pt, err := in.Open(firstFrame[encLen:])
	if err != nil {
		return nil, nil, errOpen
	}
	return in, pt, nil
}

// Open decrypts the next phone->car frame. After any error every further call
// fails.
func (in *Inbound) Open(ct []byte) ([]byte, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.failed {
		return nil, errOpen
	}
	pt, err := in.opener.Open(ct, nil)
	if err != nil {
		in.failed = true
		return nil, errOpen
	}
	return pt, nil
}

// s2cKeys derives key and base nonce from an HPKE context's exporter.
func s2cKeys(c hpke.Context) (cipher.AEAD, []byte, error) {
	key := c.Export([]byte(labelS2CKey), 32)
	base := c.Export([]byte(labelS2CNonce), 12)
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, nil, err
	}
	return g, base, nil
}

func s2cNonce(base []byte, ctr uint64) []byte {
	n := append([]byte(nil), base...)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], ctr)
	for i := 0; i < 8; i++ {
		n[4+i] ^= c[i]
	}
	return n
}

// Outbound seals car->phone frames.
type Outbound struct {
	mu   sync.Mutex
	aead cipher.AEAD
	base []byte
	ctr  uint64
	done bool // counter exhausted
}

// NewOutbound derives the car->phone stream from the receiver context.
// It may be called once per Inbound (a second call would reuse key and
// nonces) and fails once the Inbound has failed.
func NewOutbound(in *Inbound) (*Outbound, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.failed {
		return nil, errOpen
	}
	if in.outTaken {
		return nil, errors.New("session: outbound already created")
	}
	g, base, err := s2cKeys(in.opener)
	if err != nil {
		return nil, err
	}
	in.outTaken = true
	return &Outbound{aead: g, base: base}, nil
}

// Seal encrypts the next car->phone frame. It panics rather than reuse a
// nonce if the 2^64 counter space is exhausted (unreachable in practice).
func (o *Outbound) Seal(pt []byte) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		panic("session: outbound counter exhausted")
	}
	ct := o.aead.Seal(nil, s2cNonce(o.base, o.ctr), pt, nil)
	if o.ctr == ^uint64(0) {
		o.done = true
	} else {
		o.ctr++
	}
	return ct
}
