package testkit

import (
	"airgapp/gateway/capability"
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestFakeWindow(t *testing.T) {
	w := NewFakeWindow()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = w.Send(1, []byte{1})
				w.TrySend(2, []byte{2})
			}
		}()
	}
	wg.Wait()
	if len(w.Sent(1)) != 400 || len(w.Sent(2)) != 400 {
		t.Fatal("counts")
	}
	w.FailTrySend(true)
	if w.TrySend(2, nil) || len(w.Sent(2)) != 400 {
		t.Fatal("FailTrySend")
	}
	w.SetQueuedBytes(77)
	if w.QueuedBytes() != 77 {
		t.Fatal("queued")
	}
	w.SetDetail("front")
	if w.Detail() != "front" || w.KeyFingerprint() == "" {
		t.Fatal("detail/fp")
	}
	select {
	case <-w.Done():
		t.Fatal("closed early")
	default:
	}
	var cw sync.WaitGroup
	for i := 0; i < 4; i++ {
		cw.Add(1)
		go func() { defer cw.Done(); w.Close("a") }()
	}
	cw.Wait()
	w.Close("b")
	<-w.Done()
	if r := w.CloseReason(); r != "a" && r != "b" {
		t.Fatal(r)
	}
	// first reason sticks
	if w.CloseReason() != w.CloseReason() {
		t.Fatal("unstable")
	}
}

func TestFakeWindowSendAfterClose(t *testing.T) {
	w := NewFakeWindow()
	_ = w.Send(1, []byte{1})
	w.Close("x")
	if err := w.Send(1, []byte{2}); err != capability.ErrClosed {
		t.Fatal(err)
	}
	if w.TrySend(1, []byte{3}) {
		t.Fatal("TrySend after close")
	}
	if len(w.Sent(1)) != 1 {
		t.Fatal("recorded after close")
	}
}

func TestFakeWindowTicks(t *testing.T) {
	w := NewFakeWindow()
	a, b := w.IssueTick(), w.IssueTick()
	if a == b {
		t.Fatal("IssueTick ids repeat")
	}
	if age, ok := w.TickAge(a); !ok || age != 0 {
		t.Fatalf("issued tick: age %v ok %v", age, ok)
	}
	if _, ok := w.TickAge([8]byte{9}); ok {
		t.Fatal("unknown id accepted")
	}
	w.SetTickAge(a, 200*time.Millisecond, true)
	if age, ok := w.TickAge(a); !ok || age != 200*time.Millisecond {
		t.Fatalf("SetTickAge: age %v ok %v", age, ok)
	}
	w.SetTickAge(b, 0, false)
	if _, ok := w.TickAge(b); ok {
		t.Fatal("SetTickAge(ok=false) ignored")
	}
	foreign := [8]byte{7}
	w.SetTickAge(foreign, 5*time.Millisecond, true)
	if age, ok := w.TickAge(foreign); !ok || age != 5*time.Millisecond {
		t.Fatal("SetTickAge on a new id")
	}
}

func TestFakeWindowHost(t *testing.T) {
	w := NewFakeWindow()
	var _ capability.Host = w.Host()
	if w.Host().SummonOpen() || w.Host().SummonLinkDegraded() {
		t.Fatal("default host not idle")
	}
	h := NewFakeHost()
	w.SetHost(h)
	h.SetSummonOpen(true)
	if !w.Host().SummonOpen() {
		t.Fatal("SetSummonOpen")
	}
	w.Host().SetSummonLinkDegraded(true)
	if !h.SummonLinkDegraded() {
		t.Fatal("SetSummonLinkDegraded")
	}
	// Two windows sharing a host see each other's state.
	w2 := NewFakeWindow()
	w2.SetHost(h)
	if !w2.Host().SummonLinkDegraded() {
		t.Fatal("shared host")
	}
}

func TestFakeWindowPublicKey(t *testing.T) {
	w := NewFakeWindow()
	if w.PublicKey() != nil {
		t.Fatal("PublicKey before SetPublicKey")
	}
	pk := bytes.Repeat([]byte{4}, 65)
	w.SetPublicKey(pk)
	pk[1] = 0
	got := w.PublicKey()
	if len(got) != 65 || got[1] != 4 {
		t.Fatal("SetPublicKey kept the caller's slice")
	}
	got[2] = 0
	if w.PublicKey()[2] != 4 {
		t.Fatal("PublicKey returned the window's own slice")
	}
}

func TestFakeWindowLogEvent(t *testing.T) {
	w := NewFakeWindow()
	var _ capability.Window = w
	w.LogEvent("hold start view=front")
	w.LogEvent("hold end")
	got := w.Events()
	if len(got) != 2 || got[0] != "hold start view=front" || got[1] != "hold end" {
		t.Fatalf("events %q", got)
	}
}
