package helper

// The supervisor and the finish hook (design §6.1). `summonhelper run` is Supervise: it runs `summonhelper serve` as
// its child under internal/supervise, queues for com.tesla.Webrtc behind it, and when the child is gone takes the name
// and sends the abort and the disconnect. runit's finish script, `summonhelper finish`, is Finish: it covers a
// supervisor that died itself. Both send only while the session marker says a session may be open, remove the marker
// only after cmd_abort and a confirmed disconnect, and log nothing before they have sent them. Both also remove the
// live file a helper left (live.go), whatever the marker says, before any log line and before they release the name;
// the removal never changes their result (R4).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/summon"
	"airgapp/gateway/internal/summon/marker"
	"airgapp/gateway/internal/supervise"
)

const (
	TakeoverWait = time.Second           // after the child's death, the longest wait for the name
	FinishRetry  = 20 * time.Millisecond // Finish's period between DO_NOT_QUEUE requests
	FinishFor    = time.Second           // how long Finish keeps asking
	StartupGrace = 5 * time.Second       // the child's time to its first liveness byte (the oracle warm-up included)
)

// Supervise is `summonhelper run`. livePath is the child's live file (Config.LivePath), which Finish and the takeover
// remove. In order:
//
//  1. it answers the car's two methods with Reply 0, for whenever it holds the name;
//  2. it runs Finish for a stale session marker (R5), which also removes a stale live file. If that fails, it returns
//     the error and starts no child: a stale session is never left unaborted while a new one serves;
//  3. it makes its Link after that Finish released the name and before its queue request, so the Link records every
//     NameAcquired the queue brings: ownership is state, not an event;
//  4. it runs serveArgv as its child under supervise.Run, with StartupGrace;
//  5. on the child's first liveness byte it queues for the name. InQueue is the only answer it accepts; any other gets
//     the child SIGKILLed, and Supervise fails. After an error the parent holds no name, so it sends nothing and runit's
//     finish covers the session. The name itself means the child had it and lost it while it ran: the parent keeps it
//     and takes over (7);
//  6. it watches its own bus connection. If that closes while the child lives, the parent is out of the queue, so the
//     child is SIGKILLed and Supervise fails; runit's finish covers the session. No session runs without a queued
//     parent;
//  7. once the child is gone, the takeover (takeover.run).
//
// It returns nil only when ctx ended and the takeover found no marker or cleared it (or ctx ended before any child
// started). Any other end is an error (a child death that ctx did not cause is a *supervise.Exit), so `run` exits
// non-zero, and runsv runs `finish` and restarts the service.
func Supervise(ctx context.Context, bus carlink.Bus, serveArgv []string, markerPath, livePath string,
	logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	setBusLogf(bus, logf)
	if err := bus.Serve(carlink.Handlers{}); err != nil {
		return fmt.Errorf("summonhelper: serving %s: %w", carlink.WebrtcPath, err)
	}
	if err := Finish(bus, markerPath, livePath, logf); err != nil {
		return fmt.Errorf("summonhelper: a stale session is not finished, so no helper starts: %w", err)
	}
	if ctx.Err() != nil {
		return nil // stopped before any child started: no session can be open
	}

	runCtx, kill := context.WithCancel(ctx) // kill: supervise.Run SIGKILLs the child
	defer kill()
	tko := &takeover{ctx: ctx, link: carlink.NewLink(bus), markerPath: markerPath, livePath: livePath, logf: logf}
	if err := bus.OnNameLost(carlink.WebrtcName, func() { tko.lost(kill) }); err != nil {
		return fmt.Errorf("summonhelper: watching the supervisor's own bus connection: %w", err)
	}
	var tkoErr error
	err := supervise.Run(runCtx, serveArgv, func() error {
		tkoErr = tko.run()
		return tkoErr
	}, supervise.Options{
		StartupGrace: StartupGrace,
		OnFirstBeat:  func() { tko.queue(bus, kill) },
		Logf:         logf,
	})
	if err != nil {
		return err // the child could not start, or it died without a stop through runCtx
	}
	return tkoErr // a stop through runCtx: ctx ended, or a kill of the parent's own (tko.run says which)
}

