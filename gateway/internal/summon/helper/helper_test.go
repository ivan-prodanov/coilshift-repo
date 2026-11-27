package helper

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/client"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/carlink/bustest"
	"airgapp/gateway/internal/carlink/mockcar"
	"airgapp/gateway/internal/clock"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/server"
	"airgapp/gateway/internal/summon"
	"airgapp/gateway/internal/summon/marker"
	"airgapp/gateway/internal/summon/relay"
	"airgapp/gateway/internal/supervise"
)

// The helper tests run Serve in-process on a carlink.FakeBus, with the mock QtCarServer on a FakePort and a fake
// authority (role 2, owner) wrapped in a counting oracle. The phone is a Go client that plays agw's side of the relay
// socket: relay records on the unix socket, pumped into a net.Pipe, with client.Handshake on the pipe's other end.
// Socket paths live under os.MkdirTemp("", "sh"): darwin caps them at 104 bytes, which t.TempDir() can pass.

const (
	testVIN = "5YJ3E1EA7KF000000"
	wait    = 3 * time.Second // the bound on anything a test waits for that should happen

	// The car's pose, and a phone about 11 m north of it: past D3's 3 m floor.
	carLat, carLon = 52.52, 13.405
	phoneLat       = carLat + 0.0001

	typeStyle    = "autopark:style"
	typeStatus   = "autopark:status"
	typeLocation = "vehicle_data:location"
	typeForward  = "autopark:cmd_forward"
	typeAbort    = "autopark:cmd_abort"

	// What reached the car or the bus, besides the phone's data (named by its msg_type).
	kindH1         = "data_channels_ready"
	kindDisconnect = "disconnect"
)

// ---- recorders ----

// logLines records Serve's log lines. It never blocks for long, as Config.Logf must not.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Logf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (l *logLines) with(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return "log:\n  " + strings.Join(l.lines, "\n  ")
}

// eventLog is one ordered record of what the bus emitted (synchronously, inside the emit), what the test did, and
// when the test's agw read OWNED.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	e.ev = append(e.ev, s)
	e.mu.Unlock()
}

// index is the position of the first event s, or -1.
func (e *eventLog) index(s string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Index(e.ev, s)
}

func (e *eventLog) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return "events:\n  " + strings.Join(e.ev, "\n  ")
}

// countOracle is authority.Fake that counts its calls (R21) and records how long each AuthOwnerIsAuthd's context had
// left. during, if set before Serve starts, runs inside the first AuthOwnerIsAuthd call: the warm-up.
type countOracle struct {
	authority.Fake
	owner, role atomic.Int32

	mu        sync.Mutex
	remaining []time.Duration // per AuthOwnerIsAuthd call: its context's time left, or -1 without a deadline
	during    func()
}

func (o *countOracle) AuthOwnerIsAuthd(ctx context.Context) (bool, error) {
	first := o.owner.Add(1) == 1
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	o.mu.Lock()
	o.remaining = append(o.remaining, left)
	during := o.during
	o.mu.Unlock()
	if first && during != nil {
		during()
	}
	return o.Fake.AuthOwnerIsAuthd(ctx)
}

func (o *countOracle) KeyRole(ctx context.Context, pk []byte) (int32, error) {
	o.role.Add(1)
	return o.Fake.KeyRole(ctx, pk)
}

// ---- the rig: Serve, the bus, the mock car ----

type rig struct {
	t      *testing.T
	cfg    Config
	bus    *carlink.FakeBus
	car    *mockcar.Car
	oracle *countOracle
	dev    *ecdh.PrivateKey // the phone's key: never written anywhere; the oracle's role decides
	logs   *logLines
	events *eventLog
	exits  chan int // every Exit call's code

	onListen  func() // runs inside OnListening
	listening chan struct{}
	cancel    context.CancelFunc
	finished  chan struct{} // closed when Serve returned
	err       error         // Serve's result, once finished is closed

	probe *probeClock // the clock, once useProbe made it a probeClock; nil otherwise
}

// newRig builds Serve's Config for a test. The test may change r.cfg before r.start or r.serve.
func newRig(t *testing.T) *rig {
	t.Helper()
	dir, err := os.MkdirTemp("", "sh") // not t.TempDir(): darwin caps unix socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := &rig{
		t: t, bus: carlink.NewFakeBus(), oracle: &countOracle{Fake: authority.Fake{Role: 2, Owner: true}},
		logs: &logLines{}, events: &eventLog{}, exits: make(chan int, 16), listening: make(chan struct{}),
		finished: make(chan struct{}),
	}
	r.bus.OnEmit(func(member string, body map[string]any) { r.events.add("emit " + emitKind(member, body)) })
	r.car = mockcar.New(mockcar.Config{Port: mockcar.FakePort(r.bus)})
	r.car.SetPose(carLat, carLon, 90, 0, "off", "P")
	carCtx, stopCar := context.WithCancel(context.Background())
	go r.car.Run(carCtx)
	t.Cleanup(stopCar)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r.dev = newDeviceKey(t)
	r.cfg = Config{
		Bus: r.bus, Oracle: r.oracle, Key: key, VIN: testVIN,
		Socket: filepath.Join(dir, "relay.sock"), AllowUID: os.Getuid(),
		MarkerPath: filepath.Join(dir, "session"), LivePath: filepath.Join(dir, "live"),
		LogPath: filepath.Join(dir, "log"), Clock: clock.Boot(), Logf: r.logs.Logf,
		Exit: func(code int) {
			r.events.add(fmt.Sprintf("exit %d", code))
			select {
			case r.exits <- code:
			default:
			}
		},
		OnListening: func() {
			if r.onListen != nil {
				r.onListen()
			}
			close(r.listening)
		},
	}
	return r
}

// serve runs Serve and waits until it listens (nil) or returns first (its error). Cleanup stops it and checks that
// no unexpected Exit came and that the mock car saw no rule broken.
func (r *rig) serve() error {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		r.err = Serve(ctx, r.cfg)
		close(r.finished)
	}()
	r.t.Cleanup(r.stop)
	select {
	case <-r.listening:
		return nil
	case <-r.finished:
		if r.err == nil {
			r.t.Fatal("Serve returned nil before listening")
		}
		return r.err
	case <-time.After(wait):
		r.t.Fatalf("Serve neither listened nor returned within %v\n%s", wait, r.logs)
	}
	return nil
}

// start is serve for a test that needs Serve listening.
func (r *rig) start() {
	r.t.Helper()
	if err := r.serve(); err != nil {
		r.t.Fatalf("Serve returned before listening: %v\n%s", err, r.logs)
	}
}

func (r *rig) stop() {
	r.cancel()
	select {
	case <-r.finished:
	case <-time.After(5 * time.Second):
		r.t.Errorf("Serve did not return within 5 s of its context's end\n%s", r.logs)
		return
	}
	select {
	case code := <-r.exits:
		r.t.Errorf("Exit(%d) was called\n%s", code, r.logs)
	default:
	}
	if v := r.car.Violations(); len(v) > 0 {
		r.t.Errorf("the mock car saw rules broken: %q", v)
	}
}

// carGot is what the mock car received, in order: the msg_type of the phone's data, kindH1 or kindDisconnect.
func (r *rig) carGot() []string {
	var out []string
	for _, rec := range r.car.Received() {
		out = append(out, rkind(rec))
	}
	return out
}

// waitCarGot waits until the mock car has received n inputs of kind.
func (r *rig) waitCarGot(kind string, n int) {
	r.t.Helper()
	ok := r.car.WaitFor(func(got []mockcar.Received) bool {
		c := 0
		for _, rec := range got {
			if rkind(rec) == kind {
				c++
			}
		}
		return c >= n
	}, wait)
	if !ok {
		r.t.Fatalf("the car did not receive %d %q within %v; it got %q\n%s", n, kind, wait, r.carGot(), r.logs)
	}
}

func rkind(rec mockcar.Received) string {
	if rec.Member == carlink.SignalLocalToRemoteSignaling {
		switch {
		case bytes.Contains(rec.Data, []byte(`"data_channels_ready"`)):
			return kindH1
		case rec.MsgType == "autopark:set_webrtc":
			return kindDisconnect
		}
		return "signaling " + string(rec.Data)
	}
	return rec.MsgType
}

func emitKind(member string, body map[string]any) string {
	if member == carlink.SignalLocalToRemoteSignaling {
		if _, ok := body["data_channels_ready"]; ok {
			return kindH1
		}
		if _, ok := body["ice_state_change"]; ok {
			return kindDisconnect
		}
		return "signaling"
	}
	data, _ := body["data"].(string)
	return msgTypeOf([]byte(data))
}

func msgTypeOf(js []byte) string {
	var head struct {
		MsgType string `json:"msg_type"`
	}
	json.Unmarshal(js, &head)
	return head.MsgType
}

// after is what follows the first x in s.
func after(s []string, x string) []string {
	if i := slices.Index(s, x); i >= 0 {
		return s[i+1:]
	}
	return nil
}

func newDeviceKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, wait)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- agw's side of a relay connection, and the phone behind it ----

type agwConn struct {
	t  *testing.T
	uc *net.UnixConn
	rd *relay.Reader
	wr *relay.Writer
}

func (r *rig) dial() *agwConn {
	r.t.Helper()
	c, err := net.DialTimeout("unix", r.cfg.Socket, wait)
	if err != nil {
		r.t.Fatalf("dialling the relay socket: %v", err)
	}
	uc := c.(*net.UnixConn)
	r.t.Cleanup(func() { uc.Close() })
	return &agwConn{t: r.t, uc: uc, rd: relay.NewReader(uc, true), wr: relay.NewWriter(uc)}
}

