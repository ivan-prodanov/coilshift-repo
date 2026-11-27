package authority

import (
	"bytes"
	"crypto/sha1"
	"fmt"
)

// KeyInfoEntry is one row of com.tesla.Auth.KeyInfo(), signature a(ayayuut).
// In the verified authd (DefaultAuthdSHA256) a listed key's Role is what
// KeyRole(pk, 0, false) returns.
type KeyInfoEntry struct {
	ID          []byte // 4 bytes, SHA-1(PublicKey)[:4]: a label, never an authenticator
	PublicKey   []byte // 65 bytes, 0x04||X||Y
	Role        uint32 // 0 NONE, 1 SERVICE, 2 OWNER, 3 DRIVER, ... 11 INFOTAINMENT
	FormFactor  uint32 // VCSEC KeyFormFactor: 6 IOS_DEVICE, 7 ANDROID_DEVICE, ...
	SecondsLeft uint64 // always 0 in the verified authd
}

// maxRole is INFOTAINMENT, the highest role the verified authd knows.
const maxRole = 11

// KeyID is authd's 4-byte key Id, SHA-1(pk)[:4].
func KeyID(pk []byte) []byte {
	h := sha1.Sum(pk)
	return h[:4]
}

// ParseKeyInfoBody checks a KeyInfo reply as godbus decodes it: exactly one
// array of structs whose rows are exactly []byte, []byte, uint32, uint32,
// uint64. It asserts the types itself because godbus's Store silently turns an
// int32 role into a uint32.
func ParseKeyInfoBody(body []interface{}) ([]KeyInfoEntry, error) {
	if len(body) != 1 {
		return nil, fmt.Errorf("authority: %w: KeyInfo reply has %d values, want 1", ErrShape, len(body))
	}
	rows, ok := body[0].([][]interface{})
	if !ok {
		return nil, fmt.Errorf("authority: %w: KeyInfo reply is %T, want a(ayayuut)", ErrShape, body[0])
	}
	out := make([]KeyInfoEntry, 0, len(rows))
	for i, r := range rows {
		if len(r) != 5 {
			return nil, fmt.Errorf("authority: %w: KeyInfo row %d has %d fields, want 5", ErrShape, i, len(r))
		}
		id, ok1 := r[0].([]byte)
		pub, ok2 := r[1].([]byte)
		role, ok3 := r[2].(uint32)
		ff, ok4 := r[3].(uint32)
		secs, ok5 := r[4].(uint64)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			return nil, fmt.Errorf("authority: %w: KeyInfo row %d is (%T, %T, %T, %T, %T), want (ayayuut)",
				ErrShape, i, r[0], r[1], r[2], r[3], r[4])
		}
		out = append(out, KeyInfoEntry{ID: id, PublicKey: pub, Role: role, FormFactor: ff, SecondsLeft: secs})
	}
	return out, nil
}

// RoleFor returns pk's role and form factor from KeyInfo rows. It matches the
// full 65-byte key only, never the 4-byte Id, and fails closed unless exactly
// one row holds pk, with Id SHA-1(pk)[:4], SecondsLeft 0 and a known role.
// A key that is not listed is denied, as KeyRole would refuse it.
func RoleFor(entries []KeyInfoEntry, pk []byte) (role int32, formFactor uint32, err error) {
	e, err := matchKey(entries, pk)
	if err != nil {
		return 0, 0, err
	}
	return int32(e.Role), e.FormFactor, nil
}

// checkKey requires an uncompressed P-256 point, the only form authd stores.
func checkKey(pk []byte) error {
	if len(pk) != 65 || pk[0] != 0x04 {
		return fmt.Errorf("authority: %w: key must be 65 bytes 0x04||X||Y", ErrShape)
	}
	return nil
}

// matchKey is RoleFor's row: every check RoleFor makes.
func matchKey(entries []KeyInfoEntry, pk []byte) (KeyInfoEntry, error) {
	if err := checkKey(pk); err != nil {
		return KeyInfoEntry{}, err
	}
	var found []KeyInfoEntry
	for _, e := range entries {
		if bytes.Equal(e.PublicKey, pk) {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return KeyInfoEntry{}, fmt.Errorf("authority: %w", ErrKeyUnknown)
	case 1:
	default:
		return KeyInfoEntry{}, fmt.Errorf("authority: %w: %d rows", ErrKeyAmbiguous, len(found))
	}
	e := found[0]
	if !bytes.Equal(e.ID, KeyID(pk)) {
		return KeyInfoEntry{}, fmt.Errorf("authority: %w: KeyInfo Id (%d bytes) is not SHA-1(key)[:4]", ErrShape, len(e.ID))
	}
	if e.SecondsLeft != 0 {
		return KeyInfoEntry{}, fmt.Errorf("authority: %w: KeyInfo SecondsLeft %d, want 0", ErrAuthdChanged, e.SecondsLeft)
	}
	if e.Role > maxRole {
		return KeyInfoEntry{}, fmt.Errorf("authority: %w: KeyInfo role %d unknown", ErrAuthdChanged, e.Role)
	}
	return e, nil
}
