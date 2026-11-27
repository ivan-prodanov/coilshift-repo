package dashcam

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"airgapp/gateway/camhelper/proto"
)

// This file is a line-for-line port of build_events() in the laptop viewer
// (tesla-dump/2026.38.6/re/bridge/camera/dashcam_viewer.py), which the app's
// src/hooks/useDashcamClips.ts parses. Same keys, same key order, same values;
// testdata/golden_events.json was produced by that Python from
// testdata/inventory.json (testdata/gen_golden.py). Differences, all for input
// the car never writes: thumb is always null (the phone fetches a short
// ss/t window instead); an event.json that is not UTF-8 or contains a Python
// line separator only loses its own fields (the viewer dropped the whole
// listing or mis-split lines); a non-string "timestamp" is kept but not
// parsed (the viewer crashed); str() of a JSON array/object "camera" is its
// JSON text, not Python's repr.

// TeslaCam is the clip root on the car, used to build event paths.
const TeslaCam = "/mnt/usb/TeslaCam"

var cams = map[string]bool{"front": true, "back": true, "left_repeater": true, "right_repeater": true, "left_pillar": true, "right_pillar": true}

// triggerCamMap maps event.json "camera" (the Autopilot detection-camera
// index, car-verified) to the recorded camera.
var triggerCamMap = map[string]string{
	"0": "front", "1": "front", "2": "front", "3": "left_pillar", "4": "right_pillar",
	"5": "left_repeater", "6": "right_repeater", "7": "back",
}

type seg struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// event is one viewer event. Raw JSON values are kept as written.
type event struct {
	typ, folder, path string
	reason, city      json.RawMessage
	lat, lon          json.RawMessage
	triggerCam        string
	timestamp         json.RawMessage // nil: key absent
	minutes           map[string]map[string]seg
	minuteList        []string
	start             string
	triggerCamKey     string
	triggerMinute     string
	triggerOffset     *float64
	recent            bool
}

var emptyStr = json.RawMessage(`""`)

// parseFname: '2026-11-13_07-12-49-front.mp4' -> ('2026-11-13_07-12-49', 'front').
func parseFname(name string) (string, string, bool) {
	if !strings.HasSuffix(name, ".mp4") {
		return "", "", false
	}
	stem := name[:len(name)-4]
	i := strings.LastIndexByte(stem, '-')
	if i < 0 {
		return "", "", false
	}
	ts, cam := stem[:i], stem[i+1:]
	if !cams[cam] {
		return "", "", false
	}
	return ts, cam, true
}

// pyLineBreaks are the separators Python's str.splitlines() splits on that
// `tr -d '\n\r\t'` leaves in an event.json line.
const pyLineBreaks = "\v\f\x1c\x1d\x1e\u0085  "

// applyEventJSON is the viewer's J| branch: all six fields from a JSON
// object, nothing otherwise.
func (e *event) applyEventJSON(raw []byte) {
	raw = bytes.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, raw)
	if !utf8.Valid(raw) || bytes.ContainsAny(raw, pyLineBreaks) {
		return
	}
	var j map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&j); err != nil || j == nil || dec.More() {
		return
	}
	get := func(k string) json.RawMessage {
		if v, ok := j[k]; ok {
			return compact(v)
		}
		return emptyStr
	}
	e.reason, e.city, e.lat, e.lon = get("reason"), get("city"), get("est_lat"), get("est_lon")
	e.triggerCam = pyStr(j["camera"])
	e.timestamp = get("timestamp")
}

func compact(v json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, v) != nil {
		return v
	}
	return b.Bytes()
}

// pyStr is Python's str() of a json.loads value (missing -> "").
func pyStr(v json.RawMessage) string {
	if v == nil {
		return ""
	}
	v = compact(v)
	switch {
	case string(v) == "null":
		return "None"
	case string(v) == "true":
		return "True"
	case string(v) == "false":
		return "False"
	case len(v) > 0 && v[0] == '"':
		var s string
		json.Unmarshal(v, &s)
		return s
	case len(v) > 0 && (v[0] == '-' || v[0] >= '0' && v[0] <= '9'):
		lit := string(v)
		if !strings.ContainsAny(lit, ".eE") {
			if lit == "-0" {
				return "0"
			}
			return lit
		}
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			return lit
		}
		return pyFloat(f)
	}
	return string(v)
}

