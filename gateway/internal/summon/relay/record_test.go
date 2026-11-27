package relay

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"
)

// TestRecordRoundTrip: each record type has the exact wire form type ‖ length (u16 BE) ‖ payload, and reads back as
// written.
func TestRecordRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Owned(); err != nil {
		t.Fatal(err)
	}
	if err := w.Link(true); err != nil {
		t.Fatal(err)
	}
	if err := w.Link(false); err != nil {
		t.Fatal(err)
	}
	if err := w.Inner([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	wire := []byte{
		0x03, 0x00, 0x00, // OWNED
		0x02, 0x00, 0x01, 0x01, // LINK 1
		0x02, 0x00, 0x01, 0x00, // LINK 0
		0x01, 0x00, 0x03, 'a', 'b', 'c', // INNER "abc"
	}
	if !bytes.Equal(buf.Bytes(), wire) {
		t.Fatalf("wire % x, want % x", buf.Bytes(), wire)
	}

	// Each payload is the caller's to keep: they are checked only after the last Next.
	type rec struct {
		typ byte
		p   []byte
	}
	r := NewReader(bytes.NewReader(wire), true)
	var got []rec
	for i := 0; i < 4; i++ {
		typ, p, err := r.Next()
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		got = append(got, rec{typ, p})
	}
	if _, _, err := r.Next(); err != io.EOF {
		t.Fatalf("after the last record: %v, want io.EOF", err)
	}
	for i, want := range []rec{{RecOwned, nil}, {RecLink, []byte{1}}, {RecLink, []byte{0}}, {RecInner, []byte("abc")}} {
		if got[i].typ != want.typ || !bytes.Equal(got[i].p, want.p) {
			t.Fatalf("record %d = 0x%02x % x, want 0x%02x % x", i, got[i].typ, got[i].p, want.typ, want.p)
		}
	}

	// The other way, agw to the helper: INNER only, with no OWNED expected.
	buf.Reset()
	if err := NewWriter(&buf).Inner([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	r = NewReader(&buf, false)
	if typ, p, err := r.Next(); err != nil || typ != RecInner || string(p) != "hello" {
		t.Fatalf("Next = 0x%02x %q %v, want INNER \"hello\"", typ, p, err)
	}
}

// TestInnerSplitsAtMax: 40000 bytes become 3 INNER records of 16384, 16384 and 7232 bytes, and arrive intact.
func TestInnerSplitsAtMax(t *testing.T) {
	data := make([]byte, 40000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var buf bytes.Buffer
	if err := NewWriter(&buf).Inner(data); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != len(data)+3*3 {
		t.Fatalf("%d bytes on the wire, want %d (3 records)", buf.Len(), len(data)+3*3)
	}
	r := NewReader(&buf, false)
	var sizes []int
	var got []byte
	for {
		typ, p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if typ != RecInner {
			t.Fatalf("record type 0x%02x, want INNER", typ)
		}
		sizes = append(sizes, len(p))
		got = append(got, p...)
	}
	if want := []int{16384, 16384, 7232}; !slices.Equal(sizes, want) {
		t.Fatalf("record sizes %v, want %v", sizes, want)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("the payloads do not rebuild the data")
	}
}

// TestInnerEmptyWritesNothing: an empty INNER is invalid, so Inner of nothing writes nothing.
func TestInnerEmptyWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).Inner(nil); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("Inner(nil) wrote % x", buf.Bytes())
	}
}

// TestReaderRejects: every grammar violation ends the reads with an error, before the payload is read, and the error
// sticks.
func TestReaderRejects(t *testing.T) {
	owned := []byte{0x03, 0x00, 0x00}
	cat := func(bs ...[]byte) []byte { return bytes.Join(bs, nil) }
	for _, tc := range []struct {
		name        string
		expectOwned bool
		wire        []byte
		good        int // records read before the bad one
	}{
		{"zero-length INNER", false, []byte{0x01, 0x00, 0x00}, 0},
		{"INNER of 16385", false, []byte{0x01, 0x40, 0x01}, 0}, // no payload follows: rejected on the header
		{"INNER of 65535", false, []byte{0x01, 0xff, 0xff}, 0},
		{"type 0x04", false, []byte{0x04, 0x00, 0x00}, 0},
		{"type 0x00", false, []byte{0x00, 0x00, 0x00}, 0},
		{"LINK payload of 2", true, cat(owned, []byte{0x02, 0x00, 0x01, 0x02}), 1},
		{"LINK of 2 bytes", true, cat(owned, []byte{0x02, 0x00, 0x02, 0x00, 0x01}), 1},
		{"LINK of 0 bytes", true, cat(owned, []byte{0x02, 0x00, 0x00}), 1},
		{"OWNED twice", true, cat(owned, owned), 1},
		{"OWNED after INNER", true, cat(owned, []byte{0x01, 0x00, 0x01, 'x'}, owned), 2},
		{"OWNED with a payload", true, []byte{0x03, 0x00, 0x01, 0x00}, 0},
		{"INNER before OWNED", true, []byte{0x01, 0x00, 0x01, 'x'}, 0},
		{"LINK before OWNED", true, []byte{0x02, 0x00, 0x01, 0x01}, 0},
		{"OWNED not expected", false, owned, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(bytes.NewReader(tc.wire), tc.expectOwned)
			for i := 0; i < tc.good; i++ {
				if _, _, err := r.Next(); err != nil {
					t.Fatalf("record %d: %v", i, err)
				}
			}
			typ, p, err := r.Next()
			if !errors.Is(err, errBadRecord) {
				t.Fatalf("Next = 0x%02x % x %v, want a bad-record error", typ, p, err)
			}
			if _, _, again := r.Next(); again != err {
				t.Fatalf("a later Next = %v, want the same error: it sticks", again)
			}
		})
	}
}

