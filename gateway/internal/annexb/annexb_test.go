package annexb

import (
	"bytes"
	"math/rand"
	"testing"
	"time"
)

// Realistic NAL units (header byte + a few payload bytes). Slices whose
// second byte has the top bit set have first_mb_in_slice == 0.
var (
	nSPS   = []byte{0x67, 0x64, 0x00, 0x28, 0xAC, 0xD9, 0x40, 0x5A, 0x07, 0xB0, 0x11}
	nSPS2  = []byte{0x67, 0x64, 0x00, 0x29, 0xAC, 0xD9, 0x40, 0x5A, 0x07, 0xB0, 0x22}
	nPPS   = []byte{0x68, 0xEE, 0x3C, 0xB0}
	nSEI   = []byte{0x06, 0x05, 0x10, 0xB9, 0xED, 0xB9, 0x30, 0x5D, 0x21, 0x4B, 0x80}
	nAUD   = []byte{0x09, 0xF0}
	nEOS   = []byte{0x0A}
	nIDR   = []byte{0x65, 0x88, 0x84, 0x00, 0x00, 0x03, 0x01, 0x2F, 0xFF, 0xF1} // with an emulation-prevention byte
	nIDR2  = []byte{0x65, 0x40, 0x22, 0x33, 0x00, 0x00, 0x03, 0x00, 0x44}       // continuation slice (first_mb != 0)
	nP     = []byte{0x41, 0x9A, 0x02, 0x04, 0x00, 0x00, 0x03, 0x02, 0x99}
	nP2    = []byte{0x41, 0x50, 0x02, 0x04, 0x77}
	nPbad  = []byte{0xC1, 0x9A, 0x01} // forbidden_zero_bit set
	t0     = time.Unix(1000, 0)
	sc4    = []byte{0, 0, 0, 1}
	sc3    = []byte{0, 0, 1}
	defMax = 1 << 20
)

// annexB joins NALs with 4-byte start codes (the expected output form).
func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(b, sc4...)
		b = append(b, n...)
	}
	return b
}

// stream3 joins NALs with 3-byte start codes.
func stream3(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(b, sc3...)
		b = append(b, n...)
	}
	return b
}

func run(t *testing.T, s *Splitter, input []byte, chunks []int) []AU {
	t.Helper()
	var out []AU
	emit := func(a AU) { out = append(out, a) }
	off := 0
	for i, n := range chunks {
		if off+n > len(input) {
			n = len(input) - off
		}
		s.Write(input[off:off+n], t0.Add(time.Duration(i)*time.Millisecond), emit)
		off += n
	}
	if off < len(input) {
		s.Write(input[off:], t0.Add(time.Duration(len(chunks))*time.Millisecond), emit)
	}
	s.Flush(emit)
	return out
}

func whole(t *testing.T, input []byte) []AU {
	return run(t, NewSplitter(defMax), input, nil)
}

type want struct {
	data      []byte
	idr, ps   bool
	afterDrop bool
}

func check(t *testing.T, got []AU, wants []want) {
	t.Helper()
	if len(got) != len(wants) {
		for i, g := range got {
			t.Logf("got[%d] idr=%v ps=%v % x", i, g.IDR, g.ParamSets, g.Data)
		}
		t.Fatalf("got %d AUs, want %d", len(got), len(wants))
	}
	for i := range wants {
		g, w := got[i], wants[i]
		if !bytes.Equal(g.Data, w.data) {
			t.Fatalf("AU %d data\n got % x\nwant % x", i, g.Data, w.data)
		}
		if g.IDR != w.idr || g.ParamSets != w.ps || g.AfterDrop != w.afterDrop {
			t.Fatalf("AU %d flags idr=%v ps=%v drop=%v, want %v %v %v", i, g.IDR, g.ParamSets, g.AfterDrop, w.idr, w.ps, w.afterDrop)
		}
	}
}

// A GOP as h264parse config-interval=-1 emits it.
func gop() ([]byte, []want) {
	in := annexB(nSPS, nPPS, nSEI, nIDR, nP, nP, nSPS, nPPS, nIDR, nP)
	return in, []want{
		{annexB(nSPS, nPPS, nSEI, nIDR), true, true, false},
		{annexB(nP), false, false, false},
		{annexB(nP), false, false, false},
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nP), false, false, false},
	}
}