// first reads the connection's first record: nil for OWNED; io.EOF when the helper closed the connection without one.
func (a *agwConn) first() error {
	a.uc.SetReadDeadline(time.Now().Add(wait))
	typ, _, err := a.rd.Next()
	a.uc.SetReadDeadline(time.Time{})
	if err == nil && typ != relay.RecOwned {
		err = fmt.Errorf("first record 0x%02x, want OWNED", typ)
	}
	return err
}

// phone is the inner session over one relay connection.
type phone struct {
	t    *testing.T
	a    *agwConn
	in   net.Conn // the inner session's end of the pipe
	pump net.Conn // the relay pumps' end
	s    *client.Session

	mu    sync.Mutex
	cars  [][]byte // the CAR frame bodies, in order
	links []byte   // the LINK records' values, in order
	err   error    // what ended Recv
	ended chan struct{}

	// The up pump's state. upMu is held by the pump across each read, and by hold and releaseOne.
	upMu     sync.Mutex
	holding  bool     // hold was called: the pump keeps each record it reads instead of forwarding it
	held     [][]byte // the records kept, oldest first: one sealed frame each
	upFailed bool     // a forwarding write failed: nothing more is forwarded

	forwarded      atomic.Int64 // the records the up pump forwarded, released ones included
	ticksDelivered atomic.Int64 // the TICK frames down delivered to the inner session
	presences      atomic.Int64 // the PRESENCEs startPresence sent
	// forwarded, and the rig's probe renewals, when the sign-in returned: from there on each record forwarded is one
	// frame, and each frame the core dispatches renews the window's lease once.
	fwdBase, renewBase int64

	liveStop, liveDone chan struct{}
	presStop, presDone chan struct{}
	closeOnce          sync.Once
}

// trySignIn starts the relay pumps and runs client.Handshake over the pipe with the rig's phone key, with no inner ARM
// params (sent as {}): the presence lease is not enforced.
func (a *agwConn) trySignIn(r *rig) (*phone, error) { return a.trySignInWith(r, nil) }

// trySignInWith is trySignIn with the inner ARM params: {"presence": 1} opts the session into the presence lease.
func (a *agwConn) trySignInWith(r *rig, params any) (*phone, error) {
	in, pump := net.Pipe()
	p := &phone{t: a.t, a: a, in: in, pump: pump, ended: make(chan struct{})}
	go p.up()
	go p.down()
	a.t.Cleanup(p.close)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	s, err := client.Handshake(ctx, in, summon.InnerSpec().ID, params, &r.cfg.Key.PublicKey, testVIN, r.dev)
	if err != nil {
		return nil, err
	}
	p.s = s
	// The sign-in's two records (the HELLO and the ARM) went through the handshake, not the core's dispatch, and the
	// window renewed its lease at its start, before READY. The phone sends nothing more until Handshake returns.
	// fwdBase is read under upMu, the pump's lock. The ARM's Write returned before the helper could send READY, so the
	// pump's Read had taken the ARM in, under upMu, which it holds until it has counted the ARM: once the lock is ours,
	// the ARM's count is in.
	p.upMu.Lock()
	p.fwdBase = p.forwarded.Load()
	p.upMu.Unlock()
	if r.probe != nil {
		p.renewBase = r.probe.renewals.Load()
	}
	go p.recv()
	return p, nil
}

func (a *agwConn) signIn(r *rig) *phone { return a.signInWith(r, nil) }

// signInWith is signIn with the inner ARM params.
func (a *agwConn) signInWith(r *rig, params any) *phone {
	a.t.Helper()
	p, err := a.trySignInWith(r, params)
	if err != nil {
		a.t.Fatalf("sign-in: %v\n%s", err, r.logs)
	}
	return p
}

// upPoll bounds each of the up pump's reads, so hold and releaseOne get the pump's lock between two records.
const upPoll = 5 * time.Millisecond

// up carries what the inner session writes as INNER records, one record per sealed frame: the client writes a frame
// with one Write (wire.WriteFrame), and a read of the pipe takes in at most one Write. When the inner session closes
// its end, agw closes the relay connection. A failed write (the helper closed the connection) stops the forwarding,
// but the pipe stays open, drained: down closes it once the helper's last records, the BYE among them, are delivered.
// Closing it here would race that delivery, and the phone would see EOF in place of the BYE.
//
// The pump reads under upMu, upPoll at a time, and forwards or keeps what it read before it lets the lock go. So a
// record whose Write returned before hold was called has been forwarded.
func (p *phone) up() {
	buf := make([]byte, relay.MaxInner)
	for {
		p.upMu.Lock()
		p.pump.SetReadDeadline(time.Now().Add(upPoll))
		n, err := p.pump.Read(buf)
		if n > 0 {
			if p.holding {
				p.held = append(p.held, slices.Clone(buf[:n]))
			} else {
				p.forwardLocked(buf[:n])
			}
		}
		p.upMu.Unlock()
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			p.a.uc.Close()
			return
		}
	}
}

// forwardLocked writes one record to the helper and counts it. After a failed write it writes nothing more. The
// caller holds upMu.
func (p *phone) forwardLocked(rec []byte) error {
	if p.upFailed {
		return errors.New("an earlier forwarding write failed")
	}
	if err := p.a.wr.Inner(rec); err != nil {
		p.upFailed = true
		return err
	}
	p.forwarded.Add(1)
	return nil
}

// hold makes the up pump keep every record it reads from now on, as agw can: it can neither read nor forge the phone's
// sealed frames, but it can hold them back and release them later, in their order (releaseOne). The client's own
// HEARTBEATs are kept too. The down direction keeps flowing.
func (p *phone) hold() {
	p.upMu.Lock()
	p.holding = true
	p.upMu.Unlock()
}

// releaseOne forwards the oldest record held back.
func (p *phone) releaseOne() {
	p.t.Helper()
	p.upMu.Lock()
	defer p.upMu.Unlock()
	if len(p.held) == 0 {
		p.t.Fatal("no record is held back to release")
	}
	rec := p.held[0]
	p.held = p.held[1:]
	if err := p.forwardLocked(rec); err != nil {
		p.t.Fatalf("releasing a held record: %v", err)
	}
}

// tickSealedLen is a sealed TICK's length: its type, its body (the id and the issue time, 8 bytes each) and the AEAD
// tag. No other frame the helper sends has it: the challenge is 185 bytes, READY 35, a BYE at most 25, and a CAR frame
// 18 plus the car's JSON, which is never as short as 15 bytes.
const tickSealedLen = 1 + 16 + 16

// down carries INNER records to the inner session and records the LINK values. When the helper closes the relay
// connection, the pipe closes. It also reads the frame lengths in what it delivers, as agw can, and counts the TICKs
// (ticksDelivered). The pipe has no buffer, so the inner session has read every record down delivered.
func (p *phone) down() {
	var part []byte // the start of a frame not yet whole
	for {
		typ, payload, err := p.a.rd.Next()
		if err != nil {
			p.pump.Close()
			return
		}
		switch typ {
		case relay.RecInner:
			if _, err := p.pump.Write(payload); err != nil {
				return
			}
			part = p.countTicks(append(part, payload...))
		case relay.RecLink:
			p.mu.Lock()
			p.links = append(p.links, payload[0])
			p.mu.Unlock()
		}
	}
}

// countTicks counts the TICKs among the whole frames stream starts with, and returns the rest.
func (p *phone) countTicks(stream []byte) []byte {
	for len(stream) >= 4 {
		n := int(binary.BigEndian.Uint32(stream))
		if len(stream)-4 < n {
			break
		}
		if n == tickSealedLen {
			p.ticksDelivered.Add(1)
		}
		stream = stream[4+n:]
	}
	return stream
}

// waitLink waits until the latest LINK record agw got carries v.
func (p *phone) waitLink(v byte) {
	p.t.Helper()
	deadline := time.Now().Add(wait)
	for {
		p.mu.Lock()
		links := slices.Clone(p.links)
		p.mu.Unlock()
		if len(links) > 0 && links[len(links)-1] == v {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("no LINK %d within %v; agw got LINK %v", v, wait, links)
		}
		time.Sleep(time.Millisecond)
	}
}

// recv is the phone's Recv loop: it takes the TICKs in and records the CAR frames, until the session ends.
func (p *phone) recv() {
	defer close(p.ended)
	for {
		typ, body, err := p.s.Recv()
		if err != nil {
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
			return
		}
		if typ == summon.TypeCar {
			p.mu.Lock()
			p.cars = append(p.cars, slices.Clone(body))
			p.mu.Unlock()
		}
	}
}

func (p *phone) close() {
	p.closeOnce.Do(func() {
		p.stopLive()
		p.stopPresence()
		if p.s != nil {
			p.s.Close()
		}
		p.in.Close()
		p.pump.Close()
		p.a.uc.Close()
	})
}

// carTypes is the msg_type of each CAR frame received, in order.
func (p *phone) carTypes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, b := range p.cars {
		out = append(out, msgTypeOf(b[1:]))
	}
	return out
}

// carChannels is the channel byte of each CAR frame received, in order.
func (p *phone) carChannels() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []byte
	for _, b := range p.cars {
		out = append(out, b[0])
	}
	return out
}

func (p *phone) waitCar(msgType string) {
	p.t.Helper()
	deadline := time.Now().Add(wait)
	for !slices.Contains(p.carTypes(), msgType) {
		if time.Now().After(deadline) {
			p.t.Fatalf("no CAR frame %q within %v; got %q", msgType, wait, p.carTypes())
		}
		time.Sleep(time.Millisecond)
	}
}