// TestReaderTruncated: a stream cut inside a record is io.ErrUnexpectedEOF; cut between records, io.EOF.
func TestReaderTruncated(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
		want error
	}{
		{"empty", nil, io.EOF},
		{"inside a header", []byte{0x01, 0x00}, io.ErrUnexpectedEOF},
		{"inside a payload", []byte{0x01, 0x00, 0x02, 'x'}, io.ErrUnexpectedEOF},
		{"inside a LINK", []byte{0x02, 0x00, 0x01}, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := NewReader(bytes.NewReader(tc.wire), false).Next(); err != tc.want {
				t.Fatalf("Next: %v, want %v", err, tc.want)
			}
		})
	}
}

// TestWriterOwnedOnlyFirst: OWNED is the first record of a connection, once.
func TestWriterOwnedOnlyFirst(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Owned(); err != nil {
		t.Fatal(err)
	}
	if err := w.Owned(); err == nil {
		t.Fatal("a second OWNED was written")
	}
	buf.Reset()
	w = NewWriter(&buf)
	if err := w.Inner([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Owned(); err == nil {
		t.Fatal("OWNED was written after INNER")
	}
	if want := []byte{0x01, 0x00, 0x01, 'x'}; !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("wire % x, want % x", buf.Bytes(), want)
	}
}

// failWriter fails its first Write and counts every Write.
type failWriter struct{ calls int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.calls++
	return 0, errors.New("broken pipe")
}

// TestWriterErrorSticks: after a failed write the stream may hold part of a record, so nothing more is written.
func TestWriterErrorSticks(t *testing.T) {
	f := &failWriter{}
	w := NewWriter(f)
	if err := w.Inner([]byte("x")); err == nil {
		t.Fatal("Inner on a failing writer succeeded")
	}
	if err := w.Link(true); err == nil {
		t.Fatal("Link after a failed write succeeded")
	}
	if err := w.Inner([]byte("y")); err == nil {
		t.Fatal("Inner after a failed write succeeded")
	}
	if f.calls != 1 {
		t.Fatalf("%d writes, want 1: nothing is written after a failure", f.calls)
	}
}