func TestTypicalGOP(t *testing.T) {
	in, w := gop()
	check(t, whole(t, in), w)
}

func TestEverySplitPoint(t *testing.T) {
	in, w := gop()
	for cut := 1; cut < len(in); cut++ {
		got := run(t, NewSplitter(defMax), in, []int{cut})
		check(t, got, w)
	}
}

func TestByteByByte(t *testing.T) {
	in, w := gop()
	ch := make([]int, len(in))
	for i := range ch {
		ch[i] = 1
	}
	check(t, run(t, NewSplitter(defMax), in, ch), w)
}

func TestThreeByteStartCodesNormalised(t *testing.T) {
	in := stream3(nSPS, nPPS, nIDR, nP)
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nP), false, false, false},
	})
}

func TestTrailingZeroBytesTrimmed(t *testing.T) {
	var in []byte
	for _, n := range [][]byte{nSPS, nPPS, nIDR, nP} {
		in = append(in, sc4...)
		in = append(in, n...)
		in = append(in, 0, 0, 0) // trailing_zero_8bits
	}
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nP), false, false, false},
	})
}

func TestMultiSlicePictures(t *testing.T) {
	in := annexB(nSPS, nPPS, nIDR, nIDR2, nIDR2, nP, nP2, nP2, nP, nP2)
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR, nIDR2, nIDR2), true, true, false},
		{annexB(nP, nP2, nP2), false, false, false},
		{annexB(nP, nP2), false, false, false},
	})
}

func TestAUDDelimited(t *testing.T) {
	in := annexB(nAUD, nSPS, nPPS, nIDR, nAUD, nP, nAUD, nP, nP2)
	check(t, whole(t, in), []want{
		{annexB(nAUD, nSPS, nPPS, nIDR), true, true, false},
		{annexB(nAUD, nP), false, false, false},
		{annexB(nAUD, nP, nP2), false, false, false},
	})
}

func TestSEIStartsNextAU(t *testing.T) {
	in := annexB(nSPS, nPPS, nIDR, nSEI, nP, nSEI, nP)
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nSEI, nP), false, false, false},
		{annexB(nSEI, nP), false, false, false},
	})
}

func TestEndOfSequenceClosesAU(t *testing.T) {
	in := annexB(nSPS, nPPS, nIDR, nEOS, nP2)
	// nP2 has first_mb != 0, but EOS already closed the IDR AU.
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR, nEOS), true, true, false},
		{annexB(nP2), false, false, false},
	})
}

func TestIDRWithoutParamSetsGetsCachedOnes(t *testing.T) {
	in := annexB(nSPS, nPPS, nIDR, nP, nIDR, nP)
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nP), false, false, false},
		{annexB(nSPS, nPPS, nIDR), true, true, false}, // inserted
		{annexB(nP), false, false, false},
	})
}

func TestInsertionKeepsAUDFirstAndSEIAfter(t *testing.T) {
	in := annexB(nAUD, nSPS, nPPS, nIDR, nAUD, nSEI, nIDR)
	check(t, whole(t, in), []want{
		{annexB(nAUD, nSPS, nPPS, nIDR), true, true, false},
		{annexB(nAUD, nSPS, nPPS, nSEI, nIDR), true, true, false},
	})
}

func TestOnlyPPSMissing(t *testing.T) {
	in := annexB(nSPS, nPPS, nIDR, nSPS, nIDR)
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nSPS, nPPS, nIDR), true, true, false},
	})
}

func TestUpdatedSPSIsUsed(t *testing.T) {
	in := annexB(nSPS, nPPS, nIDR, nSPS2, nPPS, nIDR, nP, nIDR)
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nSPS2, nPPS, nIDR), true, true, false},
		{annexB(nP), false, false, false},
		{annexB(nSPS2, nPPS, nIDR), true, true, false},
	})
}

