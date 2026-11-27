// Command agw is the airgapp car gateway: serve, keygen, pin, probe-auth, probe-route, probe-summon.
//
// Access is the car's own: a phone signs in when it proves possession of its key (the handshake) and the car's authd
// lists that key as OWNER or DRIVER, the keys the owner manages in the Tesla app or with the key card. agw keeps no
// key list, and an open window loses its session when authd stops listing its key (internal/server/recheck.go).
package main

import (
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/capability"
	"airgapp/gateway/capability/camera"
	"airgapp/gateway/capability/dashcam"
	"airgapp/gateway/capability/echo"
	"airgapp/gateway/capability/summonrelay"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/dialout"
	"airgapp/gateway/internal/guards"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/server"
)

const (
	defaultPort = "17100"
	// defaultDialPort is the Pi forwarder's car-facing port.
	defaultDialPort = "17101"
	defaultDir      = "/var/lib/airgapp-gw"
)

// capTable is the registry of capabilities `serve` can host. To add one (for
// example CAMERA or DASHCAM), add one line here; see docs/CAPABILITY-API.md.
// benchOnly entries are test-only and are registered only with -bench.
var capTable = []struct {
	name      string
	benchOnly bool
	new       func() capability.Capability
}{
	{"ECHO", true, echo.New},
	{"CAMERA", false, camera.New},   // talks to the camera root helper (camhelper)
	{"DASHCAM", false, dashcam.New}, // read-only TeslaCam access through the helper
	// relays the phone's inner session to the summon helper's socket; agw never drives the car
	{"SUMMON", false, func() capability.Capability { return newSummonRelay() }},
}

// newSummonRelay builds SUMMON's relay (tests swap it). probeSummonUID is the uid probe-summon requires of the relay
// socket's server (tests set their own).
var (
	newSummonRelay = summonrelay.New
	probeSummonUID = summonrelay.HelperUID
)

// capNames lists the capabilities' names for the listen line, e.g. "CAMERA,DASHCAM,SUMMON".
func capNames(caps []capability.Capability) string {
	if len(caps) == 0 {
		return "none"
	}
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = c.Spec().Name
	}
	return strings.Join(names, ",")
}

// selectCaps picks the capabilities to host. Test-only (benchOnly) entries need
// -bench. Real capabilities are never served with a fake authority: with
// fake set, only benchOnly entries are registered.
func selectCaps(bench, fake bool) []capability.Capability {
	var caps []capability.Capability
	for _, e := range capTable {
		if e.benchOnly && !bench {
			continue
		}
		if !e.benchOnly && fake {
			continue
		}
		caps = append(caps, e.new())
	}
	return caps
}

// signIn is what agw needs of the camera root helper's sign-in socket: the
// authority Oracle (with KeyInfo rows for probe-auth) and its key-change
// watch. agw itself has no system bus access.
type signIn interface {
	authority.KeyEntryOracle
	Watch(ctx context.Context, h authority.WatchHandlers)
	Close() error
}

// Test hooks.
var (
	onListen  func(net.Addr)                           // called once the listener is up
	newSignIn = helperSignIn                           // the sign-in helper's client (lazy)
	busProbe  = authority.ProbeSystemBus               // the bus self-test's connect-and-close
	stderrLog = func(w io.Writer) { log.SetOutput(w) } // route server logs

	// busRefusal is how long serve waits, after a failed bus self-test,
	// before it exits: runit then retries about once a minute, not in a loop.
	busRefusal = 60 * time.Second
	// busProbeTimeout bounds each probe's handshake; busProbePause separates
	// the probes of one self-test.
	busProbeTimeout = 2 * time.Second
	busProbePause   = 500 * time.Millisecond
)

// busProbeTries is the most probes one bus self-test makes; it passes on
// busRefusalsNeeded refusals in a row.
const (
	busProbeTries     = 4
	busRefusalsNeeded = 2
)

