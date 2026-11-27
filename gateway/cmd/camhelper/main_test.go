package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/authsrv"
)

func TestUsageAndRoot(t *testing.T) {
	var buf bytes.Buffer
	if code := run(context.Background(), nil, &buf); code != 2 {
		t.Fatalf("no args: %d", code)
	}
	if code := run(context.Background(), []string{"set-dv"}, &buf); code != 2 {
		t.Fatalf("unknown subcommand: %d", code)
	}
	old := geteuid
	defer func() { geteuid = old }()
	geteuid = func() int { return 4950 }
	buf.Reset()
	if code := run(context.Background(), []string{"run"}, &buf); code != 1 || !strings.Contains(buf.String(), "must run as root") {
		t.Fatalf("non-root run: %d %s", code, buf.String())
	}
}

func TestParseGroups(t *testing.T) {
	g, err := parseGroups("29, 1021")
	if err != nil || len(g) != 2 || g[0] != 29 || g[1] != 1021 {
		t.Fatalf("%v %v", g, err)
	}
	if _, err := parseGroups("29,audio"); err == nil {
		t.Fatal("bad group accepted")
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	a, err := lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock(dir); err == nil || !strings.Contains(err.Error(), "never start it twice") {
		t.Fatalf("second lock: %v", err)
	}
	a.Close()
	b, err := lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
}

func TestEnsureDirRefusesNonDirs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")
	os.WriteFile(f, nil, 0o600)
	if err := ensureDir(f, 0o700, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("file accepted as dir")
	}
	l := filepath.Join(dir, "link")
	os.Symlink(dir, l)
	if err := ensureDir(l, 0o700, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("symlink accepted as dir")
	}
	d := filepath.Join(dir, "new")
	if err := ensureDir(d, 0o711, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(d); st.Mode().Perm() != 0o711 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestDirectoriesArePinnedUnderRun(t *testing.T) {
	for dir, ok := range map[string]bool{
		"/run/airgapp-camhelper": true, "/run/airgapp-camhelper-state": true,
		"/run": false, "/run/service": false, "/etc/airgapp-x": false, "/run/airgapp-x/sub": false,
		"/run/../etc/airgapp-x": false, "/run/airgapp-x/": false, "/var/run/airgapp-x": false,
	} {
		if err := pinned(dir); (err == nil) != ok {
			t.Errorf("pinned(%q) = %v", dir, err)
		}
	}
	old := geteuid
	defer func() { geteuid = old }()
	geteuid = func() int { return 0 }
	var buf bytes.Buffer
	if code := run(context.Background(), []string{"run", "-state", "/etc"}, &buf); code != 2 {
		t.Fatalf("a system -state dir was accepted: %d %s", code, buf.String())
	}
	if code := run(context.Background(), []string{"run", "-sock", "/run/service/x.sock"}, &buf); code != 2 {
		t.Fatalf("a system -sock dir was accepted: %d %s", code, buf.String())
	}
}

func TestSweepWithoutStateIsANoop(t *testing.T) {
	old := geteuid
	defer func() { geteuid = old }()
	geteuid = func() int { return 0 }
	oldRoot := runRoot
	defer func() { runRoot = oldRoot }()
	runRoot = t.TempDir()
	var buf bytes.Buffer
	dir := filepath.Join(runRoot, "airgapp-absent")
	if code := run(context.Background(), []string{"sweep", "-state", dir}, &buf); code != 0 {
		t.Fatalf("sweep: %d %s", code, buf.String())
	}
	// An existing state dir with nothing marked: no car command runs.
	dir = filepath.Join(runRoot, "airgapp-state")
	os.Mkdir(dir, 0o700)
	if code := run(context.Background(), []string{"sweep", "-state", dir}, &buf); code != 0 {
		t.Fatalf("sweep: %d %s", code, buf.String())
	}
}

// The sign-in socket must sit beside the camera socket, and the authd pin is
// checked before anything is created.
func TestSignInFlags(t *testing.T) {
	old := geteuid
	defer func() { geteuid = old }()
	geteuid = func() int { return 0 }
	called := false
	oldOracle := newOracle
	defer func() { newOracle = oldOracle }()
	newOracle = func(string) (authsrv.Oracle, error) { called = true; return nil, errors.New("not here") }
	var buf bytes.Buffer
	for _, args := range [][]string{
		{"run", "-auth-sock", "/run/airgapp-other/auth.sock"},
		{"run", "-authd-sha256", "00"},
		{"run", "-authd-sha256", strings.Repeat("A", 64)},
		{"run", "-authd-sha256", ""},
	} {
		buf.Reset()
		if code := run(context.Background(), args, &buf); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, buf.String())
		}
	}
	if called {
		t.Fatal("a refused flag must stop before the oracle is made")
	}
	if !isPin(authority.DefaultAuthdSHA256) {
		t.Fatal("the default pin must pass isPin")
	}
}