func TestIDRBeforeAnyParamSets(t *testing.T) {
	in := annexB(nIDR, nP, nSPS, nPPS, nIDR)
	check(t, whole(t, in), []want{
		{annexB(nIDR), true, false, false},
		{annexB(nP), false, false, false},
		{annexB(nSPS, nPPS, nIDR), true, true, false},
	})
}

func TestNonIDRWithParamSetsFlag(t *testing.T) {
	in := annexB(nSPS, nPPS, nP)
	check(t, whole(t, in), []want{{annexB(nSPS, nPPS, nP), false, true, false}})
}

func TestGarbageBeforeFirstStartCode(t *testing.T) {
	in := append([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00}, annexB(nSPS, nPPS, nIDR, nP)...)
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, true},
		{annexB(nP), false, false, false},
	})
	// Leading zero bytes are not garbage.
	in = append([]byte{0, 0, 0, 0}, annexB(nSPS, nPPS, nIDR)...)
	check(t, whole(t, in), []want{{annexB(nSPS, nPPS, nIDR), true, true, false}})
}

func TestLongGarbageWithoutStartCode(t *testing.T) {
	s := NewSplitter(defMax)
	var out []AU
	emit := func(a AU) { out = append(out, a) }
	junk := bytes.Repeat([]byte{0xAB}, 100000)
	for i := 0; i < 10; i++ {
		s.Write(junk, t0, emit)
	}
	if len(s.buf) > 2 {
		t.Fatalf("buffer grew to %d bytes on garbage", len(s.buf))
	}
	s.Write(annexB(nSPS, nPPS, nIDR, nP), t0, emit)
	s.Flush(emit)
	check(t, out, []want{
		{annexB(nSPS, nPPS, nIDR), true, true, true},
		{annexB(nP), false, false, false},
	})
}

func TestForbiddenBitNALDropped(t *testing.T) {
	in := annexB(nSPS, nPPS, nIDR, nPbad, nP)
	// The bad NAL arrived before the IDR AU was complete, so that AU is the
	// first one emitted after the drop.
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, true},
		{annexB(nP), false, false, false},
	})
}

func TestOversizeAUDroppedAndResync(t *testing.T) {
	big := append([]byte{0x41, 0x9A}, bytes.Repeat([]byte{0x55}, 300)...)
	in := annexB(nSPS, nPPS, nIDR, big, nP2, nP, nSPS, nPPS, nIDR)
	got := run(t, NewSplitter(200), in, []int{7, 13, 64, 5})
	check(t, got, []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nP), false, false, true}, // the big AU (with its continuation slice) was dropped
		{annexB(nSPS, nPPS, nIDR), true, true, false},
	})
	for _, a := range got {
		if len(a.Data) > 200 {
			t.Fatalf("AU of %d bytes exceeds max", len(a.Data))
		}
	}
}

func TestNeverEndingNALDropped(t *testing.T) {
	s := NewSplitter(1000)
	var out []AU
	emit := func(a AU) { out = append(out, a) }
	s.Write(annexB(nSPS, nPPS, nIDR), t0, emit)
	s.Write(append([]byte{0, 0, 0, 1, 0x41, 0x9A}, bytes.Repeat([]byte{0x77}, 5000)...), t0, emit)
	s.Write(bytes.Repeat([]byte{0x77}, 5000), t0, emit)
	if len(s.buf) > 1100 {
		t.Fatalf("buffer %d bytes", len(s.buf))
	}
	s.Write(annexB(nSPS, nPPS, nIDR, nP), t0, emit)
	s.Flush(emit)
	check(t, out, []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(nSPS, nPPS, nIDR), true, true, true},
		{annexB(nP), false, false, false},
	})
}

func TestEmulationPreventionNoFalseSplit(t *testing.T) {
	// 00 00 03 01 inside a payload must not be read as a start code.
	p := []byte{0x41, 0x9A, 0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x03, 0x00, 0x01}
	in := annexB(nSPS, nPPS, nIDR, p)
	check(t, whole(t, in), []want{
		{annexB(nSPS, nPPS, nIDR), true, true, false},
		{annexB(p), false, false, false},
	})
}