// helperSignIn asks the helper at sock, which must run as root (uid 0).
func helperSignIn(sock string, lg *log.Logger) signIn {
	return authority.NewHelper(sock, authority.HelperConfig{HelperUID: 0, Log: lg})
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: agw serve|keygen|pin|probe-auth|probe-route|probe-summon [flags]")
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = cmdServe(ctx, args[1:], stderr)
	case "keygen":
		err = cmdKeygen(args[1:], stdout)
	case "pin":
		err = cmdPin(args[1:], stdout)
	case "probe-auth":
		err = cmdProbe(ctx, args[1:], stdout, stderr)
	case "probe-route":
		err = cmdProbeRoute(ctx, args[1:], stdout)
	case "probe-summon":
		err = cmdProbeSummon(ctx, args[1:], stdout)
	default:
		fmt.Fprintf(stderr, "agw: unknown command %q\n", args[0])
		return 2
	}
	if err != nil {
		var u usageError
		fmt.Fprintf(stderr, "agw %s: %v\n", args[0], err)
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

type usageError struct{ error }

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// ---- listen address rules ----

var (
	vehicleLAN = netip.MustParsePrefix("192.168.90.0/24")
	cgnat      = netip.MustParsePrefix("100.64.0.0/10")
)

// tailnetIface is the interface Tailscale gives the car its tailnet address on (CoilShift's Tailscale package runs
// tailscaled in kernel mode). Tests swap ifaceAddrs.
const tailnetIface = "tailscale0"

var ifaceAddrs = func(name string) ([]net.Addr, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	return ifc.Addrs()
}

// tailnetListenAddr is -tailnet's bind address: the one IPv4 address on tailscale0, which must be inside the tailnet
// range, on the default port. It is the only way agw binds a tailnet address: -listen and -dial keep refusing
// 100.64.0.0/10, so a typo can never put agw on another node's address. Nothing here dials anything.
// tailnetRetryWait is how long serve -tailnet waits before exiting when tailscale0 has no usable address;
// tailnetCheckEvery is how often a running serve -tailnet re-reads it. Tests shorten both.
var (
	tailnetRetryWait  = 5 * time.Second
	tailnetCheckEvery = 30 * time.Second
)

// watchTailnetAddr reports on moved, once, when tailscale0's address is no longer bound (Tailscale re-registered
// the car, or it moved tailnets), so serve exits and runit rebinds. A failed read (Tailscale restarting) is not a
// move: the bound address may come back, and the listener keeps it meanwhile.
func watchTailnetAddr(ctx context.Context, bound string, moved chan<- error) {
	t := time.NewTicker(tailnetCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now, err := tailnetListenAddr()
		if err != nil || now == bound {
			continue
		}
		moved <- fmt.Errorf("-tailnet: %s moved from %s to %s; restarting to rebind", tailnetIface, bound, now)
		return
	}
}

func tailnetListenAddr() (string, error) {
	addrs, err := ifaceAddrs(tailnetIface)
	if err != nil {
		return "", fmt.Errorf("-tailnet: %s: %v (is Tailscale up?)", tailnetIface, err)
	}
	var found []netip.Addr
	for _, a := range addrs {
		pfx, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		if ip := pfx.Addr().Unmap(); ip.Is4() {
			found = append(found, ip)
		}
	}
	switch {
	case len(found) == 0:
		return "", fmt.Errorf("-tailnet: %s has no IPv4 address (is Tailscale up?)", tailnetIface)
	case len(found) > 1:
		return "", fmt.Errorf("-tailnet: %s has %d IPv4 addresses, expected one", tailnetIface, len(found))
	case !cgnat.Contains(found[0]):
		return "", fmt.Errorf("-tailnet: %s's address %s is outside the tailnet range %s", tailnetIface, found[0], cgnat)
	}
	return net.JoinHostPort(found[0].String(), defaultPort), nil
}

// checkListen enforces the listen-address rules and returns the normalised
// host:port. bench permits only loopback; otherwise loopback, unspecified,
// the vehicle LAN and the tailnet range are refused.
func checkListen(addr string, bench bool) (string, error) {
	return checkAddr(addr, bench, defaultPort, "listen")
}

// checkDial applies the same rules to the Pi's address that -dial connects
// out to, with the forwarder's car-facing default port.
func checkDial(addr string, bench bool) (string, error) {
	return checkAddr(addr, bench, defaultDialPort, "dial")
}

func checkAddr(addr string, bench bool, defPort, what string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = strings.Trim(addr, "[]"), defPort
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("%s address must be a literal IP, got %q", what, host)
	}
	ip = ip.Unmap()
	if bench {
		if !ip.IsLoopback() {
			return "", fmt.Errorf("-bench permits only loopback %s addresses (127.0.0.0/8, ::1), refusing %s", what, ip)
		}
		return net.JoinHostPort(ip.String(), port), nil
	}
	switch {
	case ip.IsUnspecified():
		return "", fmt.Errorf("refusing unspecified address %s: use the car's Wi-Fi side address only", ip)
	case ip.IsLoopback():
		return "", fmt.Errorf("refusing loopback address %s without -bench", ip)
	case vehicleLAN.Contains(ip):
		return "", fmt.Errorf("refusing %s: inside the vehicle LAN %s (R22: never touch it)", ip, vehicleLAN)
	case cgnat.Contains(ip):
		return "", fmt.Errorf("refusing %s: inside the tailnet range %s", ip, cgnat)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// ---- serve ----

func cmdServe(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlags("serve", stderr)
	listen := fs.String("listen", "", "ip[:port] to bind (the car's Wi-Fi address; default port 17100)")
	dial := fs.String("dial", "", "ip[:port] of the Pi to connect out to (default port 17101); instead of -listen")
	tailnet := fs.Bool("tailnet", false, "listen on this car's own tailnet address (the IPv4 on tailscale0), port 17100")
	keyPath := fs.String("key", defaultDir+"/state/key.pem", "long-term key (mode 0400)")
	vinFile := fs.String("vin-file", "/var/etc/vin", "file holding the VIN")
	vinLit := fs.String("vin", "", "literal VIN (-bench only)")
	logPath := fs.String("log", defaultDir+"/state/log", "session log")
	// -allow is accepted for one release and ignored: a run script from before the allowlist's removal still passes
	// it between the new binary's install and the script's rewrite (S2), and must not restart-loop agw meanwhile.
	allowPath := fs.String("allow", "", "ignored (kept for one release): access is the car's own owner/driver keys")
	bench := fs.Bool("bench", false, "bench mode: loopback only, registers test-only capabilities")
	fakeRole := fs.Int("fake-role", 0, "bench only: fake authority returning this role")
	authSock := authSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	fakeSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "fake-role" {
			fakeSet = true
		}
	})
	if fakeSet && !*bench {
		return errors.New("-fake-role requires -bench")
	}
	if *vinLit != "" && !*bench {
		return errors.New("-vin requires -bench")
	}
	modes := 0
	for _, set := range []bool{*listen != "", *dial != "", *tailnet} {
		if set {
			modes++
		}
	}
	if modes != 1 {
		return usageError{errors.New("exactly one of -listen, -dial and -tailnet is required")}
	}
	if *tailnet && *bench {
		return usageError{errors.New("-tailnet cannot be used with -bench (loopback only)")}
	}
	var addr string
	var err error
	switch {
	case *dial != "":
		addr, err = checkDial(*dial, *bench)
	case *tailnet:
		if addr, err = tailnetListenAddr(); err != nil {
			// Tailscale not up yet (or signed out): wait before exiting, so runit retries
			// about every tailnetRetryWait instead of every second.
			select {
			case <-time.After(tailnetRetryWait):
			case <-ctx.Done():
			}
			return err
		}
	default:
		addr, err = checkListen(*listen, *bench)
	}
	if err != nil {
		return err
	}
	stderrLog(stderr)
	if *allowPath != "" {
		log.Print("agw: -allow is ignored: access is the car's own owner/driver keys")
	}
	if !*bench {
		if err := busSelfTest(ctx); err != nil {
			return err
		}
	}

	// No core dumps: the process holds the long-term key (spec 4.2).
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		return fmt.Errorf("cannot disable core dumps: %w", err)
	}

	key, err := keys.Load(*keyPath)
	if err != nil {
		return err
	}
	vin, err := readVIN(*vinFile, *vinLit)
	if err != nil {
		return err
	}
	// The sign-in helper's client connects lazily: no helper (or one that
	// fails) fails each arm closed and never stops serve (runit would
	// restart-loop it, taking every capability down).
	var oracle, recheck authority.Oracle
	var helper signIn
	if fakeSet {
		oracle = authority.Fake{Role: int32(*fakeRole), Owner: true}
	} else {
		helper = newSignIn(*authSock, log.Default())
		defer helper.Close()
		oracle = helper
		// The re-checks get a request connection of their own (the helper
		// serves each apart), so one never waits behind a sign-in.
		rc := newSignIn(*authSock, log.Default())
		defer rc.Close()
		recheck = rc
	}
	caps := selectCaps(*bench, fakeSet)

	srv := server.New(server.Config{
		Key: key, VIN: vin, Oracle: oracle, RecheckOracle: recheck, Caps: caps,
		LogPath: *logPath, MaxLogBytes: 1 << 20,
	})
	var ln net.Listener
	if *dial != "" {
		ln, err = dialout.Listen(dialout.Config{Addr: addr, Log: log.Default()})
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return err
	}
	wctx, cancel := context.WithCancel(ctx)
	watching := make(chan struct{})
	defer func() {
		cancel()
		<-watching
	}()
	// The helper's key-change watch drives the open windows' re-checks.
	go func() {
		defer close(watching)
		if helper != nil {
			helper.Watch(wctx, authority.WatchHandlers{KeysChanged: srv.KeysChanged, State: srv.SetWatchUp})
		}
	}()

	switch {
	case *dial != "":
		log.Printf("agw: dialing the Pi at %s (capabilities %s)", addr, capNames(caps))
	case *tailnet:
		log.Printf("agw: listening on the tailnet at %s (capabilities %s)", ln.Addr(), capNames(caps))
	default:
		log.Printf("agw: listening on %s (capabilities %s)", ln.Addr(), capNames(caps))
	}
	if onListen != nil {
		onListen(ln.Addr())
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	moved := make(chan error, 1)
	if *tailnet {
		go watchTailnetAddr(wctx, addr, moved)
	}
	select {
	case err := <-done:
		srv.Shutdown("error")
		return err
	case err := <-moved:
		// The bound address is no longer this car's tailnet address: exit, and runit's
		// restart binds the new one.
		srv.Shutdown("tailnet address changed")
		<-done
		return err
	case <-ctx.Done():
		srv.Shutdown("sigterm")
		<-done
		return nil
	}
}

