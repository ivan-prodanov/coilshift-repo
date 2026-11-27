// Command summonhelper is the car-side Summon helper (design §6.1). As uid 1337 (user webrtc) it owns
// com.tesla.Webrtc and serves the inner SUMMON session, through the gateway server core, on the relay socket agw dials.
//
//	summonhelper run    [serve's flags]                    the supervisor: runs serve as its child (runit's run)
//	summonhelper finish [-marker path] [code signal]       remove a stale live file, and abort and disconnect a
//	                                                       session left open (runit's finish)
//	summonhelper serve  [flags]                            the helper; only as the supervisor's child
//	summonhelper keygen [-key path]                        create the helper's key (mode 0400, never overwritten)
//	summonhelper pin    [-key path] [-vin VIN | -vin-file path]   print the pin the phone adds, and nothing else
//	summonhelper probe  [-duration 120s] [-keyinfo -pub hex[,hex...] [-authd-sha256 hex]]
//	                                                       the no-motion car check: it originates no verb, abort or
//	                                                       device_location, and starts heartbeats only after the
//	                                                       car's connect-edge push, so it never feeds a session
//	                                                       another client left open
//
// run and serve read the VIN from SUMMONHELPER_VIN, which the root run script reads from /var/etc/vin; run passes its
// environment and its flags on to serve. serve refuses to run unless AGW_LIVENESS_FD is set: the supervisor kills a
// child that stops writing its liveness pipe, then takes over com.tesla.Webrtc and aborts. Every log line,
// the standard library's included, goes through one bounded queue (internal/summon/logq) to stderr, so logging never
// blocks. Logs never carry the VIN (Global Constraints): no message echoes what the operator typed, and the queue's
// writer redacts every VIN the command was given.
package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/summon/helper"
	"airgapp/gateway/internal/summon/logq"
	"airgapp/gateway/internal/summon/marker"
	"airgapp/gateway/internal/summon/probe"
	"airgapp/gateway/internal/summon/relay"
	"airgapp/gateway/internal/supervise"
)

const (
	stateDir       = "/var/lib/airgapp-summon/state"
	defaultKey     = stateDir + "/key.pem"
	defaultLog     = stateDir + "/log"
	defaultSocket  = relay.DefaultSocket
	defaultMarker  = "/run/airgapp-summon/session"
	defaultVINFile = "/var/etc/vin"
	defaultAllowed = helper.AgwUID // agw's uid: the only relay peer served

	vinEnv    = "SUMMONHELPER_VIN"
	flushWait = time.Second // at exit, how long the log queue may take to reach stderr
	redacted  = "<VIN>"
)

