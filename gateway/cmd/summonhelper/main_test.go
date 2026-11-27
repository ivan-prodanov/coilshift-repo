package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/client"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/summon/helper"
	"airgapp/gateway/internal/summon/logq"
	"airgapp/gateway/internal/summon/marker"
	"airgapp/gateway/internal/supervise"
)

const goodVIN = "5YJ3E1EA7KF000000"

// syncBuf is stderr for run: the log queue's writer goroutine writes it.
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

func runCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb syncBuf
	code = run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// keygen creates a key in a fresh directory and returns its path.
func keygen(t *testing.T) string {
	t.Helper()
	key := filepath.Join(t.TempDir(), "key.pem")
	if code, _, e := runCmd(t, "keygen", "-key", key); code != 0 {
		t.Fatalf("keygen: exit %d: %s", code, e)
	}
	return key
}

// setup records what serve set up, in order, and replaces the bus, the oracle and helper.Serve with stand-ins; it
// records the dumpability call and the key load as they pass (the key load still loads). The stand-in for
// helper.Serve logs one line through the standard logger, as the server core does, and one through Config.Logf.
type setup struct {
	mu                  sync.Mutex
	buses, oracles, ran int
	pin                 string
	cfg                 helper.Config
	stdLog              io.Writer // the standard logger's output while helper.Serve ran
	events              []string  // "dumps", "key", "oracle", "bus", "serve", in the order serve reached them
}

func (s *setup) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buses + s.oracles + s.ran
}

func (s *setup) event(e string) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *setup) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

func stubServe(t *testing.T) *setup {
	t.Helper()
	s := &setup{}
	prevBus, prevOracle, prevServe, prevDumps, prevKey := newBus, newOracle, serveHelper, disableDumps, loadKey
	disableDumps = func() error {
		s.event("dumps")
		return nil
	}
	loadKey = func(path string) (*ecdsa.PrivateKey, error) {
		s.event("key")
		return keys.Load(path)
	}
	newBus = func() (carlink.Bus, error) {
		s.event("bus")
		s.mu.Lock()
		s.buses++
		s.mu.Unlock()
		return carlink.NewFakeBus(), nil
	}
	newOracle = func(pin string) (authority.Oracle, error) {
		s.event("oracle")
		s.mu.Lock()
		s.oracles++
		s.pin = pin
		s.mu.Unlock()
		return authority.Fake{Role: 2, Owner: true}, nil
	}
	serveHelper = func(_ context.Context, cfg helper.Config) error {
		s.event("serve")
		s.mu.Lock()
		s.ran++
		s.cfg = cfg
		s.stdLog = log.Writer()
		s.mu.Unlock()
		log.Print("agw: a line through the standard logger")
		cfg.Logf("summonhelper: a line through Logf")
		return nil
	}
	t.Cleanup(func() {
		newBus, newOracle, serveHelper, disableDumps, loadKey = prevBus, prevOracle, prevServe, prevDumps, prevKey
	})
	return s
}

// unsetenv unsets key for the test; t.Setenv restores it afterwards.
func unsetenv(t *testing.T, key string) {
	t.Setenv(key, "")
	os.Unsetenv(key)
}

