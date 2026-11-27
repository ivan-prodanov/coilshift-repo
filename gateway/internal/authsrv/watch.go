package authsrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/camhelper/authproto"
)

const (
	authName  = "com.tesla.Auth"
	authPath  = dbus.ObjectPath("/Auth")
	busDriver = "org.freedesktop.DBus"
)

// Watch keeps a private system bus connection subscribed to authd's
// KeysUpdated and to com.tesla.Auth's owner changes, and reports to Notify:
// watch_up then keys_changed "resubscribed" each time the subscription is
// (re)made, keys_changed for each signal, watch_down when it is lost. It
// reconnects with backoff. Both match rules are pinned to their sender, and
// since a signal can also be sent straight to this connection (no match rule
// needed), KeysUpdated counts only from com.tesla.Auth's current unique owner
// (GetNameOwner at subscribe time, then NameOwnerChanged from the bus driver,
// whose sender no client can take).
type Watch struct {
	Notify      func(authproto.Event)
	Dial        func() (*dbus.Conn, error) // default: a private system bus connection
	Log         *log.Logger
	MinBackoff  time.Duration // default 1 s
	MaxBackoff  time.Duration // default 10 s
	StableAfter time.Duration // a subscription this old resets the backoff (default 30 s)
	Liveness    time.Duration // how often the connection is checked (default 5 s)
	CallTimeout time.Duration // AddMatch and GetNameOwner (default 2 s)
}

// The two match rules (the gateway core's carlink rule for KeysUpdated, with
// the sender pinned).
var matchRules = [][]dbus.MatchOption{
	{dbus.WithMatchSender(authName), dbus.WithMatchObjectPath(authPath),
		dbus.WithMatchInterface(authName), dbus.WithMatchMember("KeysUpdated")},
	{dbus.WithMatchSender(busDriver), dbus.WithMatchInterface(busDriver),
		dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, authName)},
}

// Run returns when ctx is done (after reporting watch_down).
func (w *Watch) Run(ctx context.Context) {
	lg := w.Log
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	minB, maxB := w.MinBackoff, w.MaxBackoff
	if minB <= 0 {
		minB = time.Second
	}
	if maxB < minB {
		maxB = 10 * time.Second
		if maxB < minB {
			maxB = minB
		}
	}
	stable := w.StableAfter
	if stable <= 0 {
		stable = 30 * time.Second
	}
	backoff := minB
	for {
		up, err := w.once(ctx)
		w.Notify(authproto.Event{Ev: authproto.EvWatchDown})
		if ctx.Err() != nil {
			return
		}
		if up >= stable {
			backoff = minB // only a subscription that held resets it: a flapping bus backs off
		}
		lg.Printf("auth watch: down (%v); retrying in %v", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff *= 2; backoff > maxB {
			backoff = maxB
		}
	}
}

// once is one connection: subscribe, learn the owner, report, then dispatch
// until it ends. up is how long the subscription held (0 if it never did).
func (w *Watch) once(ctx context.Context) (up time.Duration, err error) {
	dial := w.Dial
	if dial == nil {
		dial = systemBus
	}
	c, err := dial()
	if err != nil {
		return 0, err
	}
	defer c.Close()
	ch := make(chan *dbus.Signal, 32)
	c.Signal(ch)
	callTimeout := w.CallTimeout
	if callTimeout <= 0 {
		callTimeout = 2 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	for _, rule := range matchRules {
		if err := c.AddMatchSignalContext(cctx, rule...); err != nil {
			return 0, fmt.Errorf("AddMatch: %w", err)
		}
	}
	// The owner after the rules are in place: a change from here on arrives
	// as NameOwnerChanged, queued behind this answer.
	var t tracker
	call := c.BusObject().CallWithContext(cctx, busDriver+".GetNameOwner", 0, authName)
	switch {
	case call.Err == nil:
		if err := call.Store(&t.owner); err != nil {
			return 0, fmt.Errorf("GetNameOwner: %w", err)
		}
	case isNoOwner(call.Err):
		t.owner = "" // nobody owns it now: no KeysUpdated counts until someone does
	default:
		return 0, fmt.Errorf("GetNameOwner: %w", call.Err)
	}
	since := time.Now()
	w.Notify(authproto.Event{Ev: authproto.EvWatchUp})
	w.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonResubscribed})
	every := w.Liveness
	if every <= 0 {
		every = 5 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return time.Since(since), ctx.Err()
		case sig, ok := <-ch:
			if !ok {
				return time.Since(since), errors.New("bus connection closed")
			}
			if e, ok := t.dispatch(sig); ok {
				w.Notify(e)
			}
		case <-tick.C:
			if !c.Connected() {
				return time.Since(since), errors.New("bus connection lost")
			}
		}
	}
}

// tracker follows com.tesla.Auth's unique owner on one connection.
type tracker struct{ owner string }

// dispatch maps a signal to a keys_changed event: KeysUpdated only from the
// current owner, NameOwnerChanged for com.tesla.Auth only from the bus
// driver (which also moves the owner). Everything else is ignored: a signal
// sent straight to this connection by another client, NameAcquired and the
// like.
func (t *tracker) dispatch(sig *dbus.Signal) (authproto.Event, bool) {
	switch {
	case sig.Name == authName+".KeysUpdated" && sig.Path == authPath:
		if t.owner == "" || sig.Sender != t.owner {
			return authproto.Event{}, false
		}
		return authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated}, true
	case sig.Name == busDriver+".NameOwnerChanged" && sig.Sender == busDriver &&
		len(sig.Body) == 3 && sig.Body[0] == authName:
		next, _ := sig.Body[2].(string)
		t.owner = next
		return authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonOwnerChanged}, true
	}
	return authproto.Event{}, false
}

func isNoOwner(err error) bool {
	const name = "org.freedesktop.DBus.Error.NameHasNoOwner"
	var de dbus.Error
	var dp *dbus.Error
	return (errors.As(err, &de) && de.Name == name) || (errors.As(err, &dp) && dp.Name == name)
}

// systemBus is a private system bus connection, its handshake bounded.
func systemBus() (*dbus.Conn, error) {
	c, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}
	return c, handshake(c, 2*time.Second)
}

// handshake authenticates (EXTERNAL) and sends Hello, closing c if that takes
// longer than timeout.
func handshake(c *dbus.Conn, timeout time.Duration) error {
	t := time.AfterFunc(timeout, func() { c.Close() })
	err := c.Auth(nil)
	if err == nil {
		err = c.Hello()
	}
	if !t.Stop() && err == nil {
		err = errors.New("timed out")
	}
	if err != nil {
		c.Close()
		return fmt.Errorf("handshake: %w", err)
	}
	return nil
}