func readVIN(file, lit string) (string, error) {
	if lit != "" {
		return lit, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading VIN file: %w", err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New("VIN file is empty")
	}
	return v, nil
}

// ---- keygen / pin ----

func cmdKeygen(args []string, stdout io.Writer) error {
	fs := newFlags("keygen", io.Discard)
	keyPath := fs.String("key", defaultDir+"/state/key.pem", "where to create the key (mode 0400, never overwritten)")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if err := keys.Generate(*keyPath); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "key written (mode 0400)")
	return nil
}

func cmdPin(args []string, stdout io.Writer) error {
	fs := newFlags("pin", io.Discard)
	keyPath := fs.String("key", defaultDir+"/state/key.pem", "long-term key")
	vinFile := fs.String("vin-file", "/var/etc/vin", "file holding the VIN")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	key, err := keys.Load(*keyPath)
	if err != nil {
		return err
	}
	vin, err := readVIN(*vinFile, "")
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, keys.PinString(&key.PublicKey, vin))
	return nil
}

// ---- probe-auth key parsing ----

func parseKeyHex(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 65 || b[0] != 4 {
		return nil, errors.New("key must be 130 hex chars (65-byte uncompressed P-256)")
	}
	if _, err := ecdh.P256().NewPublicKey(b); err != nil {
		return nil, errors.New("key is not a valid P-256 point")
	}
	return b, nil
}

