// Command camhelper is the camera root helper (PLAN §4.9): the only process
// that writes the camera DataValues and spawns the capture, talk and cut
// children. It serves the gateway uid on a unix socket, and on a second one
// (auth.sock, same directory) answers the gateway's sign-in questions from
// authd's KeyInfo, so the gateway uid needs no system bus access.
//
//	camhelper run   [flags]   serve (as root; under runit)
//	camhelper sweep [flags]   one-shot: clear what a previous run left marked (runit finish)
//
// See docs/CAMERA-HELPER.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/authsrv"
	"airgapp/gateway/internal/camhelper"
)

const defaultState = "/run/airgapp-camhelper-state"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	signal.Ignore(syscall.SIGHUP)
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

// Test hooks.
var (
	geteuid   = os.Geteuid
	chown     = os.Chown
	onReady   func(sock string)
	newOracle = dbusOracle                                                 // authd's KeyInfo, as root
	runWatch  = func(ctx context.Context, w *authsrv.Watch) { w.Run(ctx) } // the key-change subscription
)

// dbusOracle is the gateway core's KeyInfo oracle, pinned to the authd build.
func dbusOracle(pin string) (authsrv.Oracle, error) {
	o, err := authority.NewDBus(pin)
	if err != nil {
		return nil, err
	}
	ko, ok := o.(authority.KeyEntryOracle)
	if !ok {
		return nil, errors.New("the authd oracle has no KeyEntry")
	}
	return ko, nil
}

func run(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "run" && args[0] != "sweep") {
		fmt.Fprintln(stderr, "usage: camhelper run|sweep [flags]")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", defaultState, "root-only state dir (markers, lock, cut files)")
	sock := fs.String("sock", proto.DefaultSocket, "unix socket (its dir is created root:<gw-gid> 0750)")
	gwUID := fs.Int("gw-uid", 4950, "the only peer uid served")
	gwGID := fs.Int("gw-gid", 4950, "group owning the socket and its dir")
	childUID := fs.Int("child-uid", 4951, "uid of the capture/talk/cut children")
	childGID := fs.Int("child-gid", 4951, "gid of the children")
	groups := fs.String("child-groups", "29,1021", "supplementary groups of the children (audio, pipewire)")
	writer := fs.String("writer", string(camhelper.WriterPWCat), "talk writer: pw-cat (default) or aplay")
	teslacam := fs.String("teslacam", "/mnt/usb/TeslaCam", "clip root")
	authSock := fs.String("auth-sock", authproto.DefaultSocket, "the sign-in socket (same directory as -sock)")
	summonLive := fs.String("summon-live", camhelper.DefaultSummonLive, "summonhelper's live file (SUMMON mode's trust signal)")
	summonUID := fs.Int("summon-uid", camhelper.DefaultSummonUID, "summonhelper's uid: the only owner of the live file and its dir")
	authdPin := fs.String("authd-sha256", authority.DefaultAuthdSHA256,
		"SHA-256 of the /usr/bin/authd build whose KeyInfo was verified (64 hex digits; it cannot be disabled)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	lg := log.New(stderr, "camhelper: ", log.LstdFlags)
	if geteuid() != 0 {
		lg.Print("must run as root")
		return 1
	}
	var err error
	if err = pinned(*state); err == nil && args[0] == "run" {
		err = pinned(filepath.Dir(*sock))
		if err == nil && filepath.Dir(*authSock) != filepath.Dir(*sock) {
			err = fmt.Errorf("-auth-sock %s must be in -sock's directory %s", *authSock, filepath.Dir(*sock))
		}
		if err == nil && !isPin(*authdPin) {
			err = fmt.Errorf("-authd-sha256 must be 64 lowercase hex digits, got %q", *authdPin)
		}
	}
	if err != nil {
		lg.Print(err)
		return 2
	}
	if args[0] == "sweep" {
		err = sweep(*state, lg)
	} else {
		var gs []uint32
		gs, err = parseGroups(*groups)
		if err == nil {
			err = serve(ctx, serveConfig{
				state: *state, sock: *sock, authSock: *authSock, authdPin: *authdPin, gwUID: *gwUID, gwGID: *gwGID,
				cred:   &syscall.Credential{Uid: uint32(*childUID), Gid: uint32(*childGID), Groups: gs},
				writer: camhelper.WriterKind(*writer), teslacam: *teslacam, log: lg,
				summonLive: *summonLive, summonUID: *summonUID,
			})
		}
	}
	if err != nil {
		lg.Print(err)
		return 1
	}
	return 0
}

func isPin(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func parseGroups(s string) ([]uint32, error) {
	var out []uint32
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad -child-groups %q", s)
		}
		out = append(out, uint32(n))
	}
	return out, nil
}

// runRoot is where the helper's own directories must live (a test hook).
var runRoot = "/run"

