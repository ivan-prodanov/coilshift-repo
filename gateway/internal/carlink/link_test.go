package carlink

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/internal/carlink/bustest"
)

// noExit fails the test if the Link calls its exit hook (the default would end the test binary).
func noExit(t *testing.T) LinkOption {
	return WithExit(func(code int) { t.Errorf("unexpected exit(%d)", code) })
}

// ownedLink is a Link on a FakeBus that owns the name.
func ownedLink(t *testing.T, opts ...LinkOption) (*FakeBus, *Link) {
	t.Helper()
	fb := NewFakeBus()
	l := NewLink(fb, append([]LinkOption{noExit(t)}, opts...)...)
	if err := l.Acquire(); err != nil {
		t.Fatal(err)
	}
	if !l.Owned() || !l.Healthy() {
		t.Fatalf("after Acquire: Owned %v, Healthy %v", l.Owned(), l.Healthy())
	}
	return fb, l
}

// counter counts calls and signals each one.
type counter struct {
	n  atomic.Int32
	ch chan struct{}
}

func newCounter() *counter { return &counter{ch: make(chan struct{}, 16)} }

func (c *counter) inc() { c.n.Add(1); c.ch <- struct{}{} }

func (c *counter) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.ch:
	case <-time.After(time.Second):
		t.Fatal("callback not called within 1 s")
	}
}

func TestLinkEmitBodies(t *testing.T) {
	fb, l := ownedLink(t)
	data := `{"msg_type":"autopark:cmd_abort"}`
	if err := l.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := l.Send([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := l.Disconnect(); err != nil {
		t.Fatal(err)
	}
	want := []Emission{
		{Member: "LocalToRemoteSignaling", Body: map[string]any{"data_channels_ready": true}},
		{Member: "PeerCommsToCarserverData", Body: map[string]any{"uid": "test_phone", "data": data}},
		{Member: "LocalToRemoteSignaling", Body: map[string]any{
			"ice_state_change": true, "msg_type": "autopark:set_webrtc", "set_webrtc_value": false,
		}},
	}
	if got := fb.Emissions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("emissions\n got %#v\nwant %#v", got, want)
	}
	if !l.Healthy() {
		t.Fatal("a confirmed disconnect left the link unhealthy")
	}
}

func TestLinkAcquireTaken(t *testing.T) {
	fb := NewFakeBus()
	l := NewLink(fb, noExit(t))
	fb.TakeName(WebrtcName)
	if err := l.Acquire(); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("Acquire = %v, want ErrNameTaken", err)
	}
	if l.Owned() || l.Healthy() {
		t.Fatalf("Owned %v, Healthy %v after a refused Acquire", l.Owned(), l.Healthy())
	}
	// The finish hook retries: once the other holder is gone, the same Link gets the name.
	fb.FreeName(WebrtcName)
	if err := l.Acquire(); err != nil {
		t.Fatal(err)
	}
	if !l.Owned() || !l.Healthy() {
		t.Fatalf("Owned %v, Healthy %v after the retry", l.Owned(), l.Healthy())
	}
}

func TestLinkHealthyFalseAfterNameLost(t *testing.T) {
	fb, l := ownedLink(t)
	fb.LoseName(WebrtcName)
	if l.Owned() || l.Healthy() {
		t.Fatalf("Owned %v, Healthy %v after NameLost", l.Owned(), l.Healthy())
	}
}

