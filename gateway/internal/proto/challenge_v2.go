package proto

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"errors"
	"math/big"
)

// The v2 challenge, the answer to a HelloV2: the v1 body, then PkG, the
// uncompressed P-256 public key of the key that signs it (the gateway's, or
// the summon helper's, long-term key), then the signature. PkG is inside the
// signed body, and the digest has a domain of its own, so a v2 signature
// never verifies as a v1 one, nor the other way round.
const (
	ChallengeV2Len     = ChallengeLen + 65     // 250
	ChallengeV2BodyLen = ChallengeBodyLen + 65 // 186
)

// ChallengeV2 is the car's reply to a HelloV2.
type ChallengeV2 struct {
	N        [16]byte
	PkE      [65]byte
	ExpiryMS uint64
	Binding  [32]byte
	PkG      [65]byte // the signing key, uncompressed
	Sig      [64]byte
}

// v1 is c's v1 fields, which encode as the first ChallengeBodyLen bytes.
func (c ChallengeV2) v1() Challenge {
	return Challenge{N: c.N, PkE: c.PkE, ExpiryMS: c.ExpiryMS, Binding: c.Binding}
}

// BodyBytes is the v2 challenge without the signature: the v1 body || PkG.
func (c ChallengeV2) BodyBytes() []byte {
	b := make([]byte, 0, ChallengeV2Len)
	b = append(b, c.v1().BodyBytes()...)
	return append(b, c.PkG[:]...)
}

// Bytes encodes the full v2 challenge.
func (c ChallengeV2) Bytes() []byte { return append(c.BodyBytes(), c.Sig[:]...) }

// ParseChallengeV2 decodes a v2 challenge and validates pkE and PkG.
func ParseChallengeV2(b []byte) (ChallengeV2, error) {
	var c ChallengeV2
	if len(b) != ChallengeV2Len {
		return c, errors.New("proto: bad v2 challenge length")
	}
	v1 := make([]byte, 0, ChallengeLen)
	v1 = append(append(v1, b[:ChallengeBodyLen]...), b[ChallengeV2BodyLen:]...)
	c1, err := ParseChallenge(v1)
	if err != nil {
		return ChallengeV2{}, err
	}
	c.N, c.PkE, c.ExpiryMS, c.Binding, c.Sig = c1.N, c1.PkE, c1.ExpiryMS, c1.Binding, c1.Sig
	copy(c.PkG[:], b[ChallengeBodyLen:ChallengeV2BodyLen])
	if _, err := c.PublicKey(); err != nil {
		return ChallengeV2{}, err
	}
	return c, nil
}

// PublicKey is PkG as an ECDSA key; an error unless it is an uncompressed
// point on P-256.
func (c ChallengeV2) PublicKey() (*ecdsa.PublicKey, error) {
	if c.PkG[0] != 0x04 {
		return nil, errors.New("proto: pkG not uncompressed")
	}
	if _, err := ecdh.P256().NewPublicKey(c.PkG[:]); err != nil {
		return nil, errors.New("proto: pkG not on curve")
	}
	k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), c.PkG[:])
	if err != nil {
		return nil, errors.New("proto: pkG not on curve")
	}
	return k, nil
}

// SignedDigestV2 is SHA-256("airgapp-gw/2 challenge" || hello || v2-challenge-without-sig).
func SignedDigestV2(hello []byte, c ChallengeV2) [32]byte {
	h := sha256.New()
	h.Write([]byte("airgapp-gw/2 challenge"))
	h.Write(hello)
	h.Write(c.BodyBytes())
	var d [32]byte
	h.Sum(d[:0])
	return d
}

// SignChallengeV2 sets c.PkG to k's public key and fills c.Sig with a raw
// r||s ECDSA P-256 signature over SignedDigestV2.
func SignChallengeV2(k *ecdsa.PrivateKey, hello []byte, c *ChallengeV2) error {
	pub, err := k.PublicKey.Bytes()
	if err != nil {
		return err
	}
	if len(pub) != len(c.PkG) {
		return errors.New("proto: signing key is not P-256")
	}
	copy(c.PkG[:], pub)
	return signRaw(k, SignedDigestV2(hello, *c), &c.Sig)
}

// VerifyChallengeV2 checks the signature over hello and the v2 body with the
// key the challenge carries, and returns that key: the caller compares it
// with the key it pinned, or pins it on first connect. It returns nil when
// PkG is not a P-256 point or the signature does not verify.
func VerifyChallengeV2(hello []byte, c ChallengeV2) *ecdsa.PublicKey {
	pub, err := c.PublicKey()
	if err != nil {
		return nil
	}
	d := SignedDigestV2(hello, c)
	der, err := asn1.Marshal(rs{new(big.Int).SetBytes(c.Sig[:32]), new(big.Int).SetBytes(c.Sig[32:])})
	if err != nil || !ecdsa.VerifyASN1(pub, d[:], der) {
		return nil
	}
	return pub
}
