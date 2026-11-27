package keys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pubBytes(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return elliptic.Marshal(elliptic.P256(), k.X, k.Y)
}

func TestKeyFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.pem")
	if err := Generate(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("0400 load: %v", err)
	}
	for _, m := range []os.FileMode{0o644, 0o600, 0o440} {
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("mode %o accepted", m)
		}
	}
}

func TestGenerateNoOverwrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.pem")
	if err := Generate(p); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	if err := Generate(p); err == nil {
		t.Fatal("overwrote")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("file changed")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o400 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestPinString(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.pem")
	if err := Generate(p); err != nil {
		t.Fatal(err)
	}
	k, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := PinString(&k.PublicKey, "5YJ3E1EA7KF000001")
	want := "airgapp-gw:1:" + hex.EncodeToString(elliptic.Marshal(elliptic.P256(), k.X, k.Y)) + ":5YJ3E1EA7KF000001"
	if s != want {
		t.Fatalf("got %s", s)
	}
	if strings.Contains(s, hex.EncodeToString(k.D.Bytes())) {
		t.Fatal("private material in pin")
	}
	if len(Fingerprint(pubBytes(t))) != 16 {
		t.Fatal("fingerprint length")
	}
}

func TestKeyFileOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.pem")
	if err := Generate(p); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Getuid())
	if _, err := loadWithUID(p, uid); err != nil {
		t.Fatalf("matching uid: %v", err)
	}
	if _, err := loadWithUID(p, uid+1); err == nil {
		t.Fatal("foreign owner accepted")
	}
}