func TestLinkFailedEmitLatchesUnhealthy(t *testing.T) {
	fb, l := ownedLink(t)
	boom := errors.New("boom")
	fb.SetEmitErr(boom)
	if err := l.Send([]byte(`{}`)); !errors.Is(err, boom) {
		t.Fatalf("Send = %v, want %v", err, boom)
	}
	if l.Healthy() {
		t.Fatal("healthy after a failed emit")
	}
	fb.SetEmitErr(nil)
	if err := l.Send([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if l.Healthy() || !l.Owned() {
		t.Fatalf("Healthy %v, Owned %v: the latch must hold while the name stays owned", l.Healthy(), l.Owned())
	}
}

func TestLinkDisconnectConfirms(t *testing.T) {
	fb, l := ownedLink(t)
	if err := l.Disconnect(); err != nil {
		t.Fatalf("Disconnect while owned = %v", err)
	}
	fb.LoseName(WebrtcName)
	n := len(fb.Emissions())
	if err := l.Disconnect(); err == nil {
		t.Fatal("Disconnect after NameLost confirmed")
	}
	if got := len(fb.Emissions()); got != n+1 {
		t.Fatalf("%d emissions after the unconfirmed Disconnect, want %d (the emit itself succeeded)", got, n+1)
	}
}

// TestLinkDisconnectNeedsOwnerFirst: a disconnect emitted before the Link owned the name left as a non-owner's
// signal, which the car ignores. Getting the name before the round trip must not confirm it.
func TestLinkDisconnectNeedsOwnerFirst(t *testing.T) {
	fb := NewFakeBus()
	l := NewLink(fb, noExit(t))
	fb.OnEmit(func(member string, body map[string]any) {
		if body["ice_state_change"] == true {
			fb.RequestName(WebrtcName, false) // the name arrives after the signal left
		}
	})
	if err := l.Disconnect(); err == nil {
		t.Fatal("a disconnect emitted before the name was owned was confirmed")
	}
	if !l.Owned() {
		t.Fatal("the name did not arrive")
	}
}

// ownerBus is a FakeBus whose NameOwner the test controls.
type ownerBus struct {
	*FakeBus
	owner func(ctx context.Context) (string, error)
}

func (b ownerBus) NameOwner(ctx context.Context, name string) (string, error) { return b.owner(ctx) }

// TestLinkUnconfirmedDisconnectLatches (R7): a failed confirmation latches Healthy false and fires OnUnhealthy once.
func TestLinkUnconfirmedDisconnectLatches(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	for _, tc := range []struct {
		name  string
		owner func(ctx context.Context) (string, error)
	}{
		{"another owner", func(context.Context) (string, error) { return ":1.99", nil }},
		{"error reply", func(context.Context) (string, error) { return "", errors.New("NameHasNoOwner") }},
		{"no reply", func(context.Context) (string, error) { <-stuck; return ":fake.1", nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := ownerBus{FakeBus: NewFakeBus(), owner: tc.owner}
			l := NewLink(b, noExit(t))
			if err := l.Acquire(); err != nil {
				t.Fatal(err)
			}
			down := newCounter()
			l.OnUnhealthy(down.inc)
			start := time.Now()
			if err := l.Disconnect(); err == nil {
				t.Fatal("Disconnect confirmed")
			}
			if d := time.Since(start); d > EmitDeadline+100*time.Millisecond {
				t.Fatalf("Disconnect took %v", d)
			}
			if l.Healthy() {
				t.Fatal("healthy after a failed confirmation")
			}
			down.wait(t)
			time.Sleep(20 * time.Millisecond)
			if got := down.n.Load(); got != 1 {
				t.Fatalf("OnUnhealthy called %d times, want 1", got)
			}
		})
	}
}

func TestLinkOnUnhealthyOnce(t *testing.T) {
	fb, l := ownedLink(t)
	down := newCounter()
	l.OnUnhealthy(down.inc)
	// Each callback runs on its own goroutine: one that blocks holds up nothing.
	block := make(chan struct{})
	defer close(block)
	l.OnUnhealthy(func() { <-block })

	fb.LoseName(WebrtcName)
	down.wait(t)
	fb.SetEmitErr(errors.New("boom"))
	if err := l.Send([]byte(`{}`)); err == nil {
		t.Fatal("Send succeeded")
	}
	time.Sleep(20 * time.Millisecond)
	if got := down.n.Load(); got != 1 {
		t.Fatalf("OnUnhealthy called %d times, want 1", got)
	}
	// Registered after the fact, a callback runs at once, once.
	late := newCounter()
	l.OnUnhealthy(late.inc)
	late.wait(t)
	time.Sleep(20 * time.Millisecond)
	if got := late.n.Load(); got != 1 {
		t.Fatalf("late OnUnhealthy called %d times, want 1", got)
	}
}

func TestLinkEmitOverrunExits(t *testing.T) {
	var mu sync.Mutex
	var codes []int
	var at time.Duration
	var start time.Time
	var logs []string
	fb, l := ownedLink(t,
		WithExit(func(code int) {
			mu.Lock()
			codes = append(codes, code)
			at = time.Since(start)
			mu.Unlock()
		}),
		WithLogf(func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, format)
			mu.Unlock()
		}),
	)
	release := make(chan struct{})
	defer close(release) // lets the abandoned emit finish
	fb.StallEmits(release)

	mu.Lock()
	start = time.Now()
	mu.Unlock()
	if err := l.Send([]byte(`{}`)); !errors.Is(err, ErrBusDead) {
		t.Fatalf("stalled Send = %v, want ErrBusDead", err)
	}
	mu.Lock()
	gotCodes, gotAt := append([]int(nil), codes...), at
	mu.Unlock()
	if !reflect.DeepEqual(gotCodes, []int{ExitEmitOverrun}) {
		t.Fatalf("exit codes %v, want [%d]", gotCodes, ExitEmitOverrun)
	}
	if gotAt < EmitDeadline || gotAt > 2*EmitDeadline {
		t.Fatalf("exit after %v, want 100-200 ms", gotAt)
	}
	if l.Healthy() {
		t.Fatal("healthy after an overrun")
	}

	t0 := time.Now()
	if err := l.Send([]byte(`{}`)); !errors.Is(err, ErrBusDead) {
		t.Fatalf("Send after the overrun = %v, want ErrBusDead", err)
	}
	if d := time.Since(t0); d > 5*time.Millisecond {
		t.Fatalf("Send after the overrun took %v, want under 5 ms", d)
	}
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		if err := l.Send([]byte(`{}`)); !errors.Is(err, ErrBusDead) {
			t.Fatalf("Send = %v", err)
		}
	}
	if err := l.Connect(); !errors.Is(err, ErrBusDead) {
		t.Fatalf("Connect = %v", err)
	}
	if err := l.Disconnect(); !errors.Is(err, ErrBusDead) {
		t.Fatalf("Disconnect = %v", err)
	}
	// A goroutine per dead send would block on the stall for good, so the count would never come back down. Polling
	// tolerates short-lived goroutines unrelated to the Link (seen once in about 100 package runs).
	after := 0
	if !eventually(time.Second, func() bool { after = runtime.NumGoroutine(); return after <= before }) {
		t.Fatalf("goroutines grew from %d to %d across dead sends", before, after)
	}
	var gotLogs []string
	if !eventually(time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		gotCodes, gotLogs = append([]int(nil), codes...), append([]string(nil), logs...)
		return len(gotLogs) > 0
	}) || len(gotLogs) != 1 || !strings.Contains(gotLogs[0], "overran") {
		t.Fatalf("logs %q, want one overrun line", gotLogs)
	}
	if len(gotCodes) != 1 {
		t.Fatalf("exit called %d times, want once", len(gotCodes))
	}
}

