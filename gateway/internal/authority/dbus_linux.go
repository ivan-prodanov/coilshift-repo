//go:build linux

package authority

import (
	"time"

	"github.com/godbus/dbus/v5"
)

// handshakeTimeout bounds one connection attempt's EXTERNAL auth and Hello.
const handshakeTimeout = 2 * time.Second

// NewDBus returns the Oracle over authd's KeyInfo, pinned to authdSHA256
// (DefaultAuthdSHA256, or a build whose KeyInfo was re-verified). It is lazy:
// the first check hashes /usr/bin/authd, reads authd's uid from /etc/passwd
// and connects to the system bus, so a missing bus or passwd entry fails
// checks closed (retried at most once a second) instead of failing here. A
// dropped connection is dialled again. The only error is a malformed pin.
// The oracle also has KeyEntry(ctx, pk) (KeyInfoEntry, error), for probe-auth.
func NewDBus(authdSHA256 string) (Oracle, error) {
	o, err := newAuthdOracle(authdSHA256, authdBin, passwdFile, dialSystemBus)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// dialSystemBus opens a private system bus connection for the oracle.
func dialSystemBus() (authBus, error) {
	c, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}
	if err := handshake(c, handshakeTimeout); err != nil {
		return nil, err
	}
	return dbusConn{c}, nil
}
