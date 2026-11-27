package session

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"errors"

	"github.com/cloudflare/circl/hpke"
)

// TestSealer is the phone side of a session: it seals phone->car frames and
// opens car->phone frames in strict order. It exists for tests and the Go
// bench client.
type TestSealer struct {
	sealer hpke.Sealer
	aead   cipher.AEAD
	base   []byte
	ctr    uint64
}

// TestSender starts a phone-side session towards the car's ephemeral key pkE,
// authenticated with skS. enc must be prepended to the first sealed frame.
func TestSender(skS *ecdh.PrivateKey, pkE []byte, info []byte) (enc []byte, s *TestSealer, err error) {
	sk, err := kemScheme.UnmarshalBinaryPrivateKey(skS.Bytes())
	if err != nil {
		return nil, nil, err
	}
	pk, err := kemScheme.UnmarshalBinaryPublicKey(pkE)
	if err != nil {
		return nil, nil, err
	}
	snd, err := Suite.NewSender(pk, info)
	if err != nil {
		return nil, nil, err
	}
	enc, sealer, err := snd.SetupAuth(rand.Reader, sk)
	if err != nil {
		return nil, nil, err
	}
	g, base, err := s2cKeys(sealer)
	if err != nil {
		return nil, nil, err
	}
	return enc, &TestSealer{sealer: sealer, aead: g, base: base}, nil
}

// Seal encrypts the next phone->car frame.
func (s *TestSealer) Seal(pt []byte) []byte {
	ct, err := s.sealer.Seal(pt, nil)
	if err != nil {
		panic(err)
	}
	return ct
}

// OpenS2C opens the next car->phone frame; a frame that fails does not
// advance the counter.
func (s *TestSealer) OpenS2C(ct []byte) ([]byte, error) {
	pt, err := s.aead.Open(nil, s2cNonce(s.base, s.ctr), ct, nil)
	if err != nil {
		return nil, errors.New("session: s2c open failed")
	}
	s.ctr++
	return pt, nil
}
