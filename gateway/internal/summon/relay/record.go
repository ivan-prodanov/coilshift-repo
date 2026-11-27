// Package relay carries the phone's inner session over relay.sock, between agw (uid 4950), which relays it, and the
// summon helper (uid 1337), which serves it. It holds the record codec both ends use, and the helper's listener.
//
// A record is type (u8) ‖ length (u16 BE) ‖ payload:
//   - INNER 0x01: 1..16384 bytes of the inner session's byte stream, either way;
//   - LINK 0x02: helper to agw, 1 byte, 0 or 1 (the link flag);
//   - OWNED 0x03: helper to agw, empty, the first record of a connection, exactly once.
//
// Anything else ends the connection: an unknown type, an empty or oversize INNER, a LINK other than 0 or 1, or an
// OWNED anywhere but first.
package relay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	RecInner  byte = 0x01
	RecLink   byte = 0x02
	RecOwned  byte = 0x03
	MaxInner       = 16384            // the largest INNER payload
	FirstWait      = 10 * time.Second // how long the helper waits for a connection's first INNER record

	// DefaultSocket is relay.sock on the car: the helper listens on it, and agw dials it.
	DefaultSocket = "/run/airgapp-summon/relay.sock"
)

// errBadRecord is a record that breaks the grammar: the connection must be closed.
var errBadRecord = errors.New("relay: bad record")

// Writer writes records to one connection. Its methods may be called concurrently: each record goes out whole, and so
// do all the records of one Inner call. After a failed write every later call returns that error, since the stream
// may hold part of a record.
type Writer struct {
	mu    sync.Mutex
	w     io.Writer
	buf   []byte
	wrote bool // a record was written, or tried: OWNED comes first or not at all
	err   error
}

// NewWriter writes records to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Inner writes p as INNER records of at most MaxInner bytes. An empty p writes nothing.
func (w *Writer) Inner(p []byte) error {
	_, err := w.inner(p)
	return err
}

// inner is Inner, also returning how many bytes of p went out in whole records.
func (w *Writer) inner(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for len(p) > 0 {
		k := min(len(p), MaxInner)
		if err := w.recordLocked(RecInner, p[:k]); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}

// Link writes a LINK record: 1 while the link is degraded, else 0.
func (w *Writer) Link(degraded bool) error {
	b := byte(0)
	if degraded {
		b = 1
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recordLocked(RecLink, []byte{b})
}

// Owned writes the OWNED record. It must be the connection's first record.
func (w *Writer) Owned() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wrote {
		return errors.New("relay: OWNED must be the first record")
	}
	return w.recordLocked(RecOwned, nil)
}

// recordLocked writes one record in one Write. The caller holds w.mu.
func (w *Writer) recordLocked(typ byte, p []byte) error {
	if w.err != nil {
		return w.err
	}
	w.wrote = true
	w.buf = append(w.buf[:0], typ, byte(len(p)>>8), byte(len(p)))
	w.buf = append(w.buf, p...)
	if _, err := w.w.Write(w.buf); err != nil {
		w.err = err
		return err
	}
	return nil
}

// Reader reads records from one connection and applies the grammar. After any error, a deadline included, every
// later Next returns it, since a record may have been cut.
type Reader struct {
	r           io.Reader
	expectOwned bool
	started     bool // a record was read
	hdr         [3]byte
	err         error
}

// NewReader reads records from r. agw's side passes expectOwned: its first record must be OWNED. The helper's side
// does not, and an OWNED there is a bad record.
func NewReader(r io.Reader, expectOwned bool) *Reader {
	return &Reader{r: r, expectOwned: expectOwned}
}

// Next returns the next record; the payload is the caller's to keep. The end of the stream between records is
// io.EOF, and inside one io.ErrUnexpectedEOF. Any error means the connection is to be closed.
func (r *Reader) Next() (typ byte, payload []byte, err error) {
	if r.err != nil {
		return 0, nil, r.err
	}
	typ, payload, err = r.next()
	if err != nil {
		r.err = err
		return 0, nil, err
	}
	r.started = true
	return typ, payload, nil
}

func (r *Reader) next() (byte, []byte, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		return 0, nil, err
	}
	typ, n := r.hdr[0], int(binary.BigEndian.Uint16(r.hdr[1:]))
	if err := r.check(typ, n); err != nil {
		return 0, nil, err
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r.r, p); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	if typ == RecLink && p[0] > 1 {
		return 0, nil, fmt.Errorf("%w: LINK value %d", errBadRecord, p[0])
	}
	return typ, p, nil
}

// check applies the grammar to a header, before any of the payload is read.
func (r *Reader) check(typ byte, n int) error {
	var ok bool
	switch typ {
	case RecInner:
		ok = n >= 1 && n <= MaxInner
	case RecLink:
		ok = n == 1
	case RecOwned:
		ok = n == 0
	default:
		return fmt.Errorf("%w: type 0x%02x", errBadRecord, typ)
	}
	if !ok {
		return fmt.Errorf("%w: type 0x%02x of %d bytes", errBadRecord, typ, n)
	}
	ownedNow := r.expectOwned && !r.started
	if typ == RecOwned && !ownedNow {
		return fmt.Errorf("%w: OWNED not first, or not expected", errBadRecord)
	}
	if typ != RecOwned && ownedNow {
		return fmt.Errorf("%w: first record 0x%02x is not OWNED", errBadRecord, typ)
	}
	return nil
}
