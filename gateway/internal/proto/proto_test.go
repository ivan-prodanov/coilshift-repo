package proto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

const vectorsPath = "../../testdata/vectors.json"

// seedScalar derives a valid P-256 scalar from SHA-256("agw test <name>"),
// retrying with a counter suffix until valid.
func seedScalar(name string) []byte {
	for i := 0; ; i++ {
		s := "agw test " + name
		if i > 0 {
			s += " " + strconv.Itoa(i)
		}
		sum := sha256.Sum256([]byte(s))
		if _, err := ecdh.P256().NewPrivateKey(sum[:]); err == nil {
			return sum[:]
		}
	}
}

func ecdhPub(t testing.TB, name string) (scalar []byte, pub [65]byte) {
	t.Helper()
	scalar = seedScalar(name)
	k, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		t.Fatal(err)
	}
	copy(pub[:], k.PublicKey().Bytes())
	return
}

func ecdsaKey(t testing.TB, name string) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), seedScalar(name))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pubBytes(k *ecdsa.PrivateKey) []byte {
	b, err := k.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	return b
}

func goodHello(t testing.TB) []byte {
	_, pk := ecdhPub(t, "device")
	return Hello{Cap: CapSummon, PkS: pk}.Bytes()
}

func TestHelloRejects(t *testing.T) {
	good := goodHello(t)
	if h, err := ParseHello(good); err != nil || h.Cap != CapSummon || h.Version != HelloV1 || !bytes.Equal(h.Bytes(), good) {
		t.Fatalf("good hello: %v", err)
	}
	if good[4] != 0x01 {
		t.Fatal("a zero Version must encode as version 0x01")
	}
	mut := func(f func([]byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	cases := map[string][]byte{
		"magic":      mut(func(b []byte) []byte { b[0] = 'X'; return b }),
		"ver0":       mut(func(b []byte) []byte { b[4] = 0; return b }),
		"ver3":       mut(func(b []byte) []byte { b[4] = 3; return b }),
		"short":      good[:70],
		"long":       append(append([]byte(nil), good...), 0),
		"cap":        mut(func(b []byte) []byte { b[5] = 0x55; return b }),
		"cap0":       mut(func(b []byte) []byte { b[5] = 0; return b }),
		"off-curve":  mut(func(b []byte) []byte { b[70] ^= 1; return b }),
		"compressed": mut(func(b []byte) []byte { b[6] = 0x02; return b }),
	}
	for name, b := range cases {
		if _, err := ParseHello(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func buildChallenge(t testing.TB, hello []byte) (Challenge, *ecdsa.PrivateKey) {
	gk := ecdsaKey(t, "gateway")
	_, pkE := ecdhPub(t, "ephemeral")
	c := Challenge{PkE: pkE, ExpiryMS: 123456789, Binding: Binding("TESTVIN0000000000")}
	for i := range c.N {
		c.N[i] = byte(0xA0 + i)
	}
	if err := SignChallenge(gk, hello, &c); err != nil {
		t.Fatal(err)
	}
	return c, gk
}

func TestChallengeSignVerify(t *testing.T) {
	hello := goodHello(t)
	c, gk := buildChallenge(t, hello)
	if !VerifyChallenge(&gk.PublicKey, hello, c) {
		t.Fatal("valid signature rejected")
	}
	if len(c.Bytes()) != 185 {
		t.Fatalf("challenge len %d", len(c.Bytes()))
	}
	p, err := ParseChallenge(c.Bytes())
	if err != nil || p != c {
		t.Fatalf("parse roundtrip: %v", err)
	}
	for i := range hello {
		h := append([]byte(nil), hello...)
		h[i] ^= 1
		if VerifyChallenge(&gk.PublicKey, h, c) {
			t.Fatalf("hello byte %d flip accepted", i)
		}
	}
	raw := c.Bytes()
	for i := range raw {
		b := append([]byte(nil), raw...)
		b[i] ^= 1
		f, err := ParseChallenge(b)
		if err != nil {
			continue // pkE no longer on curve: also rejected
		}
		if VerifyChallenge(&gk.PublicKey, hello, f) {
			t.Fatalf("challenge byte %d flip accepted", i)
		}
	}
	other := ecdsaKey(t, "other")
	if VerifyChallenge(&other.PublicKey, hello, c) {
		t.Fatal("wrong key accepted")
	}
	if _, err := ParseChallenge(raw[:184]); err == nil {
		t.Fatal("short challenge accepted")
	}
}

func TestInfoLength(t *testing.T) {
	info := Info([]byte("h"), []byte("c"))
	if len(info) != 45 || string(info[:13]) != "airgapp-gw/1\x00" {
		t.Fatalf("info %x", info)
	}
	want := sha256.Sum256([]byte("hc"))
	if !bytes.Equal(info[13:], want[:]) {
		t.Fatal("info hash")
	}
}

type vectors struct {
	Note           string `json:"note"`
	VIN            string `json:"vin"`
	DeviceSk       string `json:"device_sk"`
	DevicePk       string `json:"device_pk"`
	GatewaySk      string `json:"gateway_sk"`
	GatewayPk      string `json:"gateway_pk"`
	EphemeralSk    string `json:"ephemeral_sk"`
	EphemeralPk    string `json:"ephemeral_pk"`
	N              string `json:"n"`
	ExpiryMS       uint64 `json:"expiry_ms"`
	Hello          string `json:"hello"`
	ChallengeNoSig string `json:"challenge_no_sig"`
	SignedDigest   string `json:"signed_digest"`
	Signature      string `json:"signature"`
	Challenge      string `json:"challenge"`
	Binding        string `json:"binding"`
	Info           string `json:"info"`
}

func hx(b []byte) string { return hex.EncodeToString(b) }

func computeVectors(t testing.TB) (vectors, Challenge) {
	hello := goodHello(t)
	c, gk := buildChallenge(t, hello)
	dsk, dpk := ecdhPub(t, "device")
	esk, epk := ecdhPub(t, "ephemeral")
	d := SignedDigest(hello, c)
	return vectors{
		Note: "Keys are derived as SHA-256(\"agw test <name>\") (retry with \" <i>\" suffix until a valid scalar). " +
			"ECDSA signing in Go is randomized: signature is STORED here, not re-derived; tests verify it " +
			"against gateway_pk and recompute every other field byte for byte. Test-only keys; never use in production.",
		VIN:      "TESTVIN0000000000",
		DeviceSk: hx(dsk), DevicePk: hx(dpk[:]),
		GatewaySk: hx(seedScalar("gateway")), GatewayPk: hx(pubBytes(gk)),
		EphemeralSk: hx(esk), EphemeralPk: hx(epk[:]),
		N: hx(c.N[:]), ExpiryMS: c.ExpiryMS,
		Hello:          hx(hello),
		ChallengeNoSig: hx(c.BodyBytes()),
		SignedDigest:   hx(d[:]),
		Signature:      hx(c.Sig[:]),
		Challenge:      hx(c.Bytes()),
		Binding:        hx(c.Binding[:]),
		Info:           hx(Info(hello, c.Bytes())),
	}, c
}

func TestWriteGoldenVectors(t *testing.T) {
	if !*update {
		t.Skip("run with -update to rewrite testdata/vectors.json")
	}
	v, _ := computeVectors(t)
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(vectorsPath, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var golden vectors
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	got, _ := computeVectors(t)
	// Signature is randomized; check it separately and align before comparing.
	sigBytes, err := hex.DecodeString(golden.Signature)
	if err != nil || len(sigBytes) != 64 {
		t.Fatalf("golden signature: %v", err)
	}
	pubRaw, _ := hex.DecodeString(golden.GatewayPk)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubRaw)
	if err != nil {
		t.Fatal(err)
	}
	helloB, _ := hex.DecodeString(golden.Hello)
	chB, _ := hex.DecodeString(golden.Challenge)
	ch, err := ParseChallenge(chB)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyChallenge(pub, helloB, ch) || !bytes.Equal(ch.Sig[:], sigBytes) {
		t.Fatal("golden signature does not verify")
	}
	got.Signature, got.Challenge = golden.Signature, golden.Challenge
	// info hashes the full challenge (signature included), so recompute it from the golden challenge.
	got.Info = hx(Info(helloB, chB))
	if got != golden {
		gb, _ := json.MarshalIndent(got, "", " ")
		wb, _ := json.MarshalIndent(golden, "", " ")
		t.Fatalf("vectors differ\ngot:\n%s\nwant:\n%s", gb, wb)
	}
	if len(helloB) != 71 || len(chB) != 185 {
		t.Fatal("lengths")
	}
}
