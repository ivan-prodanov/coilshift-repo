// Package annexb splits an H.264 Annex-B byte stream into access units.
//
// The car's capture pipeline (gst-launch … h264parse … alignment=au ! fdsink)
// writes access units back to back into a pipe, and a pipe does not keep write
// boundaries, so the reader has to find the AU boundaries again. The rules
// follow H.264 7.4.1.2.3, reduced to what a camera stream contains:
//
//   - An access unit delimiter (NAL type 9) always starts a new AU.
//   - SEI (6), SPS (7), PPS (8) and types 14-18 that follow a VCL NAL of the
//     current AU start the next AU (they belong to the picture after them).
//   - A VCL NAL (type 1 or 5; 2 is treated like 1) whose first_mb_in_slice is
//     0 starts a new AU when the current AU already holds a VCL NAL.
//   - End of sequence (10) and end of stream (11) close the current AU.
//
// An AU is emitted only when the next AU begins (or on Flush), so a picture
// is delivered one NAL later than it arrived: about one frame of latency.
//
// Output AUs always use 4-byte start codes. The splitter remembers the last
// SPS and PPS and inserts them in front of an IDR access unit that lacks
// them, so every IDR it emits is decodable on its own once a parameter set
// has been seen.
package annexb

import (
	"bytes"
	"time"
)

// NAL unit types used here.
const (
	nalSlice    = 1
	nalSliceA   = 2
	nalSliceB   = 3
	nalSliceC   = 4
	nalIDR      = 5
	nalSEI      = 6
	nalSPS      = 7
	nalPPS      = 8
	nalAUD      = 9
	nalEndSeq   = 10
	nalEndStrm  = 11
	nalFiller   = 12
	nalPrefix14 = 14
	nalRsv18    = 18
)

var startCode = []byte{0, 0, 0, 1}

// AU is one access unit in Annex-B form (4-byte start codes).
type AU struct {
	Data      []byte
	IDR       bool      // contains an IDR slice (NAL type 5)
	ParamSets bool      // contains both an SPS and a PPS
	Time      time.Time // when the AU's first NAL started to arrive
	// AfterDrop is true when data was discarded since the previous emitted
	// AU (an oversized AU or garbage). A consumer must resync at an IDR.
	AfterDrop bool
}

// Splitter is a streaming Annex-B access-unit splitter. It is not safe for
// concurrent use.
type Splitter struct {
	maxAU int

	buf      []byte    // unconsumed input
	scan     int       // where the next start-code search begins
	nalStart int       // payload start of the NAL being received; -1 before the first start code
	nalTime  time.Time // arrival time of that NAL's start code

	cur      []byte // AU being built
	curTime  time.Time
	curVCL   bool
	curIDR   bool
	curSPS   bool
	curPPS   bool
	curFirst int // offset in cur of the first VCL NAL's start code (-1 = none)
	curAUD   bool
	dropping bool // the current AU overflowed: discard until the next AU starts
	dropped  bool // something was discarded since the last emitted AU

	sps, pps []byte // last parameter sets seen (NAL payloads incl. header byte)
}

// NewSplitter returns a splitter that discards any AU (or NAL) larger than
// maxAU bytes.
func NewSplitter(maxAU int) *Splitter {
	return &Splitter{maxAU: maxAU, nalStart: -1, curFirst: -1}
}

// Write feeds p (received at now) and calls emit for every AU completed by
// it. The AU data is owned by the callee.
func (s *Splitter) Write(p []byte, now time.Time, emit func(AU)) {
	s.buf = append(s.buf, p...)
	for {
		i := bytes.Index(s.buf[s.scan:], startCode[1:]) // 00 00 01
		if i < 0 {
			break
		}
		sc := s.scan + i // index of the 00 00 01
		if s.nalStart >= 0 && sc >= s.nalStart {
			s.handleNAL(trimZeros(s.buf[s.nalStart:sc]), emit)
		} else if s.nalStart < 0 && sc > 0 {
			// Bytes before the first start code are not a NAL.
			if len(bytes.Trim(s.buf[:sc], "\x00")) > 0 {
				s.dropped = true
			}
		}
		s.nalStart = sc + 3
		s.nalTime = now
		s.scan = s.nalStart
	}
	// Keep the last two bytes searchable: a start code may straddle writes.
	if len(s.buf)-2 > s.scan {
		s.scan = len(s.buf) - 2
	}
	// A NAL that never ends is garbage: end the current AU if this NAL would
	// have started a new one, then drop it and resync.
	if s.nalStart >= 0 && len(s.buf)-s.nalStart > s.maxAU {
		s.boundary(s.buf[s.nalStart:], emit)
		s.dropAU()
		s.buf = s.buf[:0]
		s.scan, s.nalStart = 0, -1
		return
	}
	if s.nalStart < 0 && len(s.buf) > 2 {
		// No start code yet: only the last two bytes can still matter.
		if len(bytes.Trim(s.buf[:len(s.buf)-2], "\x00")) > 0 {
			s.dropped = true
		}
		s.buf = append(s.buf[:0], s.buf[len(s.buf)-2:]...)
		s.scan = 0
		return
	}
	s.compact()
}

// Flush ends the stream: the NAL being received and the current AU are
// emitted.
func (s *Splitter) Flush(emit func(AU)) {
	if s.nalStart >= 0 && s.nalStart <= len(s.buf) {
		s.handleNAL(trimZeros(s.buf[s.nalStart:]), emit)
	}
	s.buf = s.buf[:0]
	s.scan, s.nalStart = 0, -1
	s.emit(emit)
}

