package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/capability"
	"airgapp/gateway/capability/echo"
	"airgapp/gateway/capability/summonrelay"
	"airgapp/gateway/client"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/authsrv"
	"airgapp/gateway/internal/dialout/dialouttest"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/summon/relay"
)

const testVIN = "TESTVIN0000000000"

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestListenRefusesForbidden(t *testing.T) {
	cases := []struct{ addr, want string }{
		{"0.0.0.0:17100", "unspecified"},
		{"[::]:17100", "unspecified"},
		{"192.168.90.100:17100", "192.168.90.0/24"},
		{"100.101.1.1:17100", "100.64.0.0/10"},
		{"127.0.0.1:17100", "loopback"},
		{"[::1]:17100", "loopback"},
		{"example.com:17100", "IP"},
	}
	for _, c := range cases {
		code, _, errs := runCmd(t, "serve", "-listen", c.addr, "-key", "/nonexistent")
		if code == 0 || !strings.Contains(errs, c.want) {
			t.Errorf("%s: code=%d stderr=%q, want refusal naming %q", c.addr, code, errs, c.want)
		}
	}
}

func TestBenchRules(t *testing.T) {
	code, _, errs := runCmd(t, "serve", "-fake-role", "2", "-listen", "127.0.0.1:0")
	if code == 0 || !strings.Contains(errs, "-fake-role requires -bench") {
		t.Errorf("fake-role without bench: code=%d %q", code, errs)
	}
	for _, a := range []string{"192.168.1.5:17100", "0.0.0.0:17100", "192.168.90.100:17100", "100.101.1.1:17100"} {
		code, _, errs = runCmd(t, "serve", "-bench", "-fake-role", "2", "-listen", a)
		if code == 0 || !strings.Contains(errs, "-bench permits only loopback") {
			t.Errorf("bench %s: code=%d %q", a, code, errs)
		}
	}
}

func setupKeys(t *testing.T) (dir, keyPath, vinPath string) {
	t.Helper()
	dir = t.TempDir()
	keyPath, vinPath = filepath.Join(dir, "key.pem"), filepath.Join(dir, "vin")
	os.WriteFile(vinPath, []byte(testVIN+"\n"), 0o600)
	if code, _, e := runCmd(t, "keygen", "-key", keyPath); code != 0 {
		t.Fatalf("keygen: %s", e)
	}
	return
}

// phoneSetup is a gateway's files on disk, in dir, and a phone that may
// sign in to it: the phone's key (written nowhere on the gateway: the car's
// authd role decides), and the gateway's key and VIN as the phone parsed
// them from its pin.
type phoneSetup struct {
	dir, keyPath, vinPath string
	dev                   *ecdh.PrivateKey // the phone's key
	pub                   *ecdsa.PublicKey // the gateway's, from the pin
	vin                   string           // from the pin
}

