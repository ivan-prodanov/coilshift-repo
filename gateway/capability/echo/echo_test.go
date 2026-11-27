package echo

import (
	"bytes"
	"context"
	"testing"

	"airgapp/gateway/capability/testkit"
)

func TestEchoSpec(t *testing.T) {
	s := New().Spec()
	if s.Name != "ECHO" || s.ID != 0x7F || s.LeaseMS != 20000 {
		t.Fatalf("spec %+v", s)
	}
	if len(s.In) != 1 || s.In[0x7E] != 1024 || len(s.Out) != 1 || s.Out[0xFE] != 1024 || len(s.Bulk) != 0 {
		t.Fatalf("spec maps %+v", s)
	}
}

func TestEchoFrame(t *testing.T) {
	c := New()
	if err := c.PreArm(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	w := testkit.NewFakeWindow()
	h, err := c.Open(context.Background(), w, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Frame(0x7E, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	got := w.Sent(0xFE)
	if len(got) != 1 || !bytes.Equal(got[0], []byte("hi")) {
		t.Fatalf("sent %v", got)
	}
	h.Teardown("x")
}

// refWindow keeps the exact slice passed to Send (no copy), like the real
// window, which owns the body until it is written.
type refWindow struct {
	*testkit.FakeWindow
	got []byte
}

func (w *refWindow) Send(t byte, body []byte) error { w.got = body; return nil }

// The Frame body is only valid during Frame; echo must not hand it to Send.
func TestEchoCopiesBodyBeforeSend(t *testing.T) {
	w := &refWindow{FakeWindow: testkit.NewFakeWindow()}
	h, err := New().Open(context.Background(), w, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := []byte("hi")
	if err := h.Frame(0x7E, in); err != nil {
		t.Fatal(err)
	}
	in[0] = 'X' // the core reuses the buffer after Frame returns
	if string(w.got) != "hi" {
		t.Fatalf("sent body aliases the Frame buffer: %q", w.got)
	}
}
