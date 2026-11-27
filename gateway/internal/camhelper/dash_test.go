package camhelper

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability/dashcam"
	"airgapp/gateway/capability/testkit"
)

func writeFile(t *testing.T, p string, data string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) tree(t *testing.T) {
	c := r.cam
	writeFile(t, c+"/SentryClips/2026-11-16_07-56-30/event.json", `{"timestamp":"2026-11-16T07:56:30","camera":"6"}`)
	writeFile(t, c+"/SentryClips/2026-11-16_07-56-30/2026-11-16_07-55-46-front.mp4", "FRONT")
	writeFile(t, c+"/SentryClips/2026-11-16_07-56-30/2026-11-16_07-55-46-back.mp4", "BACK!!")
	writeFile(t, c+"/SentryClips/2026-11-16_07-56-30/thumb.png", "png")
	writeFile(t, c+"/SentryClips/2026-11-16_07-56-30/.hidden.mp4", "x")
	os.MkdirAll(c+"/SentryClips/2026-11-16_07-56-30/dir.mp4", 0o755)
	writeFile(t, c+"/SentryClips/.trash/2026-11-01_00-00-00-front.mp4", "x")
	writeFile(t, c+"/SentryClips/notadir.mp4", "x")
	writeFile(t, c+"/SavedClips/2026-11-14_09-30-00/2026-11-14_09-29-10-front.mp4", "SAVED")
	writeFile(t, c+"/RecentClips/2026-11-20_06-00-00-front.mp4", "R1")
	writeFile(t, c+"/RecentClips/2026-11-20_06-01-00-front.mp4", "R22")
	// Symlinks out of the root are never listed or served.
	outside := filepath.Join(filepath.Dir(c), "outside")
	writeFile(t, outside+"/secret-front.mp4", "SECRET")
	os.Symlink(outside, c+"/SavedClips/escape")
	os.Symlink(outside+"/secret-front.mp4", c+"/RecentClips/2026-11-20_06-02-00-front.mp4")
	// A symlink inside the root is followed.
	os.Symlink(c+"/RecentClips/2026-11-20_06-00-00-front.mp4", c+"/RecentClips/2026-11-20_05-59-00-back.mp4")
}

func TestDashList(t *testing.T) {
	r := newRig(t)
	r.tree(t)
	c := r.dial()
	inv, err := c.DashList(ctx())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(inv)
	want := `{"dirs":[{"type":"SentryClips","folder":"2026-11-16_07-56-30","has_event":true,` +
		`"event":"eyJ0aW1lc3RhbXAiOiIyMDI2LTExLTE2VDA3OjU2OjMwIiwiY2FtZXJhIjoiNiJ9",` +
		`"files":[{"name":"2026-11-16_07-55-46-back.mp4","size":6},{"name":"2026-11-16_07-55-46-front.mp4","size":5}]},` +
		`{"type":"SavedClips","folder":"2026-11-14_09-30-00","has_event":false,"files":[{"name":"2026-11-14_09-29-10-front.mp4","size":5}]}],` +
		`"recent":[{"name":"2026-11-20_05-59-00-back.mp4","size":2},{"name":"2026-11-20_06-00-00-front.mp4","size":2},{"name":"2026-11-20_06-01-00-front.mp4","size":3}]}`
	if string(b) != want {
		t.Fatalf("inventory\n got %s\nwant %s", b, want)
	}
	// No USB drive: an empty listing, not an error.
	os.RemoveAll(r.cam)
	inv, err = c.DashList(ctx())
	if err != nil || len(inv.Dirs) != 0 || len(inv.Recent) != 0 {
		t.Fatalf("%+v %v", inv, err)
	}
}

func TestDashListPages(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 300; i++ {
		d := fmt.Sprintf("%s/SentryClips/2026-11-%02d_%02d-%02d-00", r.cam, 1+i/60, (i/10)%24, i%60)
		writeFile(t, d+"/event.json", strings.Repeat("x", 1000))
		for _, cam := range []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"} {
			writeFile(t, d+"/2026-11-01_00-00-00-"+cam+".mp4", "1")
		}
	}
	c := r.dial()
	inv, err := c.DashList(ctx())
	if err != nil || len(inv.Dirs) != 300 {
		t.Fatalf("%d dirs %v", len(inv.Dirs), err)
	}
}