// compact drops consumed bytes once they dominate the buffer.
func (s *Splitter) compact() {
	if s.nalStart < 4096 || s.nalStart < len(s.buf)/2 {
		return
	}
	// keep the start code bytes before nalStart out: nalStart moves to 0
	n := copy(s.buf, s.buf[s.nalStart:])
	s.buf = s.buf[:n]
	s.scan -= s.nalStart
	s.nalStart = 0
}

func trimZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

func isVCL(t byte) bool { return t >= nalSlice && t <= nalIDR }

// firstMBZero reports whether a slice NAL has first_mb_in_slice == 0: its
// ue(v) code is the single bit '1', the top bit of the first payload byte.
func firstMBZero(nal []byte) bool { return len(nal) >= 2 && nal[1]&0x80 != 0 }

func (s *Splitter) handleNAL(nal []byte, emit func(AU)) {
	if len(nal) == 0 {
		return
	}
	if nal[0]&0x80 != 0 { // forbidden_zero_bit
		s.dropped = true
		return
	}
	s.boundary(nal, emit)
	t := nal[0] & 0x1F
	switch t {
	case nalSPS:
		s.sps = append(s.sps[:0], nal...)
	case nalPPS:
		s.pps = append(s.pps[:0], nal...)
	}
	s.appendNAL(t, nal)
	if t == nalEndSeq || t == nalEndStrm {
		s.emit(emit)
	}
}

// boundary emits the current AU when the NAL starting with nal (at least
// its header byte) begins a new access unit.
func (s *Splitter) boundary(nal []byte, emit func(AU)) {
	if len(nal) == 0 {
		return
	}
	t := nal[0] & 0x1F
	switch {
	case t == nalAUD:
		s.emit(emit)
	case t == nalSEI || t == nalSPS || t == nalPPS || (t >= nalPrefix14 && t <= nalRsv18):
		if s.curVCL {
			s.emit(emit)
		}
	case t == nalSlice || t == nalSliceA || t == nalIDR:
		if s.curVCL && firstMBZero(nal) {
			s.emit(emit)
		}
	}
}

func (s *Splitter) appendNAL(t byte, nal []byte) {
	if s.dropping {
		return
	}
	if len(s.cur) == 0 {
		s.curTime = s.nalTime
		s.curAUD = t == nalAUD
	}
	if len(s.cur)+len(startCode)+len(nal) > s.maxAU {
		s.dropAU()
		return
	}
	if isVCL(t) && s.curFirst < 0 {
		s.curFirst = len(s.cur)
	}
	s.cur = append(s.cur, startCode...)
	s.cur = append(s.cur, nal...)
	switch t {
	case nalIDR:
		s.curIDR = true
	case nalSPS:
		s.curSPS = true
	case nalPPS:
		s.curPPS = true
	}
	if isVCL(t) {
		s.curVCL = true
	}
}

// dropAU discards the AU being built; the rest of it is ignored until the
// next AU boundary. The dropped AU counts as holding a picture, so the next
// AU-starting NAL (AUD, SPS, PPS, SEI, a slice with first_mb 0) ends it.
func (s *Splitter) dropAU() {
	s.reset()
	s.dropping = true
	s.dropped = true
	s.curVCL = true
}

func (s *Splitter) reset() {
	s.cur = nil
	s.curVCL, s.curIDR, s.curSPS, s.curPPS, s.curAUD = false, false, false, false, false
	s.curFirst = -1
}

// emit hands the current AU to the consumer (if it has a picture) and
// starts a new one.
func (s *Splitter) emit(emit func(AU)) {
	if s.dropping {
		s.dropping = false
		s.reset()
		return
	}
	if len(s.cur) == 0 {
		return
	}
	if !s.curVCL { // parameter sets / SEI with no picture: keep for the next AU
		if s.curAUD { // a lone AUD: drop it, the next AU brings its own
			s.reset()
		}
		return
	}
	data, sps, pps := s.cur, s.curSPS, s.curPPS
	if s.curIDR && (!sps || !pps) && s.sps != nil && s.pps != nil {
		data, sps, pps = s.withParamSets(), true, true
	}
	au := AU{Data: data, IDR: s.curIDR, ParamSets: sps && pps, Time: s.curTime, AfterDrop: s.dropped}
	s.dropped = false
	s.reset()
	emit(au)
}

// withParamSets returns cur with the missing cached SPS/PPS inserted in
// front of the first VCL NAL (after an AUD and any SEI already present).
func (s *Splitter) withParamSets() []byte {
	var ins []byte
	if !s.curSPS {
		ins = append(append(ins, startCode...), s.sps...)
	}
	if !s.curPPS {
		ins = append(append(ins, startCode...), s.pps...)
	}
	at := 0
	if s.curAUD { // keep the AUD first
		if i := bytes.Index(s.cur[4:], startCode); i >= 0 {
			at = 4 + i
		} else {
			at = len(s.cur)
		}
	}
	if s.curSPS && !s.curPPS && s.curFirst >= 0 {
		at = s.curFirst // the PPS goes right before the slice, after the SPS
	}
	out := make([]byte, 0, len(s.cur)+len(ins))
	out = append(out, s.cur[:at]...)
	out = append(out, ins...)
	out = append(out, s.cur[at:]...)
	return out
}