// waitEnd waits up to d for the session's end, and returns the error that ended it.
func (p *phone) waitEnd(d time.Duration) error {
	p.t.Helper()
	select {
	case <-p.ended:
	case <-time.After(d):
		p.t.Fatalf("the session did not end within %v", d)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// isEnded reports whether the session has ended.
func (p *phone) isEnded() bool {
	select {
	case <-p.ended:
		return true
	default:
		return false
	}
}

// endErr is the error that ended the session, or nil while it is up.
func (p *phone) endErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// startLive sends a LIVE with the newest tick every 20 ms while that tick is fresh, as the phone does while a control
// is held, until stopLive or the session's end.
func (p *phone) startLive() {
	p.liveStop, p.liveDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.liveDone)
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-p.liveStop:
				return
			case <-p.ended:
				return
			case <-t.C:
			}
			if id, age, ok := p.s.NewestTick(); ok && age < client.TickMaxAge {
				if p.s.Send(summon.TypeLive, id[:]) != nil {
					return
				}
			}
		}
	}()
}

func (p *phone) stopLive() {
	if p.liveStop == nil {
		return
	}
	select {
	case <-p.liveStop:
	default:
		close(p.liveStop)
	}
	<-p.liveDone
}

// startPresence sends a PRESENCE with the newest tick every `every` (real time), as the phone does about once a second
// whether a control is held or not, until stopPresence or the session's end. It echoes the newest tick however old it
// is: the helper judges its age. presences counts the PRESENCEs sent, and each one reads the newest tick after the one
// before it was sent.
func (p *phone) startPresence(every time.Duration) {
	p.presStop, p.presDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.presDone)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-p.presStop:
				return
			case <-p.ended:
				return
			case <-t.C:
			}
			if id, _, ok := p.s.NewestTick(); ok {
				if p.s.Send(summon.TypePresence, id[:]) != nil {
					return
				}
				p.presences.Add(1)
			}
		}
	}()
}

func (p *phone) stopPresence() {
	if p.presStop == nil {
		return
	}
	select {
	case <-p.presStop:
	default:
		close(p.presStop)
	}
	<-p.presDone
}

// waitPresences waits until startPresence sent n more PRESENCEs. From the second of them on, each read the newest tick
// after the call.
func (p *phone) waitPresences(n int64) {
	p.t.Helper()
	from := p.presences.Load()
	eventually(p.t, fmt.Sprintf("%d PRESENCEs sent", n), func() bool {
		return p.presences.Load() >= from+n || p.isEnded()
	})
	if p.isEnded() {
		p.t.Fatalf("the session ended while the phone sent PRESENCE: %v", p.endErr())
	}
}

// verb sends cmd_forward from about 11 m north of the car, on a fresh tick: the car's answer to H1 gave the engine
// its position, so it is forwarded and verbActive is set.
func (p *phone) verb(press uint32) {
	p.t.Helper()
	id := p.freshTick()
	js := fmt.Sprintf(`{"msg_type":%q,"latitude":%v,"longitude":%v,"client_version":2,"created:timestamp":%d}`,
		typeForward, phoneLat, carLon, time.Now().UnixMilli())
	body := make([]byte, 12, 12+len(js))
	copy(body, id[:])
	binary.BigEndian.PutUint32(body[8:], press)
	body = append(body, js...)
	if err := p.s.Send(summon.TypeVerb, body); err != nil {
		p.t.Fatalf("sending the verb: %v", err)
	}
}

func (p *phone) freshTick() [8]byte {
	p.t.Helper()
	deadline := time.Now().Add(wait)
	for {
		if id, age, ok := p.s.NewestTick(); ok && age < 50*time.Millisecond {
			return id
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("no fresh TICK within %v", wait)
		}
		time.Sleep(time.Millisecond)
	}
}

// holdVerb signs in on a fresh relay connection and holds a forwarded verb: verbActive, with LIVEs keeping D2 quiet.
// The session does not opt into the presence lease, so it has no live file (ruling F1).
func (r *rig) holdVerb() (*agwConn, *phone) {
	r.t.Helper()
	return r.holdVerbWith(nil)
}

// holdVerbWith is holdVerb with the inner ARM params. With presenceOn the session holds the live file, and the LIVEs,
// 20 ms apart on fresh ticks, stamp the presence lease, which the rig's real clock runs.
func (r *rig) holdVerbWith(params any) (*agwConn, *phone) {
	r.t.Helper()
	a := r.dial()
	if err := a.first(); err != nil {
		r.t.Fatalf("no OWNED: %v\n%s", err, r.logs)
	}
	p := a.signInWith(r, params)
	p.waitCar(typeLocation)
	p.startLive()
	p.verb(1)
	r.waitCarGot(typeForward, 1)
	return a, p
}

// ---- Car wrappers (Config.WrapCar, R3) ----

// slowDisconnect delays H4's disconnect, which runs under the engine lock, before it reaches the Link.
type slowDisconnect struct {
	summon.Car
	delay time.Duration
}

func (s slowDisconnect) Disconnect() error {
	time.Sleep(s.delay)
	return s.Car.Disconnect()
}

// wedge blocks the first Send after it is armed until unwedge, without reaching the Link: the engine lock stays held,
// as with a wedged engine, and no emit overruns.
type wedge struct {
	summon.Car
	armed   atomic.Bool
	release chan struct{}
	once    sync.Once
}

func (w *wedge) Send(data []byte) error {
	if w.armed.CompareAndSwap(true, false) {
		<-w.release
	}
	return w.Car.Send(data)
}

func (w *wedge) unwedge() { w.once.Do(func() { close(w.release) }) }

// loseInConnect loses the name inside H1's Connect, once armed: after Open installed the session and after Start's
// Marker.Set, then waits gap (OnUnhealthy's End runs meanwhile) before the real Connect.
type loseInConnect struct {
	summon.Car
	armed atomic.Bool
	fired atomic.Bool
	lose  func()
	gap   time.Duration
}

func (l *loseInConnect) Connect() error {
	if l.armed.CompareAndSwap(true, false) {
		l.fired.Store(true)
		l.lose()
		time.Sleep(l.gap)
	}
	return l.Car.Connect()
}

// preArmClock runs hook once, once armed, inside the first BootMS read made from within summon.(*Capability).PreArm.
// PreArm's predicate is Health: link.Healthy() && c.Progress() <= MaxProgress, and Progress reads the clock only after
// link.Healthy() returned true. So the hook runs after PreArm's link check passed, and before PreArm returns.
type preArmClock struct {
	clock.Clock
	armed atomic.Bool
	fired atomic.Bool
	hook  func()
}

func (c *preArmClock) BootMS() uint64 {
	if c.armed.Load() && calledFrom("summon.(*Capability).PreArm") && c.armed.CompareAndSwap(true, false) {
		c.fired.Store(true)
		c.hook()
	}
	return c.Clock.BootMS()
}

// calledFrom reports whether fn (a function name suffix) is on the caller's stack, inlined frames included.
func calledFrom(fn string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, fn) {
			return true
		}
		if !more {
			return false
		}
	}
}

// probeClock is a clock.Fake that counts four kinds of read, told apart by the function that reads: the D2 loop's
// passes, the core's lease checks, the core's lease renewals and the TICKs the core issues. The core, the D2 loop and
// TICK issuance run on real tickers that read the fake clock, so a test in fake time waits on these counts after it
// moves the clock, never for a fixed time. A read is counted before the clock is read: a read counted after a test
// loaded a count reads the clock as it was at that load, or later.
type probeClock struct {
	*clock.Fake
	passes      atomic.Int64 // summon.(*Capability).tick: a D2 pass's first read, its progress stamp
	leaseChecks atomic.Int64 // server.(*window).run: the lease check, on a 100 ms ticker
	renewals    atomic.Int64 // server.(*window).renew: at the window's start, then once per frame dispatched
	ticksIssued atomic.Int64 // server.(*window).sendTick: in the order the TICKs go out
}

func (c *probeClock) BootMS() uint64 {
	var pcs [4]uintptr
	if n := runtime.Callers(2, pcs[:]); n > 0 {
		f, _ := runtime.CallersFrames(pcs[:n]).Next() // the reading function, inlined or not
		switch fn := f.Function; {
		case strings.HasSuffix(fn, "summon.(*Capability).tick"):
			c.passes.Add(1)
		case strings.HasSuffix(fn, "server.(*window).run"):
			c.leaseChecks.Add(1)
		case strings.HasSuffix(fn, "server.(*window).renew"):
			c.renewals.Add(1)
		case strings.HasSuffix(fn, "server.(*window).sendTick"):
			c.ticksIssued.Add(1)
		}
	}
	return c.Fake.BootMS()
}

// panicOnce panics in the first Send of AbortJSON after it is armed.
type panicOnce struct {
	summon.Car
	armed atomic.Bool
}

func (p *panicOnce) Send(data []byte) error {
	if string(data) == summon.AbortJSON && p.armed.CompareAndSwap(true, false) {
		panic("test: the injected abort's Send panics")
	}
	return p.Car.Send(data)
}

// ---- the tests ----

func TestServeRefusesWhenNameTaken(t *testing.T) {
	r := newRig(t)
	r.bus.TakeName(carlink.WebrtcName) // stock webrtc-comms holds it
	err := r.serve()
	if !errors.Is(err, carlink.ErrNameTaken) {
		t.Fatalf("Serve = %v, want ErrNameTaken", err)
	}
	if _, err := os.Lstat(r.cfg.Socket); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a socket exists after the refusal (Lstat: %v)", err)
	}
	if marker.Present(r.cfg.MarkerPath) {
		t.Fatal("a session marker exists after the refusal")
	}
	if em := r.bus.Emissions(); len(em) != 0 {
		t.Fatalf("emitted %v after the refusal", em)
	}
	if n := r.oracle.owner.Load(); n != 0 {
		t.Fatalf("the oracle was asked %d times though the name was never ours", n)
	}
}