// takeover is the parent's hold on the name. The Link keeps the ownership state; mu guards what happened while the
// child lived: whether it is gone yet, and why the parent killed it, if it did.
type takeover struct {
	ctx        context.Context // the takeover's timers derive from it, without its cancellation
	link       *carlink.Link
	markerPath string
	livePath   string // the child's live file: removed on every branch
	logf       func(format string, args ...any)

	mu       sync.Mutex
	gone     bool  // the takeover began: whatever the queue request brings from now on is its own
	queueErr error // the queue request failed while the child lived, which killed it
	atOnce   bool  // the queue request got the name while the child lived: the child had lost it, and was killed
	lostErr  error // the parent's own bus connection closed while the child lived, which killed it
}

// queue is OnFirstBeat, on a goroutine of its own: the queue request (RequestName without DO_NOT_QUEUE). Any answer
// but InQueue, while the child lives, gets the child SIGKILLed. The name itself means the child had it, since it beats
// only while it owns the name, and lost it: the parent keeps the name, and the takeover aborts the session.
func (t *takeover) queue(bus carlink.Bus, kill func()) {
	owned, err := bus.RequestName(carlink.WebrtcName, true)
	if err == nil && !owned {
		t.logf("summonhelper: queued for %s behind the child", carlink.WebrtcName)
		return
	}
	t.mu.Lock()
	gone := t.gone
	if !gone {
		if err != nil {
			t.queueErr = err
		} else {
			t.atOnce = true
		}
	}
	t.mu.Unlock()
	if gone {
		return // the child died first: the name, if it came, is the takeover's
	}
	kill()
	if err != nil {
		t.logf("summonhelper: the queue request for %s failed: %v; SIGKILLing the child", carlink.WebrtcName, err)
	}
}

// lost is the parent's NameLost callback. Before the takeover the parent never gives the name up, so a NameLost then
// is its own connection closing (the bus calls every NameLost callback when it does), which also took it out of the
// queue: the child, left without a queued parent, is SIGKILLed. It runs on the bus's callback goroutine and never
// blocks.
func (t *takeover) lost(kill func()) {
	t.mu.Lock()
	gone := t.gone
	if !gone && t.lostErr == nil {
		t.lostErr = errors.New("the supervisor's own bus connection closed while the child lived")
	}
	t.mu.Unlock()
	if !gone {
		kill()
	}
}

// run is the takeover, supervise.Run's finish hook: it runs once the child is gone, before Run logs anything about
// that, and on timers of its own, so a SIGTERM cannot cut it short:
//
//  1. unless the queue request failed or the parent's own connection closed, wait up to TakeoverWait for the name;
//  2. if the marker is present, cmd_abort, then always the disconnect, confirmed; the marker goes only if both
//     succeeded;
//  3. on every branch, remove the live file the child left (RemoveLive), so that even a child stuck in D state, which
//     still holds its lock, leaves none for camhelper to find;
//  4. release the name, which also leaves the queue if the name never came.
//
// Then it logs the live file's removal, and a success; supervise.Run logs a failure. The removal never changes the
// result (R4). Without the name, or after a failed emit, the marker stays, and the error makes `run` exit non-zero, so
// runit's finish tries again. After the queue request got the name at once it takes over all the same, and still
// fails: the parent killed a child that had lost its name.
func (t *takeover) run() error {
	t.mu.Lock()
	t.gone = true
	queueErr, atOnce, lostErr := t.queueErr, t.atOnce, t.lostErr
	t.mu.Unlock()

	var err error
	done := "found no session marker: nothing sent"
	switch {
	case lostErr != nil:
		err = fmt.Errorf("no takeover, %w; any session marker stays", lostErr)
	case queueErr != nil:
		err = fmt.Errorf("no takeover, the queue request had failed (%w); any session marker stays", queueErr)
	case !t.waitName():
		err = fmt.Errorf("no takeover, %s did not come within %v; any session marker stays", carlink.WebrtcName,
			TakeoverWait)
	case marker.Present(t.markerPath):
		err = abortAndDisconnect(t.link, t.markerPath)
		done = "sent cmd_abort and the disconnect (confirmed) and removed the session marker"
	}
	removed, rmErr := RemoveLive(t.livePath) // every branch, after any abort, before the Release: never in err (R4)
	if rerr := t.link.Release(); rerr != nil {
		err = errors.Join(err, fmt.Errorf("releasing %s: %w", carlink.WebrtcName, rerr))
	}
	LogLiveRemoval(t.logf, "takeover", removed, rmErr)
	if err == nil {
		t.logf("summonhelper: takeover: the child is gone; %s", done)
	}
	if atOnce {
		err = errors.Join(errors.New("the queue request got the name at once: the child had lost it, and was SIGKILLed"),
			err)
	}
	if err != nil {
		return fmt.Errorf("takeover: %w", err)
	}
	return nil
}

