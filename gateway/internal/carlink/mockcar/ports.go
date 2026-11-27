package mockcar

import (
	"errors"
	"sync"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/internal/carlink"
)

// fakePort carries the Car on a FakeBus. FakeBus.Emit runs observers inside the helper's deadline-bounded emit, so
// the observer only queues: one goroutine at a time delivers the queue to the Car, in order, after Emit has returned.
// The goroutine exists only while the queue is non-empty (nothing to close). drain waits for the queue to empty and
// the last delivery (and the car's answers to it) to finish, which is how Step stays deterministic.
type fakePort struct {
	bus *carlink.FakeBus

	mu      sync.Mutex
	idle    *sync.Cond
	car     *Car
	queue   []carlink.Emission
	running bool
}

// FakePort delivers b's emissions to the attached Car, asynchronously and in order, and makes the car's calls on b.
func FakePort(b *carlink.FakeBus) Port {
	p := &fakePort{bus: b}
	p.idle = sync.NewCond(&p.mu)
	b.OnEmit(p.enqueue)
	return p
}

func (p *fakePort) attach(c *Car) {
	p.mu.Lock()
	p.car = c
	p.start()
	p.mu.Unlock()
}

func (p *fakePort) enqueue(member string, body map[string]any) {
	p.mu.Lock()
	p.queue = append(p.queue, carlink.Emission{Member: member, Body: body})
	p.start()
	p.mu.Unlock()
}

// start launches the delivery goroutine if there is work and none runs. p.mu is held.
func (p *fakePort) start() {
	if p.running || p.car == nil || len(p.queue) == 0 {
		return
	}
	p.running = true
	go p.deliver()
}

func (p *fakePort) deliver() {
	p.mu.Lock()
	for len(p.queue) > 0 {
		e := p.queue[0]
		p.queue = p.queue[1:]
		car := p.car
		p.mu.Unlock()
		car.Signal(e.Member, e.Body)
		p.mu.Lock()
	}
	p.running = false
	p.idle.Broadcast()
	p.mu.Unlock()
}

// drain returns once the queue is empty and no delivery is in flight.
func (p *fakePort) drain() {
	p.mu.Lock()
	for p.running || (p.car != nil && len(p.queue) > 0) {
		p.idle.Wait()
	}
	p.mu.Unlock()
}

func (p *fakePort) CallCarserverToPeer(channel string, data []byte) error {
	r := p.bus.CallCarserverToPeer(map[string]any{
		"uid": carlink.PhoneUID, "channel": channel, "data": string(data),
	})
	if r == nil {
		return errors.New("mockcar: nothing is served on the FakeBus")
	}
	return nil
}

func (p *fakePort) CallRemoteToLocal(body map[string]any) error {
	p.bus.CallRemoteToLocal(body)
	return nil
}

// dbusPort plays QtCarServer on a real bus connection.
type dbusPort struct {
	conn *dbus.Conn
	car  *Car
}

// DBusPort plays QtCarServer on a real bus. It matches signals with
// sender='com.tesla.Webrtc',path='/Webrtc',interface='com.tesla.Webrtc' (the daemon resolves the well-known name to
// its current owner, so a signal from a connection that does not own the name is never delivered), and calls methods
// on destination com.tesla.Webrtc, as QtCarServer's interface does. conn should use godbus's sequential signal
// handler (bustest's Conn does), so signals reach the Car in the bus's order.
func DBusPort(conn *dbus.Conn) Port { return &dbusPort{conn: conn} }

func (p *dbusPort) attach(c *Car) {
	p.car = c
	ch := make(chan *dbus.Signal, 256)
	p.conn.Signal(ch)
	err := p.conn.AddMatchSignal(
		dbus.WithMatchSender(carlink.WebrtcName),
		dbus.WithMatchObjectPath(carlink.WebrtcPath),
		dbus.WithMatchInterface(carlink.WebrtcIface),
	)
	if err != nil {
		c.cfg.Logf("mockcar: AddMatch: %v", err)
	}
	go func() { // ends when the connection closes (godbus closes the channel)
		for s := range ch {
			if s.Path != carlink.WebrtcPath || s.Name != carlink.WebrtcIface+"."+carlink.SignalLocalToRemoteSignaling &&
				s.Name != carlink.WebrtcIface+"."+carlink.SignalPeerCommsToCarserverData {
				continue
			}
			if len(s.Body) != 1 {
				continue
			}
			vm, ok := s.Body[0].(map[string]dbus.Variant)
			if !ok {
				continue
			}
			body := make(map[string]any, len(vm))
			for k, v := range vm {
				body[k] = v.Value()
			}
			c.Signal(s.Name[len(carlink.WebrtcIface)+1:], body)
		}
	}()
}

func (p *dbusPort) obj() dbus.BusObject { return p.conn.Object(carlink.WebrtcName, carlink.WebrtcPath) }

func (p *dbusPort) CallCarserverToPeer(channel string, data []byte) error {
	return p.obj().Call(carlink.WebrtcIface+"."+carlink.MethodCarserverToPeerCommsData, 0, map[string]dbus.Variant{
		"uid": dbus.MakeVariant(carlink.PhoneUID), "channel": dbus.MakeVariant(channel), "data": dbus.MakeVariant(string(data)),
	}).Err
}

func (p *dbusPort) CallRemoteToLocal(body map[string]any) error {
	vm := make(map[string]dbus.Variant, len(body))
	for k, v := range body {
		vm[k] = dbus.MakeVariant(v)
	}
	return p.obj().Call(carlink.WebrtcIface+"."+carlink.MethodRemoteToLocalSignaling, 0, vm).Err
}