// L1, live-2: Serve logs the live file's path first, then removes the file a helper that died left behind, before it
// asks for the name (R3), so the file goes whatever the name then says. Only a removal that found a file logs it, as
// evidence of an unclean end.
func TestServeRemovesStaleLiveFile(t *testing.T) {
	const removedLine = "summonhelper: removed a stale live file (an earlier session did not end cleanly)"
	for _, c := range []struct {
		name         string
		stale, taken bool // a file left at LivePath; stock webrtc-comms holds the name
	}{
		{"the name taken", true, true},
		{"the name free", true, false},
		{"no stale file", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			if c.stale {
				// What a SIGKILLed helper leaves: its pid, and no lock, which went with the process.
				if err := os.WriteFile(r.cfg.LivePath, []byte("123\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			atListen := errors.New("OnListening never ran")
			r.onListen = func() { _, atListen = os.Lstat(r.cfg.LivePath) }
			if c.taken {
				r.bus.TakeName(carlink.WebrtcName)
				if err := r.serve(); !errors.Is(err, carlink.ErrNameTaken) {
					t.Fatalf("Serve = %v, want ErrNameTaken", err)
				}
			} else {
				r.start()
				if !errors.Is(atListen, fs.ErrNotExist) {
					t.Fatalf("the live file at OnListening: Lstat %v, want it gone", atListen)
				}
			}
			if _, err := os.Lstat(r.cfg.LivePath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a live file is there after Serve's start (Lstat: %v)", err)
			}
			want := 0
			if c.stale {
				want = 1
			}
			if n := len(r.logs.with(removedLine)); n != want {
				t.Fatalf("%q logged %d times, want %d\n%s", removedLine, n, want, r.logs)
			}
			pathLine := "summonhelper: live file " + r.cfg.LivePath
			var first string
			r.logs.mu.Lock()
			if len(r.logs.lines) > 0 {
				first = r.logs.lines[0]
			}
			r.logs.mu.Unlock()
			if n := len(r.logs.with(pathLine)); n != 1 || first != pathLine {
				t.Fatalf("the live file's path logged %d times, first line %q; want once, first\n%s", n, first, r.logs)
			}
		})
	}
}

func TestOracleWarmedBeforeListening(t *testing.T) {
	r := newRig(t)
	atListen := int32(-1)
	r.onListen = func() { atListen = r.oracle.owner.Load() }
	r.start()
	if atListen < 1 {
		t.Fatalf("the oracle had %d AuthOwnerIsAuthd calls when OnListening fired, want at least 1", atListen)
	}
	if n := r.oracle.role.Load(); n != 0 {
		t.Fatalf("KeyRole was asked %d times before any sign-in", n)
	}
	r.oracle.mu.Lock()
	left := r.oracle.remaining[0]
	r.oracle.mu.Unlock()
	if left <= 2*time.Second || left > 3*time.Second {
		t.Fatalf("the warm-up's context had %v left at the call, want a 3 s context", left)
	}
}

// The brief's steps, checked from inside the warm-up (7): the name is ours (3), the car's calls are answered (5), and
// the socket does not exist yet (11). At OnListening it does.
func TestStartupOrder(t *testing.T) {
	r := newRig(t)
	var (
		ownerErr  error
		reply     map[string]any
		socketErr error
	)
	r.oracle.during = func() {
		_, ownerErr = r.bus.NameOwner(context.Background(), carlink.WebrtcName)
		reply = r.bus.CallCarserverToPeer(map[string]any{
			"uid": carlink.PhoneUID, "channel": carlink.ChannelTCP, "data": `{"msg_type":"autopark:status"}`,
		})
		_, socketErr = os.Lstat(r.cfg.Socket)
	}
	r.start()
	if ownerErr != nil {
		t.Errorf("the name was not ours during the warm-up: %v", ownerErr)
	}
	if got, ok := reply[carlink.ReplyKey]; !ok || got != int32(0) {
		t.Errorf("a car call during the warm-up got %v, want it served with reply 0", reply)
	}
	if !errors.Is(socketErr, fs.ErrNotExist) {
		t.Errorf("the socket existed during the warm-up (Lstat: %v)", socketErr)
	}
	if _, err := os.Lstat(r.cfg.Socket); err != nil {
		t.Errorf("no socket when OnListening fired: %v", err)
	}
}

// A failed warm-up is logged and not fatal; sign-ins then fail closed.
func TestOracleWarmFailureIsNotFatal(t *testing.T) {
	r := newRig(t)
	r.oracle.Fake.Err = errors.New("authd is not answering")
	r.start()
	if len(r.logs.with("authd is not answering")) == 0 {
		t.Fatalf("the warm-up's failure was not logged\n%s", r.logs)
	}
	a := r.dial()
	if err := a.first(); err != nil {
		t.Fatalf("no OWNED: %v", err)
	}
	if _, err := a.trySignIn(r); err == nil {
		t.Fatal("a sign-in succeeded with the oracle failing")
	}
	if got := r.carGot(); len(got) != 0 {
		t.Fatalf("the car received %q from a refused sign-in", got)
	}
}

func TestSignInAndSessionUp(t *testing.T) {
	r := newRig(t)
	r.start()
	a := r.dial()
	if err := a.first(); err != nil {
		t.Fatalf("no OWNED: %v", err)
	}
	// Opted into the presence lease, which the rig's real clock runs: the checks below end well inside its 10 s.
	p := a.signInWith(r, presenceOn)
	p.waitCar(typeLocation)
	// The forwarder that delivered the location starts after the live signal's on (capability.Open): the file is held.
	if b, err := os.ReadFile(r.cfg.LivePath); err != nil || string(b) != fmt.Sprintf("%d\n", os.Getpid()) {
		t.Fatalf("the live file holds %q (%v) while the session is up, want this process's pid", b, err)
	}
	if !liveLocked(t, r.cfg.LivePath) {
		t.Fatal("nothing holds the live file's lock while the session is up")
	}
	if got := r.carGot(); !slices.Equal(got, []string{kindH1}) {
		t.Fatalf("the car received %q, want data_channels_ready alone", got)
	}
	if got := p.carTypes(); !slices.Equal(got, []string{typeStyle, typeStatus, typeLocation}) {
		t.Fatalf("the phone's CAR frames are %q, want the car's style, status and location", got)
	}
	for i, ch := range p.carChannels() {
		if ch != summon.ChanTCP {
			t.Fatalf("CAR frame %d has channel byte %d, want %d (dcTcp)", i, ch, summon.ChanTCP)
		}
	}
	if !marker.Present(r.cfg.MarkerPath) {
		t.Fatal("no session marker while the session is up")
	}
}

// The core's 2 s pre-auth clock starts at the first INNER record, not at agw's dial (plan call 5).
func TestCoreClockStartsAtFirstInner(t *testing.T) {
	r := newRig(t)
	r.start()
	a := r.dial()
	if err := a.first(); err != nil {
		t.Fatalf("no OWNED: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	p := a.signIn(r)
	p.waitCar(typeLocation)
}

// Review Focus 2: agw re-arms while the previous connection's H4 is still emitting. B's OWNED must follow A's abort and
// disconnect, so the car sees abort, disconnect, then data_channels_ready. WrapCar makes H4's disconnect 100 ms slower,
// so an OWNED sent before A's teardown ended would come before it.
func TestRearmWaitsForTeardown(t *testing.T) {
	r := newRig(t)
	r.cfg.WrapCar = func(c summon.Car) summon.Car { return slowDisconnect{Car: c, delay: 100 * time.Millisecond} }
	r.start()
	a, _ := r.holdVerb()

	// A's relay connection closes, as when agw's outer window ends, and B dials at once.
	r.events.add("A closed")
	a.uc.Close()
	b := r.dial()
	err := b.first()
	r.events.add("B OWNED")
	if err != nil {
		t.Fatalf("B got no OWNED: %v\n%s", err, r.logs)
	}
	b.signIn(r)
	r.waitCarGot(kindH1, 2)

	want := []string{kindH1, typeForward, typeAbort, kindDisconnect, kindH1}
	if got := r.carGot(); !slices.Equal(got, want) {
		t.Fatalf("the car received %q, want %q: A's H1 and verb, abort, disconnect, then B's data_channels_ready",
			got, want)
	}
	disc, owned := r.events.index("emit "+kindDisconnect), r.events.index("B OWNED")
	if disc < 0 || owned < disc {
		t.Fatalf("B's OWNED came before A's disconnect was emitted\n%s", r.events)
	}
	if r.events.index("emit "+typeAbort) < r.events.index("A closed") {
		t.Fatalf("the abort was emitted before A closed: D2's trip, not H4's\n%s", r.events)
	}
}

// roleOracle is an OWNER oracle whose role a test changes while the helper serves; it counts its KeyRole calls.
type roleOracle struct {
	role  atomic.Int32
	calls atomic.Int32
}

func (o *roleOracle) KeyRole(context.Context, []byte) (int32, error) {
	o.calls.Add(1)
	return o.role.Load(), nil
}

func (o *roleOracle) AuthOwnerIsAuthd(context.Context) (bool, error) { return true, nil }

// warmRecorder is an OWNER oracle that records each AuthOwnerIsAuthd (the warm-up) in the rig's events.
type warmRecorder struct {
	roleOracle
	ev *eventLog
}

func (o *warmRecorder) AuthOwnerIsAuthd(context.Context) (bool, error) {
	o.ev.add("recheck oracle warmed")
	return true, nil
}

// Step 7 warms the re-checks' oracle too, before the relay socket exists (so before any session can open): its first
// re-check, about 10 s into a session, then finds a ready connection instead of paying for the setup inside
// authority.Check's 500 ms.
func TestRecheckOracleWarmedBeforeListening(t *testing.T) {
	r := newRig(t)
	rec := &warmRecorder{ev: r.events}
	rec.role.Store(2)
	r.cfg.RecheckOracle = rec
	r.onListen = func() { r.events.add("listening") }
	r.start()
	w, l := r.events.index("recheck oracle warmed"), r.events.index("listening")
	if w < 0 || l < w {
		t.Fatalf("the re-check oracle was not warmed before the socket listened\n%s", r.events)
	}
}

// The phone's key was never written anywhere: authd's OWNER role alone signs it in. When authd stops listing it as
// OWNER or DRIVER (removed from the car), the core's re-check, asked of RecheckOracle, revokes the session: H4 sends
// one abort, then one disconnect.
func TestRoleDropRevokesSession(t *testing.T) {
	r := newRig(t)
	recheck := &roleOracle{}
	recheck.role.Store(2)
	r.cfg.RecheckOracle = recheck
	var srv atomic.Pointer[server.Server]
	r.cfg.OnCore = srv.Store
	r.start()
	_, p := r.holdVerb()

	r.events.add("removed")
	recheck.role.Store(0)
	srv.Load().KeysChanged("test") // the re-check at once, as on authd's keys_changed, not after the 10 s period
	err := p.waitEnd(1500 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "BYE: revoked") {
		t.Fatalf("the session ended with %v, want the inner BYE revoked", err)
	}
	if recheck.calls.Load() == 0 {
		t.Fatal("the re-check did not ask RecheckOracle")
	}
	r.waitCarGot(kindDisconnect, 1)
	time.Sleep(100 * time.Millisecond) // a second abort or disconnect would reach the car by now
	if got := after(r.carGot(), typeForward); !slices.Equal(got, []string{typeAbort, kindDisconnect}) {
		t.Fatalf("after the verb the car received %q, want exactly one abort, then exactly one disconnect", got)
	}
	if r.events.index("emit "+typeAbort) < r.events.index("removed") {
		t.Fatalf("the abort was emitted before the role dropped: D2's trip, not H4's\n%s", r.events)
	}
	if _, err := os.Lstat(r.cfg.MarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the session marker survived a clean H4 (Lstat: %v)", err)
	}
}

func TestNameLostStopsServing(t *testing.T) {
	r := newRig(t)
	r.start()
	_, p := r.holdVerbWith(presenceOn) // opted in, so the session holds the live file
	if !liveLocked(t, r.cfg.LivePath) {
		t.Fatal("nothing holds the live file's lock while the session is up")
	}

	r.bus.LoseName(carlink.WebrtcName)
	// The window closes at once: OnUnhealthy ends the session. H4 then needs no round trip (the Link knows it is
	// unhealthy), so 500 ms is a wide margin.
	if err := p.waitEnd(500 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "BYE: error") {
		t.Fatalf("the session ended with %v, want the inner BYE error", err)
	}
	// The BYE follows Teardown, whose uninstall released the live file: an unclean end takes it too.
	if _, err := os.Lstat(r.cfg.LivePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the live file survived the session's end after NameLost (Lstat: %v)", err)
	}
	// H4 still emits abort (verbActive) and disconnect, but cannot confirm the disconnect: the marker stays, so the
	// supervisor's takeover sends its own.
	r.waitCarGot(kindDisconnect, 1)
	if got := after(r.carGot(), typeForward); !slices.Equal(got, []string{typeAbort, kindDisconnect}) {
		t.Fatalf("after the verb the car received %q, want abort, then disconnect", got)
	}
	if _, err := os.Lstat(r.cfg.MarkerPath); err != nil {
		t.Fatalf("the session marker is gone after NameLost (Lstat: %v): the takeover would not abort", err)
	}
	// The next relay connection gets no OWNED.
	b := r.dial()
	if err := b.first(); !errors.Is(err, io.EOF) {
		t.Fatalf("the next connection's first read = %v, want EOF without OWNED", err)
	}
	// The listener closes a refused connection before it logs why.
	eventually(t, "the refused connection logged as not ready", func() bool { return len(r.logs.with("not ready")) > 0 })
}

// ctx's end is srv.Shutdown("sigterm"), whose Teardown runs H4, then the name's release.
func TestShutdownRunsH4ThenReleases(t *testing.T) {
	r := newRig(t)
	r.bus.OnNameLost(carlink.WebrtcName, func() { r.events.add("name released") })
	r.start()
	_, p := r.holdVerbWith(presenceOn) // opted in, so the session holds the live file
	if !liveLocked(t, r.cfg.LivePath) {
		t.Fatal("nothing holds the live file's lock while the session is up")
	}

	r.events.add("sigterm")
	r.cancel()
	if err := p.waitEnd(wait); err == nil || !strings.Contains(err.Error(), "BYE: sigterm") {
		t.Fatalf("the session ended with %v, want the inner BYE sigterm", err)
	}
	select {
	case <-r.finished:
	case <-time.After(wait):
		t.Fatalf("Serve did not return within %v of its context's end", wait)
	}
	if r.err != nil {
		t.Fatalf("Serve = %v after its context ended, want nil", r.err)
	}
	if _, err := os.Lstat(r.cfg.LivePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the live file survived Serve (Lstat: %v)", err)
	}
	order := []string{"sigterm", "emit " + typeAbort, "emit " + kindDisconnect, "name released"}
	for i := 1; i < len(order); i++ {
		if a, b := r.events.index(order[i-1]), r.events.index(order[i]); a < 0 || b < a {
			t.Fatalf("want %q before %q\n%s", order[i-1], order[i], r.events)
		}
	}
	if _, err := os.Lstat(r.cfg.MarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the session marker survived a clean H4 (Lstat: %v)", err)
	}
	if _, err := os.Lstat(r.cfg.Socket); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the socket survived Serve (Lstat: %v)", err)
	}
	if _, err := r.bus.NameOwner(context.Background(), carlink.WebrtcName); err == nil {
		t.Fatal("the name is still ours after Serve returned")
	}
}

// Fatal (a panic in an engine's Stop or Tick) logs and calls Exit(4), a code of its own: an emit overrun exits 3
// (carlink.ExitEmitOverrun), so the supervisor's exit-status line tells the two apart (INT-1). The parent's takeover
// then aborts.
func TestFatalLogsAndExits4(t *testing.T) {
	r := newRig(t)
	pc := &panicOnce{}
	r.cfg.WrapCar = func(c summon.Car) summon.Car {
		pc.Car = c
		return pc
	}
	r.start()
	a := r.dial()
	if err := a.first(); err != nil {
		t.Fatalf("no OWNED: %v", err)
	}
	p := a.signIn(r)
	p.waitCar(typeLocation)
	p.verb(1)
	r.waitCarGot(typeForward, 1)
	pc.armed.Store(true) // no LIVE follows: D2 trips, and its injected abort's Send panics inside Tick
	select {
	case code := <-r.exits:
		if code != 4 || ExitFatal != 4 || ExitFatal == carlink.ExitEmitOverrun {
			t.Fatalf("Exit(%d), want Exit(4), which is not the emit overrun's %d", code, carlink.ExitEmitOverrun)
		}
	case <-time.After(wait):
		t.Fatalf("no Exit within %v of the panic\n%s", wait, r.logs)
	}
	if len(r.logs.with("panic in Tick")) == 0 {
		t.Fatalf("Fatal did not log the panic\n%s", r.logs)
	}
}

// Liveness (step 12): gated on Health; a failed liveness write is srv.Shutdown("supervisor-lost"), then Exit(1).
func TestLivenessWiring(t *testing.T) {
	t.Setenv(supervise.LivenessEnv, "3")
	var (
		gate    func() bool
		onFail  func(error)
		stopped atomic.Bool
		started = make(chan struct{})
	)
	prev := startLiveness
	startLiveness = func(healthy func() bool, fail func(error)) (func(), error) {
		gate, onFail = healthy, fail
		close(started)
		return func() { stopped.Store(true) }, nil
	}
	t.Cleanup(func() { startLiveness = prev })
	r := newRig(t)
	r.cfg.Liveness = true
	r.start()
	select {
	case <-started: // step 12 follows OnListening
	case <-time.After(wait):
		t.Fatal("Serve did not start liveness")
	}
	if gate == nil || onFail == nil {
		t.Fatal("Serve started liveness without a gate or an onFail")
	}
	if !gate() {
		t.Fatal("the liveness gate is false for a healthy helper")
	}
	_, p := r.holdVerb()

	go onFail(syscall.EPIPE) // the supervisor is gone
	if err := p.waitEnd(wait); err == nil || !strings.Contains(err.Error(), "BYE: error") {
		t.Fatalf("the session ended with %v, want the inner BYE error (supervisor-lost is no fixed reason)", err)
	}
	select {
	case code := <-r.exits:
		if code != 1 {
			t.Fatalf("Exit(%d), want Exit(1)", code)
		}
	case <-time.After(wait):
		t.Fatalf("no Exit within %v of the liveness failure", wait)
	}
	if d, e := r.events.index("emit "+kindDisconnect), r.events.index("exit 1"); d < 0 || e < d {
		t.Fatalf("Exit(1) came before Shutdown's H4\n%s", r.events)
	}
	if _, err := os.Lstat(r.cfg.MarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the session marker survived a clean H4 (Lstat: %v)", err)
	}
	select {
	case <-r.finished:
	case <-time.After(wait):
		t.Fatal("Serve kept running after its core shut down")
	}
	if r.err == nil {
		t.Fatal("Serve returned nil after its core shut down on its own")
	}
	if !stopped.Load() {
		t.Fatal("Serve did not stop liveness")
	}
	if gate() {
		t.Fatal("the liveness gate is true after Serve released the name")
	}
}

// Fix round 1, finding 3: Serve's step 1 disables dumpability before it takes the name, and a failure stops it there.
func TestServeDisablesDumpsFirst(t *testing.T) {
	t.Run("called before the name", func(t *testing.T) {
		r := newRig(t)
		var calls atomic.Int32
		prev := disableDumps
		disableDumps = func() error {
			calls.Add(1)
			r.events.add("dumps disabled")
			return nil
		}
		t.Cleanup(func() { disableDumps = prev }) // after Serve returned: the rig's stop runs first
		r.bus.OnNameAcquired(carlink.WebrtcName, func() { r.events.add("name acquired") })
		r.start()
		if n := calls.Load(); n != 1 {
			t.Fatalf("Serve disabled dumps %d times, want once", n)
		}
		if d, a := r.events.index("dumps disabled"), r.events.index("name acquired"); d < 0 || a < d {
			t.Fatalf("the name was acquired before dumps were disabled\n%s", r.events)
		}
	})
	t.Run("a failure stops Serve", func(t *testing.T) {
		r := newRig(t)
		prev := disableDumps
		disableDumps = func() error { return errors.New("prctl: operation not permitted") }
		t.Cleanup(func() { disableDumps = prev })
		var acquired atomic.Bool
		r.bus.OnNameAcquired(carlink.WebrtcName, func() { acquired.Store(true) })
		err := r.serve()
		if err == nil || !strings.Contains(err.Error(), "dumpability") {
			t.Fatalf("Serve = %v, want the dumpability error", err)
		}
		if acquired.Load() {
			t.Fatal("the name was acquired although dumps could not be disabled")
		}
	})
}

// Serve checks its Config before it touches anything; a refused VIN is never quoted.
func TestServeRefusesBadConfig(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"no bus", func(c *Config) { c.Bus = nil }, "no bus"},
		{"no oracle", func(c *Config) { c.Oracle = nil }, "no authority oracle"},
		{"no key", func(c *Config) { c.Key = nil }, "no long-term key"},
		{"a VIN with I", func(c *Config) { c.VIN = "5YJ3E1EA7KF00000I" }, "the VIN is not valid"},
		{"no socket", func(c *Config) { c.Socket = "" }, "paths are required"},
		{"no marker", func(c *Config) { c.MarkerPath = "" }, "paths are required"},
		{"no live file", func(c *Config) { c.LivePath = "" }, "paths are required"}, // R2: never a default under /run
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			var acquired atomic.Bool
			r.bus.OnNameAcquired(carlink.WebrtcName, func() { acquired.Store(true) })
			c.change(&r.cfg)
			err := r.serve()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Serve = %v, want an error containing %q", err, c.want)
			}
			if strings.Contains(err.Error(), "5YJ3E1EA7KF00000I") {
				t.Fatalf("the refusal quotes the VIN: %v", err)
			}
			if acquired.Load() {
				t.Fatal("the name was acquired before the refusal")
			}
		})
	}
}

