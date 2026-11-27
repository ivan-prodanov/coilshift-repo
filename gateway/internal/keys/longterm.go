// Package keys holds the gateway's long-term P-256 identity key and the
// fingerprint the logs show for a phone key.
package keys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Generate creates a new PKCS#8 PEM P-256 key at path with mode 0400. It
// refuses to overwrite an existing file.
func Generate(path string) error {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	_, werr := f.Write(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(path)
		return errors.Join(werr, cerr)
	}
	return nil
}

// Load reads the key at path. The file mode must be exactly 0400.
func Load(path string) (*ecdsa.PrivateKey, error) {
	return loadWithUID(path, uint32(os.Getuid()))
}

// loadWithUID is Load with the required owner uid injectable for tests.
func loadWithUID(path string, uid uint32) (*ecdsa.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0o400 || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, fmt.Errorf("keys: %s must be a regular file with mode 0400, got %v", path, st.Mode())
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != uid {
		return nil, fmt.Errorf("keys: %s must be owned by uid %d", path, uid)
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, rerr := f.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if len(buf) > 1<<16 {
			return nil, errors.New("keys: file too large")
		}
		if rerr != nil {
			break
		}
	}
	blk, _ := pem.Decode(buf)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, errors.New("keys: not a PKCS#8 PEM key")
	}
	pk, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	k, ok := pk.(*ecdsa.PrivateKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, errors.New("keys: not a P-256 key")
	}
	return k, nil
}

// PinString is what the phone pins: version, uncompressed public key, VIN.
// It carries public material only.
func PinString(pub *ecdsa.PublicKey, vin string) string {
	return "airgapp-gw:1:" + hex.EncodeToString(elliptic.Marshal(elliptic.P256(), pub.X, pub.Y)) + ":" + vin
}

// Fingerprint is the first 8 bytes of SHA-256(pk), hex.
func Fingerprint(pk []byte) string {
	h := sha256.Sum256(pk)
	return hex.EncodeToString(h[:8])
}
