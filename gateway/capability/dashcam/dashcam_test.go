package dashcam

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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"airgapp/gateway/camhelper/client"
	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability/testkit"
)

// ---- golden: the laptop viewer's build_events ----

type invFile struct {
	Dirs []struct {
		Type   string           `json:"type"`
		Folder string           `json:"folder"`
		Event  *string          `json:"event"`
		Files  []proto.DashFile `json:"files"`
	} `json:"dirs"`
	Recent []proto.DashFile `json:"recent"`
}

func loadInventory(t *testing.T) proto.ListPage {
	t.Helper()
	b, err := os.ReadFile("testdata/inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inv invFile
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatal(err)
	}
	var page proto.ListPage
	for _, d := range inv.Dirs {
		dd := proto.DashDir{Type: d.Type, Folder: d.Folder, Files: d.Files}
		if d.Event != nil {
			dd.HasEvent, dd.Event = true, []byte(*d.Event)
		}
		page.Dirs = append(page.Dirs, dd)
	}
	page.Recent = inv.Recent
	return page
}

func objectKeys(t *testing.T, b []byte) [][]string {
	t.Helper()
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, ev := range raw {
		dec := json.NewDecoder(bytes.NewReader(ev))
		dec.Token() // {
		var keys []string
		for dec.More() {
			k, _ := dec.Token()
			keys = append(keys, k.(string))
			var skip json.RawMessage
			dec.Decode(&skip)
		}
		out = append(out, keys)
	}
	return out
}

func TestEventsMatchTheLaptopViewer(t *testing.T) {
	golden, err := os.ReadFile("testdata/golden_events.json")
	if err != nil {
		t.Fatal(err)
	}
	evs := buildEvents(loadInventory(t))
	got, err := json.Marshal(evs)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	json.Unmarshal(got, &g)
	json.Unmarshal(golden, &w)
	if !reflect.DeepEqual(g, w) {
		gi, _ := json.MarshalIndent(g, "", " ")
		t.Fatalf("events differ from the Python viewer\n got %s", gi)
	}
	if gk, wk := objectKeys(t, got), objectKeys(t, golden); !reflect.DeepEqual(gk, wk) {
		t.Fatalf("key order\n got %v\nwant %v", gk, wk)
	}
	// Floats are written as Python writes them.
	if !bytes.Contains(got, []byte(`"trigger_offset":44.0`)) || !bytes.Contains(got, []byte(`"trigger_offset":0.0`)) {
		t.Fatal("trigger_offset not in Python float form")
	}
}

func TestPyHelpers(t *testing.T) {
	for in, want := range map[string]string{
		`"6"`: "6", `6`: "6", `-0`: "0", `6.0`: "6.0", `6.5`: "6.5", `1e16`: "1e+16", `1.5e-5`: "1.5e-05",
		`0.0001`: "0.0001", `123456789012345.0`: "123456789012345.0", `null`: "None", `true`: "True", `false`: "False",
	} {
		if got := pyStr(json.RawMessage(in)); got != want {
			t.Errorf("pyStr(%s) = %q, want %q", in, got, want)
		}
	}
	if pyStr(nil) != "" {
		t.Fatal("missing camera")
	}
	for s, ok := range map[string]bool{
		"2026-11-16T07:56:30": true, "2026-11-16_07-56-30": true, " 2026-11-16t07:56:30 ": true, "2026-1-6T7:5:3": true,
		"2026-02-29T00:00:00": false, "2024-02-29T00:00:00": true, "2026-11-16T07:56:60": false, "2026-11-16T07:56:305": false,
		"2026-11-16 07:56:30": false, "": false, "2026-13-01T00:00:00": false,
	} {
		if _, got := parseDT(s); got != ok {
			t.Errorf("parseDT(%q) = %v", s, got)
		}
	}
}

func TestEmptyInventory(t *testing.T) {
	parts := packParts(9, buildEvents(proto.ListPage{}))
	if len(parts) != 1 || string(parts[0]) != `{"id":9,"part":0,"last":true,"events":[]}` {
		t.Fatalf("%s", parts)
	}
}