// A zero AllowUID is agw's uid 4950, never root (INT-4), as a zero HelperUID is 1337 in summonrelay: a caller that
// leaves it unset gets a helper that serves agw, not one that serves only root. An explicit uid is kept.
func TestAllowUIDZeroIsAgw(t *testing.T) {
	r := newRig(t)
	r.cfg.AllowUID = 0
	cfg, err := r.cfg.check()
	if err != nil || cfg.AllowUID != 4950 || AgwUID != 4950 {
		t.Fatalf("check() = AllowUID %d, %v; want 4950", cfg.AllowUID, err)
	}
	r.cfg.AllowUID = 4242
	if cfg, err := r.cfg.check(); err != nil || cfg.AllowUID != 4242 {
		t.Fatalf("check() = AllowUID %d, %v; want the given 4242", cfg.AllowUID, err)
	}
	// Serve says whom it serves (a line logged after OnListening, so the test waits for it), and a connection from
	// this test's uid (not 4950) gets no OWNED.
	r.cfg.AllowUID = 0
	r.start()
	eventually(t, "Serve's serving line naming peer uid 4950", func() bool {
		return len(r.logs.with("for peer uid 4950")) == 1
	})
	if os.Getuid() == 4950 {
		return
	}
	c, err := net.Dial("unix", r.cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(wait))
	if b, err := io.ReadAll(c); len(b) != 0 || err != nil {
		t.Fatalf("a connection from uid %d read %q, %v; want a close without OWNED", os.Getuid(), b, err)
	}
}