// authSocketFlag is -auth-socket: the camera root helper's sign-in socket.
// The authd pin (-authd-sha256) lives in camhelper, which asks authd.
func authSocketFlag(fs *flag.FlagSet) *string {
	return fs.String("auth-socket", authproto.DefaultSocket, "the camera root helper's sign-in socket (camhelper, root)")
}

// busSelfTest refuses to serve unless the system bus definitely refuses agw.
// On this car any connected uid may call com.tesla.DisplayManager and
// com.tesla.Videod, so the gateway uid has no account and dbus-daemon
// refuses it. But dbus-daemon caches a uid it once resolved (the 2026-11-20
// /etc/passwd overlay test) until the car reboots or the bus reloads its
// configuration. Each probe opens a private connection with a bounded
// handshake and closes it at once: agw never keeps one.
//
// Only a definite refusal passes (authority.BusRefused): no bus socket, or
// dbus-daemon answering REJECTED or disconnecting agw right after BEGIN. It
// must repeat, busRefusalsNeeded probes in a row: dbus-daemon also closes a
// connection that has not finished Hello when too many are incomplete
// (max_incomplete_connections), and a probe cannot tell that close from a
// refusal of the uid, which dbus-daemon repeats every time. A probe without
// a verdict (no answer within the bound: a busy bus may still let the uid
// in) ends a run of refusals; busProbeTries probes at most. If the bus took
// the connection, or no refusal repeated, serve logs BUS SELF-TEST FAILED,
// waits busRefusal (or until shutdown) and fails, so runit retries slowly.
func busSelfTest(ctx context.Context) error {
	uid := os.Getuid()
	v, err := authority.BusUnknown, errors.New("not probed")
	refusals, inARow := 0, 0
	for i := 0; i < busProbeTries; i++ {
		if i > 0 && !pause(ctx, busProbePause) {
			break
		}
		v, err = busProbe(busProbeTimeout)
		if v == authority.BusReachable {
			break
		}
		if v != authority.BusRefused {
			inARow = 0
			log.Printf("agw: bus self-test: probe %d of %d has no verdict (%v)", i+1, busProbeTries, err)
			continue
		}
		refusals++
		if inARow++; inARow == busRefusalsNeeded {
			break
		}
		log.Printf("agw: bus self-test: probe %d of %d refused (%v); probing again to confirm", i+1, busProbeTries, err)
	}
	var refusal error
	switch {
	case v != authority.BusReachable && inARow == busRefusalsNeeded:
		log.Printf("agw: bus self-test passed: the system bus refuses uid %d, %d probes in a row (%v)", uid, inARow, err)
		return nil
	case v != authority.BusReachable && refusals > 0:
		log.Printf("agw: BUS SELF-TEST FAILED: the system bus refused agw (uid %d), but not %d probes in a row (last: %v): "+
			"dbus-daemon also drops a connection for its own connection limits, so one refusal is not a verdict",
			uid, busRefusalsNeeded, err)
		refusal = fmt.Errorf("refusing to serve: the bus self-test did not confirm a refusal of uid %d (%v)", uid, err)
	case v == authority.BusReachable:
		log.Printf("agw: BUS SELF-TEST FAILED: agw (uid %d) can reach the system bus and must not: "+
			"any connected uid may call com.tesla.DisplayManager and com.tesla.Videod on this car", uid)
		log.Print("agw: BUS SELF-TEST FAILED: dbus-daemon caches a uid it once resolved (the 2026-11-20 " +
			"/etc/passwd overlay test); a car reboot, or a reload of the bus configuration, clears it")
		refusal = fmt.Errorf("refusing to serve: uid %d can reach the system bus (reboot the car)", uid)
	default:
		log.Printf("agw: BUS SELF-TEST FAILED: no verdict on whether agw (uid %d) can reach the system bus (%v): "+
			"a bus too busy to answer may still let it in, so only a definite refusal passes", uid, err)
		refusal = fmt.Errorf("refusing to serve: the bus self-test has no verdict for uid %d (%v)", uid, err)
	}
	log.Printf("agw: BUS SELF-TEST FAILED: refusing to serve; exiting in %v", busRefusal)
	pause(ctx, busRefusal)
	return refusal
}

