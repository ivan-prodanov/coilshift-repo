package authority

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"
)

// The owner's phone key as authd listed it on the car (2026-11-20 readback):
// Id cc e6 a2 d2, role 2 OWNER, form factor 6 IOS_DEVICE. A public key.
var (
	phonePK = mustHex("04e70805d8e2d25f6337d3ba01c18d7247b38afc0330ee85b21bdf490a85eb667ff5acda7cd5af3d21a90a12d3e3c656d512a598da3a8e6494bf66ea8a9c69a0a9")
	phoneID = mustHex("cce6a2d2")
	otherPK = append([]byte{4}, bytes.Repeat([]byte{0x11}, 64)...) // a second listed key
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// sha1ID is authd's key Id, computed here independently of KeyID.
func sha1ID(pk []byte) []byte {
	h := sha1.Sum(pk)
	return h[:4]
}

func phoneRow() KeyInfoEntry {
	return KeyInfoEntry{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6}
}

func otherRow() KeyInfoEntry {
	return KeyInfoEntry{ID: sha1ID(otherPK), PublicKey: otherPK, Role: 1}
}

// wireRow is KeyInfo's reply row as authd declares it: a(ayayuut).
type wireRow struct {
	ID, PublicKey    []byte
	Role, FormFactor uint32
	SecondsLeft      uint64
}

// wireBody encodes v as a D-Bus method reply and decodes it again: the body
// exactly as godbus hands a reply of v's signature to the oracle.
func wireBody(t *testing.T, v interface{}) []interface{} {
	t.Helper()
	msg := &dbus.Message{Type: dbus.TypeMethodReply, Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldReplySerial: dbus.MakeVariant(uint32(1)),
		dbus.FieldSignature:   dbus.MakeVariant(dbus.SignatureOf(v)),
	}, Body: []interface{}{v}}
	var buf bytes.Buffer
	if err := msg.EncodeTo(&buf, binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	m, err := dbus.DecodeMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return m.Body
}

func TestKeyIDIsAuthdsID(t *testing.T) {
	if got := KeyID(phonePK); !bytes.Equal(got, phoneID) {
		t.Fatalf("KeyID = %x, want the car's cce6a2d2", got)
	}
}

func TestParseKeyInfoBodyWire(t *testing.T) {
	rows := []wireRow{
		{ID: sha1ID(otherPK), PublicKey: otherPK, Role: 1, FormFactor: 0, SecondsLeft: 7},
		{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6},
	}
	if sig := dbus.SignatureOf(rows).String(); sig != "a(ayayuut)" {
		t.Fatalf("fixture signature %s, want a(ayayuut)", sig)
	}
	got, err := ParseKeyInfoBody(wireBody(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	want := []KeyInfoEntry{
		{ID: sha1ID(otherPK), PublicKey: otherPK, Role: 1, FormFactor: 0, SecondsLeft: 7},
		{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6, SecondsLeft: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseKeyInfoBodyEmptyList(t *testing.T) {
	got, err := ParseKeyInfoBody(wireBody(t, []wireRow{}))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty list: %+v, %v", got, err)
	}
}

func TestParseKeyInfoBodyRejects(t *testing.T) {
	row := func() []interface{} { return []interface{}{phoneID, phonePK, uint32(2), uint32(6), uint64(0)} }
	set := func(i int, v interface{}) []interface{} { r := row(); r[i] = v; return r }
	body := func(rows ...[]interface{}) []interface{} { return []interface{}{rows} }
	type row4 struct { // a(ayayuu)
		ID, PublicKey    []byte
		Role, FormFactor uint32
	}
	type row6 struct { // a(ayayuuut)
		ID, PublicKey           []byte
		Role, FormFactor, Extra uint32
		SecondsLeft             uint64
	}
	type row6Tail struct { // a(ayayuutu): the five fields, then one more
		ID, PublicKey    []byte
		Role, FormFactor uint32
		SecondsLeft      uint64
		Extra            uint32
	}
	type rowI struct { // a(ayayiut)
		ID, PublicKey []byte
		Role          int32
		FormFactor    uint32
		SecondsLeft   uint64
	}
	cases := []struct {
		name string
		body []interface{}
	}{
		{"nil body", nil},
		{"no values", []interface{}{}},
		{"two values", []interface{}{[][]interface{}{row()}, uint32(0)}},
		{"one struct, not an array", []interface{}{row()}},
		{"array of variants", []interface{}{[]dbus.Variant{dbus.MakeVariant(uint32(2))}}},
		{"a string", []interface{}{"a(ayayuut)"}},
		{"4-field rows", wireBody(t, []row4{{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6}})},
		{"6-field rows", wireBody(t, []row6{{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6}})},
		{"5 good fields and a sixth", wireBody(t, []row6Tail{{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6}})},
		{"int32 role", wireBody(t, []rowI{{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6}})},
		{"Id is a string", body(set(0, "cce6a2d2"))},
		{"PublicKey is a variant", body(set(1, dbus.MakeVariant(phonePK)))},
		{"Role is int32", body(set(2, int32(2)))},
		{"FormFactor is int32", body(set(3, int32(6)))},
		{"SecondsLeft is uint32", body(set(4, uint32(0)))},
		{"second row bad", body(row(), set(2, int32(2)))},
	}
	for _, c := range cases {
		if got, err := ParseKeyInfoBody(c.body); err == nil {
			t.Errorf("%s: accepted as %+v", c.name, got)
		}
	}
}

func TestRoleForAccepts(t *testing.T) {
	with := func(f func(*KeyInfoEntry)) []KeyInfoEntry {
		e := phoneRow()
		f(&e)
		return []KeyInfoEntry{otherRow(), e}
	}
	cases := []struct {
		name string
		rows []KeyInfoEntry
		role int32
		ff   uint32
	}{
		{"phone as read on the car", []KeyInfoEntry{otherRow(), phoneRow()}, 2, 6},
		{"role 0 NONE", with(func(e *KeyInfoEntry) { e.Role = 0 }), 0, 6},
		{"role 1 SERVICE", with(func(e *KeyInfoEntry) { e.Role = 1 }), 1, 6},
		{"role 3 DRIVER", with(func(e *KeyInfoEntry) { e.Role = 3 }), 3, 6},
		{"role 8 GUEST", with(func(e *KeyInfoEntry) { e.Role = 8 }), 8, 6},
		{"role 11 INFOTAINMENT", with(func(e *KeyInfoEntry) { e.Role = 11 }), 11, 6},
		{"form factor 7 ANDROID", with(func(e *KeyInfoEntry) { e.FormFactor = 7 }), 2, 7},
	}
	for _, c := range cases {
		role, ff, err := RoleFor(c.rows, phonePK)
		if err != nil || role != c.role || ff != c.ff {
			t.Errorf("%s: role=%d ff=%d err=%v, want role=%d ff=%d", c.name, role, ff, err, c.role, c.ff)
		}
	}
}

func TestRoleForDenies(t *testing.T) {
	with := func(f func(*KeyInfoEntry)) []KeyInfoEntry {
		e := phoneRow()
		f(&e)
		return []KeyInfoEntry{otherRow(), e}
	}
	// A row that lists pk itself, with pk's own Id: only pk's shape can fail.
	listed := func(pk []byte) []KeyInfoEntry {
		return []KeyInfoEntry{{ID: sha1ID(pk), PublicKey: pk, Role: 2, FormFactor: 6}}
	}
	short64 := append([]byte{}, phonePK[1:]...)             // X||Y, no 0x04
	short33 := append([]byte{2}, phonePK[1:33]...)          // compressed (Y is even)
	prefix5 := append([]byte{5}, phonePK[1:]...)            // 65 bytes, wrong prefix
	id5 := append(append([]byte{}, phoneID...), 0)          // right Id plus a byte
	id3 := append([]byte{}, phoneID[:3]...)                 // first 3 bytes of the right Id
	idOff := append(append([]byte{}, phoneID[:3]...), 0xd3) // cce6a2d3
	cases := []struct {
		name string
		rows []KeyInfoEntry
		pk   []byte
	}{
		{"empty list", nil, phonePK},
		{"not listed", []KeyInfoEntry{otherRow()}, phonePK},
		{"listed twice", []KeyInfoEntry{phoneRow(), otherRow(), phoneRow()}, phonePK},
		{"only the Id matches", []KeyInfoEntry{{ID: phoneID, PublicKey: otherPK, Role: 2, FormFactor: 6}}, phonePK},
		{"Id mismatch", with(func(e *KeyInfoEntry) { e.ID = idOff }), phonePK},
		{"Id 3 bytes", with(func(e *KeyInfoEntry) { e.ID = id3 }), phonePK},
		{"Id 5 bytes", with(func(e *KeyInfoEntry) { e.ID = id5 }), phonePK},
		{"Id empty", with(func(e *KeyInfoEntry) { e.ID = nil }), phonePK},
		{"SecondsLeft 1", with(func(e *KeyInfoEntry) { e.SecondsLeft = 1 }), phonePK},
		{"role 12", with(func(e *KeyInfoEntry) { e.Role = 12 }), phonePK},
		{"role 2^31 (negative as int32)", with(func(e *KeyInfoEntry) { e.Role = 1 << 31 }), phonePK},
		{"64-byte key", listed(short64), short64},
		{"33-byte key", listed(short33), short33},
		{"65 bytes, prefix 0x05", listed(prefix5), prefix5},
		{"no key", listed(nil), nil},
	}
	for _, c := range cases {
		if role, ff, err := RoleFor(c.rows, c.pk); err == nil {
			t.Errorf("%s: accepted as role=%d ff=%d", c.name, role, ff)
		}
	}
}
