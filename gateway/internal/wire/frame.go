// Package wire implements length-prefixed framing: a 4-byte big-endian
// length followed by that many body bytes.
package wire

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// ErrTooLarge is returned when a frame length exceeds the permitted maximum.
var ErrTooLarge = errors.New("wire: frame too large")

// ReadFrame reads one frame. The length prefix is checked against max before
// any body allocation or read, so a hostile prefix consumes only 4 bytes.
func ReadFrame(r io.Reader, max int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if uint64(n) > uint64(max) {
		return nil, ErrTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// WriteFrame writes body as a single frame (one Write call).
func WriteFrame(w io.Writer, body []byte) error {
	if uint64(len(body)) > math.MaxUint32 {
		return ErrTooLarge
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err := w.Write(buf)
	return err
}
