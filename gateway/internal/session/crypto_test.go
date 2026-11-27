package session

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/cloudflare/circl/hpke"
)

func hx(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRFC9180AuthKAT(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/rfc9180_a33.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Info, SkSm, PkSm, PkRm, SkRm, IkmE, Enc string
		Encryptions                             []struct{ Aad, Ct, Pt string }
		Exports                                 []struct {
			ExporterContext string `json:"exporter_context"`
			L               int
			ExportedValue   string `json:"exported_value"`
		}
	}
	// field names in the file are snake/mixed; map explicitly.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	str := func(k string) string {
		var s string
		if err := json.Unmarshal(m[k], &s); err != nil {
			t.Fatal(k, err)
		}
		return s
	}
	v.Info, v.SkSm, v.PkSm, v.PkRm, v.SkRm, v.IkmE, v.Enc = str("info"), str("skSm"), str("pkSm"), str("pkRm"), str("skRm"), str("ikmE"), str("enc")
	if err := json.Unmarshal(m["encryptions"], &v.Encryptions); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(m["exports"], &v.Exports); err != nil {
		t.Fatal(err)
	}

	suite := hpke.NewSuite(hpke.KEM_P256_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES128GCM)
	sc := hpke.KEM_P256_HKDF_SHA256.Scheme()
	skS, err := sc.UnmarshalBinaryPrivateKey(hx(t, v.SkSm))
	if err != nil {
		t.Fatal(err)
	}
	pkS, err := sc.UnmarshalBinaryPublicKey(hx(t, v.PkSm))
	if err != nil {
		t.Fatal(err)
	}
	pkR, _ := sc.UnmarshalBinaryPublicKey(hx(t, v.PkRm))
	skR, _ := sc.UnmarshalBinaryPrivateKey(hx(t, v.SkRm))
	snd, err := suite.NewSender(pkR, hx(t, v.Info))
	if err != nil {
		t.Fatal(err)
	}
	enc, sealer, err := snd.SetupAuth(bytes.NewReader(hx(t, v.IkmE)), skS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, hx(t, v.Enc)) {
		t.Fatalf("enc mismatch")
	}
	rcv, _ := suite.NewReceiver(skR, hx(t, v.Info))
	opener, err := rcv.SetupAuth(enc, pkS)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range v.Encryptions {
		ct, err := sealer.Seal(hx(t, e.Pt), hx(t, e.Aad))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ct, hx(t, e.Ct)) {
			t.Fatalf("ct %d mismatch", i)
		}
		pt, err := opener.Open(ct, hx(t, e.Aad))
		if err != nil || !bytes.Equal(pt, hx(t, e.Pt)) {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	for i, e := range v.Exports {
		want := hx(t, e.ExportedValue)
		if got := sealer.Export(hx(t, e.ExporterContext), uint(e.L)); !bytes.Equal(got, want) {
			t.Fatalf("sealer export %d mismatch", i)
		}
		if got := opener.Export(hx(t, e.ExporterContext), uint(e.L)); !bytes.Equal(got, want) {
			t.Fatalf("opener export %d mismatch", i)
		}
	}
}

func keys(t *testing.T) (skS, skE *ecdh.PrivateKey) {
	t.Helper()
	var err error
	if skS, err = ecdh.P256().GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	if skE, err = ecdh.P256().GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	return
}

var testInfo = append([]byte("airgapp-gw/1\x00"), make([]byte, 32)...)

func setup(t *testing.T, msgs ...string) (*Inbound, [][]byte, *TestSealer, [][]byte) {
	t.Helper()
	skS, skE := keys(t)
	enc, snd, err := TestSender(skS, skE.PublicKey().Bytes(), testInfo)
	if err != nil {
		t.Fatal(err)
	}
	var cts [][]byte
	for _, m := range msgs {
		cts = append(cts, snd.Seal([]byte(m)))
	}
	first := append(append([]byte{}, enc...), cts[0]...)
	in, pt, err := OpenArm(skE, skS.PublicKey().Bytes(), testInfo, first)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != msgs[0] {
		t.Fatalf("first pt %q", pt)
	}
	return in, cts, snd, nil
}

func TestArmRoundTrip(t *testing.T) {
	in, cts, _, _ := setup(t, "one", "two", "three")
	for i, want := range []string{"two", "three"} {
		pt, err := in.Open(cts[i+1])
		if err != nil || string(pt) != want {
			t.Fatalf("%d: %q %v", i, pt, err)
		}
	}
}

func TestOutOfOrderFails(t *testing.T) {
	in, cts, _, _ := setup(t, "one", "two", "three")
	if _, err := in.Open(cts[2]); err == nil {
		t.Fatal("out-of-order open succeeded")
	}
	// permanently failed, even for the correct next frame
	if _, err := in.Open(cts[1]); err == nil {
		t.Fatal("context usable after failure")
	}
}

func TestWrongSenderFails(t *testing.T) {
	skS, skE := keys(t)
	other, _ := keys(t)
	enc, snd, err := TestSender(other, skE.PublicKey().Bytes(), testInfo)
	if err != nil {
		t.Fatal(err)
	}
	first := append(enc, snd.Seal([]byte("x"))...)
	if _, _, err := OpenArm(skE, skS.PublicKey().Bytes(), testInfo, first); err == nil {
		t.Fatal("wrong sender accepted")
	}
}

func TestShortFirstFrame(t *testing.T) {
	_, skE := keys(t)
	skS, _ := keys(t)
	if _, _, err := OpenArm(skE, skS.PublicKey().Bytes(), testInfo, make([]byte, 65+15)); err == nil {
		t.Fatal("short frame accepted")
	}
}

func TestS2CRoundTrip(t *testing.T) {
	in, _, snd, _ := setup(t, "one")
	out, err := NewOutbound(in)
	if err != nil {
		t.Fatal(err)
	}
	c0, c1, c2 := out.Seal([]byte("a")), out.Seal([]byte("b")), out.Seal([]byte("c"))
	for _, w := range []struct {
		ct   []byte
		want string
	}{{c0, "a"}, {c1, "b"}} {
		pt, err := snd.OpenS2C(w.ct)
		if err != nil || string(pt) != w.want {
			t.Fatalf("%q %v", pt, err)
		}
	}
	if _, err := snd.OpenS2C(c1); err == nil { // replay
		t.Fatal("replay accepted")
	}
	// c2 is now also refused: failure is permanent? No: strict order only
	// requires that a bad frame fails; the next in-order frame still opens.
	if pt, err := snd.OpenS2C(c2); err != nil || string(pt) != "c" {
		t.Fatalf("in-order after failed replay: %q %v", pt, err)
	}
	// reorder
	d0, d1 := out.Seal([]byte("d")), out.Seal([]byte("e"))
	if _, err := snd.OpenS2C(d1); err == nil {
		t.Fatal("reorder accepted")
	}
	_ = d0
}

func TestNewOutboundOnce(t *testing.T) {
	in, _, _, _ := setup(t, "one")
	if _, err := NewOutbound(in); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOutbound(in); err == nil {
		t.Fatal("second NewOutbound succeeded")
	}
}

func TestNewOutboundAfterFailure(t *testing.T) {
	in, cts, _, _ := setup(t, "one", "two", "three")
	if _, err := in.Open(cts[2]); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := NewOutbound(in); err == nil {
		t.Fatal("NewOutbound succeeded on failed Inbound")
	}
}