type okOracle struct{}

func (okOracle) AuthOwnerIsAuthd(context.Context) (bool, error) { return true, nil }
func (okOracle) KeyEntry(_ context.Context, pk []byte) (authority.KeyInfoEntry, error) {
	return authority.KeyInfoEntry{ID: authority.KeyID(pk), PublicKey: pk, Role: 2, FormFactor: 6}, nil
}

// serve end to end on the desk: both sockets come up beside each other, the
// sign-in socket answers and passes the watch on, and SIGTERM removes both.
func TestServeBothSockets(t *testing.T) {
	root, err := os.MkdirTemp("", "ch") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	oldEuid, oldChown, oldOracle, oldWatch, oldReady, oldRoot := geteuid, chown, newOracle, runWatch, onReady, runRoot
	oldLookup := lookupUser
	defer func() {
		geteuid, chown, newOracle, runWatch, onReady, runRoot = oldEuid, oldChown, oldOracle, oldWatch, oldReady, oldRoot
		lookupUser = oldLookup
	}()
	// The Direct commands relay comes up next to the other sockets when its injector user exists.
	lookupUser = func(string) (*syscall.Credential, error) {
		return &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}, nil
	}
	geteuid = func() int { return 0 }
	chown = func(string, int, int) error { return nil }
	runRoot = root
	pins := make(chan string, 1)
	newOracle = func(pin string) (authsrv.Oracle, error) { pins <- pin; return okOracle{}, nil }
	watching := make(chan struct{})
	runWatch = func(ctx context.Context, w *authsrv.Watch) {
		w.Notify(authproto.Event{Ev: authproto.EvWatchUp})
		close(watching)
		<-ctx.Done()
		w.Notify(authproto.Event{Ev: authproto.EvWatchDown})
	}
	ready := make(chan string, 1)
	onReady = func(s string) { ready <- s }

	sockDir := filepath.Join(root, "airgapp-ch")
	camSock, authSock := filepath.Join(sockDir, "c.sock"), filepath.Join(sockDir, "a.sock")
	cmdSock := filepath.Join(sockDir, "cmd.sock")
	pin := strings.Repeat("ab", 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	code := make(chan int, 1)
	go func() {
		code <- run(ctx, []string{"run", "-state", filepath.Join(root, "airgapp-st"), "-sock", camSock, "-auth-sock", authSock,
			"-authd-sha256", pin, "-gw-uid", strconv.Itoa(os.Getuid()), "-gw-gid", strconv.Itoa(os.Getgid()),
			"-child-uid", strconv.Itoa(os.Getuid()), "-child-gid", strconv.Itoa(os.Getgid()), "-child-groups", "",
			"-teslacam", root}, &out)
	}()
	select {
	case <-ready:
	case c := <-code:
		t.Fatalf("run exited %d: %s", c, out.String())
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	if got := <-pins; got != pin {
		t.Fatalf("the oracle got pin %q", got)
	}
	<-watching
	for _, p := range []string{camSock, authSock, cmdSock} {
		if st, err := os.Stat(p); err != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o660 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	c, err := net.Dial("unix", authSock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	frame := func(v interface{}) {
		b, _ := json.Marshal(v)
		proto.WriteFrame(c, authproto.TReq, b)
	}
	frame(authproto.Request{V: 1, ID: 1, Op: authproto.OpOwner})
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if typ, body, err := proto.ReadFrame(r, authproto.MaxHelperFrame); err != nil || typ != authproto.TResp || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("owner: %#x %s %v", typ, body, err)
	}
	cancel()
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("exit %d: %s", c, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
	for _, p := range []string{camSock, authSock, cmdSock} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
}