// TestLinkEmitOverrunRealGodbus: a godbus write stuck on a socket whose peer never reads trips the deadline.
func TestLinkEmitOverrunRealGodbus(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[0]), "carlink-socketpair")
	peer := os.NewFile(uintptr(fds[1]), "carlink-socketpair-peer")
	nc, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newBus(func(opts ...dbus.ConnOption) (*dbus.Conn, error) { return dbus.NewConn(nc, opts...) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	t.Cleanup(func() { peer.Close() }) // runs first: the stuck write fails, then Close can proceed

	exited := make(chan int, 1)
	l := NewLink(b, WithExit(func(code int) { exited <- code }))
	sent := make(chan error, 1)
	start := time.Now()
	go func() { sent <- l.Send(bytes.Repeat([]byte("x"), 4<<20)) }()
	select {
	case code := <-exited:
		if code != ExitEmitOverrun {
			t.Fatalf("exit(%d), want %d", code, ExitEmitOverrun)
		}
		if d := time.Since(start); d < EmitDeadline {
			t.Fatalf("exit after %v, before the deadline", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the exit hook did not fire")
	}
	if err := <-sent; !errors.Is(err, ErrBusDead) {
		t.Fatalf("Send = %v, want ErrBusDead", err)
	}
}

// TestLinkOwnedAfterQueuedAcquire (R8): a Link whose name came by queueing owns it and confirms its disconnect.
func TestLinkOwnedAfterQueuedAcquire(t *testing.T) {
	bus := bustest.Start(t)
	a := dial(t, bus)
	own(t, a, WebrtcName)
	b := dial(t, bus)
	l := NewLink(b, noExit(t))
	if owned, err := b.RequestName(WebrtcName, true); owned || err != nil {
		t.Fatalf("queued request: owned %v, err %v; want false, nil", owned, err)
	}
	if l.Owned() {
		t.Fatal("owned while queued")
	}
	a.Close()
	if !eventually(time.Second, l.Owned) {
		t.Fatal("the Link does not own the name 1 s after the owner closed")
	}
	if !l.Healthy() {
		t.Fatal("unhealthy after a queued acquire")
	}
	if err := l.Disconnect(); err != nil {
		t.Fatalf("Disconnect = %v", err)
	}
}

func TestFakeBusCalls(t *testing.T) {
	fb := NewFakeBus()
	if fb.CallCarserverToPeer(map[string]any{"uid": PhoneUID, "channel": ChannelTCP, "data": "{}"}) != nil {
		t.Fatal("a car call was answered before Serve")
	}
	var frames []CarFrame
	var bodies []map[string]any
	reply := int32(1)
	if err := fb.Serve(Handlers{
		CarFrame:  func(f CarFrame) { frames = append(frames, f) },
		Signaling: func(b map[string]any) { bodies = append(bodies, b) },
		Reply:     func() int32 { return reply },
	}); err != nil {
		t.Fatal(err)
	}
	if err := fb.Serve(Handlers{}); err == nil {
		t.Fatal("a second Serve succeeded")
	}
	r := fb.CallCarserverToPeer(map[string]any{"uid": PhoneUID, "channel": ChannelUDP, "data": `{"a":1}`})
	if !reflect.DeepEqual(r, map[string]any{ReplyKey: int32(1)}) {
		t.Fatalf("reply %#v", r)
	}
	reply = 0
	r = fb.CallCarserverToPeer(map[string]any{"uid": PhoneUID, "channel": ChannelTCP, "data": strings.Repeat("x", MaxCarData+1)})
	if !reflect.DeepEqual(r, map[string]any{ReplyKey: int32(0)}) {
		t.Fatalf("reply %#v", r)
	}
	if len(frames) != 1 {
		t.Fatalf("%d frames, want 1 (the oversize one is dropped)", len(frames))
	}
	want := []CarFrame{{Channel: ChannelUDP, UID: PhoneUID, Sender: frames[0].Sender, Data: []byte(`{"a":1}`)}}
	if !reflect.DeepEqual(frames, want) || !strings.HasPrefix(frames[0].Sender, ":") {
		t.Fatalf("frames %+v, want %+v from a unique name", frames, want)
	}
	if u := fb.CallRemoteToLocal(map[string]any{"type": int32(9)}); u != 0 {
		t.Fatalf("RemoteToLocalSignaling replied %d", u)
	}
	if !reflect.DeepEqual(bodies, []map[string]any{{"type": int32(9)}}) {
		t.Fatalf("Signaling got %v", bodies)
	}

	if _, err := fb.RequestName(WebrtcName, true); err == nil {
		t.Fatal("FakeBus queued for a name")
	}
	if _, err := fb.NameOwner(context.Background(), WebrtcName); err == nil {
		t.Fatal("NameOwner answered for a name nobody owns")
	}
	own(t, fb, WebrtcName)
	if o, err := fb.NameOwner(context.Background(), WebrtcName); err != nil || o != fb.UniqueName() {
		t.Fatalf("NameOwner = %q, %v; want %q", o, err, fb.UniqueName())
	}
}
