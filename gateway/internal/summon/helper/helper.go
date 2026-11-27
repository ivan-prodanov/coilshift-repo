// Package helper is summonhelper's serving process (design §6.1). It owns com.tesla.Webrtc, runs the inner SUMMON
// capability on the gateway server core, and serves the core on the relay socket that agw dials. It is the only
// network-reachable process that can issue a motion command, so its startup order, its health predicate and its
// NameLost handling decide whether a broken helper is killed and its abort sent.
package helper

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/clock"
	"airgapp/gateway/internal/server"
	"airgapp/gateway/internal/summon"
	"airgapp/gateway/internal/summon/marker"
	"airgapp/gateway/internal/summon/relay"
	"airgapp/gateway/internal/supervise"
)

const (
	OracleWarm         = 3 * time.Second // the warm-up AuthOwnerIsAuthd call's bound (step 7)
	ExitSupervisorLost = 1               // a liveness write failed: the supervisor is gone

	// ExitFatal is Fatal's exit code: a panic in an engine's Stop or Tick. It differs from carlink.ExitEmitOverrun
	// (3), so the supervisor's "exited on its own: exit status N" line tells the two apart: Fatal's queued log line
	// (the panic) is usually lost, since the exit is immediate and does not wait for the log queue. Both exits are
	// immediate, so the takeover's abort is never delayed.
	ExitFatal = 4

	// AgwUID is agw's uid, the relay peer Config.AllowUID 0 stands for: zero is never root, as in summonrelay.
	AgwUID = 4950

	maxSessionLog = 1 << 20 // the session log rotates past this many bytes
)

// Test seams. startLiveness is supervise.StartLiveness: a test replaces it to reach the liveness wiring without a
// supervisor. disableDumps is DisableDumps: a test records that Serve calls it first.
var (
	startLiveness = supervise.StartLiveness
	disableDumps  = DisableDumps
)

var _ summon.Marker = (*marker.File)(nil)

// Config is Serve's configuration. Bus, Oracle, Key, a valid VIN, Socket, MarkerPath and LivePath are required:
// LivePath has no default, so a caller that forgets it is refused instead of touching /run (R2). A nil RecheckOracle is
// Oracle, a zero AllowUID is AgwUID (never root), a nil Clock is clock.Boot(), a nil Now time.Now, a nil Logf
// discards, a nil Exit is os.Exit, and a nil PeerUID is peercred.PeerUID.
//
// There is no key list: a sign-in is granted when authd lists the phone's key as OWNER or DRIVER (authority.Check),
// and the server core re-checks an open session's key on its own schedule (internal/server/recheck.go), so a key the
// car stops listing loses its session.
type Config struct {
	Bus         carlink.Bus
	Oracle      authority.Oracle // sign-ins, and re-checks unless RecheckOracle is set
	Key         *ecdsa.PrivateKey
	VIN         string
	Socket      string // /run/airgapp-summon/relay.sock
	AllowUID    int    // the only relay peer uid served: agw's; 0 means AgwUID (4950), never root
	PeerUID     func(net.Conn) (int, error)
	MarkerPath  string // /run/airgapp-summon/session
	LivePath    string // camhelper/proto.SummonLivePath (/run/airgapp-summon/live); never derived from MarkerPath
	LogPath     string // /var/lib/airgapp-summon/state/log
	Clock       clock.Clock
	Now         func() time.Time
	Logf        func(format string, args ...any) // must never block (Global Constraints); logq's in production
	Exit        func(code int)                   // carlink.WithExit
	Liveness    bool                             // start supervise.StartLiveness; false only in tests
	OnListening func()                           // test hook: the socket exists (after step 11, before step 12)

	// WrapCar is a test seam (R3), nil in production. It wraps the Link before the Link is handed to the capability,
	// so a test can stand between the engine and the car (Task 11 wedges a Send with it).
	WrapCar func(summon.Car) summon.Car

	// OnCore is a test seam, nil in production: it is handed the server core once built (step 9), so a test can ask
	// for an immediate re-check (KeysChanged) instead of waiting for the core's period.
	OnCore func(*server.Server)

	// RecheckOracle answers the core's re-checks of the open session (server.Config.RecheckOracle); nil means Oracle.
	// summonhelper gives it an authd connection of its own, as agw gives its re-checks a helper connection of their
	// own, so a re-check never shares a connection with a sign-in.
	RecheckOracle authority.Oracle
}