func TestLoneParamSetsAtEOFNotEmitted(t *testing.T) {
	check(t, whole(t, annexB(nSPS, nPPS)), nil)
	check(t, whole(t, annexB(nAUD)), nil)
	check(t, whole(t, nil), nil)
}

func TestAUTimeIsFirstNALArrival(t *testing.T) {
	s := NewSplitter(defMax)
	var out []AU
	emit := func(a AU) { out = append(out, a) }
	s.Write(annexB(nSPS, nPPS), t0, emit)
	s.Write(annexB(nIDR), t0.Add(10*time.Millisecond), emit)
	s.Write(annexB(nP), t0.Add(50*time.Millisecond), emit)
	s.Flush(emit)
	if len(out) != 2 || !out[0].Time.Equal(t0) || !out[1].Time.Equal(t0.Add(50*time.Millisecond)) {
		t.Fatalf("times %v", out)
	}
}

func TestOutputIsOwned(t *testing.T) {
	s := NewSplitter(defMax)
	var out []AU
	emit := func(a AU) { out = append(out, a) }
	in := annexB(nSPS, nPPS, nIDR, nP, nP, nP)
	s.Write(in, t0, emit)
	first := append([]byte(nil), out[0].Data...)
	s.Write(annexB(nSPS, nPPS, nIDR, nP), t0, emit)
	s.Flush(emit)
	if !bytes.Equal(out[0].Data, first) {
		t.Fatal("emitted AU data was overwritten by later input")
	}
}

// Random GOP structures cut at random points produce exactly the AUs they
// were built from.
func TestRandomStreams(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		var in []byte
		var wants []want
		aud := r.Intn(2) == 0
		for au := 0; au < 2+r.Intn(12); au++ {
			var nals [][]byte
			if aud {
				nals = append(nals, nAUD)
			}
			idr := au == 0 || r.Intn(4) == 0
			if idr {
				nals = append(nals, nSPS, nPPS)
				if r.Intn(2) == 0 {
					nals = append(nals, nSEI)
				}
				nals = append(nals, randSlice(r, 0x65, true))
				for k := r.Intn(3); k > 0; k-- {
					nals = append(nals, randSlice(r, 0x65, false))
				}
			} else {
				nals = append(nals, randSlice(r, 0x41, true))
				for k := r.Intn(3); k > 0; k-- {
					nals = append(nals, randSlice(r, 0x41, false))
				}
			}
			for _, n := range nals {
				if r.Intn(3) == 0 {
					in = append(in, sc3...)
				} else {
					in = append(in, sc4...)
				}
				in = append(in, n...)
			}
			wants = append(wants, want{annexB(nals...), idr, idr, false})
		}
		var chunks []int
		for left := len(in); left > 0; {
			n := 1 + r.Intn(64)
			chunks = append(chunks, n)
			left -= n
		}
		check(t, run(t, NewSplitter(defMax), in, chunks), wants)
	}
}

// randSlice builds a slice NAL with no 00 00 0x sequences in its payload and
// a nonzero last byte.
func randSlice(r *rand.Rand, hdr byte, first bool) []byte {
	b := []byte{hdr, 0}
	if first {
		b[1] = 0x80 | byte(r.Intn(0x80))
	} else {
		b[1] = 0x01 + byte(r.Intn(0x7F))
	}
	for i := r.Intn(200); i > 0; i-- {
		b = append(b, 1+byte(r.Intn(255)))
	}
	return b
}

func BenchmarkSplit(b *testing.B) {
	idr := append([]byte{0x65, 0x88}, bytes.Repeat([]byte{0x5A}, 150000)...)
	p := append([]byte{0x41, 0x9A}, bytes.Repeat([]byte{0x5A}, 15000)...)
	var in []byte
	in = append(in, annexB(nSPS, nPPS, idr)...)
	for i := 0; i < 15; i++ {
		in = append(in, annexB(p)...)
	}
	b.SetBytes(int64(len(in)))
	for i := 0; i < b.N; i++ {
		s := NewSplitter(defMax)
		for off := 0; off < len(in); off += 65536 {
			end := min(off+65536, len(in))
			s.Write(in[off:end], t0, func(AU) {})
		}
		s.Flush(func(AU) {})
	}
}
