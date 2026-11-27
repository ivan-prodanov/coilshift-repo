package logq

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockedWriter blocks every Write until release is closed, as a stuck stderr pipe does.
type blockedWriter struct {
	release chan struct{}
	entered chan struct{} // closed by the first Write
	once    sync.Once
}

func newBlockedWriter(t *testing.T) *blockedWriter {
	w := &blockedWriter{release: make(chan struct{}), entered: make(chan struct{})}
	t.Cleanup(func() { close(w.release) })
	return w
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

// syncBuf is a Writer that records everything, safe for the writer goroutine and the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLogfNeverBlocks(t *testing.T) {
	w := newBlockedWriter(t)
	l := New(w, Lines)
	const calls = 10000
	start := time.Now()
	for i := 0; i < calls; i++ {
		l.Logf("summon: line %d", i)
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("%d Logf calls took %v with the writer blocked, want at most 50 ms", calls, took)
	}
	// The queue holds Lines lines, and the writer goroutine may hold one more in its blocked Write.
	d := l.Dropped()
	if d != calls-Lines && d != calls-Lines-1 {
		t.Fatalf("Dropped() = %d, want %d or %d (the lines the full queue could not take)", d, calls-Lines, calls-Lines-1)
	}
}

// R6: Write never blocks either, and shares the queue and the count.
func TestWriteNeverBlocks(t *testing.T) {
	w := newBlockedWriter(t)
	l := New(w, 8)
	<-startWriter(t, l, w)
	const calls = 10000
	start := time.Now()
	for i := 0; i < calls; i++ {
		if n, err := l.Write([]byte("agw: a line from the standard logger\n")); n != 37 || err != nil {
			t.Fatalf("Write = %d, %v; want 37, nil", n, err)
		}
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("%d Write calls took %v with the writer blocked, want at most 50 ms", calls, took)
	}
	if d := l.Dropped(); d != calls-8 {
		t.Fatalf("Dropped() = %d, want %d", d, calls-8)
	}
}

// startWriter queues one line and returns a channel closed once the writer goroutine is blocked inside w with it, so
// the queue is empty and its whole capacity is left.
func startWriter(t *testing.T, l *Logger, w *blockedWriter) <-chan struct{} {
	t.Helper()
	l.Logf("first")
	return w.entered
}

func TestLogqWritesInOrder(t *testing.T) {
	var buf syncBuf
	l := New(&buf, Lines)
	std := log.New(l, "", 0) // the standard library's logger, as summonhelper routes it (R6)
	var want strings.Builder
	for i := 0; i < 300; i++ {
		if i%3 == 0 {
			std.Printf("std %d", i)
			fmt.Fprintf(&want, "std %d\n", i)
		} else {
			l.Logf("logf %d", i)
			fmt.Fprintf(&want, "logf %d\n", i)
		}
	}
	if !l.Flush(time.Second) {
		t.Fatal("Flush did not finish within 1 s")
	}
	if got := buf.String(); got != want.String() {
		t.Fatalf("the writer got the lines out of order or changed:\n%s\nwant:\n%s", got, want.String())
	}
	if d := l.Dropped(); d != 0 {
		t.Fatalf("Dropped() = %d, want 0", d)
	}
}

func TestLogfEndsEachLine(t *testing.T) {
	var buf syncBuf
	l := New(&buf, 4)
	l.Logf("one")
	l.Logf("two\n")
	l.Logf("%d", 3)
	l.Flush(time.Second)
	if got := buf.String(); got != "one\ntwo\n3\n" {
		t.Fatalf("got %q, want each line ended by exactly one newline", got)
	}
}

// Write copies p: the standard logger reuses its buffer as soon as Write returns.
func TestWriteCopies(t *testing.T) {
	var buf syncBuf
	l := New(&buf, 4)
	p := []byte("first\n")
	l.Write(p)
	copy(p, "XXXXX\n")
	l.Write(nil) // nothing to queue
	l.Flush(time.Second)
	if got := buf.String(); got != "first\n" {
		t.Fatalf("got %q, want %q", got, "first\n")
	}
}

// Dropped lines are reported in the log itself once the writer catches up, so a reader knows lines are missing.
func TestDropsReported(t *testing.T) {
	var buf syncBuf
	gate := &gateWriter{w: &buf, release: make(chan struct{}), entered: make(chan struct{})}
	l := New(gate, 2)
	l.Logf("a") // the writer takes it and blocks
	<-gate.entered
	l.Logf("b")
	l.Logf("c")
	l.Logf("d") // dropped
	l.Logf("e") // dropped
	close(gate.release)
	if !l.Flush(time.Second) {
		t.Fatal("Flush did not finish within 1 s")
	}
	want := "a\nlogq: 2 line(s) dropped: the log queue was full\nb\nc\n"
	if got := buf.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if d := l.Dropped(); d != 2 {
		t.Fatalf("Dropped() = %d, want 2", d)
	}
}

// gateWriter blocks its first Write until release is closed, then writes through.
type gateWriter struct {
	w       *syncBuf
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (g *gateWriter) Write(p []byte) (int, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.w.Write(p)
}

// Flush is bounded: with the writer stuck it gives up after its timeout.
func TestFlushBounded(t *testing.T) {
	w := newBlockedWriter(t)
	l := New(w, 4)
	l.Logf("stuck")
	start := time.Now()
	if l.Flush(50 * time.Millisecond) {
		t.Fatal("Flush reported success with the writer stuck")
	}
	if took := time.Since(start); took < 50*time.Millisecond || took > time.Second {
		t.Fatalf("Flush returned after %v, want about 50 ms", took)
	}
}