// pyFloat is Python's repr() of a float.
func pyFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // shortest round-trip digits
	mant, expS, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expS)
	if exp < -4 || exp >= 16 {
		if !strings.Contains(mant, ".") {
			return mant + "e" + pyExp(exp)
		}
		return mant + "e" + pyExp(exp)
	}
	out := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(out, ".") {
		out += ".0"
	}
	return out
}

func pyExp(e int) string {
	sign := "+"
	if e < 0 {
		sign, e = "-", -e
	}
	if e < 10 {
		return sign + "0" + strconv.Itoa(e)
	}
	return sign + strconv.Itoa(e)
}

// Python strptime patterns for '%Y-%m-%dT%H:%M:%S' and '%Y-%m-%d_%H-%M-%S'
// (case-insensitive literals, 1-or-2-digit fields, as _strptime accepts).
const (
	reY = `(\d\d\d\d)`
	reM = `(1[0-2]|0[1-9]|[1-9])`
	reD = `(3[01]|[12]\d|0[1-9]|[1-9]| [1-9])`
	reH = `(2[0-3]|[0-1]\d|\d)`
	reN = `([0-5]\d|\d)`
	reS = `(6[0-1]|[0-5]\d|\d)`
)

var dtRes = []*regexp.Regexp{
	regexp.MustCompile(`^(?i)` + reY + `-` + reM + `-` + reD + `T` + reH + `:` + reN + `:` + reS + `$`),
	regexp.MustCompile(`^(?i)` + reY + `-` + reM + `-` + reD + `_` + reH + `-` + reN + `-` + reS + `$`),
}