func TestDashReadPathRules(t *testing.T) {
	r := newRig(t)
	r.tree(t)
	c := r.dial()
	p := r.cam + "/SentryClips/2026-11-16_07-56-30/2026-11-16_07-55-46-back.mp4"
	data, total, err := c.DashRead(ctx(), p, "", 2, 100)
	if err != nil || string(data) != "CK!!" || total != 6 {
		t.Fatalf("%q %d %v", data, total, err)
	}
	data, total, err = c.DashRead(ctx(), p, "", 6, 100)
	if err != nil || len(data) != 0 || total != 6 {
		t.Fatalf("past end %q %d %v", data, total, err)
	}
	data, _, err = c.DashRead(ctx(), r.cam+"/RecentClips/2026-11-20_05-59-00-back.mp4", "", 0, 10)
	if err != nil || string(data) != "R1" {
		t.Fatalf("in-root symlink %q %v", data, err)
	}
	for path, code := range map[string]string{
		"/etc/hosts":                           proto.ErrForbidden,
		r.cam:                                  proto.ErrForbidden,
		r.cam + "/../outside/secret-front.mp4": proto.ErrForbidden,
		r.cam + "/SavedClips/escape/secret-front.mp4":         proto.ErrForbidden,
		r.cam + "/RecentClips/2026-11-20_06-02-00-front.mp4":  proto.ErrForbidden,
		r.cam + "/RecentClips//2026-11-20_06-00-00-front.mp4": proto.ErrForbidden,
		r.cam + "/RecentClips/missing.mp4":                    proto.ErrNotFound,
		r.cam + "/SentryClips":                                proto.ErrNotFound, // a directory
		"relative/path.mp4":                                   proto.ErrForbidden,
	} {
		_, _, err := c.DashRead(ctx(), path, "", 0, 10)
		expectCode(t, err, code)
	}
	for _, n := range []int{0, -1, proto.MaxRead + 1} {
		_, _, err := c.DashRead(ctx(), p, "", 0, n)
		expectCode(t, err, proto.ErrBadRequest)
	}
	_, _, err = c.DashRead(ctx(), p, "", -5, 10)
	expectCode(t, err, proto.ErrBadRequest)
	_, _, err = c.DashRead(ctx(), "", "nope", 0, 10)
	expectCode(t, err, proto.ErrNotFound)
}

// fakeFFmpeg writes "CUT(<in>)" to the output path of a cut child.
func fakeFFmpeg(fail bool) func(*fakeChild) {
	return func(c *fakeChild) {
		if !c.is("ffmpeg") {
			return
		}
		a := c.spec.Argv
		out := strings.TrimPrefix(a[len(a)-1], "file:")
		in := ""
		for i := range a {
			if a[i] == "-i" {
				in = strings.TrimPrefix(a[i+1], "file:")
			}
		}
		if fail {
			os.WriteFile(out, []byte("partial"), 0o600)
			c.exit(errors.New("exit status 1"))
			return
		}
		os.WriteFile(out, []byte("CUT("+in+")"), 0o600)
		c.exit(nil)
	}
}

func TestDashCut(t *testing.T) {
	r := newRig(t)
	r.tree(t)
	r.sp.onStart = fakeFFmpeg(false)
	c := r.dial()
	p := r.cam + "/SentryClips/2026-11-16_07-56-30/2026-11-16_07-55-46-front.mp4"
	tok, size, err := c.DashCut(ctx(), p, 41, 12)
	if err != nil || tok == "" || size != int64(len("CUT("+p+")")) {
		t.Fatalf("%q %d %v", tok, size, err)
	}
	ff := r.sp.all()[0]
	want := []string{"/usr/bin/ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-protocol_whitelist", "file",
		"-ss", "41.00", "-t", "12.00", "-f", "mov", "-i", "file:" + p, "-c", "copy", "-movflags", "+faststart",
		"-fs", strconv.Itoa(128 << 20), "-f", "mp4"}
	if got := ff.spec.Argv[:len(ff.spec.Argv)-1]; strings.Join(got, "\x00") != strings.Join(want, "\x00") || ff.spec.Stdin || ff.spec.Stdout {
		t.Fatalf("ffmpeg argv %q", ff.spec.Argv)
	}
	out := strings.TrimPrefix(ff.spec.Argv[len(ff.spec.Argv)-1], "file:")
	if filepath.Dir(out) != filepath.Join(r.state, cutSubdir) {
		t.Fatalf("cut written to %s", out)
	}
	tok2, _, _ := c.DashCut(ctx(), p, 41, 12) // reused for the same window
	if tok2 != tok || len(r.sp.all()) != 1 {
		t.Fatal("cut not reused")
	}
	data, total, err := c.DashRead(ctx(), "", tok, 0, 1000)
	if err != nil || string(data) != "CUT("+p+")" || total != size {
		t.Fatalf("%q %v", data, err)
	}
	for _, bad := range [][2]float64{{-1, 5}, {0, 0}, {0, 121}, {0, -3}} {
		_, _, err := c.DashCut(ctx(), p, bad[0], bad[1])
		expectCode(t, err, proto.ErrBadRequest)
	}
	_, _, err = c.DashCut(ctx(), "/etc/passwd", 0, 5)
	expectCode(t, err, proto.ErrForbidden)
	c.Close()
	waitFor(t, "cut removed", func() bool { _, err := os.Stat(out); return os.IsNotExist(err) })
}