func TestKeygenAndPin(t *testing.T) {
	key := keygen(t)
	st, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o400 {
		t.Fatalf("key.pem has mode %v, want 0400", st.Mode().Perm())
	}

	code, out, e := runCmd(t, "pin", "-key", key, "-vin", goodVIN)
	if code != 0 {
		t.Fatalf("pin: exit %d: %s", code, e)
	}
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("pin printed %q: want the pin alone, on one line", out)
	}
	pin := strings.TrimSuffix(out, "\n")
	if !regexp.MustCompile(`^airgapp-gw:1:[0-9a-f]{130}:5YJ3E1EA7KF000000$`).MatchString(pin) {
		t.Fatalf("pin %q does not match the pin format", pin)
	}
	// It is the key's pin, as the phone parses it.
	k, err := keys.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	pub, vin, err := client.ParsePin(pin)
	if err != nil || !pub.Equal(&k.PublicKey) || vin != goodVIN {
		t.Fatalf("ParsePin(%q) = key equal %v, VIN %q, %v", pin, err == nil && pub.Equal(&k.PublicKey), vin, err)
	}

	// -vin-file gives the same pin.
	vinFile := filepath.Join(t.TempDir(), "vin")
	if err := os.WriteFile(vinFile, []byte(goodVIN+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out2, e := runCmd(t, "pin", "-key", key, "-vin-file", vinFile); code != 0 || out2 != out {
		t.Fatalf("pin -vin-file: exit %d, %q (want %q): %s", code, out2, out, e)
	}

	// keygen never overwrites a key.
	if code, _, _ := runCmd(t, "keygen", "-key", key); code == 0 {
		t.Fatal("keygen overwrote an existing key")
	}
}

func TestPinRefusesBadVIN(t *testing.T) {
	key := keygen(t)
	const bad = "5YJ3E1EA7KF00000I"
	code, out, e := runCmd(t, "pin", "-key", key, "-vin", bad)
	if code != 1 || out != "" {
		t.Fatalf("pin with an invalid VIN: exit %d, stdout %q; want exit 1 and nothing printed", code, out)
	}
	if !strings.Contains(e, "not a valid VIN") || strings.Contains(e, bad) {
		t.Fatalf("pin's refusal is %q: want it to say the VIN is not valid, without the value", e)
	}
	vinFile := filepath.Join(t.TempDir(), "vin")
	os.WriteFile(vinFile, []byte(goodVIN), 0o600)
	if code, out, _ := runCmd(t, "pin", "-key", key, "-vin", goodVIN, "-vin-file", vinFile); code != 2 || out != "" {
		t.Fatalf("pin with both -vin and -vin-file: exit %d, stdout %q; want a usage error", code, out)
	}
}

func TestServeRefusesWithoutLiveness(t *testing.T) {
	s := stubServe(t)
	key := keygen(t)
	t.Setenv(vinEnv, goodVIN)
	for _, c := range []struct {
		name  string
		unset bool
	}{{"unset", true}, {"empty", false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(supervise.LivenessEnv, "")
			if c.unset {
				os.Unsetenv(supervise.LivenessEnv)
			}
			code, _, e := runCmd(t, "serve", "-key", key)
			if code != 1 {
				t.Fatalf("serve without %s: exit %d, want 1", supervise.LivenessEnv, code)
			}
			if !strings.Contains(e, "AGW_LIVENESS_FD is not set") {
				t.Fatalf("serve's refusal is %q, want the liveness refusal", e)
			}
			if n := s.count(); n != 0 {
				t.Fatalf("serve set up %d of the bus, the oracle and the helper before refusing", n)
			}
		})
	}
}

func TestServeRefusesBadVIN(t *testing.T) {
	s := stubServe(t)
	key := keygen(t)
	cases := []struct {
		name, vin       string
		unset, liveness bool
	}{
		{"unset", "", true, true},
		{"empty", "", false, true},
		{"16 characters", "5YJ3E1EA7KF00000", false, true},
		{"contains I", "5YJ3E1EA7KF00000I", false, true},
		{"checked before liveness", "5YJ3E1EA7KF00000I", false, false}, // R21: the VIN first
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(vinEnv, c.vin)
			if c.unset {
				os.Unsetenv(vinEnv)
			}
			if c.liveness {
				t.Setenv(supervise.LivenessEnv, "3") // set: only the VIN can refuse
			} else {
				unsetenv(t, supervise.LivenessEnv)
			}
			code, out, e := runCmd(t, "serve", "-key", key)
			if code != 1 {
				t.Fatalf("serve with VIN %q: exit %d, want 1", c.vin, code)
			}
			if !strings.Contains(e, "SUMMONHELPER_VIN is not a valid VIN") {
				t.Fatalf("serve's refusal is %q, want the VIN refusal", e)
			}
			if c.vin != "" && strings.Contains(e+out, c.vin) {
				t.Fatalf("the log contains the refused value: %q", e)
			}
			if n := s.count(); n != 0 {
				t.Fatalf("serve set up %d of the bus, the oracle and the helper before refusing", n)
			}
		})
	}
}