// setupPhone runs keygen and pin as the owner does, and parses the pin as
// the phone does.
func setupPhone(t *testing.T) phoneSetup {
	t.Helper()
	var p phoneSetup
	p.dir, p.keyPath, p.vinPath = setupKeys(t)
	dev, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.dev = dev
	code, pin, e := runCmd(t, "pin", "-key", p.keyPath, "-vin-file", p.vinPath)
	if code != 0 {
		t.Fatalf("pin: %s", e)
	}
	if p.pub, p.vin, err = client.ParsePin(pin); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinPrintsNoPrivate(t *testing.T) {
	_, keyPath, vinPath := setupKeys(t)
	pem, _ := os.ReadFile(keyPath)
	code, out, errs := runCmd(t, "pin", "-key", keyPath, "-vin-file", vinPath)
	if code != 0 {
		t.Fatal(errs)
	}
	if !strings.HasPrefix(out, "airgapp-gw:1:") || !strings.Contains(out, ":"+testVIN) {
		t.Fatalf("unexpected pin %q", out)
	}
	if strings.Contains(out+errs, "PRIVATE") || strings.Contains(out+errs, strings.TrimSpace(strings.Split(string(pem), "\n")[1])) {
		t.Fatal("pin output leaks private key material")
	}
	if _, _, err := client.ParsePin(out); err != nil {
		t.Fatal(err)
	}
	// keygen refuses to overwrite
	if code, _, _ := runCmd(t, "keygen", "-key", keyPath); code == 0 {
		t.Fatal("keygen overwrote an existing key")
	}
}

// There is no allowlist: no `agw allow` command, and probe-auth refuses
// -allow. serve accepts -allow for one release and ignores it, with one log
// line: an old run script still passes it until the installer rewrites it,
// and a usage error there would restart-loop agw.
func TestNoAllowlist(t *testing.T) {
	if code, _, errs := runCmd(t, "allow", "add", "-allow", "/nonexistent", phoneHex); code != 2 || !strings.Contains(errs, "unknown command") {
		t.Fatalf("allow: code=%d %q, want the unknown-command usage error", code, errs)
	}
	const ignored = "agw: -allow is ignored: access is the car's own owner/driver keys"
	code, _, errs := runCmd(t, "serve", "-bench", "-fake-role", "2", "-listen", "127.0.0.1:0", "-vin", testVIN,
		"-key", "/nonexistent", "-allow", "/var/lib/airgapp-gw/allow")
	if code != 1 || strings.Count(errs, ignored) != 1 || !strings.Contains(errs, "nonexistent") {
		t.Fatalf("serve -allow: code=%d %q, want it ignored (one log line) and serve to go on to the key", code, errs)
	}
	if _, _, errs := runCmd(t, "serve", "-bench", "-fake-role", "2", "-listen", "127.0.0.1:0", "-vin", testVIN,
		"-key", "/nonexistent"); strings.Contains(errs, ignored) {
		t.Fatalf("serve without -allow logs the -allow line: %q", errs)
	}
	if code, _, errs := runCmd(t, "probe-auth", "-allow", "/nonexistent"); code != 2 || !strings.Contains(errs, "-allow") {
		t.Fatalf("probe-auth -allow: code=%d %q, want the flag refused", code, errs)
	}
	if code, _, errs := runCmd(t, "probe-auth"); code != 2 || !strings.Contains(errs, "-pub") {
		t.Fatalf("probe-auth without -pub: code=%d %q, want the usage error", code, errs)
	}
}

func TestServeEchoEndToEnd(t *testing.T) {
	ph := setupPhone(t)

	addrCh := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { addrCh <- a }
	defer func() { onListen = nil }()
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		codeCh <- run(ctx, []string{"serve", "-bench", "-fake-role", "2", "-listen", "127.0.0.1:0",
			"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log")}, &out, &errb)
	}()
	var addr net.Addr
	select {
	case addr = <-addrCh:
	case c := <-codeCh:
		t.Fatalf("serve exited %d: %s", c, errb.String())
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not listen")
	}

	s, err := client.Dial(context.Background(), addr.String(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
	if err != nil {
		t.Fatal(err)
	}
	s.Send(echo.TypeIn, []byte("ping"))
	if typ, body, err := s.Recv(); err != nil || typ != echo.TypeOut || string(body) != "ping" {
		t.Fatalf("echo: %x %q %v", typ, body, err)
	}
	s.Close()
	// The server frees ECHO's slot at the very end of the window's teardown,
	// after the BYE and the socket's close, and writes the session log right
	// after that (internal/server, window.teardown). A sign-in before then
	// finds the slot busy and is closed after its ARM (CR-2), so wait for the
	// first window's record before the second dial.
	sessionLog := filepath.Join(ph.dir, "log")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		b, _ := os.ReadFile(sessionLog)
		if strings.Contains(string(b), `"cap":"ECHO"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first window's session-log record never came:\n%s", b)
		}
	}

	// SIGTERM path: live session gets BYE sigterm, exit 0.
	s2, err := client.Dial(context.Background(), addr.String(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	cancel()
	if _, _, err := s2.Recv(); err == nil || !strings.Contains(err.Error(), "sigterm") {
		t.Fatalf("want BYE sigterm, got %v", err)
	}
	select {
	case c := <-codeCh:
		if c != 0 {
			t.Fatalf("exit %d: %s", c, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit")
	}
	if strings.Contains(out.String()+errb.String(), testVIN) {
		t.Fatal("VIN leaked to output")
	}
}

// The owner's phone key as authd listed it on the car (2026-11-20): Id
// cce6a2d2 (SHA-1 of the key), role 2 OWNER, form factor 6 IOS_DEVICE.
const phoneHex = "04e70805d8e2d25f6337d3ba01c18d7247b38afc0330ee85b21bdf490a85eb667ff5acda7cd5af3d21a90a12d3e3c656d512a598da3a8e6494bf66ea8a9c69a0a9"

// fakeAuthd stands in for authd behind the real sign-in server: a KeyInfo
// list matched by the gateway core's RoleFor, an owner verdict, an error.
type fakeAuthd struct {
	mu    sync.Mutex
	owner bool
	rows  []authority.KeyInfoEntry
	err   error
}

func (f *fakeAuthd) set(owner bool, err error, rows ...authority.KeyInfoEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner, f.err, f.rows = owner, err, rows
}

func (f *fakeAuthd) AuthOwnerIsAuthd(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owner && f.err == nil, f.err
}

func (f *fakeAuthd) KeyEntry(_ context.Context, pk []byte) (authority.KeyInfoEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return authority.KeyInfoEntry{}, f.err
	}
	if _, _, err := authority.RoleFor(f.rows, pk); err != nil {
		return authority.KeyInfoEntry{}, err
	}
	for _, r := range f.rows {
		if bytes.Equal(r.PublicKey, pk) {
			return r, nil
		}
	}
	return authority.KeyInfoEntry{}, errors.New("unreachable")
}

func row(pk []byte, role, ff uint32) authority.KeyInfoEntry {
	return authority.KeyInfoEntry{ID: authority.KeyID(pk), PublicKey: pk, Role: role, FormFactor: ff}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// shortDir is a temp dir with a short path (unix socket paths are limited).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startSignIn serves camhelper's sign-in socket for the test: the real
// server (internal/authsrv, as cmd/camhelper builds it) over fake authd,
// serving this test's uid; agw's client takes this test's uid as the
// helper's (root on the car). It returns the socket, the server and its log.
func startSignIn(t *testing.T, a *fakeAuthd) (string, *authsrv.Server, *syncBuf) {
	t.Helper()
	sock := filepath.Join(shortDir(t), "auth.sock")
	logs := &syncBuf{}
	srv := authsrv.New(authsrv.Config{Oracle: a, AllowUID: os.Getuid(), Log: log.New(logs, "", 0)})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(srv.Shutdown)
	old := newSignIn
	newSignIn = func(sock string, lg *log.Logger) signIn {
		return authority.NewHelper(sock, authority.HelperConfig{HelperUID: os.Getuid(), Log: lg})
	}
	t.Cleanup(func() { newSignIn = old })
	return sock, srv, logs
}

// probe-auth asks the sign-in helper only, and prints the KeyInfo row or the
// error per key.
func TestProbeAuth(t *testing.T) {
	phone, _ := hex.DecodeString(phoneHex)
	a := &fakeAuthd{}
	a.set(true, nil, row(phone, 2, 6))
	sock, _, _ := startSignIn(t, a)
	code, out, errs := runCmd(t, "probe-auth", "-auth-socket", sock, "-pub", phoneHex)
	if code != 0 || out != "owner=ok role=2 fp=fde320123c0fa5f9 id=cce6a2d2 ff=6 secs=0\n" {
		t.Fatalf("ok case: code=%d out=%q err=%q", code, out, errs)
	}
	a.set(true, nil, row(phone, 3, 7))
	code, out, errs = runCmd(t, "probe-auth", "-auth-socket", sock, "-pub", phoneHex)
	if code != 0 || out != "owner=ok role=3 fp=fde320123c0fa5f9 id=cce6a2d2 ff=7 secs=0\n" {
		t.Fatalf("driver case: code=%d out=%q err=%q", code, out, errs)
	}
	a.set(true, errors.New("permission denied"))
	code, out, errs = runCmd(t, "probe-auth", "-auth-socket", sock, "-pub", phoneHex)
	if code == 0 || out != "" || !strings.HasPrefix(errs, "owner=error fp=fde320123c0fa5f9 id=cce6a2d2: ") || !strings.Contains(errs, "permission denied") {
		t.Fatalf("error case: code=%d out=%q err=%q", code, out, errs)
	}
	a.set(true, nil)
	code, out, errs = runCmd(t, "probe-auth", "-auth-socket", sock, "-pub", phoneHex)
	if code == 0 || out != "" || !strings.HasPrefix(errs, "owner=ok role=error fp=fde320123c0fa5f9 id=cce6a2d2: ") ||
		!strings.Contains(errs, "not in authd's KeyInfo list") {
		t.Fatalf("unlisted case: code=%d out=%q err=%q", code, out, errs)
	}
	a.set(true, nil, row(phone, 0, 6))
	if code, out, _ = runCmd(t, "probe-auth", "-auth-socket", sock, "-pub", phoneHex); code == 0 || !strings.Contains(out, "role=0") {
		t.Fatalf("role 0 must fail: code=%d out=%q", code, out)
	}
	a.set(false, nil, row(phone, 2, 6))
	if code, _, errs = runCmd(t, "probe-auth", "-auth-socket", sock, "-pub", phoneHex); code == 0 || !strings.HasPrefix(errs, "owner=not-authd fp=fde320123c0fa5f9 id=cce6a2d2") {
		t.Fatalf("non-authd owner must fail: code=%d err=%q", code, errs)
	}
	// No helper: the error, never a bus.
	none := filepath.Join(shortDir(t), "none.sock")
	if code, _, errs = runCmd(t, "probe-auth", "-auth-socket", none, "-pub", phoneHex); code == 0 || !strings.Contains(errs, "owner=error") || !strings.Contains(errs, "sign-in helper") {
		t.Fatalf("no helper: code=%d err=%q", code, errs)
	}
}

// The authd pin lives in camhelper now: agw has no -authd-sha256.
func TestNoAuthdPinFlag(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	if code, _, errs := runCmd(t, "probe-auth", "-authd-sha256", pin, "-pub", phoneHex); code != 2 || !strings.Contains(errs, "authd-sha256") {
		t.Fatalf("probe-auth: code=%d %q", code, errs)
	}
	if code, _, errs := runCmd(t, "serve", "-bench", "-authd-sha256", pin, "-listen", "127.0.0.1:0"); code != 2 || !strings.Contains(errs, "authd-sha256") {
		t.Fatalf("serve: code=%d %q", code, errs)
	}
}

// startServe runs serve in the background until the test ends and returns
// the listen address, the exit code channel and stderr.
func startServe(t *testing.T, args ...string) (net.Addr, chan int, *syncBuf, context.CancelFunc) {
	t.Helper()
	addrCh := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { addrCh <- a }
	t.Cleanup(func() { onListen = nil })
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	errb := &syncBuf{}
	go func() { codeCh <- run(ctx, append([]string{"serve"}, args...), io.Discard, errb) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-codeCh:
		case <-time.After(5 * time.Second):
			t.Error("serve did not exit")
		}
	})
	select {
	case a := <-addrCh:
		return a, codeCh, errb, cancel
	case c := <-codeCh:
		t.Fatalf("serve exited %d: %s", c, errb.String())
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not listen")
	}
	return nil, nil, nil, nil
}

// Access is the car's own, end to end through the real sign-in helper: a
// phone key that authd lists as OWNER, never written anywhere on the gateway,
// signs in, with a v2 hello (the gateway's key trusted on first connect and
// returned) and with a v1 hello (the pinned key); a key authd lists with role
// 0 is refused.
func TestServeCarKeysAndHelloV2(t *testing.T) {
	ph := setupPhone(t)
	stranger, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAuthd{}
	a.set(true, nil, row(ph.dev.PublicKey().Bytes(), 2, 6), row(stranger.PublicKey().Bytes(), 0, 6))
	sock, _, _ := startSignIn(t, a)
	addr, _, errb, _ := startServe(t, "-bench", "-listen", "127.0.0.1:0", "-auth-socket", sock,
		"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log"))
	sessionLog := filepath.Join(ph.dir, "log")
	waitRecords := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			b, _ := os.ReadFile(sessionLog)
			if strings.Count(string(b), `"cap":"ECHO"`) >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("session-log record %d never came:\n%s", n, b)
			}
		}
	}
	echoOnce := func(s *client.Session) {
		t.Helper()
		s.Send(echo.TypeIn, []byte("ping"))
		if typ, body, err := s.Recv(); err != nil || typ != echo.TypeOut || string(body) != "ping" {
			t.Fatalf("echo: %x %q %v", typ, body, err)
		}
		s.Close()
	}

	// v2, nothing pinned: the challenge carries agw's long-term key.
	s, gk, err := client.DialV2(context.Background(), addr.String(), proto.CapEcho, nil, nil, ph.vin, ph.dev)
	if err != nil {
		t.Fatalf("v2 sign-in: %v\n%s", err, errb)
	}
	if !gk.Equal(ph.pub) {
		t.Fatal("the v2 challenge carried a key other than agw's pin key")
	}
	echoOnce(s)
	waitRecords(1)

	// v2 with the key pinned, then v1: both still sign in.
	s, _, err = client.DialV2(context.Background(), addr.String(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
	if err != nil {
		t.Fatalf("v2 pinned sign-in: %v", err)
	}
	echoOnce(s)
	waitRecords(2)
	s, err = client.Dial(context.Background(), addr.String(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
	if err != nil {
		t.Fatalf("v1 sign-in: %v", err)
	}
	echoOnce(s)
	waitRecords(3)

	// A key authd lists with role 0 proves possession, and is still refused.
	if s, _, err := client.DialV2(context.Background(), addr.String(), proto.CapEcho, nil, nil, ph.vin, stranger); err == nil {
		s.Close()
		t.Fatal("a role 0 key signed in")
	}
	waitLogCount(t, errb, "role 0 not permitted fp="+keys.Fingerprint(stranger.PublicKey().Bytes()), 1)
}

// serve keeps serving when the sign-in helper cannot be reached: each arm
// fails closed instead.
func TestServeKeepsServingWithoutAuthority(t *testing.T) {
	ph := setupPhone(t)
	none := filepath.Join(shortDir(t), "none.sock")
	addr, codeCh, errb, cancel := startServe(t, "-bench", "-listen", "127.0.0.1:0", "-auth-socket", none,
		"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log"))
	if s, err := client.Dial(context.Background(), addr.String(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev); err == nil {
		s.Close()
		t.Fatal("armed without authority")
	}
	select {
	case c := <-codeCh:
		t.Fatalf("serve exited %d after a failed authority check: %s", c, errb.String())
	default:
	}
	cancel()
	select {
	case c := <-codeCh:
		if c != 0 {
			t.Fatalf("exit %d: %s", c, errb.String())
		}
		codeCh <- c // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit")
	}
}

// serve signs in through the helper, and its watch re-checks the open
// window when keys change: a key authd no longer lists is revoked.
func TestServeSignsInThroughTheHelper(t *testing.T) {
	ph := setupPhone(t)
	pk := ph.dev.PublicKey().Bytes()
	a := &fakeAuthd{}
	a.set(true, nil, row(pk, 2, 6))
	sock, asrv, helperLog := startSignIn(t, a)
	addr, codeCh, errb, cancel := startServe(t, "-bench", "-listen", "127.0.0.1:0", "-auth-socket", sock,
		"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log"))
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(helperLog.String(), "watcher attached") {
		if time.Now().After(deadline) {
			t.Fatalf("agw's watch did not attach:\n%s\n%s", helperLog.String(), errb.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, err := client.Dial(context.Background(), addr.String(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
	if err != nil {
		t.Fatalf("arm through the helper: %v\n%s", err, errb.String())
	}
	defer s.Close()
	s.Send(echo.TypeIn, []byte("ping"))
	if typ, body, err := s.Recv(); err != nil || typ != echo.TypeOut || string(body) != "ping" {
		t.Fatalf("echo: %x %q %v", typ, body, err)
	}
	// The key leaves authd's list; authd's KeysUpdated reaches agw.
	a.set(true, nil)
	asrv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
	if _, _, err := s.Recv(); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("want BYE revoked, got %v", err)
	}
	cancel()
	select {
	case c := <-codeCh:
		if c != 0 {
			t.Fatalf("exit %d: %s", c, errb.String())
		}
		codeCh <- c
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit")
	}
	if logs := errb.String(); !strings.Contains(logs, "authority re-check (keys_changed") {
		t.Fatalf("agw log:\n%s", logs)
	}
}

// A burst of keys_changed notices for a key that stays listed (authd may
// send several while keys are edited on the car's screen) must not spend
// the helper's rate limit (20, then 10/s, shared with sign-ins): one pass at
// once and the rest held into one pass a second later, so the window
// survives. With a pass per notice, the helper answered rate_limited and the
// window was revoked.
func TestServeSurvivesAKeysChangedBurst(t *testing.T) {
	ph := setupPhone(t)
	pk := ph.dev.PublicKey().Bytes()
	a := &fakeAuthd{}
	a.set(true, nil, row(pk, 2, 6))
	sock, asrv, helperLog := startSignIn(t, a)
	addr, _, errb, _ := startServe(t, "-bench", "-listen", "127.0.0.1:0", "-auth-socket", sock,
		"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log"))
	waitLog := func(what string, cond func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond(helperLog.String()) {
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %s:\n%s\n%s", what, helperLog.String(), errb.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitLog("the watch", func(l string) bool { return strings.Contains(l, "watcher attached") })
	s, err := client.Dial(context.Background(), addr.String(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
	if err != nil {
		t.Fatalf("arm: %v\n%s", err, errb.String())
	}
	defer s.Close()
	for i := 0; i < 30; i++ {
		asrv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
		time.Sleep(10 * time.Millisecond)
	}
	keyOKs := func(l string) int { return strings.Count(l, ": ok role=2") }
	waitLog("the sign-in, the burst's first pass and its held pass", func(l string) bool { return keyOKs(l) >= 3 })
	s.Send(echo.TypeIn, []byte("ping"))
	if typ, body, err := s.Recv(); err != nil || typ != echo.TypeOut || string(body) != "ping" {
		t.Fatalf("after the burst: %x %q %v\n%s", typ, body, err, errb.String())
	}
	if l := helperLog.String(); strings.Contains(l, "rate limited") || keyOKs(l) > 5 {
		t.Fatalf("%d key answers for one sign-in and a burst:\n%s", keyOKs(l), l)
	}
	if strings.Contains(errb.String(), "revoking") {
		t.Fatalf("agw log:\n%s", errb.String())
	}
}

// countingSignIn counts the KeyRole calls made through one sign-in client.
type countingSignIn struct {
	signIn
	keyRoles atomic.Int32
}

func (c *countingSignIn) KeyRole(ctx context.Context, pk []byte) (int32, error) {
	c.keyRoles.Add(1)
	return c.signIn.KeyRole(ctx, pk)
}

// serve gives the re-checks a sign-in client (a request connection) of
// their own, so a re-check never waits behind a sign-in.
func TestServeRechecksOnTheirOwnConnection(t *testing.T) {
	ph := setupPhone(t)
	pk := ph.dev.PublicKey().Bytes()
	a := &fakeAuthd{}
	a.set(true, nil, row(pk, 2, 6))
	sock, asrv, helperLog := startSignIn(t, a)
	var mu sync.Mutex
	var clients []*countingSignIn
	inner := newSignIn
	newSignIn = func(sock string, lg *log.Logger) signIn {
		c := &countingSignIn{signIn: inner(sock, lg)}
		mu.Lock()
		clients = append(clients, c)
		mu.Unlock()
		return c
	}
	t.Cleanup(func() { newSignIn = inner })
	addr, _, errb, _ := startServe(t, "-bench", "-listen", "127.0.0.1:0", "-auth-socket", sock,
		"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log"))
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(helperLog.String(), "watcher attached") {
		if time.Now().After(deadline) {
			t.Fatalf("agw's watch did not attach:\n%s\n%s", helperLog.String(), errb.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, err := client.Dial(context.Background(), addr.String(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
	if err != nil {
		t.Fatalf("arm: %v\n%s", err, errb.String())
	}
	defer s.Close()
	a.set(true, nil)
	asrv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
	if _, _, err := s.Recv(); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("want BYE revoked, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(clients) != 2 {
		t.Fatalf("serve made %d sign-in clients, want 2 (sign-ins and the watch; re-checks)", len(clients))
	}
	if n := clients[0].keyRoles.Load(); n != 2 {
		t.Fatalf("the sign-in client was asked %d times, want only the sign-in's two (the hello's filter, the ARM's gate)", n)
	}
	if n := clients[1].keyRoles.Load(); n == 0 {
		t.Fatal("the re-check did not use its own client")
	}
}

// probeBus swaps the bus self-test's probe and wait for one test (and
// shortens the pause between probes).
func probeBus(t *testing.T, probe func(time.Duration) (authority.BusVerdict, error), wait time.Duration) {
	oldProbe, oldWait, oldPause := busProbe, busRefusal, busProbePause
	busProbe, busRefusal, busProbePause = probe, wait, time.Millisecond
	t.Cleanup(func() { busProbe, busRefusal, busProbePause = oldProbe, oldWait, oldPause })
}

// verdicts is a probe answering vs in turn (the last one from then on),
// counting its calls.
func verdicts(t *testing.T, probed *int, vs ...authority.BusVerdict) func(time.Duration) (authority.BusVerdict, error) {
	return func(d time.Duration) (authority.BusVerdict, error) {
		if d < time.Second || d > 3*time.Second {
			t.Errorf("probe bounded by %v, want about 2 s", d)
		}
		v := vs[min(*probed, len(vs)-1)]
		*probed++
		switch v {
		case authority.BusReachable:
			return v, nil
		case authority.BusRefused:
			return v, errors.New("system bus: disconnected after BEGIN: EOF")
		}
		return v, errors.New("system bus: no answer within 2s (Hello)")
	}
}

// failsWithoutServing runs serve (not -bench) and requires it to fail after
// the wait, with BUS SELF-TEST FAILED and want in its log, never passing the
// self-test or going on to the key.
func failsWithoutServing(t *testing.T, wait time.Duration, want ...string) string {
	t.Helper()
	start := time.Now()
	code, _, errs := runCmd(t, "serve", "-listen", "192.0.2.1:17100", "-key", "/nonexistent")
	if code != 1 {
		t.Fatalf("code=%d: %s", code, errs)
	}
	if d := time.Since(start); d < wait {
		t.Fatalf("failed after %v, before the wait", d)
	}
	for _, w := range append([]string{"BUS SELF-TEST FAILED", "refusing to serve"}, want...) {
		if !strings.Contains(errs, w) {
			t.Fatalf("stderr lacks %q:\n%s", w, errs)
		}
	}
	if strings.Contains(errs, "self-test passed") || strings.Contains(errs, "nonexistent") {
		t.Fatalf("serve passed the self-test or went on to load the key:\n%s", errs)
	}
	return errs
}

// While agw can reach the system bus (dbus-daemon's uid cache after the old
// overlay test), serve logs it loudly, waits, and fails without serving.
func TestServeRefusesWhileTheBusIsReachable(t *testing.T) {
	probed := 0
	probeBus(t, verdicts(t, &probed, authority.BusReachable), 100*time.Millisecond)
	failsWithoutServing(t, 100*time.Millisecond, fmt.Sprintf("uid %d", os.Getuid()), "must not", "reboot",
		fmt.Sprintf("agw serve: refusing to serve: uid %d can reach the system bus", os.Getuid()))
	if probed != 1 {
		t.Fatalf("probed %d times, want 1", probed)
	}
	// The wait ends early at shutdown, still failing serve.
	probeBus(t, verdicts(t, new(int), authority.BusReachable), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- run(ctx, []string{"serve", "-listen", "192.0.2.1:17100", "-key", "/nonexistent"}, io.Discard, io.Discard)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case c := <-codeCh:
		if c != 1 {
			t.Fatalf("exit %d after SIGTERM in the wait, want 1", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM did not end the wait")
	}
}

// A probe without a verdict (the bus did not answer within the bound: a
// busy bus may still let agw in) is never a pass: after busProbeTries
// probes serve fails like a reachable bus.
func TestServeRefusesWithoutABusVerdict(t *testing.T) {
	probed := 0
	probeBus(t, verdicts(t, &probed, authority.BusUnknown), 100*time.Millisecond)
	failsWithoutServing(t, 100*time.Millisecond, "no verdict", "no answer within 2s",
		fmt.Sprintf("agw serve: refusing to serve: the bus self-test has no verdict for uid %d", os.Getuid()))
	if probed != busProbeTries {
		t.Fatalf("probed %d times, want %d", probed, busProbeTries)
	}
}

// A probe without a verdict is tried again: two refusals after it pass, a
// connection after it fails.
func TestServeBusSelfTestRetriesForAVerdict(t *testing.T) {
	probed := 0
	probeBus(t, verdicts(t, &probed, authority.BusUnknown, authority.BusRefused), time.Hour)
	code, _, errs := runCmd(t, "serve", "-listen", "192.0.2.1:17100", "-key", "/nonexistent")
	if code != 1 || probed != 3 || !strings.Contains(errs, "bus self-test passed") || !strings.Contains(errs, "nonexistent") ||
		strings.Contains(errs, "BUS SELF-TEST FAILED") {
		t.Fatalf("unknown, refused, refused: code=%d probed=%d: %s", code, probed, errs)
	}
	probed = 0
	probeBus(t, verdicts(t, &probed, authority.BusUnknown, authority.BusReachable), 10*time.Millisecond)
	failsWithoutServing(t, 10*time.Millisecond, "can reach the system bus")
	if probed != 2 {
		t.Fatalf("unknown, reachable: probed %d times, want 2", probed)
	}
}

// Only a definite refusal, two probes in a row, passes, and serve goes on
// (here to the missing key); -bench skips the self-test.
func TestServeBusSelfTestPasses(t *testing.T) {
	probed := 0
	probeBus(t, verdicts(t, &probed, authority.BusRefused), time.Hour)
	code, _, errs := runCmd(t, "serve", "-listen", "192.0.2.1:17100", "-key", "/nonexistent")
	if code != 1 || probed != 2 || !strings.Contains(errs, "nonexistent") || !strings.Contains(errs, "disconnected after BEGIN") ||
		!strings.Contains(errs, fmt.Sprintf("bus self-test passed: the system bus refuses uid %d", os.Getuid())) ||
		strings.Contains(errs, "BUS SELF-TEST FAILED") {
		t.Fatalf("code=%d probed=%d: %s", code, probed, errs)
	}
	if code, _, errs = runCmd(t, "serve", "-bench", "-fake-role", "2", "-listen", "127.0.0.1:0", "-key", "/nonexistent"); code != 1 || probed != 2 {
		t.Fatalf("-bench: code=%d probed=%d: %s", code, probed, errs)
	}
}

// One refusal is not a verdict. dbus-daemon also closes a connection that
// has not finished Hello when too many are incomplete
// (max_incomplete_connections, 64 on the car), and the probe cannot tell
// that close from a refusal of the uid. A refusal must repeat on the next
// probe: a connection after one fails, and refusals with a probe without a
// verdict between them do not pass.
func TestServeBusSelfTestConfirmsARefusal(t *testing.T) {
	probed := 0
	probeBus(t, verdicts(t, &probed, authority.BusRefused, authority.BusReachable), 10*time.Millisecond)
	failsWithoutServing(t, 10*time.Millisecond, "can reach the system bus")
	if probed != 2 {
		t.Fatalf("refused, reachable: probed %d times, want 2", probed)
	}

	probed = 0
	probeBus(t, verdicts(t, &probed, authority.BusRefused, authority.BusUnknown, authority.BusRefused, authority.BusUnknown), 10*time.Millisecond)
	failsWithoutServing(t, 10*time.Millisecond, "not 2 probes in a row", "own connection limits",
		fmt.Sprintf("agw serve: refusing to serve: the bus self-test did not confirm a refusal of uid %d", os.Getuid()))
	if probed != busProbeTries {
		t.Fatalf("refused, unknown, refused, unknown: probed %d times, want %d", probed, busProbeTries)
	}

	probed = 0
	probeBus(t, verdicts(t, &probed, authority.BusRefused, authority.BusUnknown, authority.BusRefused, authority.BusRefused), time.Hour)
	code, _, errs := runCmd(t, "serve", "-listen", "192.0.2.1:17100", "-key", "/nonexistent")
	if code != 1 || probed != 4 || !strings.Contains(errs, "bus self-test passed") || !strings.Contains(errs, "nonexistent") ||
		strings.Contains(errs, "BUS SELF-TEST FAILED") {
		t.Fatalf("refused, unknown, refused, refused: code=%d probed=%d: %s", code, probed, errs)
	}
}

// The real probe against a system bus that takes the connection but does
// not answer within the bound (a busy or stalled dbus-daemon, which may
// still let the uid in): serve refuses, it never logs a pass.
func TestServeRefusesASlowBus(t *testing.T) {
	sock := filepath.Join(shortDir(t), "bus")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path="+sock)
	t.Setenv("DBUS_LAUNCHD_SESSION_BUS_SOCKET", sock)
	var conns, closed atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer c.Close()
				io.Copy(io.Discard, c) // reads, answers nothing
				closed.Add(1)
			}()
		}
	}()
	oldTimeout := busProbeTimeout
	busProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { busProbeTimeout = oldTimeout })
	probeBus(t, authority.ProbeSystemBus, 10*time.Millisecond)
	failsWithoutServing(t, 10*time.Millisecond, "no verdict", "no answer within 100ms")
	if n := conns.Load(); n != busProbeTries {
		t.Fatalf("%d probes reached the bus, want %d", n, busProbeTries)
	}
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() != busProbeTries {
		if time.Now().After(deadline) {
			t.Fatalf("the probes left %d connection(s) open", busProbeTries-closed.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSelectCaps(t *testing.T) {
	old := capTable
	defer func() { capTable = old }()
	capTable = append(append(capTable[:0:0], old...), struct {
		name      string
		benchOnly bool
		new       func() capability.Capability
	}{"REAL", false, func() capability.Capability { return realCap{} }})
	names := func(bench, fake bool) string {
		var n []string
		for _, c := range selectCaps(bench, fake) {
			n = append(n, c.Spec().Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(false, false); got != "CAMERA,DASHCAM,SUMMON,VCMD,REAL" {
		t.Errorf("car build: %q (ECHO must be absent)", got)
	}
	if got := names(true, false); got != "ECHO,CAMERA,DASHCAM,SUMMON,VCMD,REAL" {
		t.Errorf("bench: %q", got)
	}
	if got := names(true, true); got != "ECHO" {
		t.Errorf("bench+fake: %q (real caps must never run on a fake authority)", got)
	}
	if got := names(false, true); got != "" {
		t.Errorf("fake without bench must host nothing real, got %q", got)
	}
}

type realCap struct{ capability.Capability }

func (realCap) Spec() capability.Spec { return capability.Spec{Name: "REAL", ID: 0x04} }

// serve hosts the SUMMON relay through newSummonRelay, with the real sign-in (never -fake-role, which serves only the
// bench rows): the listen line names it, and a phone's SUMMON window carries its bytes to the helper's relay socket
// and back.
func TestServeRegistersSummonRelay(t *testing.T) {
	ph := setupPhone(t)
	a := &fakeAuthd{}
	a.set(true, nil, row(ph.dev.PublicKey().Bytes(), 2, 6))
	authSock, _, _ := startSignIn(t, a)
	relaySock := filepath.Join(shortDir(t), "relay.sock")
	ln, err := relay.Listen(relay.ListenConfig{Path: relaySock, AllowUID: os.Getuid(), Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	old := newSummonRelay
	newSummonRelay = func() capability.Capability {
		return summonrelay.NewWithConfig(summonrelay.Config{Socket: relaySock, HelperUID: os.Getuid(),
			RouteCheck: func(context.Context) error { return nil }})
	}
	t.Cleanup(func() { newSummonRelay = old })

	addr, _, errb, _ := startServe(t, "-bench", "-listen", "127.0.0.1:0", "-auth-socket", authSock,
		"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log"))
	if logs := errb.String(); !strings.Contains(logs, "agw: listening on "+addr.String()+" (capabilities ECHO,CAMERA,DASHCAM,SUMMON,VCMD)") {
		t.Fatalf("the listen line does not name the served capabilities:\n%s", logs)
	}

	s, err := client.Dial(context.Background(), addr.String(), proto.CapSummon, nil, ph.pub, ph.vin, ph.dev)
	if err != nil {
		t.Fatalf("arm SUMMON: %v\n%s", err, errb.String())
	}
	defer s.Close()
	if err := s.Send(summonrelay.TypeInner, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	var c net.Conn
	select {
	case c = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatalf("the phone's bytes never reached the helper:\n%s", errb.String())
	}
	defer c.Close()
	got := make([]byte, 5)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "hello" {
		t.Fatalf("the helper read %q, %v", got, err)
	}
	if _, err := c.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if typ, body, err := s.Recv(); err != nil || typ != summonrelay.TypeInnerOut || string(body) != "world" {
		t.Fatalf("the phone got 0x%02x %q, %v", typ, body, err)
	}
}

// prefsServer serves body as tailscaled's LocalAPI prefs on a unix socket and returns the socket.
func prefsServer(t *testing.T, body string) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "ts.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/localapi/v0/prefs" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

// probe-route runs the relay's R1 gate against tailscaled's prefs, as the installer does as uid 4950. It prints only
// the verdict, never the prefs.
func TestProbeRoute(t *testing.T) {
	code, out, errs := runCmd(t, "probe-route", "-tailscale-sock", prefsServer(t, `{"AdvertiseRoutes":[]}`))
	if code != 0 || out != "route gate: ok\n" {
		t.Fatalf("no routes: code=%d out=%q err=%q", code, out, errs)
	}
	body := `{"AdvertiseRoutes":["192.168.90.0/24"],"LoggedOutLogin":"owner@example.com"}`
	code, out, errs = runCmd(t, "probe-route", "-tailscale-sock", prefsServer(t, body))
	if code != 1 || out != "route gate: refused\n" {
		t.Fatalf("vehicle LAN advertised: code=%d out=%q err=%q", code, out, errs)
	}
	if strings.Contains(out+errs, "192.168.90") || strings.Contains(out+errs, "owner@example.com") {
		t.Fatalf("the prefs body was echoed: out=%q err=%q", out, errs)
	}
	code, out, errs = runCmd(t, "probe-route", "-tailscale-sock", filepath.Join(shortDir(t), "none.sock"))
	if code != 1 || out != "route gate: refused\n" {
		t.Fatalf("no tailscaled: code=%d out=%q err=%q", code, out, errs)
	}
}

// probe-summon checks the relay socket as the relay's Open does (the server's uid, then OWNED) and closes without
// sending a record, as the installer does as uid 4950.
func TestProbeSummon(t *testing.T) {
	old := probeSummonUID
	probeSummonUID = os.Getuid()
	t.Cleanup(func() { probeSummonUID = old })
	sock := filepath.Join(shortDir(t), "relay.sock")
	var ready atomic.Bool
	ready.Store(true)
	logs := &syncBuf{}
	ln, err := relay.Listen(relay.ListenConfig{Path: sock, AllowUID: os.Getuid(), Ready: ready.Load,
		Logf: func(format string, args ...any) { fmt.Fprintf(logs, format+"\n", args...) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	if code, out, errs := runCmd(t, "probe-summon", "-socket", sock); code != 0 || out != "summon relay: ok\n" {
		t.Fatalf("ok: code=%d out=%q err=%q", code, out, errs)
	}
	waitLogCount(t, logs, "relay: closed a connection: first record: EOF", 1) // OWNED read, nothing sent

	refused := func(what string) string {
		t.Helper()
		code, out, errs := runCmd(t, "probe-summon", "-socket", sock)
		if code != 1 || !strings.HasPrefix(out, "summon relay: refused: ") {
			t.Fatalf("%s: code=%d out=%q err=%q", what, code, out, errs)
		}
		return out
	}
	probeSummonUID = os.Getuid() + 1
	if out := refused("served by another uid"); !strings.Contains(out, fmt.Sprint(os.Getuid())) {
		t.Fatalf("the refusal does not name the server's uid: %q", out)
	}
	probeSummonUID = os.Getuid()
	ready.Store(false) // the helper does not own the name, or is unhealthy: it closes without OWNED
	refused("helper not ready")
	ready.Store(true)
	sock = filepath.Join(shortDir(t), "none.sock")
	refused("no socket")
}

// probe-summon replays Open exactly (INT-3): it waits for OWNED only as long as Open does, summonrelay.OwnedWait
// (500 ms). An OWNED that comes later fails every real Open, so it fails the check too; one in time passes.
func TestProbeSummonWaitsAsOpenDoes(t *testing.T) {
	old := probeSummonUID
	probeSummonUID = os.Getuid()
	t.Cleanup(func() { probeSummonUID = old })
	if summonrelay.OwnedWait != 500*time.Millisecond {
		t.Fatalf("summonrelay.OwnedWait = %v, want 500ms (the Global Constraints)", summonrelay.OwnedWait)
	}
	// A helper that sends OWNED after delay on every connection.
	helper := func(delay time.Duration) string {
		t.Helper()
		sock := filepath.Join(shortDir(t), "relay.sock")
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.AcceptUnix()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					time.Sleep(delay)
					relay.NewWriter(c).Owned()
					c.SetReadDeadline(time.Now().Add(2 * time.Second))
					io.Copy(io.Discard, c) // until the probe closes
				}()
			}
		}()
		return sock
	}
	late := helper(900 * time.Millisecond) // after Open's 500 ms, within the old 1 s
	start := time.Now()
	code, out, errs := runCmd(t, "probe-summon", "-socket", late)
	if code != 1 || !strings.HasPrefix(out, "summon relay: refused: no OWNED within 500ms") {
		t.Fatalf("an OWNED 900 ms late: code=%d out=%q err=%q; want the refusal Open would give", code, out, errs)
	}
	if took := time.Since(start); took > 880*time.Millisecond {
		t.Fatalf("probe-summon waited %v for OWNED, want about 500 ms", took)
	}
	if code, out, errs := runCmd(t, "probe-summon", "-socket", helper(100*time.Millisecond)); code != 0 ||
		out != "summon relay: ok\n" {
		t.Fatalf("an OWNED 100 ms late: code=%d out=%q err=%q", code, out, errs)
	}
}

func TestServeNeedsExactlyOneOfListenAndDial(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "-bench", "-fake-role", "2", "-listen", "127.0.0.1:0", "-dial", "127.0.0.1:5"},
		{"serve", "-bench", "-fake-role", "2"},
		{"serve", "-tailnet", "-dial", "192.168.4.1"},
		{"serve", "-tailnet", "-listen", "192.168.4.2"},
	} {
		code, _, errs := runCmd(t, args...)
		if code != 2 || !strings.Contains(errs, "exactly one of -listen, -dial and -tailnet") {
			t.Errorf("%v: code=%d stderr=%q", args, code, errs)
		}
	}
}

func TestCheckDial(t *testing.T) {
	cases := []struct {
		in    string
		bench bool
		want  string // the result, or "!" + a substring of the error
	}{
		{"192.168.4.1", false, "192.168.4.1:17101"},
		{"192.168.4.1:9", false, "192.168.4.1:9"},
		{"192.168.90.10:17101", false, "!R22"},
		{"100.100.1.1", false, "!tailnet"},
		{"0.0.0.0", false, "!unspecified"},
		{"127.0.0.1", false, "!loopback"},
		{"127.0.0.1:5", true, "127.0.0.1:5"},
		{"pi.local", false, "!literal IP"},
	}
	for _, c := range cases {
		got, err := checkDial(c.in, c.bench)
		if strings.HasPrefix(c.want, "!") {
			if err == nil || !strings.Contains(err.Error(), c.want[1:]) {
				t.Errorf("%s: got %q, %v, want error containing %q", c.in, got, err, c.want[1:])
			}
		} else if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v, want %q", c.in, got, err, c.want)
		}
	}
}

func TestServeDialEndToEnd(t *testing.T) {
	ph := setupPhone(t)

	pi := dialouttest.Start(t)
	addrCh := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { addrCh <- a }
	defer func() { onListen = nil }()
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		codeCh <- run(ctx, []string{"serve", "-bench", "-fake-role", "2", "-dial", pi.CarAddr(),
			"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log")}, &out, &errb)
	}()
	defer func() {
		cancel()
		select {
		case <-codeCh:
		case <-time.After(5 * time.Second):
			t.Error("serve did not exit")
		}
	}()
	select {
	case <-addrCh:
	case c := <-codeCh:
		t.Fatalf("serve exited %d: %s", c, errb.String())
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start")
	}
	waitIdle := func(d time.Duration) {
		t.Helper()
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if pi.Idle() == 3 {
				return
			}
		}
		t.Fatalf("Pi idle = %d, want 3", pi.Idle())
	}
	waitIdle(5 * time.Second)

	for i := 0; i < 2; i++ {
		s, err := client.Dial(context.Background(), pi.PhoneAddr(), proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		s.Send(echo.TypeIn, []byte("ping"))
		if typ, body, err := s.Recv(); err != nil || typ != echo.TypeOut || string(body) != "ping" {
			t.Fatalf("echo %d: %x %q %v", i, typ, body, err)
		}
		s.Close()
		waitIdle(time.Second)
	}
}

func TestServeTailnetRefusesBench(t *testing.T) {
	code, _, errs := runCmd(t, "serve", "-bench", "-fake-role", "2", "-tailnet")
	if code != 2 || !strings.Contains(errs, "-tailnet cannot be used with -bench") {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
}

func TestTailnetListenAddr(t *testing.T) {
	prefix := func(ss ...string) []net.Addr {
		var out []net.Addr
		for _, s := range ss {
			ip, n, err := net.ParseCIDR(s)
			if err != nil {
				t.Fatal(err)
			}
			n.IP = ip
			out = append(out, n)
		}
		return out
	}
	cases := []struct {
		name  string
		addrs []net.Addr
		err   error
		want  string // the bind address, or "!" + a substring of the error
	}{
		{"one tailnet v4, v6 ignored", prefix("100.100.1.1/32", "fd7a:115c:a1e0::1/128"), nil, "100.100.1.1:17100"},
		{"another tailnet v4", prefix("100.101.1.1/32"), nil, "100.101.1.1:17100"},
		{"no interface", nil, errors.New("no such network interface"), "!is Tailscale up?"},
		{"no v4", prefix("fd7a:115c:a1e0::1/128"), nil, "!no IPv4 address"},
		{"two v4", prefix("100.100.1.1/32", "100.101.1.1/32"), nil, "!2 IPv4 addresses"},
		{"outside the range", prefix("192.168.4.233/24"), nil, "!outside the tailnet range"},
		{"vehicle LAN", prefix("192.168.90.100/24"), nil, "!outside the tailnet range"},
	}
	orig := ifaceAddrs
	t.Cleanup(func() { ifaceAddrs = orig })
	for _, c := range cases {
		var asked string
		ifaceAddrs = func(name string) ([]net.Addr, error) { asked = name; return c.addrs, c.err }
		got, err := tailnetListenAddr()
		if asked != "tailscale0" {
			t.Errorf("%s: asked for interface %q, want tailscale0", c.name, asked)
		}
		if strings.HasPrefix(c.want, "!") {
			if err == nil || !strings.Contains(err.Error(), c.want[1:]) {
				t.Errorf("%s: got %q, %v, want error containing %q", c.name, got, err, c.want[1:])
			}
		} else if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v, want %q", c.name, got, err, c.want)
		}
	}
}

// TestServeTailnetNoAddress: the tailnet range cannot be bound in a test, so this checks the refusal end to end:
// serve stops before reading the key when tailscale0 has no address. Binding itself is -listen's net.Listen.
func TestServeTailnetNoAddress(t *testing.T) {
	orig := ifaceAddrs
	t.Cleanup(func() { ifaceAddrs = orig })
	ifaceAddrs = func(string) ([]net.Addr, error) { return nil, nil }
	origWait := tailnetRetryWait
	t.Cleanup(func() { tailnetRetryWait = origWait })
	tailnetRetryWait = 50 * time.Millisecond
	start := time.Now()
	code, _, errs := runCmd(t, "serve", "-tailnet", "-key", "/nonexistent")
	if time.Since(start) < tailnetRetryWait {
		t.Errorf("exited before the retry wait: runit would restart it in a tight loop")
	}
	if code != 1 || !strings.Contains(errs, "tailscale0 has no IPv4 address") {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
}

func TestWatchTailnetAddr(t *testing.T) {
	origAddrs, origEvery := ifaceAddrs, tailnetCheckEvery
	t.Cleanup(func() { ifaceAddrs, tailnetCheckEvery = origAddrs, origEvery })
	tailnetCheckEvery = 5 * time.Millisecond
	addr := func(s string) []net.Addr {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(s), Mask: net.CIDRMask(32, 32)}}
	}

	var mu sync.Mutex
	cur, curErr := addr("100.100.1.1"), error(nil)
	ifaceAddrs = func(string) ([]net.Addr, error) { mu.Lock(); defer mu.Unlock(); return cur, curErr }
	set := func(a []net.Addr, err error) { mu.Lock(); cur, curErr = a, err; mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	moved := make(chan error, 1)
	go watchTailnetAddr(ctx, "100.100.1.1:17100", moved)

	// Unchanged, then unreadable (Tailscale restarting): no move.
	time.Sleep(30 * time.Millisecond)
	set(nil, errors.New("no such network interface"))
	time.Sleep(30 * time.Millisecond)
	select {
	case err := <-moved:
		t.Fatalf("reported a move without one: %v", err)
	default:
	}
	// A new address: one move, naming both.
	set(addr("100.101.1.1"), nil)
	select {
	case err := <-moved:
		if !strings.Contains(err.Error(), "100.100.1.1:17100") || !strings.Contains(err.Error(), "100.101.1.1:17100") {
			t.Fatalf("move error %q", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no move reported")
	}
}
