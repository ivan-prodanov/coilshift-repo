package camhelper

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/camera"
	"airgapp/gateway/capability/dashcam"
	gwclient "airgapp/gateway/client"
	"airgapp/gateway/internal/authority"
	gwproto "airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/server"
)

const e2eVIN = "TESTVIN0000000000"

// gateway runs the real agw server (fake authority) hosting the real CAMERA
// and DASHCAM capabilities, wired to this rig's helper socket.
type gateway struct {
	addr   string
	gw     *ecdsa.PrivateKey
	dev    *ecdh.PrivateKey
	srv    *server.Server
	logDir string
}

func startGateway(t *testing.T, r *rig) *gateway {
	t.Helper()
	g := &gateway{logDir: t.TempDir()}
	g.gw, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	g.dev, _ = ecdh.P256().GenerateKey(rand.Reader)
	quiet := log.New(io.Discard, "", 0)
	caps := []capability.Capability{
		camera.NewWithConfig(camera.Config{Dial: camera.SocketDialer(r.sock), Tick: 20 * time.Millisecond, Log: quiet}),
		dashcam.NewWithConfig(dashcam.Config{Dial: dashcam.SocketDialer(r.sock), Root: r.cam, Log: quiet}),
	}
	g.srv = server.New(server.Config{Key: g.gw, VIN: e2eVIN, Oracle: authority.Fake{Role: 2, Owner: true},
		Caps: caps, LogPath: filepath.Join(g.logDir, "session.log")})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.addr = ln.Addr().String()
	go g.srv.Serve(ln)
	t.Cleanup(func() { g.srv.Shutdown("sigterm") })
	return g
}

func (g *gateway) dial(t *testing.T, cap byte, params any) *gwclient.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := gwclient.Dial(ctx, g.addr, cap, params, &g.gw.PublicKey, e2eVIN, g.dev)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// recvUntil reads frames until want returns true (or fails after 5 s).
func recvUntil(t *testing.T, s *gwclient.Session, what string, want func(typ byte, body []byte) bool) {
	t.Helper()
	got := make(chan bool, 1)
	go func() {
		for {
			typ, body, err := s.Recv()
			if err != nil {
				got <- false
				return
			}
			if want(typ, body) {
				got <- true
				return
			}
		}
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("session ended before %s", what)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Phone -> agw (HPKE, real server) -> CAMERA -> camhelper -> fake car, and
// back: STREAMS, VIDEO from a capture child, TALK PCM into the writer, and
// the §4.9 release when the phone closes.
func TestEndToEndCameraAndTalk(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Tick = 50 * time.Millisecond }) // route watchdog + status events
	r.car.setLinks(feedersOK)
	g := startGateway(t, r)
	s := g.dial(t, gwproto.CapCamera, map[string]any{"view": "rear", "mode": "full", "talk": true})

	recvUntil(t, s, "STREAMS rear", func(typ byte, body []byte) bool {
		return typ == camera.TStreams && strings.Contains(string(body), `"view":"rear"`)
	})
	ch := r.sp.find(t, "port=9900")
	sps, pps := []byte{0x67, 0x64, 0x00, 0x28}, []byte{0x68, 0xEE, 0x3C, 0xB0}
	idr, p := []byte{0x65, 0x88, 0x84, 0x21}, []byte{0x41, 0x9A, 0x02, 0x04}
	go ch.outW.Write(annexB(sps, pps, idr, p, p, p)) // an AU is complete once the next one starts
	var vids [][]byte
	recvUntil(t, s, "two VIDEO frames", func(typ byte, body []byte) bool {
		if typ == camera.TVideo {
			vids = append(vids, append([]byte(nil), body...))
		}
		return len(vids) == 2
	})
	if vids[0][0] != 1 || vids[0][1] != 0 || vids[0][2] != 3 || !bytes.Equal(vids[0][12:], annexB(sps, pps, idr)) {
		t.Fatalf("IDR VIDEO % x", vids[0][:16])
	}
	if vids[1][2] != 0 || !bytes.Equal(vids[1][12:], annexB(p)) {
		t.Fatalf("P VIDEO % x", vids[1][:16])
	}

	writer := r.sp.find(t, "pw-cat")
	recvUntil(t, s, "talk ready", func(typ byte, body []byte) bool {
		var ts struct{ State string }
		return typ == camera.TTalkState && json.Unmarshal(body, &ts) == nil && ts.State == "ready"
	})
	msg := bytes.Repeat([]byte{0x11, 0x22}, 4800) // 100 ms
	if err := s.Send(camera.TTalkStart, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(camera.TTalkPCM, msg); err != nil {
		t.Fatal(err)
	}
	s.Send(camera.TTalkStop, []byte(`{}`))
	waitFor(t, "PCM at the writer", func() bool { return bytes.Contains(writer.stdin(), msg) })
	waitFor(t, "boost and back", func() bool {
		ops := strings.Join(r.car.snapshot(), ",")
		return strings.Contains(ops, "gain=0") && strings.Contains(ops, "gain=-20")
	})

	s.Close()
	waitFor(t, "the §4.9 release", func() bool { // sel0: front's default selectable 3
		return strings.HasSuffix(strings.Join(r.car.snapshot(), ","), strings.Join([]string{ra0, sa0, sel0, "gain=-200"}, ","))
	})
	waitFor(t, "children stopped", func() bool { return ch.stopped.Load() && writer.stopped.Load() })
	if len(r.markers()) != 0 {
		t.Fatalf("markers %v", r.markers())
	}
	// The server writes the session's log line last in its teardown, after the
	// handler's release, the BYE, the socket close and the slot free
	// (internal/server/window.go), so wait for it rather than read it once.
	var b []byte
	waitFor(t, "the session log line", func() bool {
		b, _ = os.ReadFile(filepath.Join(g.logDir, "session.log"))
		return bytes.Contains(b, []byte(`"cap":"CAMERA"`))
	})
	if !bytes.Contains(b, []byte("view=rear")) || bytes.Contains(b, msg[:64]) {
		t.Fatalf("session log %s", b)
	}
}

func TestEndToEndDashcam(t *testing.T) {
	r := newRig(t)
	r.tree(t)
	g := startGateway(t, r)
	s := g.dial(t, gwproto.CapDashcam, map[string]any{})
	s.Send(dashcam.TList, []byte(`{"id":1}`))
	recvUntil(t, s, "LIST_RESULT", func(typ byte, body []byte) bool {
		return typ == dashcam.TListResult && bytes.Contains(body, []byte(`"last":true`)) && bytes.Contains(body, []byte(`"trigger_cam_key":"right_repeater"`))
	})
	p := r.cam + "/SentryClips/2026-11-16_07-56-30/2026-11-16_07-55-46-back.mp4"
	body, _ := json.Marshal(map[string]any{"id": 2, "path": p, "offset": 0, "length": 0})
	s.Send(dashcam.TGet, body)
	recvUntil(t, s, "CHUNK", func(typ byte, b []byte) bool {
		return typ == dashcam.TChunk && binary.BigEndian.Uint32(b) == 2 && string(b[20:]) == "BACK!!"
	})
	s.Send(dashcam.TGet, []byte(`{"id":3,"path":"/etc/passwd","offset":0,"length":0}`))
	recvUntil(t, s, "ERROR", func(typ byte, b []byte) bool {
		return typ == dashcam.TError && string(b) == `{"id":3,"error":"forbidden_path"}`
	})
}