func TestDashCutFailureAndEviction(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxCuts = 2 })
	r.tree(t)
	r.sp.onStart = fakeFFmpeg(true)
	c := r.dial()
	p := r.cam + "/SentryClips/2026-11-16_07-56-30/2026-11-16_07-55-46-front.mp4"
	_, _, err := c.DashCut(ctx(), p, 1, 2)
	expectCode(t, err, proto.ErrCutFailed)
	if ents, _ := os.ReadDir(filepath.Join(r.state, cutSubdir)); len(ents) != 0 {
		t.Fatal("failed cut left a file")
	}
	r.sp.mu.Lock()
	r.sp.onStart = fakeFFmpeg(false)
	r.sp.mu.Unlock()
	var toks []string
	for i := 0; i < 3; i++ {
		tok, _, err := c.DashCut(ctx(), p, float64(i), 2)
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, tok)
	}
	if ents, _ := os.ReadDir(filepath.Join(r.state, cutSubdir)); len(ents) != 2 {
		t.Fatalf("%d cut files kept", len(ents))
	}
	_, _, err = c.DashRead(ctx(), "", toks[0], 0, 10)
	expectCode(t, err, proto.ErrNotFound) // the oldest was evicted
}

// DASHCAM end to end against the real helper (fake ffmpeg).
func TestDashcamCapabilityWithTheHelper(t *testing.T) {
	r := newRig(t)
	r.tree(t)
	r.sp.onStart = fakeFFmpeg(false)
	w := testkit.NewFakeWindow()
	h, err := dashcam.NewWithConfig(dashcam.Config{Dial: dashcam.SocketDialer(r.sock), Root: r.cam, Log: log.New(io.Discard, "", 0)}).
		Open(context.Background(), w, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { w.Close("x"); h.Teardown("x") }()
	h.Frame(dashcam.TList, []byte(`{"id":1}`))
	waitFor(t, "list", func() bool { return len(w.Sent(dashcam.TListResult)) == 1 })
	var res struct {
		Events []struct {
			Type          string `json:"type"`
			Path          string `json:"path"`
			TriggerCamKey string `json:"trigger_cam_key"`
		} `json:"events"`
	}
	json.Unmarshal(w.Sent(dashcam.TListResult)[0], &res)
	if len(res.Events) != 3 || res.Events[0].Type != "SentryClips" || res.Events[0].TriggerCamKey != "right_repeater" ||
		res.Events[0].Path != dashcam.TeslaCam+"/SentryClips/2026-11-16_07-56-30" || res.Events[2].Type != "RecentClips" {
		t.Fatalf("%s", w.Sent(dashcam.TListResult)[0])
	}
	p := r.cam + "/SentryClips/2026-11-16_07-56-30/2026-11-16_07-55-46-front.mp4"
	h.Frame(dashcam.TGet, []byte(fmt.Sprintf(`{"id":2,"path":%q,"offset":0,"length":0,"ss":40,"t":12}`, p)))
	h.Frame(dashcam.TGet, []byte(fmt.Sprintf(`{"id":3,"path":%q,"offset":1,"length":3}`, p)))
	waitFor(t, "chunks", func() bool { return len(w.Sent(dashcam.TChunk)) == 2 })
	c := w.Sent(dashcam.TChunk)
	if binary.BigEndian.Uint32(c[0]) != 2 || !bytes.Equal(c[0][20:], []byte("CUT("+p+")")) {
		t.Fatalf("cut chunk %q", c[0][20:])
	}
	if binary.BigEndian.Uint32(c[1]) != 3 || string(c[1][20:]) != "RON" || binary.BigEndian.Uint64(c[1][12:]) != 5 {
		t.Fatalf("range chunk %q", c[1][20:])
	}
	h.Teardown("close")
	waitFor(t, "cut files removed on disconnect", func() bool {
		ents, _ := os.ReadDir(filepath.Join(r.state, cutSubdir))
		return len(ents) == 0
	})
}