// The flags and the environment reach helper.Serve, and so do the defaults.
func TestServeFlagsReachServe(t *testing.T) {
	s := stubServe(t)
	key := keygen(t)
	t.Setenv(vinEnv, goodVIN)
	t.Setenv(supervise.LivenessEnv, "3")
	pin := strings.Repeat("ab", 32)
	code, _, e := runCmd(t, "serve", "-key", key, "-socket", "/x/relay.sock", "-log", "/x/log",
		"-marker", "/x/session", "-authd-sha256", pin, "-allow-uid", "4321")
	if code != 0 {
		t.Fatalf("serve: exit %d: %s", code, e)
	}
	k, err := keys.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	cfg, gotPin, oracles := s.cfg, s.pin, s.oracles
	s.mu.Unlock()
	if cfg.Socket != "/x/relay.sock" || cfg.LogPath != "/x/log" ||
		cfg.MarkerPath != "/x/session" || cfg.AllowUID != 4321 || gotPin != pin {
		t.Fatalf("serve passed socket %q, log %q, marker %q, uid %d, authd pin %q",
			cfg.Socket, cfg.LogPath, cfg.MarkerPath, cfg.AllowUID, gotPin)
	}
	// The re-checks get an authd oracle of their own, pinned as the sign-ins' is.
	if cfg.RecheckOracle == nil || oracles != 2 {
		t.Fatalf("serve built %d oracles and passed RecheckOracle %v, want two: the sign-ins' and the re-checks'",
			oracles, cfg.RecheckOracle)
	}
	if cfg.VIN != goodVIN || cfg.Key == nil || !cfg.Key.Equal(k) || cfg.Bus == nil || cfg.Oracle == nil {
		t.Fatalf("serve passed VIN ok %v, the key ok %v, bus %v, oracle %v",
			cfg.VIN == goodVIN, cfg.Key != nil && cfg.Key.Equal(k), cfg.Bus, cfg.Oracle)
	}
	if !cfg.Liveness || cfg.Logf == nil || cfg.WrapCar != nil || cfg.OnListening != nil {
		t.Fatalf("serve passed Liveness %v, Logf set %v, WrapCar set %v, OnListening set %v",
			cfg.Liveness, cfg.Logf != nil, cfg.WrapCar != nil, cfg.OnListening != nil)
	}

	if code, _, e := runCmd(t, "serve", "-key", key); code != 0 {
		t.Fatalf("serve: exit %d: %s", code, e)
	}
	s.mu.Lock()
	cfg, gotPin = s.cfg, s.pin
	s.mu.Unlock()
	if cfg.Socket != "/run/airgapp-summon/relay.sock" ||
		cfg.LogPath != "/var/lib/airgapp-summon/state/log" || cfg.MarkerPath != "/run/airgapp-summon/session" ||
		cfg.AllowUID != 4950 || gotPin != authority.DefaultAuthdSHA256 {
		t.Fatalf("serve's defaults: socket %q, log %q, marker %q, uid %d, authd pin %q",
			cfg.Socket, cfg.LogPath, cfg.MarkerPath, cfg.AllowUID, gotPin)
	}
	// The allowlist flag is gone: a run script that still passes it is refused, not silently served.
	if code, _, _ := runCmd(t, "serve", "-key", key, "-allow", "/x/allow"); code != 2 {
		t.Fatalf("serve -allow: exit %d, want the usage error 2", code)
	}
}

