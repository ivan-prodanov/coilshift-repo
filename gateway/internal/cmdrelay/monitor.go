package cmdrelay

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/godbus/dbus/v5"
)

// The router's replies for the authd source: com.tesla.Auth.RouteMessageBytes(ay), sent by command-router.
const (
	authName    = "com.tesla.Auth"
	authMember  = "RouteMessageBytes"
	monitorRule = "type='method_call',destination='" + authName + "',interface='" + authName + "',member='" +
		authMember + "'"
	// The router's owner changes (a restart): only its current owner's replies are delivered.
	ownerRule = "type='signal',sender='org.freedesktop.DBus',interface='org.freedesktop.DBus',member='NameOwnerChanged'," +
		"arg0='" + routerName + "'"
)

// Monitor watches the system bus for the router's replies with org.freedesktop.DBus.Monitoring.BecomeMonitor (root
// may monitor the system bus) and hands each body to Deliver. It reconnects with a backoff until ctx ends, and
// reports whether it is up through SetUp.
type Monitor struct {
	Dial    func() (*dbus.Conn, error) // nil: a private system bus connection (Auth, Hello)
	Deliver func(body []byte)
	SetUp   func(bool)
	Log     *log.Logger
}

func systemBus() (*dbus.Conn, error) {
	c, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}
	if err := c.Auth(nil); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.Hello(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Run monitors until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	dial := m.Dial
	if dial == nil {
		dial = systemBus
	}
	lg := m.Log
	if lg == nil {
		lg = log.Default()
	}
	wait := 100 * time.Millisecond
	for ctx.Err() == nil {
		err := m.once(ctx, dial)
		m.SetUp(false)
		if ctx.Err() != nil {
			return
		}
		lg.Printf("cmdrelay: monitor down (%v); retrying in %v", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < 5*time.Second {
			wait *= 2
		}
	}
}

func (m *Monitor) once(ctx context.Context, dial func() (*dbus.Conn, error)) error {
	conn, err := dial()
	if err != nil {
		return err
	}
	// godbus's Close closes the eavesdrop channel while its reader may still send on it (a panic that would end
	// camhelper): stop eavesdropping first.
	defer func() {
		conn.Eavesdrop(nil)
		conn.Close()
	}()
	// Who the router is now, asked before becoming a monitor (a monitor cannot make calls); NameOwnerChanged keeps it
	// current afterwards.
	var owner string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, routerName).Store(&owner); err != nil {
		owner = "" // the router is not running: nothing is delivered until it appears
	}
	// Eavesdrop diverts every incoming message, BecomeMonitor's own reply included, so the call goes out with no
	// reply expected and success is the bus driver's NameLost for our unique name (a monitor loses all its names).
	// Eavesdropping from the start also means godbus never answers a monitored call: a monitor must not send.
	ch := make(chan *dbus.Message, 256)
	conn.Eavesdrop(ch)
	names := conn.Names()
	if len(names) == 0 {
		return errors.New("no unique name")
	}
	self := names[0]
	call := conn.BusObject().Go("org.freedesktop.DBus.Monitoring.BecomeMonitor", dbus.FlagNoReplyExpected, nil,
		[]string{monitorRule, ownerRule}, uint32(0))
	if call.Err != nil {
		return call.Err
	}
	becameBy := time.After(becomeBudget)
	up := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return errors.New("connection closed")
		case <-becameBy:
			if !up {
				return errors.New("BecomeMonitor: no NameLost (not allowed to monitor?)")
			}
		case msg := <-ch:
			if !up && lostOwnName(msg, self) {
				up = true
				m.SetUp(true)
				continue
			}
			if o, ok := routerOwnerChange(msg); ok {
				owner = o
				continue
			}
			if body := replyBody(msg); body != nil && owner != "" && hdr(msg, dbus.FieldSender) == owner {
				m.Deliver(body)
			}
		}
	}
}

// becomeBudget bounds the wait for BecomeMonitor to take effect.
const becomeBudget = 3 * time.Second

// lostOwnName is the bus driver's NameLost signal for name.
func lostOwnName(msg *dbus.Message, name string) bool {
	if msg == nil || msg.Type != dbus.TypeSignal || hdr(msg, dbus.FieldSender) != "org.freedesktop.DBus" ||
		hdr(msg, dbus.FieldMember) != "NameLost" || len(msg.Body) != 1 {
		return false
	}
	s, _ := msg.Body[0].(string)
	return s == name
}

// replyBody is the byte array of a com.tesla.Auth.RouteMessageBytes call, or nil for anything else.
func replyBody(msg *dbus.Message) []byte {
	if msg == nil || msg.Type != dbus.TypeMethodCall || len(msg.Body) != 1 {
		return nil
	}
	if hdr(msg, dbus.FieldDestination) != authName || hdr(msg, dbus.FieldInterface) != authName ||
		hdr(msg, dbus.FieldMember) != authMember {
		return nil
	}
	b, ok := msg.Body[0].([]byte)
	if !ok {
		return nil
	}
	return b
}

func hdr(msg *dbus.Message, f dbus.HeaderField) string {
	v, ok := msg.Headers[f]
	if !ok {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

// routerOwnerChange is the bus driver's NameOwnerChanged for the router: its new owner ("" when it went away).
func routerOwnerChange(msg *dbus.Message) (string, bool) {
	if msg == nil || msg.Type != dbus.TypeSignal || hdr(msg, dbus.FieldSender) != "org.freedesktop.DBus" ||
		hdr(msg, dbus.FieldMember) != "NameOwnerChanged" || len(msg.Body) != 3 {
		return "", false
	}
	name, _ := msg.Body[0].(string)
	newOwner, _ := msg.Body[2].(string)
	if name != routerName {
		return "", false
	}
	return newOwner, true
}