// check validates cfg and fills in its defaults. It has no side effects: a refusal touches neither the bus nor the
// filesystem.
func (cfg Config) check() (Config, error) {
	switch {
	case cfg.Bus == nil:
		return cfg, errors.New("summonhelper: no bus")
	case cfg.Oracle == nil:
		return cfg, errors.New("summonhelper: no authority oracle")
	case cfg.Key == nil:
		return cfg, errors.New("summonhelper: no long-term key")
	case !ValidVIN(cfg.VIN):
		return cfg, errors.New("summonhelper: the VIN is not valid") // never the value (Global Constraints)
	case cfg.Socket == "" || cfg.MarkerPath == "" || cfg.LivePath == "":
		return cfg, errors.New("summonhelper: the socket, marker and live paths are required")
	case cfg.Liveness && os.Getenv(supervise.LivenessEnv) == "":
		return cfg, fmt.Errorf("summonhelper: %s is not set: no supervisor reads the liveness pipe", supervise.LivenessEnv)
	}
	if cfg.AllowUID == 0 {
		cfg.AllowUID = AgwUID
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Boot()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Exit == nil {
		cfg.Exit = os.Exit
	}
	return cfg, nil
}

// Serve runs the helper until ctx ends, in the brief's order:
//
//  1. disable dumpability (Linux): the process holds its long-term key;
//  2. log the live file's path, then remove a live file left there (RemoveLive), before the name is asked for and so
//     whatever the name then says (R3): a stray second serve thus unlinks a serving helper's file, and the Summon
//     camera fails closed. A removal that found a file is logged, as evidence of an unclean end;
//  3. acquire com.tesla.Webrtc with DO_NOT_QUEUE: ErrNameTaken is an error, and the process exits 1;
//  4. build the capability: the Link (wrapped by WrapCar, if set) is its Car, the marker file its Marker, its SetLink
//     is bound late to the step 11 listener, its Fatal logs and calls Exit(ExitFatal), after which the parent's
//     takeover sends the abort, and its Live holds the live file, locked, while the engine of a session that opted
//     into the presence lease is up (ruling F1);
//  5. serve the car's calls on the bus, wired to the capability;
//  6. on the Link's first unhealthy moment (NameLost, a failed or overrun emit, an unconfirmed disconnect), end the
//     open session at once; its H4 then cannot confirm the disconnect, so the marker stays for the takeover;
//  7. warm the oracle, then RecheckOracle if set, each with one AuthOwnerIsAuthd within OracleWarm: a failure is
//     logged, and sign-ins (or re-checks) fail closed until the oracle answers;
//  8. (no key list: authd's OWNER/DRIVER roles decide sign-ins, step 9's core asks them);
//  9. build the server core: it signs in only keys authd lists as OWNER or DRIVER, and re-checks an open session's
//     key (internal/server/recheck.go) through RecheckOracle, revoking it when authd stops listing the key;
//  10. start the D2 loop (NewCapability seeded its progress stamp);
//  11. listen on the relay socket, handing out OWNED only while Health holds; then OnListening, if set;
//  12. with Liveness, write the liveness pipe while Health holds; when a write fails, the supervisor is gone: shut
//     the core down ("supervisor-lost", which runs H4) and Exit(ExitSupervisorLost);
//  13. serve the core on the relay socket.
//
// When ctx ends, srv.Shutdown("sigterm") runs H4 through the open session's Teardown, then the name is released, and
// Serve returns nil. If the core stops on its own, Serve shuts it down the same way and returns an error. Serve never
// touches the standard library's logger: the bench runs agw and the helper in one process (R6).
func Serve(ctx context.Context, cfg Config) error {
	cfg, err := cfg.check()
	if err != nil {
		return err
	}
	logf := cfg.Logf

	// 1. No core dump, and no ptrace by the uid's other processes.
	if err := disableDumps(); err != nil {
		return fmt.Errorf("summonhelper: disabling dumpability: %w", err)
	}
	// The car calls a DBusBus drops are logged by size, on godbus's read goroutine: through Logf, which never blocks.
	setBusLogf(cfg.Bus, logf)

	// 2. The live file. One left there is not this process's: it goes before the name is asked for (R3).
	logf("summonhelper: live file %s", cfg.LivePath)
	switch removed, err := RemoveLive(cfg.LivePath); {
	case err != nil:
		logf("summonhelper: removing a stale live file: %v (%s)", err, leftLive)
	case removed:
		logf("summonhelper: removed a stale live file (an earlier session did not end cleanly)")
	}

	// 3. The name.
	link := carlink.NewLink(cfg.Bus, carlink.WithExit(cfg.Exit), carlink.WithLogf(logf))
	if err := link.Acquire(); err != nil {
		return fmt.Errorf("summonhelper: %s: %w", carlink.WebrtcName, err)
	}

	// 4. The capability.
	var car summon.Car = link
	if cfg.WrapCar != nil {
		car = cfg.WrapCar(link)
	}
	var listener atomic.Pointer[relay.Listener] // step 11's, for SetLink
	var health func() bool                      // Health(link, c), once c exists
	c := summon.NewCapability(summon.CapConfig{
		Car:     car,
		Marker:  healthyMarker{m: marker.New(cfg.MarkerPath), link: link},
		Clock:   cfg.Clock,
		Now:     cfg.Now,
		Logf:    logf,
		Healthy: func() bool { return health() }, // PreArm: the predicate that gates OWNED and liveness
		SetLink: func(degraded bool) {
			if l := listener.Load(); l != nil {
				l.SetLink(degraded) // never blocks (R11)
			}
		},
		Fatal: func(err error) {
			logf("summonhelper: fatal: %v: exiting %d, so the supervisor's takeover aborts", err, ExitFatal)
			cfg.Exit(ExitFatal)
		},
		Live: func() func() {
			h, err := holdLive(cfg.LivePath)
			if err != nil {
				logf("summonhelper: live file not created: %v (no Summon camera for this session)", err)
				return nil
			}
			return func() {
				if err := h.release(); err != nil {
					logf("summonhelper: live file not removed: %v (its lock is released, so it proves nothing)", err)
				}
			}
		},
	})
	health = Health(link, c)

	// 5. The car's calls.
	if err := cfg.Bus.Serve(carlink.Handlers{CarFrame: c.CarFrame, Signaling: c.Signaling, Reply: c.Reply}); err != nil {
		link.Release()
		return fmt.Errorf("summonhelper: serving %s: %w", carlink.WebrtcPath, err)
	}

	// 6. NameLost or a failed emit ends the open session at once. It also fires on Release, when no session is open,
	// and End is then a no-op.
	link.OnUnhealthy(func() { c.End("error") })

	// 7. The oracles. The re-checks' too: it is lazy, and its first use, about 10 s into a session (often in motion),
	// must not pay for its setup inside authority.Check's 500 ms, which an overrun turns into a revocation (an abort).
	warmOracle(ctx, cfg.Oracle, logf)
	if cfg.RecheckOracle != nil {
		warmOracle(ctx, cfg.RecheckOracle, logf)
	}

	// 9. The core. Its re-check loop starts with srv.Serve (step 13).
	srv := server.New(server.Config{
		Key: cfg.Key, VIN: cfg.VIN, Oracle: cfg.Oracle, RecheckOracle: cfg.RecheckOracle, Clock: cfg.Clock,
		Caps: []capability.Capability{c}, LogPath: cfg.LogPath, MaxLogBytes: maxSessionLog,
	})
	if cfg.OnCore != nil {
		cfg.OnCore(srv)
	}

	// 10. The D2 loop. It runs until Serve's end, past ctx's: H4 at shutdown runs with the loop still stamping.
	loopCtx, stopLoop := context.WithCancel(context.WithoutCancel(ctx))
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		c.Loop(loopCtx)
	}()
	// stop ends the D2 loop (10) and liveness (12), then releases the name (3). Every path
	// closes the listener (11) before it: srv.Shutdown does once srv.Serve has it, and step 12's failure closes it.
	stop := func(stopLive func()) {
		stopLoop()
		<-loopDone
		if stopLive != nil {
			stopLive()
		}
		link.Release()
	}

	// 11. The relay socket.
	l, err := relay.Listen(relay.ListenConfig{
		Path: cfg.Socket, AllowUID: cfg.AllowUID, PeerUID: cfg.PeerUID, Ready: health, Logf: logf,
	})
	if err != nil {
		srv.Shutdown("error")
		stop(nil)
		return fmt.Errorf("summonhelper: relay socket: %w", err)
	}
	listener.Store(l)
	if cfg.OnListening != nil {
		cfg.OnListening()
	}

	// 12. Liveness.
	var stopLive func()
	if cfg.Liveness {
		stopLive, err = startLiveness(health, func(err error) {
			logf("summonhelper: the liveness pipe failed (%v): the supervisor is gone; shutting down", err)
			srv.Shutdown("supervisor-lost")
			cfg.Exit(ExitSupervisorLost)
		})
		if err == nil && stopLive == nil {
			err = fmt.Errorf("%s is not set", supervise.LivenessEnv)
		}
		if err != nil {
			srv.Shutdown("error")
			l.Close()
			stop(nil)
			return fmt.Errorf("summonhelper: liveness: %w", err)
		}
	}

	logf("summonhelper: serving SUMMON on %s for peer uid %d", cfg.Socket, cfg.AllowUID)

	// 13. The core.
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		srv.Shutdown("sigterm") // H4 for an open session, through its Teardown
		<-served
		err = nil
	case err = <-served:
		srv.Shutdown("error")
		if err == nil {
			err = errors.New("summonhelper: the core stopped serving")
		}
	}
	stop(stopLive)
	logf("summonhelper: stopped")
	return err
}