// L5, R2: serve's live file is the one camhelper reads, proto.SummonLivePath, through the liveFile hook. It never
// follows -marker.
func TestServeLivePathDefault(t *testing.T) {
	if liveFile != proto.SummonLivePath {
		t.Fatalf("the liveFile hook is %q, want proto.SummonLivePath %q", liveFile, proto.SummonLivePath)
	}
	s := stubServe(t)
	key := keygen(t)
	t.Setenv(vinEnv, goodVIN)
	t.Setenv(supervise.LivenessEnv, "3")
	if code, _, e := runCmd(t, "serve", "-key", key, "-marker", "/x/session"); code != 0 {
		t.Fatalf("serve: exit %d: %s", code, e)
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if cfg.LivePath != proto.SummonLivePath {
		t.Fatalf("serve -marker /x/session passed the live path %q, want %q", cfg.LivePath, proto.SummonLivePath)
	}
}

// R6: while serve runs, the standard library's logger writes to the logq queue, and both it and Config.Logf reach
// stderr. Afterwards the standard logger is back where it was.
func TestStdLogThroughQueue(t *testing.T) {
	s := stubServe(t)
	key := keygen(t)
	t.Setenv(vinEnv, goodVIN)
	t.Setenv(supervise.LivenessEnv, "3")
	before := log.Writer()
	code, _, e := runCmd(t, "serve", "-key", key)
	if code != 0 {
		t.Fatalf("serve: exit %d: %s", code, e)
	}
	s.mu.Lock()
	w := s.stdLog
	s.mu.Unlock()
	if _, ok := w.(*logq.Logger); !ok {
		t.Fatalf("the standard logger wrote to %T while serve ran, want the *logq.Logger", w)
	}
	for _, line := range []string{"agw: a line through the standard logger", "summonhelper: a line through Logf"} {
		if !strings.Contains(e, line) {
			t.Fatalf("stderr lacks %q:\n%s", line, e)
		}
	}
	if log.Writer() != before {
		t.Fatal("the standard logger was not restored after run")
	}
}

// Fix round 1, finding 3: serve disables dumpability first, before it loads the key and before the bus (Serve does it
// again). A failure there stops serve before the key is read.
func TestServeDisablesDumpsFirst(t *testing.T) {
	if reflect.ValueOf(disableDumps).Pointer() != reflect.ValueOf(helper.DisableDumps).Pointer() {
		t.Fatal("serve's dumpability hook is not helper.DisableDumps")
	}
	if reflect.ValueOf(loadKey).Pointer() != reflect.ValueOf(keys.Load).Pointer() {
		t.Fatal("serve's key hook is not keys.Load")
	}
	s := stubServe(t)
	key := keygen(t)
	t.Setenv(vinEnv, goodVIN)
	t.Setenv(supervise.LivenessEnv, "3")
	if code, _, e := runCmd(t, "serve", "-key", key); code != 0 {
		t.Fatalf("serve: exit %d: %s", code, e)
	}
	if got, want := s.order(), []string{"dumps", "key", "oracle", "oracle", "bus", "serve"}; !slices.Equal(got, want) {
		t.Fatalf("serve reached %q, want %q", got, want)
	}

	prev := disableDumps
	disableDumps = func() error {
		s.event("dumps failed")
		return errors.New("prctl: operation not permitted")
	}
	defer func() { disableDumps = prev }()
	code, _, e := runCmd(t, "serve", "-key", key)
	if code != 1 || !strings.Contains(e, "dumpability") {
		t.Fatalf("serve with dumps that cannot be disabled: exit %d, %q; want exit 1 and the dumpability error", code, e)
	}
	if got := s.order()[6:]; !slices.Equal(got, []string{"dumps failed"}) {
		t.Fatalf("after the dumpability failure serve reached %q, want nothing more", got)
	}
}

// Fix round 1, finding 2 (Global Constraints: logs never contain the VIN): no refusal, usage error or log line carries
// the VIN. The usage cases set no VIN source, so only the rule that nothing typed is echoed protects them. The rest
// give the VIN through SUMMONHELPER_VIN, -vin or -vin-file, then let it reach an error through a path or a value it
// was typed into, or through Serve's own logs: the log's writer redacts every VIN the command was given.
func TestNoVINInLogs(t *testing.T) {
	const (
		vin    = "1HGCM82633A004352" // the sentinel
		badVIN = "1HGCM82633A00435I" // a sentinel with an I: refused, and not echoed either
	)
	s := stubServe(t)
	stubOracle, stubSrv := newOracle, serveHelper
	key := keygen(t)
	dir := t.TempDir()
	vinFile := filepath.Join(dir, "vin")
	if err := os.WriteFile(vinFile, []byte(vin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, vin)                      // a path that holds the VIN and does not exist
	echoPin := func(pin string) (authority.Oracle, error) { // NewDBus on Linux quotes a malformed pin
		return nil, fmt.Errorf("authority: the authd pin must be 64 hex digits (it cannot be disabled), got %q", pin)
	}
	leakySrv := func(_ context.Context, cfg helper.Config) error { // a core that logs the VIN everywhere it can
		cfg.Logf("summonhelper: vin %s", cfg.VIN)
		log.Printf("agw: vin %s", cfg.VIN)
		return fmt.Errorf("listening on %s: %s", cfg.Socket, cfg.VIN)
	}
	cases := []struct {
		name     string
		env      string // SUMMONHELPER_VIN; "" unsets it
		liveness bool
		args     []string
		code     int
		oracle   func(string) (authority.Oracle, error)
		srv      func(context.Context, helper.Config) error
	}{
		// Usage: the VIN typed where it does not belong, with no VIN source set.
		{name: "the VIN as the command", args: []string{vin}, code: 2},
		{name: "serve: a positional VIN", args: []string{"serve", "-key", key, vin}, code: 2},
		{name: "serve: the VIN as a flag", args: []string{"serve", "-" + vin}, code: 2},
		{name: "serve: the VIN as -allow-uid", args: []string{"serve", "-allow-uid", vin}, code: 2},
		{name: "serve: -key without its value", args: []string{"serve", "-key"}, code: 2},
		{name: "serve -h", args: []string{"serve", "-h"}, code: 2},
		{name: "pin: a positional VIN", args: []string{"pin", "-key", key, vin}, code: 2},
		{name: "pin: the VIN as -vin-file", args: []string{"pin", "-key", key, "-vin-file", missing}, code: 1},
		{name: "pin: -vin with -vin-file", args: []string{"pin", "-key", key, "-vin", vin, "-vin-file", vinFile}, code: 2},
		{name: "keygen: a positional VIN", args: []string{"keygen", "-key", filepath.Join(dir, "k2"), vin}, code: 2},
		// Refusals.
		{name: "serve: no liveness", env: vin, args: []string{"serve", "-key", key}, code: 1},
		{name: "serve: a VIN with I", env: badVIN, liveness: true, args: []string{"serve", "-key", key}, code: 1},
		{name: "pin: a -vin with I", args: []string{"pin", "-key", key, "-vin", badVIN}, code: 1},
		// The known VIN, typed into a path or a value that an error quotes.
		{name: "serve: the VIN in the -key path", env: vin, liveness: true, args: []string{"serve", "-key", missing}, code: 1},
		{name: "serve: the VIN as -authd-sha256", env: vin, liveness: true,
			args: []string{"serve", "-key", key, "-authd-sha256", vin}, code: 1, oracle: echoPin},
		{name: "serve: Serve logs and returns the VIN", env: vin, liveness: true,
			args: []string{"serve", "-key", key, "-socket", missing}, code: 1, srv: leakySrv},
		{name: "pin: the VIN in the -key path (-vin)", args: []string{"pin", "-key", missing, "-vin", vin}, code: 1},
		{name: "pin: the VIN in the -key path (-vin-file)", args: []string{"pin", "-key", missing, "-vin-file", vinFile},
			code: 1},
		{name: "keygen: the VIN in the -key path", env: vin,
			args: []string{"keygen", "-key", filepath.Join(dir, "nodir", vin)}, code: 1},
		// Success: the pin itself carries the VIN on stdout, by design; stderr does not.
		{name: "pin", args: []string{"pin", "-key", key, "-vin", vin}, code: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.env != "" {
				t.Setenv(vinEnv, c.env)
			} else {
				unsetenv(t, vinEnv)
			}
			if c.liveness {
				t.Setenv(supervise.LivenessEnv, "3")
			} else {
				unsetenv(t, supervise.LivenessEnv)
			}
			newOracle, serveHelper = stubOracle, stubSrv
			if c.oracle != nil {
				newOracle = c.oracle
			}
			if c.srv != nil {
				serveHelper = c.srv
			}
			defer func() { newOracle, serveHelper = stubOracle, stubSrv }()
			code, out, e := runCmd(t, c.args...)
			if code != c.code {
				t.Fatalf("exit %d, want %d; stderr %q", code, c.code, e)
			}
			for _, v := range []string{vin, badVIN} {
				if strings.Contains(e, v) {
					t.Fatalf("stderr carries the VIN %s:\n%s", v, e)
				}
			}
			if c.code != 0 && out != "" {
				t.Fatalf("stdout %q on a failure", out)
			}
		})
	}
	_ = s
}

// runSetup records what run started: the bus, and helper.Supervise's argv, marker path and live path. self is the
// executable.
type runSetup struct {
	mu           sync.Mutex
	buses, sups  int
	argv         []string
	marker, live string
	err          error // what the stand-in Supervise returns
}

func (s *runSetup) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buses + s.sups
}

