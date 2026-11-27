package server

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// logEntry is one session-log line. Wall-clock times (for the log only), the
// key fingerprint, the capability name, its detail and the end reason. Never
// keys, N, VIN or payloads.
type logEntry struct {
	Start  string     `json:"start"`
	End    string     `json:"end"`
	FP     string     `json:"fp"`
	Cap    string     `json:"cap"`
	Detail string     `json:"detail"`
	Reason string     `json:"reason"`
	Events []logEvent `json:"events,omitempty"` // Window.LogEvent, bounded
}

// logEvent is one Window.LogEvent entry: ms since the window start and the
// capability's short text (never media).
type logEvent struct {
	T int64  `json:"t"`
	E string `json:"e"`
}

// sessionLog appends JSON lines to path, rotating to path+".1" before a line
// would take the file past max bytes. Failures are reported on stderr and
// never stop the server.
type sessionLog struct {
	mu   sync.Mutex
	path string
	max  int64
}

func (l *sessionLog) write(e logEntry) {
	if l.path == "" {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		log.Printf("agw: session log: %v", err)
		return
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 {
		if st, err := os.Stat(l.path); err == nil && st.Size() > 0 && st.Size()+int64(len(line)) > l.max {
			if err := os.Rename(l.path, l.path+".1"); err != nil {
				log.Printf("agw: session log rotate: %v", err)
			}
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		log.Printf("agw: session log: %v", err)
		return
	}
	if _, err := f.Write(line); err != nil {
		log.Printf("agw: session log: %v", err)
	}
	if err := f.Close(); err != nil {
		log.Printf("agw: session log: %v", err)
	}
}
