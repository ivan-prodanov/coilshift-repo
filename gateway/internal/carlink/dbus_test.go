package carlink

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/internal/carlink/bustest"
)

// dial is a DBusBus on the private bus, closed at cleanup.
func dial(t *testing.T, bus *bustest.Bus) *DBusBus {
	t.Helper()
	b, err := DialBus(bus.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// own makes b the primary owner of name.
func own(t *testing.T, b Bus, name string) {
	t.Helper()
	if owned, err := b.RequestName(name, false); !owned || err != nil {
		t.Fatalf("RequestName(%s): owned %v, err %v", name, owned, err)
	}
}

// eventually polls cond every millisecond until it holds or d has passed.
func eventually(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// settle returns once b's callback runner has run every name and KeysUpdated callback for what b has read so far:
// b acquires a fresh name, and the NameAcquired callback for it runs after everything queued before it.
func settle(t *testing.T, b *DBusBus) {
	t.Helper()
	name := fmt.Sprintf("com.airgapp.Settle%d", settleSeq.Add(1))
	ran := make(chan struct{})
	var once sync.Once
	if err := b.OnNameAcquired(name, func() { once.Do(func() { close(ran) }) }); err != nil {
		t.Fatal(err)
	}
	own(t, b, name)
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the callback runner did not reach the settle name")
	}
}

var settleSeq atomic.Int64

func TestRequestNameDoNotQueue(t *testing.T) {
	bus := bustest.Start(t)
	a, b := dial(t, bus), dial(t, bus)
	own(t, a, WebrtcName)
	if owned, err := b.RequestName(WebrtcName, false); owned || err != nil {
		t.Fatalf("B: owned %v, err %v; want false, nil", owned, err)
	}
	// B did not queue: once A is gone, nobody owns the name.
	aName := a.UniqueName()
	a.Close()
	var owner string
	var err error
	if !eventually(time.Second, func() bool {
		owner, err = b.NameOwner(context.Background(), WebrtcName)
		return err != nil || owner != aName
	}) {
		t.Fatal("A still owns the name 1 s after closing")
	}
	if err == nil {
		t.Fatalf("the name went to %q (B is %q); want no owner", owner, b.UniqueName())
	}
}

func TestQueuedOwnerAcquiresOnOwnerDeath(t *testing.T) {
	bus := bustest.Start(t)
	a, b := dial(t, bus), dial(t, bus)
	own(t, a, WebrtcName)
	acquired := make(chan struct{}, 4)
	if err := b.OnNameAcquired(WebrtcName, func() { acquired <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	if owned, err := b.RequestName(WebrtcName, true); owned || err != nil {
		t.Fatalf("queued request: owned %v, err %v; want false, nil (InQueue)", owned, err)
	}
	a.Close()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("no NameAcquired within 1 s of the owner's death")
	}
	owner, err := b.NameOwner(context.Background(), WebrtcName)
	if err != nil || owner != b.UniqueName() {
		t.Fatalf("owner %q, %v; want %q", owner, err, b.UniqueName())
	}
}

func TestNameLostOnClose(t *testing.T) {
	bus := bustest.Start(t)
	a := dial(t, bus)
	own(t, a, WebrtcName)
	var webrtc, other atomic.Int32
	if err := a.OnNameLost(WebrtcName, func() { webrtc.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := a.OnNameLost("com.airgapp.Other", func() { other.Add(1) }); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if !eventually(time.Second, func() bool { return webrtc.Load() > 0 && other.Load() > 0 }) {
		t.Fatalf("NameLost callbacks after close: webrtc %d, other %d; want 1 each", webrtc.Load(), other.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if webrtc.Load() != 1 || other.Load() != 1 {
		t.Fatalf("NameLost callbacks after close: webrtc %d, other %d; want 1 each", webrtc.Load(), other.Load())
	}
	if err := a.OnNameLost(WebrtcName, func() {}); err == nil {
		t.Fatal("OnNameLost on a closed bus succeeded")
	}
}

// TestNameEventsReachOnlyEarlierCallbacks: a name event reaches the callbacks registered before the bus read it,
// never one registered after. Otherwise a Link made after an earlier Link released the name could latch on that
// earlier NameLost (the supervisor's Link after its stale-marker Finish, Task 11).
func TestNameEventsReachOnlyEarlierCallbacks(t *testing.T) {
	bus := bustest.Start(t)
	b := dial(t, bus)
	gate := make(chan struct{})
	var blockOnce sync.Once
	// The first callback holds the runner, so the events below are still queued when the late callbacks register.
	if err := b.OnNameAcquired(WebrtcName, func() { blockOnce.Do(func() { <-gate }) }); err != nil {
		t.Fatal(err)
	}
	own(t, b, WebrtcName)
	if err := b.ReleaseName(WebrtcName); err != nil {
		t.Fatal(err)
	}
	var lateLost, lateAcquired atomic.Int32
	if err := b.OnNameLost(WebrtcName, func() { lateLost.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := b.OnNameAcquired(WebrtcName, func() { lateAcquired.Add(1) }); err != nil {
		t.Fatal(err)
	}
	close(gate)
	settle(t, b)
	if lateLost.Load() != 0 || lateAcquired.Load() != 0 {
		t.Fatalf("late callbacks ran for earlier events: lost %d, acquired %d", lateLost.Load(), lateAcquired.Load())
	}
	// A later event does reach them.
	own(t, b, WebrtcName)
	settle(t, b)
	if lateAcquired.Load() != 1 {
		t.Fatalf("late NameAcquired callback ran %d times for a later event, want 1", lateAcquired.Load())
	}
}

// TestNameEventsOnlyFromBusDriver: a NameLost or NameAcquired sent straight to the bus by another client does not count.
func TestNameEventsOnlyFromBusDriver(t *testing.T) {
	bus := bustest.Start(t)
	b := dial(t, bus)
	own(t, b, WebrtcName)
	var lost, acquired atomic.Int32
	if err := b.OnNameLost(WebrtcName, func() { lost.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := b.OnNameAcquired(WebrtcName, func() { acquired.Add(1) }); err != nil {
		t.Fatal(err)
	}
	forger := bus.Conn(t)
	for _, member := range []string{"NameLost", "NameAcquired"} {
		msg := &dbus.Message{
			Type: dbus.TypeSignal,
			Headers: map[dbus.HeaderField]dbus.Variant{
				dbus.FieldPath:        dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/DBus")),
				dbus.FieldInterface:   dbus.MakeVariant("org.freedesktop.DBus"),
				dbus.FieldMember:      dbus.MakeVariant(member),
				dbus.FieldDestination: dbus.MakeVariant(b.UniqueName()),
				dbus.FieldSignature:   dbus.MakeVariant(dbus.SignatureOf(WebrtcName)),
			},
			Body: []any{WebrtcName},
		}
		forger.Send(msg, nil)
	}
	// The forger's round trip proves the bus handled both signals; settle then proves b ran whatever it got.
	if err := forger.BusObject().Call("org.freedesktop.DBus.GetId", 0).Err; err != nil {
		t.Fatal(err)
	}
	settle(t, b)
	if lost.Load() != 0 || acquired.Load() != 0 {
		t.Fatalf("forged name events counted: lost %d, acquired %d", lost.Load(), acquired.Load())
	}
}

func TestCarCallsDeliveredInOrder(t *testing.T) {
	bus := bustest.Start(t)
	h := dial(t, bus)
	var mu sync.Mutex
	var got []CarFrame
	if err := h.Serve(Handlers{
		CarFrame: func(f CarFrame) { mu.Lock(); got = append(got, f); mu.Unlock() },
		Reply:    func() int32 { return 1 },
	}); err != nil {
		t.Fatal(err)
	}
	own(t, h, WebrtcName)

	car := bus.Conn(t)
	obj := car.Object(WebrtcName, WebrtcPath)
	const n = 500
	channel := func(i int) string {
		if i%2 == 0 {
			return ChannelUDP
		}
		return ChannelTCP
	}
	replies := make(chan *dbus.Call, n) // never full: the read goroutine must not block on it
	sem := make(chan struct{}, 100)     // R1: at most 100 calls outstanding
	checked := make(chan []string, 1)
	go func() {
		var bad []string
		for i := 0; i < n; i++ {
			c := <-replies
			<-sem
			if c.Err != nil {
				bad = append(bad, c.Err.Error())
				continue
			}
			r, ok := c.Body[0].(map[string]dbus.Variant)
			if !ok || len(r) != 1 || r[ReplyKey].Signature().String() != "i" || r[ReplyKey].Value() != int32(1) {
				bad = append(bad, fmt.Sprint(c.Body))
			}
		}
		checked <- bad
	}()
	for i := 1; i <= n; i++ {
		sem <- struct{}{}
		obj.Go(WebrtcIface+"."+MethodCarserverToPeerCommsData, 0, replies, map[string]dbus.Variant{
			"uid":     dbus.MakeVariant(PhoneUID),
			"channel": dbus.MakeVariant(channel(i)),
			"data":    dbus.MakeVariant(strconv.Itoa(i)),
		})
	}
	select {
	case bad := <-checked:
		if len(bad) > 0 {
			t.Fatalf("%d bad replies, first %s", len(bad), bad[0])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("replies did not all arrive")
	}
	// Each frame was handed over before godbus dispatched its call, so all 500 are in.
	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Fatalf("%d frames, want %d", len(got), n)
	}
	for i, f := range got {
		want := CarFrame{Channel: channel(i + 1), UID: PhoneUID, Sender: car.Names()[0], Data: []byte(strconv.Itoa(i + 1))}
		if !reflect.DeepEqual(f, want) {
			t.Fatalf("frame %d = %+v (data %q), want %+v (data %q)", i, f, f.Data, want, want.Data)
		}
	}
}

func TestSignalingDelivered(t *testing.T) {
	bus := bustest.Start(t)
	h := dial(t, bus)
	bodies := make(chan map[string]any, 1)
	if err := h.Serve(Handlers{Signaling: func(b map[string]any) { bodies <- b }}); err != nil {
		t.Fatal(err)
	}
	if err := h.Serve(Handlers{}); err == nil {
		t.Fatal("a second Serve succeeded")
	}
	own(t, h, WebrtcName)
	car := bus.Conn(t)
	var u uint32 = 7
	err := car.Object(WebrtcName, WebrtcPath).Call(WebrtcIface+"."+MethodRemoteToLocalSignaling, 0, map[string]dbus.Variant{
		"type":       dbus.MakeVariant(int32(9)),
		"uid":        dbus.MakeVariant(PhoneUID),
		"session_id": dbus.MakeVariant(""),
	}).Store(&u)
	if err != nil || u != 0 {
		t.Fatalf("reply %d, %v; want 0", u, err)
	}
	select {
	case b := <-bodies:
		want := map[string]any{"type": int32(9), "uid": PhoneUID, "session_id": ""}
		if !reflect.DeepEqual(b, want) {
			t.Fatalf("Signaling got %#v, want %#v", b, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Signaling was not called")
	}
}

// TestCarFramesDroppedBySize: data over MaxCarData and malformed maps never reach CarFrame. They are logged by size,
// never by content, and the call is still answered.
func TestCarFramesDroppedBySize(t *testing.T) {
	bus := bustest.Start(t)
	h := dial(t, bus)
	var mu sync.Mutex
	var logs []string
	h.SetLogf(func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	frames := make(chan CarFrame, 8)
	if err := h.Serve(Handlers{CarFrame: func(f CarFrame) { frames <- f }, Reply: func() int32 { return 1 }}); err != nil {
		t.Fatal(err)
	}
	own(t, h, WebrtcName)
	car := bus.Conn(t)
	obj := car.Object(WebrtcName, WebrtcPath)
	call := func(arg any) (*dbus.Call, map[string]dbus.Variant) {
		c := obj.Call(WebrtcIface+"."+MethodCarserverToPeerCommsData, 0, arg)
		var r map[string]dbus.Variant
		if c.Err == nil {
			c.Store(&r)
		}
		return c, r
	}
	frame := func(data any) map[string]dbus.Variant {
		return map[string]dbus.Variant{
			"uid": dbus.MakeVariant(PhoneUID), "channel": dbus.MakeVariant(ChannelTCP), "data": dbus.MakeVariant(data),
		}
	}
	big := strings.Repeat("y", MaxCarData)
	over := strings.Repeat("z", MaxCarData+1)
	for _, arg := range []map[string]dbus.Variant{
		frame(over),
		frame(int32(5)),
		{"channel": dbus.MakeVariant(ChannelTCP), "data": dbus.MakeVariant("{}")}, // no uid
		frame(big),
	} {
		c, r := call(arg)
		if c.Err != nil || r[ReplyKey].Value() != int32(1) {
			t.Fatalf("reply %v, %v; want {%s: 1}", r, c.Err, ReplyKey)
		}
	}
	if c, _ := call("not a map"); c.Err == nil {
		t.Fatal("a call with a string argument was answered without an error")
	}
	select {
	case f := <-frames:
		if string(f.Data) != big {
			t.Fatalf("delivered %d bytes, want the %d-byte frame", len(f.Data), MaxCarData)
		}
	case <-time.After(time.Second):
		t.Fatal("the MaxCarData frame was not delivered")
	}
	select {
	case f := <-frames:
		t.Fatalf("a dropped frame was delivered: %d bytes", len(f.Data))
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 4 {
		t.Fatalf("%d log lines, want 4: %q", len(logs), logs)
	}
	if !strings.Contains(logs[0], strconv.Itoa(MaxCarData+1)) {
		t.Errorf("oversize log %q lacks the size", logs[0])
	}
	for _, l := range logs {
		if strings.Contains(l, "zzzz") || strings.Contains(l, "not a map") || strings.Contains(l, PhoneUID) {
			t.Errorf("log line carries content: %q", l)
		}
	}
}

func TestKeysUpdatedSubscriptionIsSenderPinned(t *testing.T) {
	bus := bustest.Start(t)
	h := dial(t, bus)
	var n atomic.Int32
	if err := h.SubscribeKeysUpdated(func() { n.Add(1) }); err != nil {
		t.Fatal(err)
	}
	auth := bus.Conn(t)
	if r, err := auth.RequestName(AuthName, dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("auth RequestName: %v, %v", r, err)
	}
	if err := auth.Emit(AuthPath, AuthIface+"."+SignalKeysUpdated); err != nil {
		t.Fatal(err)
	}
	if !eventually(time.Second, func() bool { return n.Load() >= 1 }) {
		t.Fatal("the owner's KeysUpdated did not arrive")
	}

	other := bus.Conn(t)
	if err := other.Emit(AuthPath, AuthIface+"."+SignalKeysUpdated); err != nil {
		t.Fatal(err)
	}
	// Sent straight to h, a signal needs no match rule: it must not count either.
	other.Send(&dbus.Message{
		Type: dbus.TypeSignal,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:        dbus.MakeVariant(dbus.ObjectPath(AuthPath)),
			dbus.FieldInterface:   dbus.MakeVariant(AuthIface),
			dbus.FieldMember:      dbus.MakeVariant(SignalKeysUpdated),
			dbus.FieldDestination: dbus.MakeVariant(h.UniqueName()),
		},
	}, nil)
	// The non-owner's round trip proves the bus handled both signals; settle then proves h ran whatever it got.
	if err := other.BusObject().Call("org.freedesktop.DBus.GetId", 0).Err; err != nil {
		t.Fatal(err)
	}
	settle(t, h)
	if got := n.Load(); got != 1 {
		t.Fatalf("KeysUpdated callback ran %d times, want 1 (the owner's only)", got)
	}
}

func TestCallerInfo(t *testing.T) {
	bus := bustest.Start(t)
	h := dial(t, bus)
	car := bus.Conn(t)
	uid, pid, err := h.CallerInfo(context.Background(), car.Names()[0])
	if err != nil {
		t.Fatal(err)
	}
	if uid != uint32(os.Getuid()) {
		t.Fatalf("uid %d, want %d", uid, os.Getuid())
	}
	if runtime.GOOS == "linux" && pid != uint32(os.Getpid()) {
		t.Fatalf("pid %d, want %d", pid, os.Getpid())
	}
	if _, _, err := h.CallerInfo(context.Background(), ":1.999999"); err == nil {
		t.Fatal("CallerInfo for an unknown sender succeeded")
	}
}