// pause waits d, or until ctx ends (then false).
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ---- probe-auth ----

// cmdProbe asks the sign-in helper (-auth-socket) only owner and key for the
// -pub key(s) and prints, per key, "owner=ok role=N fp=<fingerprint, as the
// session log shows it> id=<authd's key Id> ff=<form factor> secs=<seconds
// left>". Role 2 (OWNER) or 3 (DRIVER) is what a sign-in needs. It never
// serves and never touches the system bus.
func cmdProbe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("probe-auth", io.Discard)
	pubs := fs.String("pub", "", "comma-separated hex public key(s) (required)")
	authSock := authSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	var pks [][]byte
	for _, h := range strings.Split(*pubs, ",") {
		if strings.TrimSpace(h) == "" {
			continue
		}
		pk, err := parseKeyHex(h)
		if err != nil {
			return err
		}
		pks = append(pks, pk)
	}
	if len(pks) == 0 {
		return usageError{errors.New("no keys to probe: -pub hex[,hex...]")}
	}
	o := newSignIn(*authSock, log.New(stderr, "agw probe-auth: ", 0))
	defer o.Close()
	var bad error
	for _, pk := range pks {
		fp := keys.Fingerprint(pk)                    // the fingerprint, as the session log and agw's log lines show it
		id := hex.EncodeToString(authority.KeyID(pk)) // authd's key Id, as the car shows it
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		owner, err := o.AuthOwnerIsAuthd(c)
		if err != nil {
			cancel()
			fmt.Fprintf(stderr, "owner=error fp=%s id=%s: %v\n", fp, id, err)
			bad = errors.New("authority probe failed")
			continue
		}
		if !owner {
			cancel()
			fmt.Fprintf(stderr, "owner=not-authd fp=%s id=%s\n", fp, id)
			bad = errors.New("authority probe failed")
			continue
		}
		e, err := o.KeyEntry(c, pk)
		cancel()
		if err != nil {
			fmt.Fprintf(stderr, "owner=ok role=error fp=%s id=%s: %v\n", fp, id, err)
			bad = errors.New("authority probe failed")
			continue
		}
		fmt.Fprintf(stdout, "owner=ok role=%d fp=%s id=%s ff=%d secs=%d\n", e.Role, fp, id, e.FormFactor, e.SecondsLeft)
		if e.Role != 2 && e.Role != 3 {
			bad = errors.New("authority probe failed: role not OWNER/DRIVER")
		}
	}
	return bad
}