// warmOracle is step 7: one AuthOwnerIsAuthd within OracleWarm, so the first sign-in does not pay for the oracle's
// lazy setup. A failure is logged, and is not fatal: sign-ins fail closed until the oracle answers.
func warmOracle(ctx context.Context, o authority.Oracle, logf func(string, ...any)) {
	wctx, cancel := context.WithTimeout(ctx, OracleWarm)
	defer cancel()
	owner, err := o.AuthOwnerIsAuthd(wctx)
	switch {
	case err != nil:
		logf("summonhelper: warming the authority oracle: %v; sign-ins fail closed until it answers", err)
	case !owner:
		logf("summonhelper: warming the authority oracle: com.tesla.Auth is not authd's; sign-ins fail closed")
	}
}

// healthyMarker is the session marker, refused while the link is unhealthy (fix round 1). PreArm's health check is a
// snapshot, and OnUnhealthy fires once ever: a NameLost between PreArm and Open's install would find no session to
// end, and the session would arm on a lost name. Open installs the session before Start's first step, Marker.Set, so
// a NameLost before this check refuses the session (no marker, no H1), and one after it finds the session current, so
// OnUnhealthy's End closes it and H4 runs. The Link latches unhealthy at once, so the check never misses one.
type healthyMarker struct {
	m    *marker.File
	link *carlink.Link
}

var _ summon.Marker = healthyMarker{}

// Set refuses while the link is unhealthy, before it touches the file.
func (h healthyMarker) Set() error {
	if !h.link.Healthy() {
		return errors.New("summonhelper: the car link is not owned and healthy")
	}
	return h.m.Set()
}

// Clear passes through.
func (h healthyMarker) Clear() error { return h.m.Clear() }

// Health is the liveness and Ready predicate: the link is healthy and the D2 loop stamped within MaxProgress. It
// never blocks, as relay.ListenConfig.Ready and supervise.StartLiveness require: the Link's lock is never held across
// a bus call, and Progress reads an atomic.
func Health(link *carlink.Link, c *summon.Capability) func() bool {
	return func() bool { return link.Healthy() && c.Progress() <= summon.MaxProgress }
}

// ValidVIN accepts exactly 17 characters of [A-HJ-NPR-Z0-9].
func ValidVIN(v string) bool {
	if len(v) != 17 {
		return false
	}
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z' && c != 'I' && c != 'O' && c != 'Q':
		default:
			return false
		}
	}
	return true
}