const self = "/x/summonhelper"

func stubRun(t *testing.T) *runSetup {
	t.Helper()
	s := &runSetup{}
	prevBus, prevSup, prevExe := newBus, superviseHelper, executable
	newBus = func() (carlink.Bus, error) {
		s.mu.Lock()
		s.buses++
		s.mu.Unlock()
		return carlink.NewFakeBus(), nil
	}
	superviseHelper = func(_ context.Context, bus carlink.Bus, argv []string, markerPath, livePath string,
		logf func(string, ...any)) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sups++
		s.argv, s.marker, s.live = slices.Clone(argv), markerPath, livePath
		return s.err
	}
	executable = func() (string, error) { return self, nil }
	t.Cleanup(func() { newBus, superviseHelper, executable = prevBus, prevSup, prevExe })
	return s
}

// run reads the VIN from SUMMONHELPER_VIN, as serve does, and refuses before it starts anything.
func TestRunRequiresVIN(t *testing.T) {
	s := stubRun(t)
	for _, c := range []struct {
		name, vin string
		unset     bool
	}{
		{"unset", "", true},
		{"empty", "", false},
		{"16 characters", "5YJ3E1EA7KF00000", false},
		{"contains I", "5YJ3E1EA7KF00000I", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(vinEnv, c.vin)
			if c.unset {
				os.Unsetenv(vinEnv)
			}
			code, out, e := runCmd(t, "run")
			if code != 1 {
				t.Fatalf("run with VIN %q: exit %d, want 1", c.vin, code)
			}
			if !strings.Contains(e, "SUMMONHELPER_VIN is not a valid VIN") {
				t.Fatalf("run's refusal is %q, want the VIN refusal", e)
			}
			if c.vin != "" && strings.Contains(e+out, c.vin) {
				t.Fatalf("the log contains the refused value: %q", e)
			}
			if n := s.count(); n != 0 {
				t.Fatalf("run started %d of the bus and the supervisor before refusing", n)
			}
		})
	}
}