func TestPartsSplitAndBounded(t *testing.T) {
	var page proto.ListPage
	for i := 0; i < 400; i++ {
		d := proto.DashDir{Type: "SentryClips", Folder: fmt.Sprintf("2026-11-%02d_%02d-00-00", 1+i%28, i%24)}
		for m := 0; m < 3; m++ {
			for _, c := range []string{"back", "front", "left_pillar", "left_repeater", "right_pillar", "right_repeater"} {
				d.Files = append(d.Files, proto.DashFile{Name: fmt.Sprintf("2026-11-%02d_%02d-%02d-00-%s.mp4", 1+i%28, i%24, m, c), Size: 40000000})
			}
		}
		page.Dirs = append(page.Dirs, d)
	}
	evs := buildEvents(page)
	parts := packParts(77, evs)
	if len(parts) < 3 {
		t.Fatalf("%d parts", len(parts))
	}
	var all []json.RawMessage
	for i, p := range parts {
		if len(p) > MaxPart {
			t.Fatalf("part %d is %d bytes", i, len(p))
		}
		var r struct {
			ID     uint32            `json:"id"`
			Part   int               `json:"part"`
			Last   bool              `json:"last"`
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(p, &r); err != nil {
			t.Fatal(err)
		}
		if r.ID != 77 || r.Part != i || r.Last != (i == len(parts)-1) || len(r.Events) == 0 {
			t.Fatalf("part %d header %+v", i, r)
		}
		all = append(all, r.Events...)
	}
	whole, _ := json.Marshal(evs)
	joined, _ := json.Marshal(all)
	if !bytes.Equal(whole, joined) {
		t.Fatal("parts do not concatenate to the full event list")
	}
}

func TestHugeRecentTrimmedToFit(t *testing.T) {
	var page proto.ListPage
	for m := 0; m < 200; m++ {
		for _, c := range []string{"back", "front", "left_pillar", "left_repeater", "right_pillar", "right_repeater"} {
			page.Recent = append(page.Recent, proto.DashFile{Name: fmt.Sprintf("2026-11-20_%02d-%02d-00-%s.mp4", m/60, m%60, c), Size: 1})
		}
	}
	parts := packParts(1, buildEvents(page))
	if len(parts) != 1 || len(parts[0]) > MaxPart {
		t.Fatalf("%d parts, %d bytes", len(parts), len(parts[0]))
	}
	var r struct {
		Events []struct {
			MinuteList []string                   `json:"minute_list"`
			Minutes    map[string]json.RawMessage `json:"minutes"`
		} `json:"events"`
	}
	json.Unmarshal(parts[0], &r)
	ml := r.Events[0].MinuteList
	if ml[0] != "2026-11-20_03-19-00" || len(ml) == 200 || len(ml) != len(r.Events[0].Minutes) {
		t.Fatalf("kept %d minutes from %s", len(ml), ml[0])
	}
}

// ---- the capability with a fake helper ----

type fakeHelper struct {
	mu      sync.Mutex
	files   map[string][]byte
	inv     proto.ListPage
	listErr error
	readErr map[string]string
	cutErr  string
	cuts    []string
	reads   int
	block   chan struct{}
	done    chan struct{}
	closed  int
}

func newFake() *fakeHelper {
	return &fakeHelper{files: map[string][]byte{}, readErr: map[string]string{}, done: make(chan struct{})}
}

func (f *fakeHelper) DashList(ctx context.Context) (proto.ListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inv, f.listErr
}

func (f *fakeHelper) DashRead(ctx context.Context, path, cut string, off int64, n int) ([]byte, int64, error) {
	f.mu.Lock()
	blk := f.block
	f.reads++
	f.mu.Unlock()
	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= 0 || n > proto.MaxRead {
		return nil, 0, &client.Error{Code: proto.ErrBadRequest}
	}
	key := path
	if cut != "" {
		key = "cut:" + cut
	}
	if code := f.readErr[key]; code != "" {
		return nil, 0, &client.Error{Code: code}
	}
	b, ok := f.files[key]
	if !ok {
		return nil, 0, &client.Error{Code: proto.ErrNotFound}
	}
	if off >= int64(len(b)) {
		return []byte{}, int64(len(b)), nil
	}
	end := min(off+int64(n), int64(len(b)))
	return append([]byte(nil), b[off:end]...), int64(len(b)), nil
}

func (f *fakeHelper) DashCut(ctx context.Context, path string, ss, t float64) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cuts = append(f.cuts, fmt.Sprintf("%s|%.2f|%.2f", path, ss, t))
	if f.cutErr != "" {
		return "", 0, &client.Error{Code: f.cutErr}
	}
	tok := fmt.Sprintf("tok%d", len(f.cuts))
	f.files["cut:"+tok] = []byte("cut-of-" + path)
	return tok, int64(len(f.files["cut:"+tok])), nil
}

func (f *fakeHelper) Done() <-chan struct{} { return f.done }
func (f *fakeHelper) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed == 0 {
		close(f.done)
	}
	f.closed++
	return nil
}

