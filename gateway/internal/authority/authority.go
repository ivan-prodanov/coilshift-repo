// Package authority asks the car's authd whether a phone key holds an
// owner/driver role. The role comes from com.tesla.Auth.KeyInfo, asked of the
// unique bus name owned by the authd uid, and only while /usr/bin/authd is
// the verified build (DefaultAuthdSHA256).
//
// Two Oracles: NewDBus asks authd over the system bus (the camera root
// helper uses it, as root); NewHelper asks that helper's sign-in socket
// (agw uses it: the gateway uid has no system bus access).
package authority

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Oracle is the minimal authd surface the gateway uses.
type Oracle interface {
	KeyRole(ctx context.Context, pk []byte) (int32, error)
	AuthOwnerIsAuthd(ctx context.Context) (bool, error)
}

// KeyEntryOracle is an Oracle that also shows the KeyInfo row behind a role:
// the D-Bus oracle and Fake. probe-auth prints it; Check does not use it.
type KeyEntryOracle interface {
	Oracle
	KeyEntry(ctx context.Context, pk []byte) (KeyInfoEntry, error)
}

const (
	roleOwner  = 2
	roleDriver = 3

	checkTimeout = 500 * time.Millisecond
)

// ownerInKeyRole is an Oracle whose KeyRole makes the owner check itself
// and fails without it: the sign-in helper, whose op key runs the owner check
// before it reads the key's row.
type ownerInKeyRole interface{ keyRoleChecksOwner() }

// Check fails closed: the bus name must be owned by authd and the key's role
// must be OWNER or DRIVER, all within 500 ms. An oracle whose KeyRole makes
// the owner check itself (the sign-in helper) is asked once.
func Check(ctx context.Context, o Oracle, pk []byte) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if _, both := o.(ownerInKeyRole); !both {
		ok, err := o.AuthOwnerIsAuthd(ctx)
		if err != nil {
			return fmt.Errorf("authority: owner check: %w", err)
		}
		if !ok {
			return fmt.Errorf("authority: %w", ErrAuthOwner)
		}
	}
	role, err := o.KeyRole(ctx, pk)
	if err != nil {
		return fmt.Errorf("authority: KeyRole: %w", err)
	}
	if role != roleOwner && role != roleDriver {
		return fmt.Errorf("authority: role %d not permitted", role)
	}
	return ctx.Err()
}

// lookupUID finds name's uid in a passwd-format file.
func lookupUID(passwdPath, name string) (uint32, error) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) < 3 || parts[0] != name {
			continue
		}
		u, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("authority: bad uid for %s: %w", name, err)
		}
		return uint32(u), nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("authority: user %q not found in %s", name, passwdPath)
}
