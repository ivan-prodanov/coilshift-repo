package authority

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// DefaultAuthdSHA256 pins the authd build (/usr/bin/authd, firmware 2026.8.6)
// whose KeyInfo was verified to carry the role KeyRole(pk, 0, false) returns.
// Any other build fails every check closed until KeyInfo is re-verified for it
// and its hash is passed with -authd-sha256.
const DefaultAuthdSHA256 = "33d62d9e818afcb9b4a4af8cec6ecda2ff1801c5a667bd527e39a2b4f6f477e9"

const (
	authdBin    = "/usr/bin/authd"
	passwdFile  = "/etc/passwd"
	authDest    = "com.tesla.Auth"
	redialEvery = time.Second // at most one bus connection attempt per second
)

// authBus is the bus surface the oracle uses: the bus driver's
// GetNameOwner and GetConnectionUnixUser, and com.tesla.Auth.KeyInfo() at
// /Auth on one destination. Each value is returned exactly as received.
type authBus interface {
	NameOwner(ctx context.Context, name string) (string, error)
	UnixUser(ctx context.Context, name string) (uint32, error)
	KeyInfo(ctx context.Context, dest string) ([]interface{}, error)
	Connected() bool
	Close() error
}

// authdOracle answers from authd's KeyInfo list. It is lazy: the authd pin,
// /etc/passwd and the bus are first touched by a check, and any failure fails
// that check closed.
type authdOracle struct {
	pin        [32]byte // expected SHA-256 of binPath
	binPath    string
	passwdPath string
	dial       func() (authBus, error)
	now        func() time.Time

	mu      sync.Mutex
	pinned  bool          // binPath matched pin; never hashed again
	pinErr  error         // binPath did not match: every check fails
	bus     authBus       // nil until connected
	uid     uint32        // authd's uid, read from passwdPath when bus was dialled
	dialing chan struct{} // closed when the attempt in flight ends
	tried   time.Time     // start of the last attempt
	lastErr error         // why there is no bus
}

var _ KeyEntryOracle = (*authdOracle)(nil)

func newAuthdOracle(pinHex, binPath, passwdPath string, dial func() (authBus, error)) (*authdOracle, error) {
	b, err := hex.DecodeString(pinHex)
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("authority: the authd pin must be 64 hex digits (it cannot be disabled), got %q", pinHex)
	}
	o := &authdOracle{binPath: binPath, passwdPath: passwdPath, dial: dial, now: time.Now}
	copy(o.pin[:], b)
	return o, nil
}

