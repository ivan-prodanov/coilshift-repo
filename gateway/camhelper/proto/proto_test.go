package proto

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	small := []byte(`{"id":1,"op":"hold"}`)
	big := bytes.Repeat([]byte{0xAA}, 100000)
	if err := WriteFrame(&buf, TReq, small); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, TAU, big); err != nil {
		t.Fatal(err)
	}
	buf.Write(Encode(TPCM, nil))
	r := bufio.NewReader(&buf)
	for _, w := range []struct {
		t    byte
		body []byte
	}{{TReq, small}, {TAU, big}, {TPCM, []byte{}}} {
		typ, body, err := ReadFrame(r, MaxHelperFrame)
		if err != nil || typ != w.t || !bytes.Equal(body, w.body) {
			t.Fatalf("got %x len %d %v", typ, len(body), err)
		}
	}
	if _, _, err := ReadFrame(r, MaxHelperFrame); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestFrameLimits(t *testing.T) {
	for _, raw := range [][]byte{
		{0, 0, 0, 0},             // zero length
		{0, 1, 0, 1, TReq},       // 65537 > 64 KiB
		{0xFF, 0xFF, 0xFF, 0xFF}, // huge
	} {
		_, _, err := ReadFrame(bufio.NewReader(bytes.NewReader(raw)), MaxClientFrame)
		if !errors.Is(err, ErrFrame) {
			t.Fatalf("%x: want ErrFrame, got %v", raw, err)
		}
	}
	// Truncated body.
	_, _, err := ReadFrame(bufio.NewReader(bytes.NewReader([]byte{0, 0, 0, 5, TReq, 'a'})), MaxClientFrame)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("want unexpected EOF, got %v", err)
	}
}

func TestHeaders(t *testing.T) {
	b := make([]byte, AUHeader+3)
	PutAUHeader(b, 9901, FlagIDR|FlagParamSets, 123456789)
	port, flags, ok := AUPort(b)
	if !ok || port != 9901 || flags != 3 || b[3] != 0 {
		t.Fatalf("au header %v %d %d", ok, port, flags)
	}
	if _, _, ok := AUPort(b[:5]); ok {
		t.Fatal("short AU accepted")
	}
	d := make([]byte, DataHeader+2)
	PutDataHeader(d, 7, 1<<40)
	d[12], d[13] = 'x', 'y'
	id, total, data, err := ParseData(d)
	if err != nil || id != 7 || total != 1<<40 || string(data) != "xy" {
		t.Fatalf("data %d %d %q %v", id, total, data, err)
	}
	id, page, err := ParseList(ListBody(9, []byte("{}")))
	if err != nil || id != 9 || string(page) != "{}" {
		t.Fatalf("list %d %q %v", id, page, err)
	}
	if MaxAU+AUHeader != 1<<20 {
		t.Fatal("AU limit must keep VIDEO bodies at 1 MiB")
	}
	if MaxRead+20 != 65536 {
		t.Fatal("read limit must keep CHUNK bodies at 64 KiB")
	}
}