// parseDT is the viewer's parse_dt on a string (local time; compared only
// with itself, so UTC is used).
func parseDT(s string) (time.Time, bool) {
	s = strings.TrimFunc(s, isPySpace)
	for _, re := range dtRes {
		m := re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		n := make([]int, 6)
		for i := range n {
			n[i], _ = strconv.Atoi(strings.TrimSpace(m[i+1]))
		}
		if n[5] > 59 {
			continue // datetime() rejects leap seconds
		}
		t := time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, time.UTC)
		if t.Day() != n[2] { // e.g. February 30
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

func isPySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func (e *event) timestampString() (string, bool) {
	if e.timestamp == nil {
		return "", true // absent: ev.get("timestamp", "")
	}
	var s string
	if err := json.Unmarshal(e.timestamp, &s); err != nil {
		var null any
		if json.Unmarshal(e.timestamp, &null) == nil && isFalsy(null) {
			return "", true
		}
		return "", false
	}
	return s, true
}

func isFalsy(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case json.Number:
		f, _ := x.Float64()
		return f == 0
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// finalize is the viewer's finalize(ev).
func (e *event) finalize() {
	mins := make([]string, 0, len(e.minutes))
	for m := range e.minutes {
		mins = append(mins, m)
	}
	sort.Strings(mins)
	e.minuteList = mins
	e.start = e.folder
	if len(mins) > 0 {
		e.start = mins[0]
	}
	e.triggerCamKey = triggerCamMap[e.triggerCam]
	tss, ok := e.timestampString()
	if !ok {
		return
	}
	ts, ok := parseDT(tss)
	if !ok || len(mins) == 0 {
		return
	}
	chosenIdx := -1
	var chosenT time.Time
	for i, m := range mins {
		md, ok := parseDT(m)
		if !ok {
			continue
		}
		if !md.After(ts) {
			chosenIdx, chosenT = i, md
		} else {
			break
		}
	}
	if chosenIdx < 0 {
		md0, ok := parseDT(mins[0])
		if !ok {
			return
		}
		chosenIdx, chosenT = 0, md0
	}
	off := ts.Sub(chosenT).Seconds()
	span := 60.0
	if chosenIdx+1 < len(mins) {
		if nxt, ok := parseDT(mins[chosenIdx+1]); ok {
			span = math.Min(span, nxt.Sub(chosenT).Seconds())
		}
	}
	if off <= span {
		e.triggerMinute = mins[chosenIdx]
		o := math.Max(0, math.Round(off*10)/10)
		e.triggerOffset = &o
	}
}

// buildEvents turns the helper's TeslaCam inventory into the viewer's events.
func buildEvents(inv proto.ListPage) []*event {
	var events []*event
	for _, d := range inv.Dirs {
		e := &event{
			typ: d.Type, folder: d.Folder, path: TeslaCam + "/" + d.Type + "/" + d.Folder,
			reason: emptyStr, city: emptyStr, lat: emptyStr, lon: emptyStr,
			minutes: map[string]map[string]seg{},
		}
		if d.HasEvent {
			e.applyEventJSON(d.Event)
		}
		for _, f := range d.Files {
			ts, cam, ok := parseFname(f.Name)
			if !ok {
				continue
			}
			if e.minutes[ts] == nil {
				e.minutes[ts] = map[string]seg{}
			}
			e.minutes[ts][cam] = seg{Path: e.path + "/" + f.Name, Size: f.Size}
		}
		if len(e.minutes) == 0 {
			continue
		}
		e.finalize()
		events = append(events, e)
	}
	recent := map[string]map[string]seg{}
	for _, f := range inv.Recent {
		ts, cam, ok := parseFname(f.Name)
		if !ok {
			continue
		}
		if recent[ts] == nil {
			recent[ts] = map[string]seg{}
		}
		recent[ts][cam] = seg{Path: TeslaCam + "/RecentClips/" + f.Name, Size: f.Size}
	}
	if len(recent) > 0 {
		rmins := make([]string, 0, len(recent))
		for m := range recent {
			rmins = append(rmins, m)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(rmins)))
		ts, _ := json.Marshal(rmins[0])
		events = append(events, &event{
			typ: "RecentClips", folder: "RecentClips", path: TeslaCam + "/RecentClips",
			reason: json.RawMessage(`"rolling buffer"`), city: emptyStr, lat: emptyStr, lon: emptyStr,
			timestamp: ts, minutes: recent, minuteList: rmins, start: rmins[0], recent: true,
		})
	}
	order := map[string]int{"SentryClips": 0, "SavedClips": 1, "RecentClips": 2}
	rank := func(t string) int {
		if r, ok := order[t]; ok {
			return r
		}
		return 9
	}
	sort.SliceStable(events, func(i, j int) bool {
		ri, rj := rank(events[i].typ), rank(events[j].typ)
		if ri != rj {
			return ri < rj
		}
		return events[i].start < events[j].start
	})
	return events
}

// MarshalJSON writes the keys in the viewer's order.
func (e *event) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	n := 0
	put := func(k string, v any) {
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		switch x := v.(type) {
		case json.RawMessage:
			b.Write(x)
		default:
			vb, _ := marshalNoEscape(x)
			b.Write(vb)
		}
	}
	var off any
	if e.triggerOffset != nil {
		off = json.RawMessage(pyFloat(*e.triggerOffset))
	}
	b.WriteByte('{')
	put("type", e.typ)
	put("folder", e.folder)
	put("path", e.path)
	put("reason", e.reason)
	put("city", e.city)
	put("lat", e.lat)
	put("lon", e.lon)
	put("trigger_cam", e.triggerCam)
	if e.recent {
		put("trigger_cam_key", "")
		put("trigger_minute", "")
		put("trigger_offset", nil)
		put("timestamp", e.timestamp)
		put("minutes", e.minutes)
		put("minute_list", e.minuteList)
		put("start", e.start)
		put("thumb", nil)
	} else {
		put("minutes", e.minutes)
		put("thumb", nil)
		if e.timestamp != nil {
			put("timestamp", e.timestamp)
		}
		put("minute_list", e.minuteList)
		put("start", e.start)
		put("trigger_cam_key", e.triggerCamKey)
		put("trigger_minute", e.triggerMinute)
		put("trigger_offset", off)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
