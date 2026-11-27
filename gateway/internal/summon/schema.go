// Package summon holds the car-side Summon logic. schema.go is the gate
// between the phone and the car: the helper forwards only the Canonical bytes
// Parse returns, so whatever Parse accepts reaches the car verbatim.
package summon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// Kind is the frame type a body arrived in.
type Kind uint8

const (
	KindVerb      Kind = iota + 1 // VERB frames
	KindHeartbeat                 // HB frames
	KindMsg                       // MSG frames
)

// AbortJSON is what the helper sends for any accepted cmd_abort.
const AbortJSON = `{"msg_type":"autopark:cmd_abort"}`

var (
	// ErrDrop: skip this message, the session continues.
	ErrDrop = errors.New("summon: non-conforming message, dropped")
	// ErrFatal: the session ends.
	ErrFatal = errors.New("summon: non-conforming message, session ends")
)

// Message is a validated phone-to-car message.
type Message struct {
	Type             string  // msg_type
	Canonical        []byte  // exactly what the helper sends to the car
	Lat, Lon         float64 // verbs and device_location
	GoalLat, GoalLon float64
	HasGoal          bool // both goal_latitude and goal_longitude present
}

type valKind uint8

const (
	vNum      valKind = iota + 1 // finite JSON number
	vStr                         // any string
	vVersion2                    // the number 2, written "2"
	vSummon                      // "find_me" or "pin_drop"
	vNone                        // the string "<none>"
)

type field struct {
	kind     valKind
	optional bool
}

const tsKey = "created:timestamp"

// spec describes one msg_type: its frame, its keys, and whether a shape error
// is Fatal (motion verbs) or Drop (everything else).
type spec struct {
	frame        Kind
	fatalOnShape bool
	keys         map[string]field
}

var num = field{kind: vNum}
var str = field{kind: vStr}

var specs = map[string]spec{
	"autopark:cmd_find_me": {KindVerb, true, map[string]field{
		"latitude": num, "longitude": num, "accuracy": num,
		"goal_latitude": num, "goal_longitude": num,
		"client_version": {kind: vVersion2}, "summon_type": {kind: vSummon},
		tsKey: num,
	}},
	"autopark:cmd_forward": {KindVerb, true, map[string]field{
		"latitude": num, "longitude": num,
		"client_version": {kind: vVersion2}, tsKey: num,
	}},
	"autopark:cmd_reverse": {KindVerb, true, map[string]field{
		"latitude": num, "longitude": num,
		"client_version": {kind: vVersion2}, tsKey: num,
	}},
	"autopark:heartbeat_app_webrtc": {KindHeartbeat, false, map[string]field{
		"timestamp": num, "car_timestamp": {kind: vNum, optional: true}, tsKey: num,
	}},
	"autopark:device_location": {KindMsg, false, map[string]field{
		"latitude": num, "longitude": num, "accuracy": num,
		"altitude": num, "altitude_accuracy": num,
		"goal_latitude": num, "goal_longitude": num,
		"summon_type": {kind: vSummon}, tsKey: num,
	}},
	"autopark:info": {KindMsg, false, map[string]field{
		"txid": {kind: vNone}, "x_tesla_user_agent": str, "model": str,
		"name": str, "tag": str, "public_key_hash": str, tsKey: num,
	}},
	"control:keepalive": {KindMsg, false, map[string]field{
		tsKey: {kind: vNum, optional: true},
	}},
}

const abortType = "autopark:cmd_abort"

func drop(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrDrop, fmt.Sprintf(format, a...))
}

func fatal(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrFatal, fmt.Sprintf(format, a...))
}

// decodeObject parses body as one JSON object, numbers kept as json.Number.
// ok is false when body is not a JSON object at all; trailing is true when a
// valid object is followed by more non-whitespace data (a framing error).
func decodeObject(body []byte) (m map[string]any, ok, trailing bool) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&m); err != nil || m == nil {
		return nil, false, false
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, true, true
	}
	return m, true, false
}

