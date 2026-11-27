package proto

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateV2 = flag.Bool("update-v2", false, "rewrite testdata/vectors-v2.json")

const vectorsV2Path = "../../testdata/vectors-v2.json"

func goodHelloV2(t testing.TB) []byte {
	_, pk := ecdhPub(t, "device")
	return Hello{Version: HelloV2, Cap: CapSummon, PkS: pk}.Bytes()
}

func buildChallengeV2(t testing.TB, hello []byte) (ChallengeV2, *ecdsa.PrivateKey) {
	gk := ecdsaKey(t, "gateway")
	_, pkE := ecdhPub(t, "ephemeral")
	c := ChallengeV2{PkE: pkE, ExpiryMS: 123456789, Binding: Binding("TESTVIN0000000000")}
	for i := range c.N {
		c.N[i] = byte(0xA0 + i)
	}
	if err := SignChallengeV2(gk, hello, &c); err != nil {
		t.Fatal(err)
	}
	return c, gk
}

func TestHelloV2(t *testing.T) {
	hello := goodHelloV2(t)
	if len(hello) != HelloLen || hello[4] != 0x02 {
		t.Fatalf("v2 hello %x", hello)
	}
	h, err := ParseHello(hello)
	if err != nil || h.Version != HelloV2 || h.Cap != CapSummon || !bytes.Equal(h.Bytes(), hello) {
		t.Fatalf("v2 hello: %+v %v", h, err)
	}
	// Only the version byte differs from v1's.
	v1 := goodHello(t)
	if !bytes.Equal(v1[:4], hello[:4]) || !bytes.Equal(v1[5:], hello[5:]) {
		t.Fatal("a v2 hello differs from v1's beyond the version byte")
	}
}

func TestChallengeV2RoundTrip(t *testing.T) {
	hello := goodHelloV2(t)
	c, gk := buildChallengeV2(t, hello)
	raw := c.Bytes()
	if len(raw) != 250 || ChallengeV2Len != 250 || len(c.BodyBytes()) != 186 || ChallengeV2BodyLen != 186 {
		t.Fatalf("lengths: challenge %d, body %d", len(raw), len(c.BodyBytes()))
	}
	// The v2 body is v1's body, then PkG, the signing key.
	v1 := Challenge{N: c.N, PkE: c.PkE, ExpiryMS: c.ExpiryMS, Binding: c.Binding}
	if !bytes.Equal(raw[:ChallengeBodyLen], v1.BodyBytes()) || !bytes.Equal(raw[ChallengeBodyLen:186], pubBytes(gk)) ||
		!bytes.Equal(raw[186:], c.Sig[:]) {
		t.Fatal("layout: want v1 body || pkG || sig")
	}
	p, err := ParseChallengeV2(raw)
	if err != nil || p != c {
		t.Fatalf("parse roundtrip: %v", err)
	}
	pub := VerifyChallengeV2(hello, p)
	if pub == nil || !pub.Equal(&gk.PublicKey) {
		t.Fatal("valid v2 challenge rejected, or the wrong key returned")
	}
	for _, n := range []int{249, 251, ChallengeLen} {
		b := make([]byte, n)
		if _, err := ParseChallengeV2(b); err == nil {
			t.Fatalf("a %d-byte v2 challenge accepted", n)
		}
	}
	if _, err := ParseChallenge(raw); err == nil {
		t.Fatal("ParseChallenge accepted a v2 challenge")
	}
}

// A different PkG, even a valid key, fails: PkG is signed. So does any flipped
// bit, of the hello or the challenge.
func TestChallengeV2Tampered(t *testing.T) {
	hello := goodHelloV2(t)
	c, _ := buildChallengeV2(t, hello)
	other := ecdsaKey(t, "other")
	swapped := c
	copy(swapped.PkG[:], pubBytes(other))
	if VerifyChallengeV2(hello, swapped) != nil {
		t.Fatal("a swapped pkG verified")
	}
	// An attacker's own key and signature verify only as that key: the caller's pin check rejects it.
	forged := ChallengeV2{N: c.N, PkE: c.PkE, ExpiryMS: c.ExpiryMS, Binding: c.Binding}
	if err := SignChallengeV2(other, hello, &forged); err != nil {
		t.Fatal(err)
	}
	if pub := VerifyChallengeV2(hello, forged); pub == nil || pub.Equal(&ecdsaKey(t, "gateway").PublicKey) {
		t.Fatal("a challenge signed by another key verified as the gateway's")
	}
	raw := c.Bytes()
	for i := range raw {
		b := append([]byte(nil), raw...)
		b[i] ^= 1
		f, err := ParseChallengeV2(b)
		if err != nil {
			continue // pkE or pkG no longer on the curve: also rejected
		}
		if VerifyChallengeV2(hello, f) != nil {
			t.Fatalf("v2 challenge byte %d flip accepted", i)
		}
	}
	for i := range hello {
		h := append([]byte(nil), hello...)
		h[i] ^= 1
		if VerifyChallengeV2(h, c) != nil {
			t.Fatalf("hello byte %d flip accepted", i)
		}
	}
	bad := c
	bad.PkG[0] = 0x02
	if _, err := ParseChallengeV2(bad.Bytes()); err == nil {
		t.Fatal("a compressed pkG parsed")
	}
	bad = c
	bad.PkG[64] ^= 1
	if _, err := ParseChallengeV2(bad.Bytes()); err == nil {
		t.Fatal("an off-curve pkG parsed")
	}
}