func TestServeRefusesLivenessWithoutSupervisor(t *testing.T) {
	t.Setenv(supervise.LivenessEnv, "")
	os.Unsetenv(supervise.LivenessEnv)
	r := newRig(t)
	var acquired atomic.Bool
	r.bus.OnNameAcquired(carlink.WebrtcName, func() { acquired.Store(true) })
	r.cfg.Liveness = true
	err := r.serve()
	if err == nil || !strings.Contains(err.Error(), supervise.LivenessEnv) {
		t.Fatalf("Serve = %v, want a refusal naming %s", err, supervise.LivenessEnv)
	}
	if acquired.Load() {
		t.Fatal("the name was acquired before the refusal: Config is checked before anything is touched")
	}
	if _, err := r.bus.NameOwner(context.Background(), carlink.WebrtcName); err == nil {
		t.Fatal("the name is ours after the refusal")
	}
	if _, err := os.Lstat(r.cfg.Socket); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a socket exists after the refusal (Lstat: %v)", err)
	}
}

// Step 4: the capability's SetLink is bound late to the step 11 listener, so the link flag reaches agw as LINK records.
func TestLinkFlagReachesAgw(t *testing.T) {
	r := newRig(t)
	r.start()
	_, p := r.holdVerb()

	// Three LIVEs in a row with an unknown tick set the link flag (design §6.2). The fresh LIVEs then resume, so D2
	// stays quiet.
	p.stopLive()
	var unknown [8]byte
	for i := 0; i < 3; i++ {
		if err := p.s.Send(summon.TypeLive, unknown[:]); err != nil {
			t.Fatal(err)
		}
	}
	p.startLive()
	p.waitLink(1)
	// The phone's release ends the hold, which clears the flag.
	if err := p.s.Send(summon.TypeMsg, []byte(summon.AbortJSON)); err != nil {
		t.Fatal(err)
	}
	p.waitLink(0)
}

// PreArm checks Health: a phone that got OWNED while the helper was healthy cannot open a session once the name is
// lost, and the car gets no H1.
func TestNoSessionAfterNameLost(t *testing.T) {
	r := newRig(t)
	r.start()
	a := r.dial()
	if err := a.first(); err != nil {
		t.Fatalf("no OWNED: %v", err)
	}
	r.bus.LoseName(carlink.WebrtcName)
	if _, err := a.trySignIn(r); err == nil {
		t.Fatal("a session opened after NameLost")
	}
	if em := r.bus.Emissions(); len(em) != 0 {
		t.Fatalf("emitted %v after NameLost", em)
	}
	if marker.Present(r.cfg.MarkerPath) {
		t.Fatal("a session marker exists: a session started after NameLost")
	}
	// PreArm refused it, so Open never ran. The healthy marker would refuse it one step later, and log it.
	if lines := r.logs.with("did not start"); len(lines) != 0 {
		t.Fatalf("PreArm let the session through to Open, refused only at the marker: %q", lines)
	}
}

// Fix round 1, finding 1: a NameLost after PreArm's health check and before Open installs the session. OnUnhealthy
// fires once ever, and its End finds no session yet, so only Start can still refuse: the marker's Set checks the link.
// With or without a gap (in which OnUnhealthy's End runs), the sign-in is refused: no marker, nothing to the car.
func TestNameLostBetweenPreArmAndOpen(t *testing.T) {
	for _, gap := range []time.Duration{0, 20 * time.Millisecond} {
		t.Run(fmt.Sprintf("gap %v", gap), func(t *testing.T) {
			r := newRig(t)
			pc := &preArmClock{Clock: clock.Boot()}
			r.cfg.Clock = pc
			pc.hook = func() {
				r.events.add("NameLost inside PreArm, after link.Healthy()")
				r.bus.LoseName(carlink.WebrtcName)
				time.Sleep(gap)
			}
			r.start()
			a := r.dial()
			if err := a.first(); err != nil {
				t.Fatalf("no OWNED: %v", err)
			}
			pc.armed.Store(true)
			p, err := a.trySignIn(r)
			if !pc.fired.Load() {
				t.Fatal("the hook never ran inside PreArm: the test did not reach the gap")
			}
			if err == nil {
				t.Errorf("a session armed after the name was lost (CAR %q)", p.carTypes())
			}
			if em := r.bus.Emissions(); len(em) != 0 {
				t.Errorf("emitted %v after the name was lost", em)
			}
			if got := r.carGot(); len(got) != 0 {
				t.Errorf("the car received %q", got)
			}
			if marker.Present(r.cfg.MarkerPath) {
				t.Error("a session marker exists: the session started")
			}
			// PreArm passed, so the refusal is Start's: the healthy marker's.
			if len(r.logs.with("session marker: summonhelper: the car link is not owned and healthy")) == 0 {
				t.Errorf("the refusal was not the marker's\n%s", r.logs)
			}
		})
	}
}