// pinned refuses any directory that is not /run/airgapp-<name>: the helper
// creates, chowns and chmods these, so it must never be pointed at a system
// directory.
func pinned(dir string) error {
	if dir != filepath.Clean(dir) || filepath.Dir(dir) != runRoot || !strings.HasPrefix(filepath.Base(dir), "airgapp-") {
		return fmt.Errorf("%s: the helper's directories must be %s/airgapp-<name>", dir, runRoot)
	}
	return nil
}

// ensureDir creates dir (or fixes an existing one) with the given owner and
// mode, refusing a symlink or a non-directory in its place.
func ensureDir(dir string, mode os.FileMode, uid, gid int) error {
	st, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(dir, mode); err != nil {
			return err
		}
	case err != nil:
		return err
	case !st.IsDir():
		return fmt.Errorf("%s exists and is not a directory (refusing)", dir)
	}
	if err := chown(dir, uid, gid); err != nil {
		return err
	}
	return os.Chmod(dir, mode)
}

// lock takes the state lock (one helper or sweep at a time) or fails.
func lock(state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another camhelper holds " + state + "/lock (never start it twice)")
	}
	return f, nil
}

func sweep(state string, lg *log.Logger) error {
	if _, err := os.Lstat(state); errors.Is(err, os.ErrNotExist) {
		return nil // nothing ever ran (or /run was cleared by a reboot)
	}
	l, err := lock(state)
	if err != nil {
		lg.Printf("sweep skipped: %v", err)
		return nil
	}
	defer l.Close()
	camhelper.Sweep(camhelper.SweepConfig{Car: camhelper.ExecCar{}, StateDir: state, Log: lg})
	return nil
}

type serveConfig struct {
	state, sock  string
	authSock     string
	authdPin     string
	gwUID, gwGID int
	cred         *syscall.Credential
	writer       camhelper.WriterKind
	teslacam     string
	summonLive   string
	summonUID    int
	log          *log.Logger
}

func serve(ctx context.Context, c serveConfig) error {
	if c.writer != camhelper.WriterPWCat && c.writer != camhelper.WriterAplay {
		return fmt.Errorf("unknown -writer %q", c.writer)
	}
	if err := ensureDir(c.state, 0o711, 0, 0); err != nil {
		return err
	}
	l, err := lock(c.state)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := ensureDir(filepath.Join(c.state, "cut"), 0o700, int(c.cred.Uid), int(c.cred.Gid)); err != nil {
		return err
	}
	car := camhelper.ExecCar{}
	camhelper.Sweep(camhelper.SweepConfig{Car: car, StateDir: c.state, Log: c.log})

	oracle, err := newOracle(c.authdPin)
	if err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(c.sock), 0o750, 0, c.gwGID); err != nil {
		return err
	}
	ln, err := listenUnix(c.sock, c.gwGID)
	if err != nil {
		return err
	}
	aln, err := listenUnix(c.authSock, c.gwGID)
	if err != nil {
		ln.Close()
		return err
	}
	asrv := authsrv.New(authsrv.Config{Oracle: oracle, AllowUID: c.gwUID, Log: c.log})
	wctx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		runWatch(wctx, &authsrv.Watch{Notify: asrv.Notify, Log: c.log})
	}()
	defer func() {
		stopWatch()
		<-watchDone
		asrv.Shutdown()
		os.Remove(c.authSock)
	}()
	srv := camhelper.New(camhelper.Config{
		Car: car, Spawner: camhelper.ExecSpawner{Cred: c.cred}, StateDir: c.state,
		TeslaCam: c.teslacam, AllowUID: c.gwUID, Writer: c.writer, Log: c.log,
		SummonLive: c.summonLive, SummonUID: c.summonUID,
	})
	c.log.Printf("serving uid %d on %s (%s; children uid %d groups %v); sign-in on %s (authd pin %.12s…)",
		c.gwUID, c.sock, camhelper.Describe(), c.cred.Uid, c.cred.Groups, c.authSock, c.authdPin)
	if onReady != nil {
		onReady(c.sock)
	}
	done := make(chan error, 2)
	go func() { done <- srv.Serve(ln) }()
	go func() {
		if err := asrv.Serve(aln); err != nil {
			done <- fmt.Errorf("sign-in socket: %w", err)
		}
	}()
	select {
	case err := <-done:
		srv.Shutdown()
		return err
	case <-ctx.Done():
		c.log.Print("SIGTERM: releasing everything")
		srv.Shutdown()
		<-done
		os.Remove(c.sock)
		return nil
	}
}

// listenUnix listens on path (removing a stale socket of a previous run; we
// hold the lock), root:gid 0660.
func listenUnix(path string, gid int) (net.Listener, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket (refusing)", path)
		}
		os.Remove(path)
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := chown(path, 0, gid); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