// R14: run accepts serve's flags and forwards them to its child unchanged, as `<self> serve <flags>`; it supervises
// the marker -marker names. Its takeover removes the live file camhelper reads, whatever -marker says (L5, R2).
func TestRunForwardsServeFlags(t *testing.T) {
	s := stubRun(t)
	t.Setenv(vinEnv, goodVIN)
	pin := strings.Repeat("ab", 32)
	args := []string{"-key", "/x/key.pem", "-socket", "/x/relay.sock", "-log", "/x/log",
		"-marker", "/x/session", "-authd-sha256=" + pin, "-allow-uid", "4321"}
	if code, _, e := runCmd(t, append([]string{"run"}, args...)...); code != 0 {
		t.Fatalf("run: exit %d: %s", code, e)
	}
	s.mu.Lock()
	argv, mk, live, buses := s.argv, s.marker, s.live, s.buses
	s.mu.Unlock()
	if want := append([]string{self, "serve"}, args...); !slices.Equal(argv, want) {
		t.Fatalf("run's child argv is %q, want %q", argv, want)
	}
	if mk != "/x/session" || buses != 1 {
		t.Fatalf("run supervised marker %q on %d buses, want /x/session on one", mk, buses)
	}
	if live != proto.SummonLivePath {
		t.Fatalf("run -marker /x/session supervised the live file %q, want %q", live, proto.SummonLivePath)
	}

	if code, _, e := runCmd(t, "run"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, e)
	}
	s.mu.Lock()
	argv, mk, live = s.argv, s.marker, s.live
	s.mu.Unlock()
	if !slices.Equal(argv, []string{self, "serve"}) || mk != "/run/airgapp-summon/session" ||
		live != proto.SummonLivePath {
		t.Fatalf("run's defaults: argv %q, marker %q, live file %q", argv, mk, live)
	}

	// A flag serve would refuse, or an argument, is refused before anything starts; Supervise's error is exit 1.
	before := s.count()
	for _, bad := range [][]string{{"run", "-nonsense"}, {"run", "positional"}, {"run", "-allow-uid", "x"}} {
		if code, _, _ := runCmd(t, bad...); code != 2 {
			t.Fatalf("%q: exit %d, want 2", bad, code)
		}
	}
	if s.count() != before {
		t.Fatal("run started something after a usage error")
	}
	s.mu.Lock()
	s.err = errors.New("supervise: child exited with status 1")
	s.mu.Unlock()
	if code, _, e := runCmd(t, "run"); code != 1 || !strings.Contains(e, "child exited") {
		t.Fatalf("run after Supervise failed: exit %d, %q; want exit 1 and the error", code, e)
	}
}

