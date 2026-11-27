package wire

import (
	"bytes"
	"errors"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 1024} {
		body := bytes.Repeat([]byte{0xab}, n)
		var buf bytes.Buffer
		if err := WriteFrame(&buf, body); err != nil {
			t.Fatalf("write %d: %v", n, err)
		}
		got, err := ReadFrame(&buf, 1024)
		if err != nil {
			t.Fatalf("read %d: %v", n, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("round trip mismatch for %d bytes", n)
		}
	}
}

func TestFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, make([]byte, 1025)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrame(&buf, 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestFrameHostileLength(t *testing.T) {
	r := bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3})
	if _, err := ReadFrame(r, 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if r.Len() != 3 {
		t.Fatalf("reader consumed %d bytes, want 4", 7-r.Len())
	}
}