// v2's digest has its own domain and covers PkG: it never equals v1's, and a
// v1 signature over the same fields never verifies as v2.
func TestChallengeV2DigestDiffersFromV1(t *testing.T) {
	hello := goodHelloV2(t)
	c, gk := buildChallengeV2(t, hello)
	v1 := Challenge{N: c.N, PkE: c.PkE, ExpiryMS: c.ExpiryMS, Binding: c.Binding}
	if SignedDigestV2(hello, c) == SignedDigest(hello, v1) {
		t.Fatal("v2 digest equals v1's")
	}
	if err := SignChallenge(gk, hello, &v1); err != nil {
		t.Fatal(err)
	}
	cross := c
	cross.Sig = v1.Sig
	if VerifyChallengeV2(hello, cross) != nil {
		t.Fatal("a v1 signature verified as v2")
	}
	// And the other way: v2's signature never verifies as v1.
	v1.Sig = c.Sig
	if VerifyChallenge(&gk.PublicKey, hello, v1) {
		t.Fatal("a v2 signature verified as v1")
	}
}

type vectorsV2 struct {
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

func computeVectorsV2(t testing.TB) vectorsV2 {
	hello := goodHelloV2(t)
	c, gk := buildChallengeV2(t, hello)
	dsk, dpk := ecdhPub(t, "device")
	esk, epk := ecdhPub(t, "ephemeral")
	d := SignedDigestV2(hello, c)
	return vectorsV2{
		Note: "HELLO/CHALLENGE version 2 (the challenge carries pkG, the signing key, before the signature; digest " +
			"domain \"airgapp-gw/2 challenge\"). Keys as in vectors.json. ECDSA signing in Go is randomized: " +
			"signature is STORED here, not re-derived. Test-only keys; never use in production.",
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
	}
}

func TestWriteGoldenVectorsV2(t *testing.T) {
	if !*updateV2 {
		t.Skip("run with -update-v2 to rewrite testdata/vectors-v2.json")
	}
	b, _ := json.MarshalIndent(computeVectorsV2(t), "", "  ")
	if err := os.WriteFile(vectorsV2Path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenVectorsV2(t *testing.T) {
	raw, err := os.ReadFile(vectorsV2Path)
	if err != nil {
		t.Fatal(err)
	}
	var golden vectorsV2
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	got := computeVectorsV2(t)
	pubRaw, _ := hex.DecodeString(golden.GatewayPk)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubRaw)
	if err != nil {
		t.Fatal(err)
	}
	helloB, _ := hex.DecodeString(golden.Hello)
	chB, _ := hex.DecodeString(golden.Challenge)
	ch, err := ParseChallengeV2(chB)
	if err != nil {
		t.Fatal(err)
	}
	if k := VerifyChallengeV2(helloB, ch); k == nil || !k.Equal(pub) || hx(ch.Sig[:]) != golden.Signature {
		t.Fatal("golden v2 signature does not verify with the embedded key")
	}
	got.Signature, got.Challenge = golden.Signature, golden.Challenge
	got.Info = hx(Info(helloB, chB))
	if got != golden {
		gb, _ := json.MarshalIndent(got, "", " ")
		wb, _ := json.MarshalIndent(golden, "", " ")
		t.Fatalf("v2 vectors differ\ngot:\n%s\nwant:\n%s", gb, wb)
	}
	if len(helloB) != 71 || len(chB) != 250 {
		t.Fatal("lengths")
	}
}
