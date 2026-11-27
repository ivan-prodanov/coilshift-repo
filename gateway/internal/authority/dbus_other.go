//go:build !linux

package authority

import "errors"

// NewDBus is only available on Linux (the car); see dbus_linux.go.
func NewDBus(authdSHA256 string) (Oracle, error) {
	return nil, errors.New("authority: D-Bus oracle requires linux")
}