// ---- probe-route / probe-summon ----

// cmdProbeRoute runs the SUMMON relay's R1 gate, guards.RouteOff as its PreArm does, against tailscaled's LocalAPI,
// and prints "route gate: ok" or "route gate: refused". The installer runs it as agw's uid, which must be able to
// read the LocalAPI. A refusal's reason goes to stderr. Nothing ever echoes the prefs.
func cmdProbeRoute(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlags("probe-route", io.Discard)
	sock := fs.String("tailscale-sock", guards.DefaultTailscaleSock, "tailscaled's LocalAPI socket")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{errors.New("no arguments expected")}
	}
	if err := guards.RouteOff(ctx, *sock); err != nil {
		fmt.Fprintln(stdout, "route gate: refused")
		return err // a reason only: RouteOff's errors never quote the prefs
	}
	fmt.Fprintln(stdout, "route gate: ok")
	return nil
}

// cmdProbeSummon checks the summon helper's relay socket exactly as the SUMMON relay's Open does: its server is uid
// probeSummonUID, and its first record, within Open's own bound summonrelay.OwnedWait (500 ms), is OWNED. So an
// OWNED that comes too late for Open fails the check too. It closes without sending a record and prints
// "summon relay: ok" or "summon relay: refused: <reason>". The installer runs it as agw's uid, retried: the cross-uid
// connect test.
func cmdProbeSummon(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlags("probe-summon", io.Discard)
	sock := fs.String("socket", summonrelay.DefaultSocket, "the summon helper's relay socket")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{errors.New("no arguments expected")}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*summonrelay.OwnedWait) // the dial too; the OWNED wait ends first
	defer cancel()
	if err := summonrelay.Probe(ctx, *sock, probeSummonUID, summonrelay.OwnedWait); err != nil {
		fmt.Fprintf(stdout, "summon relay: refused: %v\n", err)
		return errors.New("refused")
	}
	fmt.Fprintln(stdout, "summon relay: ok")
	return nil
}