// Test hooks.
var (
	newBus = func() (carlink.Bus, error) {
		b, err := carlink.SystemBus()
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	newOracle    = authority.NewDBus
	runProbe     = probe.Run
	probeMarker  = defaultMarker // the probe's session marker: the helper's, which finish and the supervisor check
	serveHelper  = helper.Serve
	disableDumps = helper.DisableDumps
	loadKey      = keys.Load

	// liveFile is the live file camhelper reads (R2), whatever -marker says (L5): serve's helper.Config.LivePath, and
	// the file run's takeover and finish remove.
	liveFile = proto.SummonLivePath

	superviseHelper = helper.Supervise
	executable      = os.Executable
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run runs one command. Its log lines, and the standard library's while it runs (R6: the server core logs
// through it), go through one logq.Logger on stderr, which run flushes, for at most flushWait, before it returns.
// The queue writes through a redactor that already knows SUMMONHELPER_VIN.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	red := &redactor{w: stderr}
	red.add(os.Getenv(vinEnv))
	lq := logq.New(red, logq.Lines)
	prev := log.Writer()
	log.SetOutput(lq)
	defer func() {
		log.SetOutput(prev)
		lq.Flush(flushWait)
	}()
	if len(args) == 0 {
		lq.Logf("usage: summonhelper run|finish|serve|keygen|pin|probe [flags]")
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = cmdRun(ctx, args[1:], lq)
	case "finish":
		err = cmdFinish(args[1:], lq)
	case "serve":
		err = cmdServe(ctx, args[1:], lq)
	case "keygen":
		err = cmdKeygen(args[1:], stdout, lq)
	case "pin":
		err = cmdPin(args[1:], stdout, lq, red)
	case "probe":
		err = cmdProbe(ctx, args[1:], stdout, lq)
	default:
		// The command is not echoed: it may be the VIN, typed in the wrong place.
		lq.Logf("summonhelper: unknown command; usage: summonhelper run|finish|serve|keygen|pin|probe [flags]")
		return 2
	}
	if err != nil {
		lq.Logf("summonhelper %s: %v", args[0], err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

type usageError struct{ error }

// redactor is the log queue's writer: it writes each line to w with every VIN the command was given replaced by
// "<VIN>". It is the safety net under the rule that no message echoes what the operator typed: the VIN can still
// reach a message through a path or a value it was typed into (an error quotes those), or through a log line of the
// core's. It runs on the queue's writer goroutine.
type redactor struct {
	w io.Writer

	mu   sync.Mutex
	vins [][]byte
}

// add makes r redact v from every line it writes from now on, lines still queued included, when v is a valid VIN. Any
// other value is refused, without being echoed, before a message could quote it.
func (r *redactor) add(v string) {
	if !helper.ValidVIN(v) {
		return
	}
	r.mu.Lock()
	r.vins = append(r.vins, []byte(v))
	r.mu.Unlock()
}

func (r *redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	vins := r.vins
	r.mu.Unlock()
	out := p
	for _, v := range vins {
		if bytes.Contains(out, v) {
			out = bytes.ReplaceAll(out, v, []byte(redacted))
		}
	}
	if _, err := r.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// newFlags returns a FlagSet that prints nothing itself: the flag package's own messages quote what was typed.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse parses args with fs and echoes nothing that was typed, since any of it may be the VIN: a bad flag is reported
// in fixed words, -h lists the flags (their names and defaults are the command's own), and arguments are counted,
// never quoted.
func parse(fs *flag.FlagSet, args []string, lq *logq.Logger) error {
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		lq.Logf("usage of summonhelper %s:", fs.Name())
		fs.SetOutput(lq)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
		return usageError{errors.New("help requested")}
	case err != nil:
		return usageError{errors.New("bad flags: an unknown flag, a flag without its value, or a value that does not " +
			"parse (not echoed: it may be the VIN); -h lists the flags")}
	case fs.NArg() != 0:
		return usageError{fmt.Errorf("%s takes no arguments, and got %d (not echoed: one may be the VIN)", fs.Name(),
			fs.NArg())}
	}
	return nil
}

// serveFlags are serve's flags. run accepts the same ones and passes them to its child unchanged (R14).
type serveFlags struct {
	socket, key, log, marker, authdPin *string
	allowUID                           *int
}

func addServeFlags(fs *flag.FlagSet) serveFlags {
	return serveFlags{
		socket:   fs.String("socket", defaultSocket, "the relay socket agw dials (mode 0660, in the setgid run directory)"),
		key:      fs.String("key", defaultKey, "the helper's long-term key (mode 0400, owned by this uid)"),
		log:      fs.String("log", defaultLog, "the session log"),
		marker:   fs.String("marker", defaultMarker, "the session marker the takeover and the finish hook check"),
		authdPin: fs.String("authd-sha256", authority.DefaultAuthdSHA256, "SHA-256 of the /usr/bin/authd build whose KeyInfo was verified"),
		allowUID: fs.Int("allow-uid", defaultAllowed, "the only relay peer uid served (agw's)"),
	}
}

// ---- run / finish ----

// cmdRun is the supervisor (design §6.1; helper.Supervise). It takes serve's flags and forwards them unchanged to its
// child, `<self> serve <flags>` (R14), which inherits the environment, SUMMONHELPER_VIN included. It checks the flags
// and the VIN before it starts anything. Its Finish and its takeover remove liveFile, the child's live file.
func cmdRun(ctx context.Context, args []string, lq *logq.Logger) error {
	fs := newFlags("run")
	f := addServeFlags(fs)
	if err := parse(fs, args, lq); err != nil {
		return err
	}
	if vin := os.Getenv(vinEnv); !helper.ValidVIN(vin) {
		return fmt.Errorf("refusing to run: %s is not a valid VIN (%d characters; want 17 of A-H, J-N, P, R-Z "+
			"and 0-9; the value is not logged)", vinEnv, len(vin))
	}
	self, err := executable()
	if err != nil {
		return fmt.Errorf("finding this executable: %w", err)
	}
	// The bus is never closed here: the process exits when Supervise returns, and a Close can deadlock behind a stuck
	// write (plan call 3).
	bus, err := newBus()
	if err != nil {
		return fmt.Errorf("the system bus: %w", err)
	}
	return superviseHelper(ctx, bus, append([]string{self, "serve"}, args...), *f.marker, liveFile, lq.Logf)
}

// cmdFinish is runit's finish hook (helper.Finish). It removes a stale live file (liveFile) whatever the marker says,
// and logs nothing before an abort it has still to send. Without a session marker it removes the file, logs that, and
// does nothing more: it never touches the bus. With one, helper.Finish removes the file first and logs that only after
// cmd_abort, the confirmed disconnect and the name's release; if the bus cannot be reached, Finish never runs, so
// cmdFinish removes and logs the file itself, since it sends nothing. It fails, keeping the marker, unless cmd_abort
// and the confirmed disconnect went out and the marker is gone; the live file's removal never fails it.
//
// runsv runs ./finish with two arguments: run's exit code (-1 after a signal) and the signal. A finish script that
// passes them on must never stop the hook, so up to two trailing integers are ignored, "-1" included, which the flag
// package would take for a flag.
func cmdFinish(args []string, lq *logq.Logger) error {
	fs := newFlags("finish")
	markerPath := fs.String("marker", defaultMarker, "the session marker serve sets while a session may be open")
	if err := parse(fs, withoutRunsvArgs(args), lq); err != nil {
		return err
	}
	removeLive := func() { // only where nothing is sent
		removed, rmErr := helper.RemoveLive(liveFile)
		helper.LogLiveRemoval(lq.Logf, "finish", removed, rmErr)
	}
	if !marker.Present(*markerPath) {
		removeLive()
		lq.Logf("summonhelper finish: no session marker: nothing to send")
		return nil
	}
	bus, err := newBus()
	if err != nil {
		removeLive()
		return fmt.Errorf("the system bus: %w", err)
	}
	return helper.Finish(bus, *markerPath, liveFile, lq.Logf)
}

// withoutRunsvArgs is args without up to two trailing integers: what runsv passes ./finish.
func withoutRunsvArgs(args []string) []string {
	n := len(args)
	for i := 0; i < 2 && n > 0; i++ {
		if _, err := strconv.Atoi(args[n-1]); err != nil {
			break
		}
		n--
	}
	return args[:n]
}

// ---- serve ----

// cmdServe disables dumpability first, since the process is about to hold its key (Serve does it again). Then it
// checks the VIN, then the liveness pipe, before it touches the key, the authority or the bus (R21).
func cmdServe(ctx context.Context, args []string, lq *logq.Logger) error {
	if err := disableDumps(); err != nil {
		return fmt.Errorf("disabling dumpability: %w", err)
	}
	fs := newFlags("serve")
	f := addServeFlags(fs)
	if err := parse(fs, args, lq); err != nil {
		return err
	}
	vin := os.Getenv(vinEnv) // run gave it to the log's redactor
	if !helper.ValidVIN(vin) {
		return fmt.Errorf("refusing to serve: %s is not a valid VIN (%d characters; want 17 of A-H, J-N, P, R-Z "+
			"and 0-9; the value is not logged)", vinEnv, len(vin))
	}
	if os.Getenv(supervise.LivenessEnv) == "" {
		return fmt.Errorf("refusing to serve: %s is not set: serve runs only as the child of the supervisor, "+
			"which kills it when its liveness pipe falls silent", supervise.LivenessEnv)
	}
	key, err := loadKey(*f.key)
	if err != nil {
		return err
	}
	oracle, err := newOracle(*f.authdPin)
	if err != nil {
		return err
	}
	// The re-checks of an open session get an authd connection of their own (the oracle is lazy: it connects at the
	// first re-check), as agw's get a sign-in helper connection of their own.
	recheck, err := newOracle(*f.authdPin)
	if err != nil {
		return err
	}
	// The bus is never closed here: the process exits when serve returns, and a Close can deadlock behind a stuck
	// write (plan call 3).
	bus, err := newBus()
	if err != nil {
		return fmt.Errorf("the system bus: %w", err)
	}
	return serveHelper(ctx, helper.Config{
		Bus: bus, Oracle: oracle, RecheckOracle: recheck, Key: key, VIN: vin,
		Socket: *f.socket, AllowUID: *f.allowUID,
		MarkerPath: *f.marker, LivePath: liveFile, LogPath: *f.log,
		Logf: lq.Logf, Liveness: true,
	})
}

// ---- keygen / pin ----

func cmdKeygen(args []string, stdout io.Writer, lq *logq.Logger) error {
	fs := newFlags("keygen")
	keyPath := fs.String("key", defaultKey, "where to create the key (mode 0400, never overwritten)")
	if err := parse(fs, args, lq); err != nil {
		return err
	}
	if err := keys.Generate(*keyPath); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "key written (mode 0400)")
	return nil
}

// cmdPin prints keys.PinString for the helper's key and the VIN, and nothing else. It reads the VIN before the key,
// so the log's redactor knows the VIN before an error can quote a path it was typed into.
func cmdPin(args []string, stdout io.Writer, lq *logq.Logger, red *redactor) error {
	fs := newFlags("pin")
	keyPath := fs.String("key", defaultKey, "the helper's long-term key")
	vinLit := fs.String("vin", "", "the VIN (instead of -vin-file)")
	vinFile := fs.String("vin-file", defaultVINFile, "the file holding the VIN")
	if err := parse(fs, args, lq); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if set["vin"] && set["vin-file"] {
		return usageError{errors.New("-vin and -vin-file exclude each other")}
	}
	vin := *vinLit
	if !set["vin"] {
		b, err := os.ReadFile(*vinFile)
		if err != nil {
			return fmt.Errorf("reading the VIN file: %w", pathless(err))
		}
		vin = strings.TrimSpace(string(b))
	}
	red.add(vin)
	if !helper.ValidVIN(vin) {
		return fmt.Errorf("the VIN is not a valid VIN (%d characters; want 17 of A-H, J-N, P, R-Z and 0-9; the value "+
			"is not logged)", len(vin))
	}
	key, err := loadKey(*keyPath)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, keys.PinString(&key.PublicKey, vin))
	return nil
}

// ---- probe ----

// keyInfoWait bounds one key's KeyRole for -keyinfo.
const keyInfoWait = 2 * time.Second

// callerBus is the DBusBus's caller lookup and KeysUpdated watch, which the probe reports.
type callerBus interface {
	CallerInfo(ctx context.Context, sender string) (uid, pid uint32, err error)
	SubscribeKeysUpdated(f func()) error
}

var _ callerBus = (*carlink.DBusBus)(nil)

// cmdProbe is the no-motion car check (internal/summon/probe), run by the owner as uid 1337 with summonhelper and
// stock webrtc-comms stopped. It originates no verb, abort or device_location, and starts heartbeats only after the
// car's connect-edge push (autopark:style), so it never feeds a session another client left open: without that push
// it sends only H1 and the disconnect, and exits 1. Its messages are H1, heartbeats, keepalives and the disconnect,
// and it prints its report on stdout once the session is torn down. SIGINT, SIGTERM, SIGHUP and SIGQUIT end it early,
// with the disconnect; an emit that overruns its deadline exits 3 (the Link's default), leaving the marker for
// `summonhelper finish`. Its session marker is the helper's own (probeMarker), never a typed path, so finish and the
// supervisor always see it. It never holds the helper's key, so it leaves dumpability alone.
func cmdProbe(ctx context.Context, args []string, stdout io.Writer, lq *logq.Logger) error {
	// A dead reader of stdout or stderr (a pipe whose reader died with the SSH session) must not cut the probe off
	// between H1 and its disconnect. Go kills a process at its next write to such a fd 1 or 2 unless SIGPIPE is
	// ignored; ignored, the write fails with EPIPE instead: logq drops the line and the report's write fails
	// harmlessly. Nothing in this process has written to fd 1 or 2 before this point, and only the probe command
	// ignores it: the other commands keep Go's default.
	signal.Ignore(syscall.SIGPIPE)
	fs := newFlags("probe")
	duration := fs.Duration("duration", probe.DefaultDuration, "how long to hold the session after H1")
	keyinfo := fs.Bool("keyinfo", false, "also ask authd for the role of each -pub key (prints each entry's index "+
		"and role only)")
	pubs := fs.String("pub", "", "comma-separated hex public key(s) -keyinfo asks about (65-byte uncompressed P-256)")
	authdPin := fs.String("authd-sha256", authority.DefaultAuthdSHA256, "SHA-256 of the /usr/bin/authd build whose "+
		"KeyInfo was verified (-keyinfo)")
	if err := parse(fs, args, lq); err != nil {
		return err
	}
	if *duration <= 0 {
		return usageError{errors.New("-duration must be positive")}
	}
	// authority.NewDBus quotes a malformed pin, and nothing typed is echoed: refuse it here, in fixed words.
	if pin, err := hex.DecodeString(*authdPin); *keyinfo && (err != nil || len(pin) != 32) {
		return usageError{errors.New("-authd-sha256 must be 64 hex digits (not echoed: it may be the VIN)")}
	}
	var pks [][]byte
	if *keyinfo || *pubs != "" {
		if !*keyinfo {
			return usageError{errors.New("-pub is for -keyinfo")}
		}
		var err error
		if pks, err = parsePubs(*pubs); err != nil {
			return usageError{err}
		}
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()
	var roles func(context.Context) ([]string, error)
	if *keyinfo {
		if o, err := newOracle(*authdPin); err != nil {
			lq.Logf("summonhelper probe: -keyinfo: %v: the roles are not checked", err)
		} else {
			roles = keyRoles(o, pks)
		}
	}
	// The bus is never closed here: the process exits when the probe returns, and a Close can deadlock behind a stuck
	// write (plan call 3).
	bus, err := newBus()
	if err != nil {
		return fmt.Errorf("the system bus: %w", err)
	}
	// HBEvery, KeepaliveEvery and StyleWait stay zero: the probe's own defaults (100 ms, 15 s, 2 s) apply.
	cfg := probe.Config{
		Bus: bus, Duration: *duration, Roles: roles, MarkerPath: probeMarker, Out: stdout, Logf: lq.Logf,
	}
	if b, ok := bus.(callerBus); ok {
		cfg.CallerInfo, cfg.KeysUpdated = b.CallerInfo, b.SubscribeKeysUpdated
	}
	_, err = runProbe(ctx, cfg)
	return err
}

// parsePubs is -pub: comma-separated hex keys, each a 65-byte uncompressed P-256 point, in order, each once. A bad
// key is named by its position only, never echoed (it may be the VIN, typed in the wrong place).
func parsePubs(list string) ([][]byte, error) {
	var pks [][]byte
	seen := map[string]bool{}
	n := 0
	for _, h := range strings.Split(list, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		n++
		pk, err := hex.DecodeString(h)
		if err == nil && (len(pk) != 65 || pk[0] != 4) {
			err = errors.New("not 65 bytes")
		}
		if err == nil {
			_, err = ecdh.P256().NewPublicKey(pk)
		}
		if err != nil {
			return nil, fmt.Errorf("-pub key %d is not a 65-byte uncompressed P-256 key in hex (not echoed)", n)
		}
		if !seen[string(pk)] {
			seen[string(pk)] = true
			pks = append(pks, pk)
		}
	}
	if len(pks) == 0 {
		return nil, errors.New("-keyinfo needs -pub: the key(s) to ask authd about")
	}
	return pks, nil
}

// keyRoles is -keyinfo's Roles: each -pub key's KeyRole, asked of authd through o, as "entry <i>: role <r>", or the
// oracle's error in its place. The entries are numbered from 1 in -pub's order, each distinct key where it first
// appears. A line never carries a key, its fingerprint or the VIN; the oracle's errors quote none of them.
func keyRoles(o authority.Oracle, pks [][]byte) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		lines := make([]string, 0, len(pks))
		for i, pk := range pks {
			c, cancel := context.WithTimeout(ctx, keyInfoWait)
			role, err := o.KeyRole(c, pk)
			cancel()
			if err != nil {
				lines = append(lines, fmt.Sprintf("entry %d: role error: %v", i+1, err))
				continue
			}
			lines = append(lines, fmt.Sprintf("entry %d: role %d", i+1, role))
		}
		return lines, nil
	}
}

// pathless is err without the path an *os.PathError carries, which the operator typed and may be the VIN.
func pathless(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}
