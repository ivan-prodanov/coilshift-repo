package session

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"airgapp/gateway/internal/proto"
)

// TestCryptoKitArm opens frames sealed by CryptoKit (Auth mode, fixed keys),
// checks Go and Swift derive the same s2c key/base, and writes go_s2c.json
// for the Swift side to open.
func TestCryptoKitArm(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/cryptokit_arm.json")
	if os.IsNotExist(err) {
		t.Skip("cryptokit_arm.json missing; run make interop")
	}
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Hello, Challenge string
		Frames           []struct{ Frame, Plaintext string }
		S2CKey           string `json:"s2c_key"`
		S2CBase          string `json:"s2c_base"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	var v struct {
		DeviceSK, DevicePK, EphemeralSK string
	}
	vr, err := os.ReadFile("../../testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vm map[string]any
	if err := json.Unmarshal(vr, &vm); err != nil {
		t.Fatal(err)
	}
	v.DevicePK, v.EphemeralSK = vm["device_pk"].(string), vm["ephemeral_sk"].(string)
	skE, err := ecdh.P256().NewPrivateKey(hx(t, v.EphemeralSK))
	if err != nil {
		t.Fatal(err)
	}
	hello, chal := hx(t, a.Hello), hx(t, a.Challenge)
	info := proto.Info(hello, chal)
	if len(a.Frames) < 3 {
		t.Fatal("need >=3 frames")
	}
	in, pt, err := OpenArm(skE, hx(t, v.DevicePK), info, hx(t, a.Frames[0].Frame))
	if err != nil {
		t.Fatal("OpenArm:", err)
	}
	if !bytes.Equal(pt, hx(t, a.Frames[0].Plaintext)) {
		t.Fatal("arm plaintext mismatch")
	}
	for i, f := range a.Frames[1:] {
		pt, err := in.Open(hx(t, f.Frame))
		if err != nil || !bytes.Equal(pt, hx(t, f.Plaintext)) {
			t.Fatalf("frame %d: %v", i+1, err)
		}
	}

	// s2c: Go's exporter output must equal CryptoKit's.
	gk := in.opener.Export([]byte(labelS2CKey), 32)
	out, err := NewOutbound(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gk, hx(t, a.S2CKey)) || !bytes.Equal(out.base, hx(t, a.S2CBase)) {
		t.Fatal("s2c key/base differ between Go and CryptoKit")
	}
	type fr struct {
		Frame     string `json:"frame"`
		Plaintext string `json:"plaintext"`
	}
	var frames []fr
	for _, p := range [][]byte{{0x80, '{', '}'}, {0x82}, append([]byte{0xA0}, bytes.Repeat([]byte{7}, 40)...)} {
		frames = append(frames, fr{hex.EncodeToString(out.Seal(p)), hex.EncodeToString(p)})
	}
	b, _ := json.MarshalIndent(map[string]any{"key": a.S2CKey, "base": a.S2CBase, "frames": frames}, "", "  ")
	if err := os.WriteFile("../../testdata/go_s2c.json", b, 0o644); err != nil {
		t.Fatal(err)
	}
}