// Parse validates body as a phone-to-car message of frame type k.
//
// Decision table (every error wraps ErrDrop or ErrFatal):
//
//	any frame, object followed by non-whitespace ... Fatal (framing error)
//	any frame, top-level "_request" key ............ Fatal (checked first)
//	VERB, body not a JSON object ................... Fatal
//	VERB, msg_type missing/not a verb/other frame .. Fatal
//	VERB, shape error ............................. Fatal
//	HB,   body not an object / no string msg_type .. Drop
//	HB,   msg_type not the heartbeat ............... Fatal
//	HB,   shape error ............................. Drop
//	MSG,  body not an object / no string msg_type .. Drop
//	MSG,  msg_type is a verb or the heartbeat ...... Fatal
//	MSG,  msg_type not in the table ................ Drop
//	MSG,  cmd_abort ................................ accepted, AbortJSON
//	MSG,  device_location/info/keepalive shape error  Drop
//	unknown Kind ................................... Fatal
func Parse(k Kind, body []byte) (Message, error) {
	if k != KindVerb && k != KindHeartbeat && k != KindMsg {
		return Message{}, fatal("unknown frame kind %d", k)
	}
	obj, ok, trailing := decodeObject(body)
	if trailing {
		return Message{}, fatal("data after the JSON object (framing error)")
	}
	if !ok {
		if k == KindVerb {
			return Message{}, fatal("verb body is not a JSON object")
		}
		return Message{}, drop("body is not a JSON object")
	}
	// R20: _request is checked before anything else, in every frame.
	if _, has := obj["_request"]; has {
		return Message{}, fatal("_request key")
	}
	typ, isStr := obj["msg_type"].(string)
	if !isStr {
		if k == KindVerb {
			return Message{}, fatal("verb has no string msg_type")
		}
		return Message{}, drop("no string msg_type")
	}

	if typ == abortType {
		if k != KindMsg {
			return Message{}, fatal("%s in frame kind %d", typ, k)
		}
		return Message{Type: typ, Canonical: []byte(AbortJSON)}, nil
	}

	sp, known := specs[typ]
	switch {
	case !known:
		if k == KindMsg {
			return Message{}, drop("unknown msg_type %q", typ)
		}
		return Message{}, fatal("unknown msg_type %q in frame kind %d", typ, k)
	case sp.frame != k:
		return Message{}, fatal("msg_type %q in the wrong frame (kind %d)", typ, k)
	}

	shapeErr := drop
	if sp.fatalOnShape {
		shapeErr = fatal
	}
	for key, v := range obj {
		if key == "msg_type" {
			continue
		}
		f, allowed := sp.keys[key]
		if !allowed {
			return Message{}, shapeErr("%s: unexpected key %q", typ, key)
		}
		if err := checkValue(f.kind, v); err != nil {
			return Message{}, shapeErr("%s: key %q: %v", typ, key, err)
		}
	}
	for key, f := range sp.keys {
		if _, have := obj[key]; !have && !f.optional {
			return Message{}, shapeErr("%s: missing key %q", typ, key)
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return Message{}, fatal("re-encode: %v", err)
	}
	m := Message{Type: typ, Canonical: bytes.TrimSuffix(buf.Bytes(), []byte("\n"))}
	m.Lat, _ = floatOf(obj["latitude"])
	m.Lon, _ = floatOf(obj["longitude"])
	gla, ok1 := floatOf(obj["goal_latitude"])
	glo, ok2 := floatOf(obj["goal_longitude"])
	if ok1 && ok2 {
		m.GoalLat, m.GoalLon, m.HasGoal = gla, glo, true
	}
	return m, nil
}

func floatOf(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func checkValue(kind valKind, v any) error {
	switch kind {
	case vNum:
		f, ok := floatOf(v)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("not a finite number")
		}
	case vVersion2:
		if n, ok := v.(json.Number); !ok || n.String() != "2" {
			return errors.New("must be exactly 2")
		}
	case vStr:
		if _, ok := v.(string); !ok {
			return errors.New("not a string")
		}
	case vSummon:
		if s, ok := v.(string); !ok || s != "find_me" && s != "pin_drop" {
			return errors.New("must be find_me or pin_drop")
		}
	case vNone:
		if s, ok := v.(string); !ok || s != "<none>" {
			return errors.New(`must be "<none>"`)
		}
	}
	return nil
}