// waitName waits up to TakeoverWait for the Link to hold the name. A name that came while the child still ran counts
// at once.
func (t *takeover) waitName() bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), TakeoverWait)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !t.link.Owned() {
		select {
		case <-ctx.Done():
			return t.link.Owned()
		case <-tick.C:
		}
	}
	return true
}

// Finish is the runit finish hook (`summonhelper finish`), and Supervise's first step. It first removes the live file a
// helper left (RemoveLive), whatever the marker says. Without a session marker it does nothing more. With one, it
// acquires the name (DO_NOT_QUEUE, every FinishRetry for FinishFor), sends cmd_abort and then, always, the disconnect
// (confirmed), removes the marker only if both succeeded, and releases the name. Then it logs the live file's removal
// and a success; the caller logs the error. It returns nil only when no marker is left: the removal never changes the
// result (R4). Its outcome is logged after the marker check, and with a marker only after the name's release.
func Finish(bus carlink.Bus, markerPath, livePath string, logf func(format string, args ...any)) error {
	removed, rmErr := RemoveLive(livePath) // first, whatever the marker says: never Finish's error (R4)
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !marker.Present(markerPath) {
		LogLiveRemoval(logf, "finish", removed, rmErr)
		return nil
	}
	setBusLogf(bus, logf)
	link := carlink.NewLink(bus)
	err := acquire(link)
	if err == nil {
		err = abortAndDisconnect(link, markerPath)
	}
	if rerr := link.Release(); rerr != nil {
		err = errors.Join(err, fmt.Errorf("releasing %s: %w", carlink.WebrtcName, rerr))
	}
	LogLiveRemoval(logf, "finish", removed, rmErr)
	if err != nil {
		return err
	}
	logf("summonhelper: finish: sent cmd_abort and the disconnect (confirmed) and removed the session marker")
	return nil
}

// acquire asks for the name with DO_NOT_QUEUE every FinishRetry until FinishFor has passed.
func acquire(link *carlink.Link) error {
	deadline := time.Now().Add(FinishFor)
	for {
		err := link.Acquire()
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s not acquired within %v (%w); the session marker stays", carlink.WebrtcName,
				FinishFor, err)
		}
		time.Sleep(FinishRetry)
	}
}

// abortAndDisconnect sends cmd_abort and then, whatever became of it, the disconnect, which Link.Disconnect confirms
// with a round trip. The marker is removed only when both succeeded.
func abortAndDisconnect(link *carlink.Link, markerPath string) error {
	abortErr := link.Send([]byte(summon.AbortJSON))
	discErr := link.Disconnect()
	if err := errors.Join(abortErr, discErr); err != nil {
		return fmt.Errorf("the session marker stays: %w", err)
	}
	if err := marker.New(markerPath).Clear(); err != nil {
		return fmt.Errorf("cmd_abort and the disconnect went out, but the session marker stays: %w", err)
	}
	return nil
}

// setBusLogf hands a DBusBus the logger for the car calls it drops. The bus calls it on godbus's read goroutine, so it
// must never block: logq's in production.
func setBusLogf(bus carlink.Bus, logf func(format string, args ...any)) {
	if b, ok := bus.(interface{ SetLogf(func(string, ...any)) }); ok {
		b.SetLogf(logf)
	}
}
