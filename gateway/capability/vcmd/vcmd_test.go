package vcmd

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"airgapp/gateway/capability/testkit"
	"airgapp/gateway/internal/cmdrelay"
)

type injector struct {
	mu  sync.Mutex
	got chan []byte
}

func (i *injector) Inject(_ context.Context, msg []byte) error {
	i.got <- append([]byte(nil), msg...)
	return nil
}

func startRelay(t *testing.T) (*cmdrelay.Server, string, *injector) {
	t.Helper()
	dir, err := os.MkdirTemp("", "vc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "cmd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	inj := &injector{got: make(chan []byte, 8)}
	srv := cmdrelay.New(cmdrelay.Config{AllowUID: os.Getuid(), Inject: inj, Log: log.New(io.Discard, "", 0)})
	srv.SetMonitorUp(true)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(); ln.Close() })
	return srv, sock, inj
}

// pb helpers (the shapes in internal/cmdrelay's tests).
func fb(num int, v []byte) []byte {
	tag := uint64(num<<3 | 2)
	var b []byte
	for tag >= 0x80 {
		b = append(b, byte(tag)|0x80)
		tag >>= 7
	}
	return append(append(append(b, byte(tag)), byte(len(v))), v...)
}

func request(domain int, from []byte) []byte {
	m := fb(6, []byte{0x08, byte(domain)})
	m = append(m, fb(7, fb(2, from))...)
	return append(m, fb(51, bytes.Repeat([]byte{0xAB}, 16))...)
}

func reply(to []byte) []byte {
	m := fb(6, fb(2, to))
	m = append(m, fb(7, []byte{0x08, 3})...)
	return append(m, fb(50, bytes.Repeat([]byte{0xAB}, 16))...)
}

func waitSent(t *testing.T, w *testkit.FakeWindow, typ byte, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := w.Sent(typ); len(s) >= n {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("type 0x%02x: %d frames, want %d", typ, len(w.Sent(typ)), n)
	return nil
}

func fromAddress(msg []byte) []byte {
	// to (6) is 2+2 bytes, then from: 0x3a len 0x12 len ra...
	i := 2 + int(msg[1])
	inner := msg[i+2 : i+2+int(msg[i+1])]
	return inner[2 : 2+int(inner[1])]
}

func TestRoundTrip(t *testing.T) {
	srv, sock, inj := startRelay(t)
	cap := NewWithConfig(Config{Socket: sock, HelperUID: os.Getuid(), Log: log.New(io.Discard, "", 0)})
	sp := cap.Spec()
	if sp.Name != "VCMD" || sp.ID != ID || sp.In[TypeMsg] != MaxMsg {
		t.Fatalf("spec %+v", sp)
	}
	w := testkit.NewFakeWindow()
	h, err := cap.Open(context.Background(), w, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Teardown("test")
	waitSent(t, w, TypeUp, 1)

	phone := bytes.Repeat([]byte{0x11}, 16)
	if err := h.Frame(TypeMsg, request(3, phone)); err != nil {
		t.Fatal(err)
	}
	var injected []byte
	select {
	case injected = <-inj.got:
	case <-time.After(3 * time.Second):
		t.Fatal("not injected")
	}
	car := fromAddress(injected)
	if bytes.Equal(car, phone) {
		t.Fatal("address not rewritten")
	}
	srv.Deliver(reply(car))
	got := waitSent(t, w, TypeOut, 1)
	if !bytes.Equal(got[0], reply(phone)) {
		t.Fatalf("reply %x", got[0])
	}
	// A refused message comes back as ERR.
	if err := h.Frame(TypeMsg, request(5, phone)); err != nil {
		t.Fatal(err)
	}
	if e := waitSent(t, w, TypeErr, 1); !bytes.HasPrefix(e[0], []byte("refused\x00")) {
		t.Fatalf("err %q", e[0])
	}
}

func TestWrongServerUID(t *testing.T) {
	_, sock, _ := startRelay(t)
	cap := NewWithConfig(Config{Socket: sock, HelperUID: os.Getuid() + 1, Log: log.New(io.Discard, "", 0)})
	if _, err := cap.Open(context.Background(), testkit.NewFakeWindow(), nil); err == nil {
		t.Fatal("opened against a relay of another uid")
	}
}

func TestNoRelay(t *testing.T) {
	cap := NewWithConfig(Config{Socket: filepath.Join(os.TempDir(), "no-such-vcmd.sock"), Log: log.New(io.Discard, "", 0)})
	if _, err := cap.Open(context.Background(), testkit.NewFakeWindow(), nil); err == nil {
		t.Fatal("opened without a relay")
	}
}

func TestRelayGoneClosesTheWindow(t *testing.T) {
	srv, sock, _ := startRelay(t)
	cap := NewWithConfig(Config{Socket: sock, HelperUID: os.Getuid(), Log: log.New(io.Discard, "", 0)})
	w := testkit.NewFakeWindow()
	h, err := cap.Open(context.Background(), w, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Teardown("test")
	waitSent(t, w, TypeUp, 1)
	srv.Shutdown()
	select {
	case <-w.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the window outlived the relay")
	}
	if r := w.CloseReason(); r != "relay" {
		t.Fatalf("close reason %q", r)
	}
}

func TestFullLaneDrops(t *testing.T) {
	_, sock, _ := startRelay(t)
	cap := NewWithConfig(Config{Socket: sock, HelperUID: os.Getuid(), Log: log.New(io.Discard, "", 0)})
	w := testkit.NewFakeWindow()
	w.FailTrySend(true)
	h, err := cap.Open(context.Background(), w, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Teardown("test")
	select {
	case <-w.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a full lane did not end the window")
	}
	if r := w.CloseReason(); r != "error" {
		t.Fatalf("close reason %q", r)
	}
}
