package authority

import "errors"

// Error classes. Every error the authd oracle returns wraps exactly one of
// them, so a caller (the sign-in helper) can classify it with errors.Is.
// Check's own refusals that are not one of these (a role other than OWNER or
// DRIVER, its 500 ms deadline) wrap none. Every one denies.
var (
	// ErrAuthOwner: com.tesla.Auth's owner is empty, not a unique (':')
	// name, or does not run as authd's uid.
	ErrAuthOwner = errors.New("com.tesla.Auth is not owned by authd")
	// ErrKeyUnknown: no KeyInfo row holds the key.
	ErrKeyUnknown = errors.New("key not in authd's KeyInfo list")
	// ErrKeyAmbiguous: two or more KeyInfo rows hold the key.
	ErrKeyAmbiguous = errors.New("key listed more than once in authd's KeyInfo list")
	// ErrShape: a reply or a KeyInfo row is not what the verified authd and
	// bus send (value count, types, an Id that is not SHA-1(key)[:4]), or the
	// key is not 65 bytes 0x04||X||Y.
	ErrShape = errors.New("wrong shape")
	// ErrBus: no system bus connection (the dial, its handshake, or what is
	// read before it: authd's pin and /etc/passwd), or a bus call failed.
	ErrBus = errors.New("system bus")
	// ErrAuthdChanged: /usr/bin/authd is not the pinned build, or KeyInfo
	// shows what the verified build never sends (SecondsLeft not 0, a role
	// above 11). KeyInfo must be re-verified for this authd.
	ErrAuthdChanged = errors.New("authd changed, re-verify KeyInfo")
)
