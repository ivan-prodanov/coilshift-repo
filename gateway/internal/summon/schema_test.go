package summon

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	findMe   = `{"msg_type":"autopark:cmd_find_me","latitude":52.5,"longitude":13.4,"accuracy":-1,"goal_latitude":52.51,"goal_longitude":13.41,"client_version":2,"summon_type":"pin_drop","created:timestamp":1700000000123}`
	forward  = `{"msg_type":"autopark:cmd_forward","latitude":52.5,"longitude":13.4,"client_version":2,"created:timestamp":1700000000123}`
	reverse  = `{"msg_type":"autopark:cmd_reverse","latitude":52.5,"longitude":13.4,"client_version":2,"created:timestamp":1700000000123}`
	hb       = `{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":1700000000123,"car_timestamp":99,"created:timestamp":1700000000124}`
	devLoc   = `{"msg_type":"autopark:device_location","latitude":52.5,"longitude":13.4,"accuracy":5,"altitude":-1,"altitude_accuracy":-1,"goal_latitude":52.51,"goal_longitude":13.41,"summon_type":"find_me","created:timestamp":1700000000123}`
	info     = `{"msg_type":"autopark:info","txid":"<none>","x_tesla_user_agent":"ua","model":"m","name":"n","tag":"t","public_key_hash":"h","created:timestamp":1700000000123}`
	keepaliv = `{"msg_type":"control:keepalive"}`
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		t.Fatalf("canonical not JSON: %v: %s", err, b)
	}
	return m
}

func TestParseAcceptsTeslaShapes(t *testing.T) {
	cases := []struct {
		name string
		k    Kind
		body string
		typ  string
		goal bool
	}{
		{"find_me", KindVerb, findMe, "autopark:cmd_find_me", true},
		{"forward", KindVerb, forward, "autopark:cmd_forward", false},
		{"reverse", KindVerb, reverse, "autopark:cmd_reverse", false},
		{"heartbeat", KindHeartbeat, hb, "autopark:heartbeat_app_webrtc", false},
		{"heartbeat no car_timestamp", KindHeartbeat, `{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":1,"created:timestamp":2}`, "autopark:heartbeat_app_webrtc", false},
		{"device_location", KindMsg, devLoc, "autopark:device_location", true},
		{"info", KindMsg, info, "autopark:info", false},
		{"keepalive", KindMsg, keepaliv, "control:keepalive", false},
		{"keepalive created", KindMsg, `{"msg_type":"control:keepalive","created:timestamp":3}`, "control:keepalive", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := Parse(c.k, []byte(c.body))
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if m.Type != c.typ {
				t.Fatalf("type %q", m.Type)
			}
			if m.HasGoal != c.goal {
				t.Fatalf("HasGoal %v", m.HasGoal)
			}
			got, want := decode(t, m.Canonical), decode(t, []byte(c.body))
			if len(got) != len(want) {
				t.Fatalf("keys differ: %v vs %v", got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("key %s: %v vs %v", k, got[k], v)
				}
			}
		})
	}
	m, _ := Parse(KindVerb, []byte(findMe))
	if m.Lat != 52.5 || m.Lon != 13.4 || m.GoalLat != 52.51 || m.GoalLon != 13.41 {
		t.Fatalf("coords %+v", m)
	}
	m, _ = Parse(KindMsg, []byte(devLoc))
	if m.Lat != 52.5 || m.GoalLon != 13.41 {
		t.Fatalf("coords %+v", m)
	}
}

func TestParseCanonicalKeepsNumbers(t *testing.T) {
	m, err := Parse(KindVerb, []byte(findMe))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"accuracy":-1`, `"client_version":2`, `"created:timestamp":1700000000123`} {
		if !strings.Contains(string(m.Canonical), s) {
			t.Errorf("missing %s in %s", s, m.Canonical)
		}
	}
	if bytes.ContainsAny(m.Canonical, " \n") {
		t.Errorf("not compact: %s", m.Canonical)
	}
}

func TestParseInfoCanonicalKeepsLiteralNone(t *testing.T) {
	m, err := Parse(KindMsg, []byte(info))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m.Canonical), `"txid":"<none>"`) {
		t.Fatalf("escaped: %s", m.Canonical)
	}
	if bytes.HasSuffix(m.Canonical, []byte("\n")) {
		t.Fatal("trailing newline")
	}
}

func TestParseAbortIsCanonical(t *testing.T) {
	m, err := Parse(KindMsg, []byte(`{"msg_type":"autopark:cmd_abort","created:timestamp":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Canonical) != AbortJSON || m.Type != "autopark:cmd_abort" {
		t.Fatalf("%+v", m)
	}
}