// Fix round 1, finding 1, the other side: a NameLost once the session is installed (here inside H1's Connect, after
// the marker's check). End finds the session current, so it is refused or closed with H4: never left open, and the
// car is never left connected.
func TestNameLostDuringStart(t *testing.T) {
	for _, gap := range []time.Duration{0, 20 * time.Millisecond} {
		t.Run(fmt.Sprintf("gap %v", gap), func(t *testing.T) {
			r := newRig(t)
			lc := &loseInConnect{gap: gap}
			r.cfg.WrapCar = func(c summon.Car) summon.Car {
				lc.Car = c
				return lc
			}
			lc.lose = func() { r.bus.LoseName(carlink.WebrtcName) }
			r.start()
			a := r.dial()
			if err := a.first(); err != nil {
				t.Fatalf("no OWNED: %v", err)
			}
			lc.armed.Store(true)
			p, err := a.trySignIn(r)
			if !lc.fired.Load() {
				t.Fatal("the name was not lost inside Connect: the test did not reach the gap")
			}
			if err == nil {
				if e := p.waitEnd(500 * time.Millisecond); e == nil || !strings.Contains(e.Error(), "BYE: error") {
					t.Fatalf("the session ended with %v, want the inner BYE error", e)
				}
			}
			// H1 went out inside Connect, so H4's disconnect must follow it.
			r.waitCarGot(kindDisconnect, 1)
			eventually(t, "the car no longer connected", func() bool { return !r.car.Connected() })
			if got := r.carGot(); !slices.Equal(got, []string{kindH1, kindDisconnect}) {
				t.Fatalf("the car received %q, want data_channels_ready, then the disconnect", got)
			}
			// The disconnect could not be confirmed on a lost name: the marker stays for the takeover.
			if !marker.Present(r.cfg.MarkerPath) {
				t.Fatal("the marker is gone after an unconfirmed disconnect")
			}
		})
	}
}

// The deadman chain's first link, in-process: a wedged engine (a Send stuck under its lock) stops the D2 loop's
// stamps, and the liveness gate turns false after MaxProgress, so the supervisor kills the helper and aborts.
func TestWedgedEngineFailsLiveness(t *testing.T) {
	t.Setenv(supervise.LivenessEnv, "3")
	var gate func() bool
	started := make(chan struct{})
	prev := startLiveness
	startLiveness = func(healthy func() bool, _ func(error)) (func(), error) {
		gate = healthy
		close(started)
		return func() {}, nil
	}
	t.Cleanup(func() { startLiveness = prev })
	r := newRig(t)
	r.cfg.Liveness = true
	w := &wedge{release: make(chan struct{})}
	r.cfg.WrapCar = func(c summon.Car) summon.Car {
		w.Car = c
		return w
	}
	r.start()
	select {
	case <-started: // step 12 follows OnListening
	case <-time.After(wait):
		t.Fatal("Serve did not start liveness")
	}
	_, p := r.holdVerb()
	defer w.unwedge() // before the cleanups: H4 needs the engine lock
	if !gate() {
		t.Fatal("the liveness gate is false before the wedge")
	}

	w.armed.Store(true) // the next Send blocks under the engine lock
	p.stopLive()        // D2 trips 300 ms later, and its abort's Send wedges the D2 loop inside Tick
	start := time.Now()
	eventually(t, "the liveness gate false with the engine wedged", func() bool { return !gate() })
	if took := time.Since(start); took < summon.TripAfter {
		t.Fatalf("the gate turned false %v after the last LIVE, before D2 could trip: not for the wedge", took)
	}
	w.unwedge()
	eventually(t, "the liveness gate true again once the engine moves", gate)
}

