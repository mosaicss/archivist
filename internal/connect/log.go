package connect

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Logger writes scrubbed, timestamped lines. Every message passes Scrub, so
// a credential reaching a log call still prints only as a fingerprint.
type Logger struct {
	w      io.Writer
	prefix string
	debug  bool
}

// NewLogger writes to w.
func NewLogger(w io.Writer) *Logger { return &Logger{w: w} }

// With returns a logger that prefixes every line (e.g. a session id).
func (l *Logger) With(prefix string) *Logger {
	if l == nil {
		return nil
	}
	return &Logger{w: l.w, prefix: l.prefix + prefix + " ", debug: l.debug}
}

// SetDebug turns Debugf lines on (loggers derived later inherit it).
func (l *Logger) SetDebug(on bool) {
	if l != nil {
		l.debug = on
	}
}

// Debugf logs one line when debug lines are on.
func (l *Logger) Debugf(format string, args ...any) {
	if l != nil && l.debug {
		l.Printf("debug: "+format, args...)
	}
}

// Printf logs one line.
func (l *Logger) Printf(format string, args ...any) {
	if l == nil || l.w == nil {
		return
	}
	msg := Scrub(fmt.Sprintf(format, args...))
	msg = strings.TrimRight(msg, "\n")
	line := time.Now().UTC().Format("15:04:05.000") + " connect: " + l.prefix + msg + "\n"
	sharedLogMu.Lock()
	defer sharedLogMu.Unlock()
	_, _ = io.WriteString(l.w, line)
}

// sharedLogMu serialises writers derived through With.
var sharedLogMu sync.Mutex
