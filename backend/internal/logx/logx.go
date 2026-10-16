// Package logx provides a single runtime log for the whole service: every HTTP
// request, routing decision and error lands in one append-only file under
// data/logs so that failures can be traced back to their source instead of
// scattering across stderr. Levels are INFO/WARN/ERROR; each line carries an
// ISO timestamp, so a crash or a bad request is always observable afterwards.
package logx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger writes timestamped, leveled lines to an append-only file (and, if set,
// to stdout) with a single mutex so concurrent handlers never interleave.
type Logger struct {
	mu sync.Mutex
	f  *os.File
	o  io.Writer
}

// New opens (creating if needed) the log file at path and returns a Logger.
// On failure it returns a best-effort logger that still mirrors to stdout,
// so the service keeps running and the error is not silently lost.
func New(path string) *Logger {
	l := &Logger{o: os.Stdout}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		l.Errorf("logx: mkdir %s: %v", filepath.Dir(path), err)
		return l
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		l.Errorf("logx: open %s: %v", path, err)
		return l
	}
	l.f = f
	return l
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

func (l *Logger) Infof(format string, args ...any)  { l.log("INFO", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.log("WARN", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.log("ERROR", format, args...) }

func (l *Logger) log(level, format string, args ...any) {
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), level,
		fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.WriteString(line)
	}
	if l.o != nil {
		l.o.Write([]byte(line))
	}
}
