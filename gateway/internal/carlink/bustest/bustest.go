// Package bustest runs a private dbus-daemon for tests and the bench. Nothing here touches the system or session bus:
// each Start launches its own daemon on a socket in a fresh temp directory, with an allow-all policy.
package bustest

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// Bus is one private dbus-daemon.
type Bus struct {
	addr string
}

// The allow-all policy, as internal/authsrv's tests use. The limits stay at the daemon's defaults.
const config = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>custom</type>
  <listen>unix:path=SOCKET</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow user="*"/>
    <allow own="*"/>
    <allow send_type="signal"/>
    <allow send_destination="*"/>
    <allow receive_type="*"/>
  </policy>
</busconfig>
`

// Start launches a dbus-daemon that lives until t's cleanup. It skips t only when dbus-daemon is not on PATH; any
// other failure fails t.
func Start(t testing.TB) *Bus {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("no dbus-daemon")
	}
	// Not t.TempDir(): darwin caps unix socket paths at 104 bytes.
	dir, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	conf := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(conf, []byte(strings.Replace(config, "SOCKET", filepath.Join(dir, "bus"), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config-file="+conf, "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	hung := time.AfterFunc(10*time.Second, func() { cmd.Process.Kill() })
	line, err := bufio.NewReader(out).ReadString('\n')
	hung.Stop()
	if err != nil {
		t.Fatalf("dbus-daemon printed no address: %v", err)
	}
	return &Bus{addr: strings.TrimSpace(line)}
}

// Addr is the daemon's address, for dbus.Dial or carlink.DialBus.
func (b *Bus) Addr() string { return b.addr }

// Conn is a new client connection (Dial, Auth, Hello), closed at t's cleanup. Its signal handler is godbus's
// sequential one, so a channel registered with Signal receives signals in the order the bus sent them.
func (b *Bus) Conn(t testing.TB) *dbus.Conn {
	t.Helper()
	c, err := dbus.Dial(b.addr, dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Auth(nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Hello(); err != nil {
		t.Fatal(err)
	}
	return c
}