type rig struct {
	t    *testing.T
	w    *testkit.FakeWindow
	h    *handler
	fh   *fakeHelper
	fail bool
	mu   sync.Mutex
}

func open(t *testing.T, cfg ...func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, w: testkit.NewFakeWindow(), fh: newFake()}
	c := Config{
		Dial: func() (Helper, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.fail {
				return nil, errors.New("no helper")
			}
			return r.fh, nil
		},
		Root: TeslaCam, ReadTimeout: time.Second, RateBPS: 1 << 30, Log: log.New(io.Discard, "", 0),
	}
	for _, f := range cfg {
		f(&c)
	}
	h, err := NewWithConfig(c).Open(context.Background(), r.w, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.h = h.(*handler)
	t.Cleanup(func() { r.w.Close("test"); r.h.Teardown("test") })
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type chunk struct {
	id         uint32
	off, total uint64
	data       []byte
}

func (r *rig) chunks() []chunk {
	var out []chunk
	for _, b := range r.w.Sent(TChunk) {
		out = append(out, chunk{binary.BigEndian.Uint32(b[0:4]), binary.BigEndian.Uint64(b[4:12]), binary.BigEndian.Uint64(b[12:20]), b[20:]})
	}
	return out
}

func (r *rig) errors() []string {
	var out []string
	for _, b := range r.w.Sent(TError) {
		out = append(out, string(b))
	}
	return out
}

func (r *rig) get(body string) {
	r.t.Helper()
	if err := r.h.Frame(TGet, []byte(body)); err != nil {
		r.t.Fatal(err)
	}
}

func TestSpec(t *testing.T) {
	s := New().Spec()
	if s.Name != "DASHCAM" || s.ID != 0x03 || s.LeaseMS != 20000 ||
		fmt.Sprint(s.In) != fmt.Sprint(map[byte]int{0x40: 256, 0x41: 512, 0x42: 64}) ||
		fmt.Sprint(s.Out) != fmt.Sprint(map[byte]int{0xC0: 65536, 0xC1: 65536, 0xC2: 256}) || !s.Bulk[0xC1] || len(s.Bulk) != 1 {
		t.Fatalf("%+v", s)
	}
	if _, err := New().Open(context.Background(), testkit.NewFakeWindow(), json.RawMessage(`{"x":1}`)); err == nil {
		t.Fatal("unknown ARM param accepted")
	}
}

func TestList(t *testing.T) {
	r := open(t)
	r.fh.inv = loadInventory(t)
	if err := r.h.Frame(TList, []byte(`{"id":5}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "LIST_RESULT", func() bool { return len(r.w.Sent(TListResult)) > 0 })
	var res struct {
		ID     uint32 `json:"id"`
		Part   int    `json:"part"`
		Last   bool   `json:"last"`
		Events []any  `json:"events"`
	}
	json.Unmarshal(r.w.Sent(TListResult)[0], &res)
	if res.ID != 5 || res.Part != 0 || !res.Last || len(res.Events) != 15 {
		t.Fatalf("%+v", res)
	}
	r.fh.listErr = errors.New("session gone")
	r.h.Frame(TList, []byte(`{"id":6}`))
	waitFor(t, "ERROR", func() bool { return len(r.errors()) == 1 })
	if e := r.errors()[0]; e != `{"id":6,"error":"io"}` {
		t.Fatal(e)
	}
}

func TestGetWholeFileInChunks(t *testing.T) {
	r := open(t)
	p := TeslaCam + "/RecentClips/2026-11-20_06-00-00-front.mp4"
	data := make([]byte, 200000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	r.fh.files[p] = data
	r.get(fmt.Sprintf(`{"id":1,"path":%q,"offset":0,"length":0}`, p))
	waitFor(t, "chunks", func() bool { return len(r.chunks()) == 4 })
	var got []byte
	for i, c := range r.chunks() {
		if c.id != 1 || c.total != 200000 || c.off != uint64(i*MaxChunk) || len(c.data) > MaxChunk {
			t.Fatalf("chunk %d %+v", i, c)
		}
		got = append(got, c.data...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data")
	}
	for _, b := range r.w.Sent(TChunk) {
		if len(b) > MaxPart {
			t.Fatal("CHUNK over 64 KiB")
		}
	}
}

func TestGetRangeAndPastEnd(t *testing.T) {
	r := open(t)
	p := TeslaCam + "/SavedClips/x/a-front.mp4"
	r.fh.files[p] = bytes.Repeat([]byte("abcdefghij"), 10000)
	r.get(fmt.Sprintf(`{"id":2,"path":%q,"offset":100,"length":1000}`, p))
	r.get(fmt.Sprintf(`{"id":3,"path":%q,"offset":99990,"length":1000}`, p))
	r.get(fmt.Sprintf(`{"id":4,"path":%q,"offset":500000,"length":10}`, p))
	waitFor(t, "chunks", func() bool { return len(r.chunks()) == 3 })
	c := r.chunks()
	if c[0].id != 2 || c[0].off != 100 || len(c[0].data) != 1000 || !bytes.Equal(c[0].data, r.fh.files[p][100:1100]) {
		t.Fatalf("range %+v", c[0].off)
	}
	if c[1].id != 3 || string(c[1].data) != "abcdefghij" || c[1].total != 100000 {
		t.Fatalf("tail %+v", c[1])
	}
	if c[2].id != 4 || len(c[2].data) != 0 || c[2].off != 500000 || c[2].total != 100000 {
		t.Fatalf("past end %+v", c[2])
	}
}

func TestGetWindowUsesCut(t *testing.T) {
	r := open(t)
	p := TeslaCam + "/SentryClips/e/2026-11-16_07-55-46-front.mp4"
	r.get(fmt.Sprintf(`{"id":7,"path":%q,"offset":0,"length":0,"ss":41,"t":12}`, p))
	waitFor(t, "chunk", func() bool { return len(r.chunks()) == 1 })
	if c := r.chunks()[0]; string(c.data) != "cut-of-"+p || int(c.total) != len("cut-of-"+p) {
		t.Fatalf("%+v", c)
	}
	if r.fh.cuts[0] != p+"|41.00|12.00" {
		t.Fatal(r.fh.cuts)
	}
	r.fh.cutErr = proto.ErrCutFailed
	r.get(fmt.Sprintf(`{"id":8,"path":%q,"offset":0,"length":0,"ss":1,"t":2}`, p))
	waitFor(t, "error", func() bool { return len(r.errors()) == 1 })
	if r.errors()[0] != `{"id":8,"error":"cut_failed"}` {
		t.Fatal(r.errors())
	}
}

func TestGetErrors(t *testing.T) {
	r := open(t)
	r.fh.readErr[TeslaCam+"/forbidden.mp4"] = proto.ErrForbidden
	r.get(`{"id":1,"path":"/etc/shadow","offset":0,"length":0}`)
	r.get(`{"id":2,"path":"/mnt/usb/TeslaCamX/a.mp4","offset":0,"length":0}`)
	r.get(`{"id":3,"path":"/mnt/usb/TeslaCam/../../etc/passwd","offset":0,"length":0}`)
	r.get(`{"id":4,"path":"/mnt/usb/TeslaCam/missing.mp4","offset":0,"length":0}`)
	r.get(`{"id":5,"path":"/mnt/usb/TeslaCam/forbidden.mp4","offset":0,"length":0}`)
	waitFor(t, "errors", func() bool { return len(r.errors()) == 5 })
	got := strings.Join(r.errors(), "\n")
	for _, want := range []string{
		`{"id":1,"error":"forbidden_path"}`, `{"id":2,"error":"forbidden_path"}`, `{"id":3,"error":"forbidden_path"}`,
		`{"id":4,"error":"not_found"}`, `{"id":5,"error":"forbidden_path"}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in\n%s", want, got)
		}
	}
	if r.fh.reads != 2 {
		t.Fatalf("%d helper reads: string-refused paths must not reach the helper", r.fh.reads)
	}
}

func TestHelperDownIsIO(t *testing.T) {
	r := open(t)
	r.mu.Lock()
	r.fail = true
	r.mu.Unlock()
	r.get(`{"id":1,"path":"/mnt/usb/TeslaCam/a.mp4","offset":0,"length":0}`)
	r.h.Frame(TList, []byte(`{"id":2}`))
	waitFor(t, "errors", func() bool { return len(r.errors()) == 2 })
	if !strings.Contains(strings.Join(r.errors(), ""), `"error":"io"`) {
		t.Fatal(r.errors())
	}
}

func TestBadFramesTearDown(t *testing.T) {
	r := open(t)
	for _, c := range []struct {
		t    byte
		body string
	}{
		{TList, `{}`}, {TList, `{"id":-1}`}, {TList, `{"id":1,"x":2}`}, {TGet, ``}, {TGet, `{"id":1,"path":"/mnt/usb/TeslaCam/a","offset":-1,"length":0}`},
		{TGet, `{"id":1,"path":"/mnt/usb/TeslaCam/a","offset":0,"length":0,"ss":1}`},
		{TGet, `{"id":1,"path":"/mnt/usb/TeslaCam/a","offset":0,"length":0,"ss":1,"t":121}`},
		{TGet, `{"id":1,"path":"/mnt/usb/TeslaCam/a","offset":0,"length":0,"ss":-1,"t":5}`},
		{TCancel, `{}`}, {0x4F, `{}`},
	} {
		if err := r.h.Frame(c.t, []byte(c.body)); err == nil {
			t.Fatalf("0x%02x %s accepted", c.t, c.body)
		}
	}
}

func TestCancelStopsATransfer(t *testing.T) {
	r := open(t, func(c *Config) { c.RateBPS = 200000 }) // ~0.3 s between chunks
	p := TeslaCam + "/RecentClips/big-front.mp4"
	r.fh.files[p] = make([]byte, 10<<20)
	r.get(fmt.Sprintf(`{"id":9,"path":%q,"offset":0,"length":0}`, p))
	waitFor(t, "first chunk", func() bool { return len(r.chunks()) == 1 })
	r.h.Frame(TCancel, []byte(`{"id":9}`))
	q := TeslaCam + "/RecentClips/small-front.mp4"
	r.fh.mu.Lock()
	r.fh.files[q] = []byte("hello")
	r.fh.mu.Unlock()
	r.get(fmt.Sprintf(`{"id":10,"path":%q,"offset":0,"length":0}`, q))
	waitFor(t, "next request served", func() bool { return len(r.chunks()) == 2 })
	time.Sleep(50 * time.Millisecond)
	if c := r.chunks(); len(c) != 2 || c[1].id != 10 {
		t.Fatalf("chunks after cancel: %d", len(c))
	}
}

func TestFlowControlWaitsForTheLane(t *testing.T) {
	r := open(t, func(c *Config) { c.QueueBytes = 100 })
	p := TeslaCam + "/a-front.mp4"
	r.fh.files[p] = []byte("x")
	r.w.SetQueuedBytes(1 << 20)
	r.get(fmt.Sprintf(`{"id":1,"path":%q,"offset":0,"length":0}`, p))
	time.Sleep(50 * time.Millisecond)
	if len(r.chunks()) != 0 {
		t.Fatal("read while the bulk lane was full")
	}
	r.w.SetQueuedBytes(0)
	waitFor(t, "chunk", func() bool { return len(r.chunks()) == 1 })
}

func TestQueueFullAndDuplicateAreBusy(t *testing.T) {
	r := open(t)
	r.fh.block = make(chan struct{})
	p := TeslaCam + "/a-front.mp4"
	r.fh.files[p] = []byte("x")
	for id := 1; id <= maxQueued+2; id++ {
		r.get(fmt.Sprintf(`{"id":%d,"path":%q,"offset":0,"length":0}`, id, p))
	}
	r.get(fmt.Sprintf(`{"id":3,"path":%q,"offset":0,"length":0}`, p)) // duplicate of a queued id
	waitFor(t, "busy", func() bool { return len(r.errors()) >= 2 })
	got := strings.Join(r.errors(), "")
	if strings.Count(got, `"busy"`) < 2 || !strings.Contains(got, `{"id":3,"error":"busy"}`) {
		t.Fatal(got)
	}
	close(r.fh.block)
}

func TestTeardownBoundedAndClosesSession(t *testing.T) {
	r := open(t)
	r.fh.block = make(chan struct{}) // a read that never answers
	p := TeslaCam + "/a-front.mp4"
	r.fh.files[p] = []byte("x")
	r.get(fmt.Sprintf(`{"id":1,"path":%q,"offset":0,"length":0}`, p))
	waitFor(t, "read in flight", func() bool { r.fh.mu.Lock(); defer r.fh.mu.Unlock(); return r.fh.reads == 1 })
	start := time.Now()
	r.h.Teardown("open overran") // window not closed: must not wait on Done
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("Teardown took %v", d)
	}
	if r.fh.closed != 1 {
		t.Fatal("helper session not closed")
	}
	close(r.fh.block)
}

func TestRateLimit(t *testing.T) {
	r := open(t, func(c *Config) { c.RateBPS = 1 << 20 })
	p := TeslaCam + "/a-front.mp4"
	r.fh.files[p] = make([]byte, 300000)
	start := time.Now()
	r.get(fmt.Sprintf(`{"id":1,"path":%q,"offset":0,"length":0}`, p))
	waitFor(t, "all chunks", func() bool { return len(r.chunks()) == 5 })
	if d := time.Since(start); d < 180*time.Millisecond {
		t.Fatalf("300 KB at 1 MiB/s took only %v", d)
	}
}