func TestParseFatal(t *testing.T) {
	cases := []struct {
		name string
		k    Kind
		body string
	}{
		{"find_me no accuracy", KindVerb, strings.Replace(findMe, `"accuracy":-1,`, "", 1)},
		{"summon_type auto", KindVerb, strings.Replace(findMe, "pin_drop", "auto", 1)},
		{"client_version 3", KindVerb, strings.Replace(findMe, `"client_version":2`, `"client_version":3`, 1)},
		{"verb carries device_location", KindVerb, devLoc},
		{"verb not json", KindVerb, `nope`},
		{"verb not object", KindVerb, `[1]`},
		{"verb no msg_type", KindVerb, `{"latitude":1}`},
		{"verb unknown type", KindVerb, `{"msg_type":"x"}`},
		{"verb carries abort", KindVerb, `{"msg_type":"autopark:cmd_abort"}`},
		{"hb carries forward", KindHeartbeat, forward},
		{"hb unknown type", KindHeartbeat, `{"msg_type":"x"}`},
		{"msg carries reverse", KindMsg, reverse},
		{"msg carries find_me", KindMsg, findMe},
		{"msg carries heartbeat", KindMsg, hb},
		{"_request in msg", KindMsg, `{"_request":{"a":1}}`},
		{"_request in verb", KindVerb, `{"_request":1,"msg_type":"autopark:cmd_forward"}`},
		{"_request in hb", KindHeartbeat, `{"_request":1,"msg_type":"autopark:heartbeat_app_webrtc"}`},
		{"_request in abort", KindMsg, `{"msg_type":"autopark:cmd_abort","_request":{}}`},
		{"_request in garbage msg type", KindMsg, `{"_request":1,"msg_type":"zzz"}`},
		{"forward lat string", KindVerb, strings.Replace(forward, `"latitude":52.5`, `"latitude":"52.5"`, 1)},
		{"forward missing created", KindVerb, strings.Replace(forward, `,"created:timestamp":1700000000123`, "", 1)},
		{"unknown kind", Kind(0), forward},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.k, []byte(c.body))
			if !errors.Is(err, ErrFatal) {
				t.Fatalf("want ErrFatal, got %v", err)
			}
			if errors.Is(err, ErrDrop) {
				t.Fatal("also ErrDrop")
			}
		})
	}
}

func TestParseDrop(t *testing.T) {
	cases := []struct {
		name string
		k    Kind
		body string
	}{
		{"device_location no goal_latitude", KindMsg, strings.Replace(devLoc, `"goal_latitude":52.51,`, "", 1)},
		{"device_location bad summon_type", KindMsg, strings.Replace(devLoc, "find_me", "x", 1)},
		{"info txid x", KindMsg, strings.Replace(info, "<none>", "x", 1)},
		{"info model number", KindMsg, strings.Replace(info, `"model":"m"`, `"model":1`, 1)},
		{"keepalive extra key", KindMsg, `{"msg_type":"control:keepalive","x":1}`},
		{"keepalive created string", KindMsg, `{"msg_type":"control:keepalive","created:timestamp":"1"}`},
		{"hb car_timestamp string", KindHeartbeat, strings.Replace(hb, `"car_timestamp":99`, `"car_timestamp":"99"`, 1)},
		{"hb no timestamp", KindHeartbeat, `{"msg_type":"autopark:heartbeat_app_webrtc","created:timestamp":1}`},
		{"hb not json", KindHeartbeat, `x`},
		{"hb no msg_type", KindHeartbeat, `{"timestamp":1}`},
		{"hb msg_type number", KindHeartbeat, `{"msg_type":1}`},
		{"msg cmd_pause", KindMsg, `{"msg_type":"autopark:cmd_pause"}`},
		{"msg not json", KindMsg, `nope`},
		{"msg not object", KindMsg, `"x"`},
		{"msg no msg_type", KindMsg, `{"a":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.k, []byte(c.body))
			if !errors.Is(err, ErrDrop) {
				t.Fatalf("want ErrDrop, got %v", err)
			}
			if errors.Is(err, ErrFatal) {
				t.Fatal("also ErrFatal")
			}
		})
	}
}

func TestParseExtraKeyOnVerbIsFatal(t *testing.T) {
	body := strings.Replace(forward, `"client_version":2`, `"client_version":2,"speed":1`, 1)
	if _, err := Parse(KindVerb, []byte(body)); !errors.Is(err, ErrFatal) {
		t.Fatalf("got %v", err)
	}
}

func TestParseRejectsNonFiniteAndOddNumbers(t *testing.T) {
	for _, n := range []string{"1e999", "-1e999"} {
		body := strings.Replace(forward, `"latitude":52.5`, `"latitude":`+n, 1)
		if _, err := Parse(KindVerb, []byte(body)); !errors.Is(err, ErrFatal) {
			t.Errorf("%s: got %v", n, err)
		}
	}
	body := strings.Replace(forward, `"client_version":2`, `"client_version":2.0`, 1)
	if _, err := Parse(KindVerb, []byte(body)); !errors.Is(err, ErrFatal) {
		t.Errorf("2.0: got %v", err)
	}
}

func TestParseTrailingDataIsFatal(t *testing.T) {
	cases := []struct {
		name string
		k    Kind
		body string
	}{
		{"verb", KindVerb, forward},
		{"hb", KindHeartbeat, hb},
		{"abort", KindMsg, AbortJSON},
		{"device_location", KindMsg, devLoc},
	}
	for _, c := range cases {
		for _, tail := range []string{" {}", "x"} {
			_, err := Parse(c.k, []byte(c.body+tail))
			if !errors.Is(err, ErrFatal) || errors.Is(err, ErrDrop) {
				t.Errorf("%s + %q: got %v", c.name, tail, err)
			}
		}
		for _, tail := range []string{"   ", "\t", "\n", " \t\n"} {
			m, err := Parse(c.k, []byte(c.body+tail))
			if err != nil {
				t.Errorf("%s + %q: %v", c.name, tail, err)
			} else if c.name == "abort" && string(m.Canonical) != AbortJSON {
				t.Errorf("abort canonical %s", m.Canonical)
			}
		}
	}
	for _, k := range []Kind{KindHeartbeat, KindMsg} {
		for _, b := range []string{`[]`, `"x"`, `null`} {
			if _, err := Parse(k, []byte(b)); !errors.Is(err, ErrDrop) {
				t.Errorf("kind %d %s: %v", k, b, err)
			}
		}
	}
	for _, b := range []string{`[]`, `"x"`, `null`} {
		if _, err := Parse(KindVerb, []byte(b)); !errors.Is(err, ErrFatal) {
			t.Errorf("verb %s: %v", b, err)
		}
	}
}
