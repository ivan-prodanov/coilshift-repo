// Package logq is summonhelper's logger: a bounded queue of lines that one goroutine writes out, so logging never
// blocks the caller (Global Constraints). The D2 loop, the engine lock, the car-frame forwarder and godbus's read
// goroutine all log; none of them may wait for stderr. When the queue is full, a line is dropped and counted, and the
// writer reports the count in the log once it catches up.
package logq

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Lines is summonhelper's queue length (Global Constraints).
const Lines = 1024

// Logger never blocks: a queue of n lines drained by one goroutine into w; when full, lines are dropped and counted.
// It is also an io.Writer that never blocks (R6), for the standard library's logger. A Logger lives as long as the
// process: it has no Close.
type Logger struct {
	q       chan item
	w       io.Writer
	dropped atomic.Uint64
}

// item is one queued line, or a Flush marker.
type item struct {
	line  []byte
	flush chan struct{} // a Flush marker: closed once every earlier line is written
}

// New starts the writer goroutine. n must be at least 1.
func New(w io.Writer, n int) *Logger {
	if n < 1 {
		panic("logq: the queue needs room for at least one line")
	}
	l := &Logger{q: make(chan item, n), w: w}
	go l.drain()
	return l
}

// Logf queues one line, formatted as fmt.Sprintf does, with a newline added unless it ends in one.
func (l *Logger) Logf(format string, args ...any) {
	b := fmt.Appendf(nil, format, args...)
	if len(b) == 0 || b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	l.put(b)
}

// Write queues a copy of p as one line, as it is: the standard library's logger ends each line with a newline and
// reuses p once Write returns. It never fails.
func (l *Logger) Write(p []byte) (int, error) {
	if len(p) > 0 {
		l.put(append([]byte(nil), p...))
	}
	return len(p), nil
}

// put queues b, or drops and counts it when the queue is full.
func (l *Logger) put(b []byte) {
	select {
	case l.q <- item{line: b}:
	default:
		l.dropped.Add(1)
	}
}

// Dropped is how many lines the full queue could not take so far.
func (l *Logger) Dropped() uint64 { return l.dropped.Load() }

// Flush waits up to d until every line queued before it is written, and reports whether that happened in time. It is
// the only method that waits: it is for a process about to exit, so that its last lines (a refusal) reach stderr.
func (l *Logger) Flush(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	done := make(chan struct{})
	select {
	case l.q <- item{flush: done}:
	case <-t.C:
		return false
	}
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// drain is the writer goroutine. A write error is ignored: there is nowhere else to report it. After each item it
// reports the lines dropped since its last report.
func (l *Logger) drain() {
	var reported uint64
	for it := range l.q {
		if it.line != nil {
			l.w.Write(it.line)
		}
		if d := l.dropped.Load(); d != reported {
			fmt.Fprintf(l.w, "logq: %d line(s) dropped: the log queue was full\n", d-reported)
			reported = d
		}
		if it.flush != nil {
			close(it.flush)
		}
	}
}
