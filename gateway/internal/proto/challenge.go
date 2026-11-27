package proto

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"math/big"
)

// ChallengeLen is the full challenge length; ChallengeBodyLen excludes sig.
const (
	ChallengeLen     = 185
	ChallengeBodyLen = 121
)

// Challenge is the car's reply to hello.
type Challenge struct {
	N        [16]byte
	PkE      [65]byte
	ExpiryMS uint64
	Binding  [32]byte
	Sig      [64]byte
}

// Binding returns SHA-256("airgapp-gw/1 car" || vin).
func Binding(vin string) [32]byte {
	return sha256.Sum256([]byte("airgapp-gw/1 car" + vin))
}

// BodyBytes is the challenge without the signature.
func (c Challenge) BodyBytes() []byte {
	b := make([]byte, 0, ChallengeLen)
	b = append(b, c.N[:]...)
	b = append(b, c.PkE[:]...)
	b = binary.BigEndian.AppendUint64(b, c.ExpiryMS)
	return append(b, c.Binding[:]...)
}

// Bytes encodes the full challenge.
func (c Challenge) Bytes() []byte { return append(c.BodyBytes(), c.Sig[:]...) }

// ParseChallenge decodes a challenge and validates pkE.
func ParseChallenge(b []byte) (Challenge, error) {
	var c Challenge
	if len(b) != ChallengeLen {
		return c, errors.New("proto: bad challenge length")
	}
	copy(c.N[:], b[:16])
	copy(c.PkE[:], b[16:81])
	c.ExpiryMS = binary.BigEndian.Uint64(b[81:89])
	copy(c.Binding[:], b[89:121])
	copy(c.Sig[:], b[121:])
	if _, err := ecdh.P256().NewPublicKey(c.PkE[:]); err != nil {
		return Challenge{}, errors.New("proto: pkE not on curve")
	}
	return c, nil
}

// SignedDigest is SHA-256("airgapp-gw/1 challenge" || hello || challenge-without-sig).
func SignedDigest(hello []byte, c Challenge) [32]byte {
	h := sha256.New()
	h.Write([]byte("airgapp-gw/1 challenge"))
	h.Write(hello)
	h.Write(c.BodyBytes())
	var d [32]byte
	h.Sum(d[:0])
	return d
}

type rs struct{ R, S *big.Int }

// SignChallenge fills c.Sig with a raw r||s ECDSA P-256 signature.
func SignChallenge(k *ecdsa.PrivateKey, hello []byte, c *Challenge) error {
	return signRaw(k, SignedDigest(hello, *c), &c.Sig)
}

// signRaw signs digest d with k into sig as raw r||s.
func signRaw(k *ecdsa.PrivateKey, d [32]byte, sig *[64]byte) error {
	der, err := ecdsa.SignASN1(rand.Reader, k, d[:])
	if err != nil {
		return err
	}
	var v rs
	if _, err := asn1.Unmarshal(der, &v); err != nil {
		return err
	}
	v.R.FillBytes(sig[:32])
	v.S.FillBytes(sig[32:])
	return nil
}

// VerifyChallenge checks the signature over hello and the challenge body.
func VerifyChallenge(pub *ecdsa.PublicKey, hello []byte, c Challenge) bool {
	d := SignedDigest(hello, c)
	der, err := asn1.Marshal(rs{new(big.Int).SetBytes(c.Sig[:32]), new(big.Int).SetBytes(c.Sig[32:])})
	if err != nil {
		return false
	}
	return ecdsa.VerifyASN1(pub, d[:], der)
}