// AuthOwnerIsAuthd reports whether com.tesla.Auth is owned by authd's uid.
func (o *authdOracle) AuthOwnerIsAuthd(ctx context.Context) (bool, error) {
	if _, _, err := o.owner(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// KeyRole is pk's role in authd's KeyInfo list (RoleFor's checks).
func (o *authdOracle) KeyRole(ctx context.Context, pk []byte) (int32, error) {
	entries, err := o.keyInfo(ctx)
	if err != nil {
		return 0, err
	}
	role, _, err := RoleFor(entries, pk)
	return role, err
}

// KeyEntry is the whole KeyInfo row KeyRole decides on, for probe-auth.
func (o *authdOracle) KeyEntry(ctx context.Context, pk []byte) (KeyInfoEntry, error) {
	entries, err := o.keyInfo(ctx)
	if err != nil {
		return KeyInfoEntry{}, err
	}
	return matchKey(entries, pk)
}

// keyInfo calls KeyInfo on the unique name that passed the owner check, never
// on com.tesla.Auth itself: a name handed over after the check is not asked.
func (o *authdOracle) keyInfo(ctx context.Context) ([]KeyInfoEntry, error) {
	bus, name, err := o.owner(ctx)
	if err != nil {
		return nil, err
	}
	body, err := bus.KeyInfo(ctx, name)
	if err != nil {
		return nil, callErr("KeyInfo", err)
	}
	return ParseKeyInfoBody(body)
}

// owner returns the bus and the unique name that owns com.tesla.Auth, checked
// to run as authd's uid.
func (o *authdOracle) owner(ctx context.Context) (authBus, string, error) {
	bus, uid, err := o.conn(ctx)
	if err != nil {
		return nil, "", err
	}
	name, err := bus.NameOwner(ctx, authDest)
	if err != nil {
		return nil, "", callErr("GetNameOwner("+authDest+")", err)
	}
	if !strings.HasPrefix(name, ":") {
		return nil, "", fmt.Errorf("authority: %w: its owner is %q, not a unique name", ErrAuthOwner, name)
	}
	got, err := bus.UnixUser(ctx, name)
	if err != nil {
		return nil, "", callErr("GetConnectionUnixUser("+name+")", err)
	}
	if got != uid {
		return nil, "", fmt.Errorf("authority: %w: owned by uid %d, authd's uid is %d", ErrAuthOwner, got, uid)
	}
	return bus, name, nil
}

// callErr is a failed bus call: ErrShape when the reply had the wrong type
// (dbusConn's only), ErrBus for anything else.
func callErr(call string, err error) error {
	if errors.Is(err, ErrShape) {
		return fmt.Errorf("authority: %s: %w", call, err)
	}
	return fmt.Errorf("authority: %w: %s: %w", ErrBus, call, err)
}

// conn returns the bus and authd's uid. Connecting runs in the background, at
// most once per redialEvery; a check waits for it only as long as ctx allows.
// A dropped connection is closed and dialled again.
func (o *authdOracle) conn(ctx context.Context) (authBus, uint32, error) {
	for {
		o.mu.Lock()
		if o.pinErr != nil {
			err := o.pinErr
			o.mu.Unlock()
			return nil, 0, err
		}
		if o.bus != nil && !o.bus.Connected() {
			o.bus.Close()
			o.bus, o.lastErr = nil, errors.New("connection lost")
		}
		if o.bus != nil {
			bus, uid := o.bus, o.uid
			o.mu.Unlock()
			return bus, uid, nil
		}
		wait := o.dialing
		if wait == nil {
			if !o.tried.IsZero() && o.now().Sub(o.tried) < redialEvery {
				err := o.lastErr
				o.mu.Unlock()
				return nil, 0, fmt.Errorf("authority: %w: not connected, retrying at most once per %v: %w", ErrBus, redialEvery, err)
			}
			o.tried = o.now()
			wait = make(chan struct{})
			o.dialing = wait
			go o.connect(wait)
		}
		o.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, 0, fmt.Errorf("authority: %w: waiting for the connection: %w", ErrBus, ctx.Err())
		}
	}
}

// connect is one attempt: the authd pin, authd's uid, then the bus. Its
// error (lastErr) carries no class: conn reports it as ErrBus, or the pin
// mismatch as ErrAuthdChanged.
func (o *authdOracle) connect(done chan struct{}) {
	var (
		bus authBus
		uid uint32
	)
	err := o.checkPin()
	if err == nil {
		uid, err = lookupUID(o.passwdPath, "authd")
	}
	if err == nil {
		if bus, err = o.dial(); err != nil {
			bus, err = nil, fmt.Errorf("dial: %w", err)
		}
	}
	o.mu.Lock()
	o.bus, o.uid, o.lastErr, o.dialing = bus, uid, err, nil
	o.mu.Unlock()
	close(done)
}

// checkPin hashes binPath until it has matched once. A mismatch is permanent;
// a read error is retried by the next attempt.
func (o *authdOracle) checkPin() error {
	o.mu.Lock()
	pinned := o.pinned
	o.mu.Unlock()
	if pinned {
		return nil
	}
	sum, err := fileSHA256(o.binPath)
	if err != nil {
		return fmt.Errorf("authority: hashing authd for its pin: %w", err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if sum != o.pin {
		o.pinErr = fmt.Errorf("authority: %w: %s has sha256 %x, the verified build is %x; "+
			"KeyInfo must be re-verified for this build before its hash is passed with -authd-sha256",
			ErrAuthdChanged, o.binPath, sum, o.pin)
		return o.pinErr
	}
	o.pinned = true
	return nil
}

func fileSHA256(path string) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