// The carry-forward from Task 4: Serve hands a DBusBus its Logf, so the car calls the bus drops are logged.
func TestRealBusLogsDroppedCarCalls(t *testing.T) {
	bus := bustest.Start(t)
	b, err := carlink.DialBus(bus.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	r := newRig(t)
	r.cfg.Bus = b
	r.start()
	car := bus.Conn(t)
	car.Object(carlink.WebrtcName, carlink.WebrtcPath).Call(
		carlink.WebrtcIface+"."+carlink.MethodCarserverToPeerCommsData, 0, map[string]dbus.Variant{
			"uid": dbus.MakeVariant(carlink.PhoneUID), "channel": dbus.MakeVariant(carlink.ChannelTCP),
			"data": dbus.MakeVariant(int32(42)), // not a string: the bus drops the frame
		})
	eventually(t, "the dropped car call logged through Config.Logf", func() bool {
		return len(r.logs.with("dropped a car frame")) > 0
	})
}

func TestHealth(t *testing.T) {
	type hrig struct {
		bus   *carlink.FakeBus
		link  *carlink.Link
		clk   *clock.Fake
		c     *summon.Capability
		exits atomic.Int32
	}
	setup := func(t *testing.T) *hrig {
		h := &hrig{bus: carlink.NewFakeBus(), clk: clock.NewFake()}
		h.link = carlink.NewLink(h.bus, carlink.WithExit(func(code int) { h.exits.Store(int32(code)) }))
		if err := h.link.Acquire(); err != nil {
			t.Fatal(err)
		}
		h.c = summon.NewCapability(summon.CapConfig{
			Car: h.link, Marker: marker.New(filepath.Join(t.TempDir(), "session")), Clock: h.clk,
			Fatal: func(error) {},
		})
		return h
	}
	t.Run("healthy", func(t *testing.T) {
		h := setup(t)
		if !Health(h.link, h.c)() {
			t.Fatal("false with the name owned, no emit failed and a fresh progress stamp")
		}
	})
	t.Run("progress", func(t *testing.T) {
		h := setup(t)
		health := Health(h.link, h.c)
		h.clk.Advance(250)
		if !health() {
			t.Fatal("false at exactly 250 ms since the D2 loop's stamp")
		}
		h.clk.Advance(1)
		if health() {
			t.Fatal("true at 251 ms since the D2 loop's stamp")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go h.c.Loop(ctx)
		eventually(t, "Health true again once the D2 loop stamps", health)
	})
	t.Run("emit overrun", func(t *testing.T) {
		h := setup(t)
		release := make(chan struct{})
		defer close(release)
		h.bus.StallEmits(release)
		if err := h.link.Send([]byte(summon.AbortJSON)); !errors.Is(err, carlink.ErrBusDead) {
			t.Fatalf("Send over a stalled bus = %v, want ErrBusDead", err)
		}
		if Health(h.link, h.c)() {
			t.Fatal("true after an emit overran")
		}
		if code := h.exits.Load(); code != carlink.ExitEmitOverrun {
			t.Fatalf("the overrun's exit code is %d, want %d", code, carlink.ExitEmitOverrun)
		}
	})
	t.Run("name lost", func(t *testing.T) {
		h := setup(t)
		h.bus.LoseName(carlink.WebrtcName)
		if Health(h.link, h.c)() {
			t.Fatal("true after NameLost")
		}
	})
	t.Run("emit failed", func(t *testing.T) {
		h := setup(t)
		h.bus.SetEmitErr(errors.New("write: broken pipe"))
		if err := h.link.Send([]byte(summon.AbortJSON)); err == nil {
			t.Fatal("Send succeeded on a failing bus")
		}
		if Health(h.link, h.c)() {
			t.Fatal("true after an emit failed")
		}
	})
}

func TestValidVIN(t *testing.T) {
	for _, v := range []string{testVIN, "ABCDEFGHJKLMNPRST", "UVWXYZ01234567890"} {
		if !ValidVIN(v) {
			t.Errorf("ValidVIN(%q) = false, want true", v)
		}
	}
	for _, v := range []string{
		"",
		"5YJ3E1EA7KF00000",   // 16
		"5YJ3E1EA7KF0000000", // 18
		"5YJ3E1EA7KF00000I", "5YJ3E1EA7KF00000O", "5YJ3E1EA7KF00000Q",
		"5yj3e1ea7kf000000",
		"5YJ3E1EA7KF00000 ",
		"5YJ3E1EA7KF00000\n",
		"5YJ3E1EA7KF0000é", // 17 bytes, one of them not ASCII
	} {
		if ValidVIN(v) {
			t.Errorf("ValidVIN(%q) = true, want false", v)
		}
	}
}

// ---- F5r: agw holding back the phone's frames ----
//
// agw relays the phone's sealed inner frames. It can neither read nor forge one, but it can hold them back and release
// them later, in their order, and each frame the core dispatches renews the core's 20 s lease, however old the frame
// is. These tests run the real Serve on a probeClock, and play agw with the up pump's hold and releaseOne.

const (
	presenceLine = "summon: no fresh presence for " // the presence lease's end line

	// presenceEvery is the test phone's PRESENCE period, in real time. The tests step the clock a fake second at a time,
	// and let the phone echo each second's tick before the next step.
	presenceEvery = 10 * time.Millisecond

	// afterAdvance bounds an effect of moving the clock. The real tickers that read it run every 10 ms (the D2 loop) and
	// every 100 ms (the core's lease check).
	afterAdvance = 2 * time.Second
)

// presenceOn is the inner ARM params that opt the session into the presence lease.
var presenceOn = map[string]any{"presence": 1}

// useProbe makes the rig's clock a probeClock, a fake one, which Serve hands to the core and the capability alike.
func (r *rig) useProbe() *probeClock {
	r.probe = &probeClock{Fake: clock.NewFake()}
	r.cfg.Clock = r.probe
	return r.probe
}

// advance moves the clock on by ms, then waits until the phone holds a TICK issued at the new time. The first TICK the
// core issues after the call reads the new time, and once down has delivered the TICK that follows it, the phone has
// taken it in.
func (r *rig) advance(p *phone, ms uint64) {
	r.t.Helper()
	r.probe.Advance(ms)
	first := r.probe.ticksIssued.Load() + 1
	eventually(r.t, "the phone holding a TICK issued after the clock moved", func() bool {
		return p.ticksDelivered.Load() > first || p.isEnded()
	})
	if p.isEnded() {
		r.t.Fatalf("the session ended: %v\n%s", p.endErr(), r.logs)
	}
	// A TICK is counted as issued before it is queued, so down can never have delivered more. If it has, the probe no
	// longer sees sendTick, or down counts other frames, and the wait above proved nothing.
	if issued, got := r.probe.ticksIssued.Load(), p.ticksDelivered.Load(); got > issued {
		r.t.Fatalf("down delivered %d TICKs, but the probe counted %d issued", got, issued)
	}
}

// waitDispatched waits until the core has dispatched every frame the up pump forwarded since the sign-in: each one
// renewed the window's lease once.
func (r *rig) waitDispatched(p *phone) {
	r.t.Helper()
	eventually(r.t, "the core dispatching every frame forwarded", func() bool {
		return r.probe.renewals.Load()-p.renewBase >= p.forwarded.Load()-p.fwdBase
	})
}

// settle waits until two D2 passes and two of the core's lease checks have started since the call, or the session has
// ended. The first of each read the clock as it is now, and had finished when the second started. So whatever the
// clock as it is makes them do, the presence lease's close or the core lease's, has been done.
func (r *rig) settle(p *phone) {
	r.t.Helper()
	passes, checks := r.probe.passes.Load(), r.probe.leaseChecks.Load()
	eventually(r.t, "two D2 passes and two lease checks on the clock as it is", func() bool {
		return p.isEnded() || (r.probe.passes.Load() >= passes+2 && r.probe.leaseChecks.Load() >= checks+2)
	})
}

// assertOpen settles the clock's checks, then fails the test unless the session is up: not ended and no presence
// lease line. With live, the session opted into the presence lease, and the live file must be locked; without, it did
// not, and there must be no live file at all (ruling F1).
func (r *rig) assertOpen(p *phone, at string, live bool) {
	r.t.Helper()
	r.settle(p)
	if p.isEnded() {
		r.t.Fatalf("%s: the session ended: %v\n%s", at, p.endErr(), r.logs)
	}
	if lines := r.logs.with(presenceLine); len(lines) > 0 {
		r.t.Fatalf("%s: the presence lease ended the session: %q", at, lines)
	}
	if !live {
		if _, err := os.Lstat(r.cfg.LivePath); !errors.Is(err, fs.ErrNotExist) {
			r.t.Fatalf("%s: a session that did not opt in has a live file (Lstat: %v)", at, err)
		}
		return
	}
	if !liveLocked(r.t, r.cfg.LivePath) {
		r.t.Fatalf("%s: nothing holds the live file's lock while the session is up", at)
	}
}

// assertLeaseEnd waits up to afterAdvance for the session's end. It fails the test unless the phone got the inner BYE
// lease, the car got H1 and then H4's disconnect and nothing else, and the live file is gone.
func (r *rig) assertLeaseEnd(p *phone, at string) {
	r.t.Helper()
	select {
	case <-p.ended:
	case <-time.After(afterAdvance):
		r.t.Fatalf("%s: the session did not end within %v\n%s", at, afterAdvance, r.logs)
	}
	if err := p.endErr(); err == nil || !strings.Contains(err.Error(), "BYE: lease") {
		r.t.Fatalf("%s: the session ended with %v, want the inner BYE lease\n%s", at, err, r.logs)
	}
	r.waitCarGot(kindDisconnect, 1)
	if got := r.carGot(); !slices.Equal(got, []string{kindH1, kindDisconnect}) {
		r.t.Fatalf("the car received %q, want data_channels_ready, then H4's disconnect", got)
	}
	// The BYE follows Teardown, whose uninstall released the live file of a session that opted in; one that did not
	// never had one.
	if _, err := os.Lstat(r.cfg.LivePath); !errors.Is(err, fs.ErrNotExist) {
		r.t.Fatalf("the live file survived the session's end (Lstat: %v)", err)
	}
}

// startHeldBack signs a phone in with params, then runs the attack's first two phases. First the phone sends PRESENCE
// every fake second for 5 s, and every record is forwarded. Then agw holds every record while the phone keeps sending
// for 10 more fake seconds, and the phone stops. startHeldBack returns with the clock 10000 ms after the last echo the
// helper received fresh: the tick the last forwarded PRESENCEs echoed was issued at the end of the first phase.
//
// The phone pauses across the switch to holding, so every PRESENCE agw holds echoes a tick issued after the last fresh
// echo. A release that stamped one (an echo over 3 s old) would therefore stretch the session.
func startHeldBack(t *testing.T, params any) (*rig, *phone) {
	t.Helper()
	r := newRig(t)
	r.useProbe()
	r.start()
	a := r.dial()
	if err := a.first(); err != nil {
		t.Fatalf("no OWNED: %v", err)
	}
	p := a.signInWith(r, params)
	p.waitCar(typeLocation)

	// Forwarded: each fake second, the phone echoes a tick issued in that second.
	p.startPresence(presenceEvery)
	for i := 0; i < 5; i++ {
		r.advance(p, 1000)
		p.waitPresences(2)
	}
	p.stopPresence()
	// A PRESENCE reads the clock twice, in TickAge and then in stamp, and stamps the second reading less the age from the
	// first. One handled across the next step would therefore stamp its tick's issue time plus the step. So the last
	// record forwarded is a HEARTBEAT, which the phone sends anyway. The core handles frames one at a time, and a
	// HEARTBEAT has no handler: it only renews the lease. Once waitDispatched counts that renewal, every PRESENCE before
	// it has been handled whole, with the clock at fresh. (The HEARTBEAT's own renewal may read the moved clock, which
	// can only put the core's deadline up to a second later.)
	if err := p.s.Send(proto.TypeHeartbeat, nil); err != nil {
		t.Fatalf("the phone's HEARTBEAT: %v", err)
	}
	p.hold()
	r.waitDispatched(p)

	// Held: the phone goes on for 10 fake seconds, then leaves.
	for i := 0; i < 10; i++ {
		r.advance(p, 1000)
		if i == 0 {
			p.startPresence(presenceEvery)
		}
		p.waitPresences(2)
	}
	p.stopPresence()
	return r, p
}

// The control for today's bound: an opted-out phone (params {}) whose agw forwards nothing after the sign-in. Nothing
// renews the core's 20 s lease, and that lease alone ends the session: up at 19999 ms, ended with BYE lease at 20001 ms.
// The car is disconnected. The presence lease plays no part, and the session never had a live file (ruling F1).
func TestSilentAgwEndsSessionAtLease(t *testing.T) {
	r := newRig(t)
	clk := r.useProbe()
	r.start()
	a := r.dial()
	if err := a.first(); err != nil {
		t.Fatalf("no OWNED: %v", err)
	}
	p := a.signIn(r)
	p.hold() // agw forwards nothing more, the client's own HEARTBEATs included
	p.waitCar(typeLocation)
	r.waitDispatched(p) // whatever was forwarded renewed the lease before the clock moves

	clk.Advance(19999)
	r.assertOpen(p, "19999 ms after the sign-in", false)
	clk.Advance(2)
	r.assertLeaseEnd(p, "20001 ms after the sign-in")
	if lines := r.logs.with(presenceLine); len(lines) != 0 {
		t.Fatalf("the presence lease ended an opted-out session: %q", lines)
	}
}

// F5r: agw holds back an opted-in phone's frames, then releases them late, one every 15 fake seconds once the phone has
// left. A release renews the core's 20 s lease, but the tick it carries is at least 9 s old, so it stamps no presence.
// The session ends with BYE lease 10001 ms after the last echo the helper received fresh, which is 1 ms after the first
// release. The car is disconnected, the live file is removed, and the presence line is logged once.
func TestHeldBackFramesCannotStretchSession(t *testing.T) {
	r, p := startHeldBack(t, presenceOn)
	r.assertOpen(p, "10000 ms after the last fresh echo", true)
	p.releaseOne() // as the phone leaves; the next release would be due 15 s later
	r.waitDispatched(p)
	r.assertOpen(p, "10000 ms after the last fresh echo, one held frame released", true)
	r.probe.Advance(1)
	r.assertLeaseEnd(p, "10001 ms after the last fresh echo")
	want := "summon: no fresh presence for 10001 ms: ending the session (lease)"
	if lines := r.logs.with(presenceLine); !slices.Equal(lines, []string{want}) {
		t.Fatalf("the presence lease's lines are %q, want %q once", lines, want)
	}
}

// F5r's control: the same schedule with an opted-out phone. Each held frame agw releases renews the core's 20 s lease,
// so 4 releases, one every 15 fake seconds, keep the session up 60 s after the phone left: the bound the opt-in
// removes. Without the latest release, each check would find the core's lease expired. The stretched session has no
// live file at any point (ruling F1), so it never reaches the camera's SUMMON mode.
func TestHeldBackFramesStretchAnOptedOutSession(t *testing.T) {
	r, p := startHeldBack(t, nil)
	r.assertOpen(p, "10000 ms after the last fresh echo", false)
	for i := 1; i <= 4; i++ {
		p.releaseOne()
		r.waitDispatched(p)
		r.settle(p) // the renewal stores its deadline right after its counted read: well before the clock moves on
		r.probe.Advance(15000)
		r.assertOpen(p, fmt.Sprintf("%d s after the phone left, %d held frames released", 15*i, i), false)
	}
}