// finish: it removes a stale live file first, whatever the marker says. No marker is exit 0 without the bus; with a
// marker it runs helper.Finish, exit 0 once the abort and the disconnect are confirmed and the marker removed, and 1
// when the name cannot be had, keeping the marker.
func TestFinishCommand(t *testing.T) {
	var bus *carlink.FakeBus
	var buses int
	var busErr error    // what newBus fails with, if set
	var liveAtBus error // Lstat of the live file when finish opened the bus
	prev, prevLive := newBus, liveFile
	newBus = func() (carlink.Bus, error) {
		buses++
		_, liveAtBus = os.Lstat(liveFile)
		if busErr != nil {
			return nil, busErr
		}
		return bus, nil
	}
	// For the whole test, the runsv subtest's default marker included: finish never removes the host's live file.
	liveFile = filepath.Join(t.TempDir(), "live")
	t.Cleanup(func() { newBus, liveFile = prev, prevLive })
	mk := filepath.Join(t.TempDir(), "session")
	// leaveLive leaves what a helper that died leaves: its pid, and no lock, which went with it.
	leaveLive := func(t *testing.T) {
		t.Helper()
		if err := os.WriteFile(liveFile, []byte("123\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const removed = "summonhelper: finish removed the live file"
	liveRemoved := func(t *testing.T, stderr string) {
		t.Helper()
		if _, err := os.Lstat(liveFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the live file survived finish (Lstat: %v)", err)
		}
		if n := strings.Count(stderr, removed); n != 1 {
			t.Fatalf("finish logged %q %d times, want once:\n%s", removed, n, stderr)
		}
	}

	t.Run("no marker", func(t *testing.T) {
		bus, buses = carlink.NewFakeBus(), 0
		leaveLive(t)
		code, _, e := runCmd(t, "finish", "-marker", mk)
		if code != 0 || buses != 0 {
			t.Fatalf("finish without a marker: exit %d after %d bus connections; want 0 without the bus", code, buses)
		}
		if !strings.Contains(e, "no session marker") {
			t.Fatalf("finish logged %q, want that it found no marker", e)
		}
		liveRemoved(t, e)
	})
	t.Run("a marker", func(t *testing.T) {
		bus, buses = carlink.NewFakeBus(), 0
		leaveLive(t)
		if err := os.WriteFile(mk, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, e := runCmd(t, "finish", "-marker", mk)
		if code != 0 {
			t.Fatalf("finish: exit %d: %s", code, e)
		}
		em := bus.Emissions()
		if len(em) != 2 || em[0].Body["data"] != `{"msg_type":"autopark:cmd_abort"}` ||
			em[1].Body["msg_type"] != "autopark:set_webrtc" {
			t.Fatalf("finish emitted %v, want the abort, then the disconnect", em)
		}
		if _, err := os.Lstat(mk); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the marker survived finish (Lstat: %v)", err)
		}
		// With a marker, finish leaves the live file to helper.Finish, which removes it first and logs that only after
		// the abort, the disconnect and the name's release: nothing is logged before an abort still to be sent.
		if liveAtBus != nil {
			t.Fatalf("the live file was gone when finish opened the bus (Lstat: %v): finish removed it, and logged "+
				"that, before helper.Finish sent the abort", liveAtBus)
		}
		liveRemoved(t, e)
	})
	// The bus cannot be reached, so helper.Finish never runs and nothing is sent: finish removes the live file itself,
	// and the marker stays for the next try.
	t.Run("a marker, no bus", func(t *testing.T) {
		bus, buses, busErr = carlink.NewFakeBus(), 0, errors.New("test: no system bus")
		t.Cleanup(func() {
			busErr = nil
			os.Remove(mk)
		})
		leaveLive(t)
		if err := os.WriteFile(mk, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, e := runCmd(t, "finish", "-marker", mk)
		if code != 1 || buses != 1 || !strings.Contains(e, "test: no system bus") {
			t.Fatalf("finish with no bus: exit %d after %d bus connections: %s; want 1, the bus's error", code, buses, e)
		}
		if em := bus.Emissions(); len(em) != 0 {
			t.Fatalf("finish emitted %v with no bus", em)
		}
		if _, err := os.Lstat(mk); err != nil {
			t.Fatalf("the marker is gone after a failed finish: %v", err)
		}
		liveRemoved(t, e)
	})
	t.Run("the name is held", func(t *testing.T) {
		bus, buses = carlink.NewFakeBus(), 0
		bus.TakeName(carlink.WebrtcName)
		if err := os.WriteFile(mk, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, e := runCmd(t, "finish", "-marker", mk); code != 1 {
			t.Fatalf("finish with the name held: exit %d, want 1: %s", code, e)
		}
		if em := bus.Emissions(); len(em) != 0 {
			t.Fatalf("finish emitted %v without the name", em)
		}
		if _, err := os.Lstat(mk); err != nil {
			t.Fatalf("the marker is gone after a failed finish: %v", err)
		}
	})
	// Fix round 1, finding 3: runsv runs ./finish with two arguments, run's exit code (-1 after a signal) and the
	// signal; a finish script that passes them on must still get the abort and the disconnect sent.
	t.Run("runsv's arguments", func(t *testing.T) {
		for _, a := range [][]string{{"0", "15"}, {"-1", "9"}, {"111"}} {
			bus, buses = carlink.NewFakeBus(), 0
			if err := os.WriteFile(mk, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if code, _, e := runCmd(t, append([]string{"finish", "-marker", mk}, a...)...); code != 0 {
				t.Fatalf("finish -marker %s %q: exit %d, want 0: %s", mk, a, code, e)
			}
			if em := bus.Emissions(); len(em) != 2 || em[0].Body["data"] != `{"msg_type":"autopark:cmd_abort"}` {
				t.Fatalf("finish %q emitted %v, want the abort, then the disconnect", a, em)
			}
			if _, err := os.Lstat(mk); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the marker survived finish %q (Lstat: %v)", a, err)
			}
		}
		// Alone, as a script's `finish "$@"` passes them: never a usage error.
		if marker.Present(defaultMarker) {
			t.Skip("this machine has a session marker at the default path")
		}
		for _, a := range [][]string{{"0", "15"}, {"-1", "9"}} {
			if code, _, e := runCmd(t, append([]string{"finish"}, a...)...); code != 0 || !strings.Contains(e, "no session marker") {
				t.Fatalf("finish %q: exit %d, %q; want 0 and no marker found", a, code, e)
			}
		}
	})
	t.Run("usage", func(t *testing.T) {
		for _, a := range [][]string{{"positional"}, {"1", "2", "3"}, {"-nonsense"}} {
			if code, _, _ := runCmd(t, append([]string{"finish"}, a...)...); code != 2 {
				t.Fatalf("finish %q: exit %d, want 2", a, code)
			}
		}
	})
}

func TestUsage(t *testing.T) {
	if code, _, _ := runCmd(t); code != 2 {
		t.Fatalf("no command: exit %d, want 2", code)
	}
	if code, _, _ := runCmd(t, "nonsense"); code != 2 {
		t.Fatalf("an unknown command: exit %d, want 2", code)
	}
	if code, _, _ := runCmd(t, "pin", "-nonsense"); code != 2 {
		t.Fatalf("an unknown flag: exit %d, want 2", code)
	}
}
