// Package logx provides a single runtime log for the whole service: every HTTP
// request, routing decision, tool call, external API call and error lands in
// one append-only file under data/logs so that failures can be traced back to
// their source instead of scattering across stderr. Levels are
// DEBUG/INFO/WARN/ERROR; each line carries an ISO timestamp. Logs auto-rotate
// at 10 MB and keep the last 5 backups, so a long-running service never fills
// the disk. A bounded in-memory ring lets the UI read the most recent entries
// without opening files on disk.
package logx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxSize  = 10 * 1024 * 1024 // 10 MB per log file
	maxFiles = 5                // keep agenthub.log.1 .. .5
	ringSize = 1000             // keep last 1000 entries in memory
)

// Level is a log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Entry is one structured record kept in the in-memory ring.
type Entry struct {
	TS    string `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// Logger writes timestamped, leveled lines to an append-only file (and, if set,
// to stdout) with a single mutex so concurrent handlers never interleave.
type Logger struct {
	mu    sync.Mutex
	f     *os.File
	o     io.Writer
	path  string
	level Level
	ring  []Entry
	ringI int
}

// std is the process-wide default logger. Init sets it; before Init (or on
// failure) Default() falls back to a stdout-only logger so callers never nil.
var (
	stdMu sync.RWMutex
	std   *Logger
)

func fallback() *Logger {
	l := &Logger{o: os.Stdout, level: LevelDebug, path: ""}
	l.ring = make([]Entry, 0, ringSize)
	return l
}

// Init creates the process-wide logger at path and returns it. Call once at
// startup. debug enables DEBUG-level output.
func Init(path string, debug bool) *Logger {
	l := &Logger{o: os.Stdout, path: path, ring: make([]Entry, 0, ringSize)}
	if debug {
		l.level = LevelDebug
	} else {
		l.level = LevelInfo
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		l.Errorf("logx: mkdir %s: %v", filepath.Dir(path), err)
	} else if err := l.open(); err != nil {
		l.Errorf("logx: open %s: %v", path, err)
	}
	stdMu.Lock()
	std = l
	stdMu.Unlock()
	return l
}

// Default returns the process-wide logger, falling back to stdout-only.
func Default() *Logger {
	stdMu.RLock()
	l := std
	stdMu.RUnlock()
	if l != nil {
		return l
	}
	return fallback()
}

// New opens a logger at a path. Kept for backwards compatibility; new code
// should prefer Init/Default.
func New(path string) *Logger {
	l := &Logger{o: os.Stdout, path: path, level: LevelInfo, ring: make([]Entry, 0, ringSize)}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		l.Errorf("logx: mkdir %s: %v", filepath.Dir(path), err)
		return l
	}
	if err := l.open(); err != nil {
		l.Errorf("logx: open %s: %v", path, err)
		return l
	}
	return l
}

// open (re)opens the log file in append mode. Caller must hold l.mu.
func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

// rotateIfNeeded checks file size and cuts when over maxSize.
// Caller must hold l.mu.
func (l *Logger) rotateIfNeeded() {
	if l.f == nil {
		return
	}
	fi, err := l.f.Stat()
	if err != nil || fi.Size() < maxSize {
		return
	}
	l.f.Close()
	l.f = nil
	for i := maxFiles - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", l.path, i)
		new := fmt.Sprintf("%s.%d", l.path, i+1)
		_ = os.Rename(old, new)
	}
	_ = os.Rename(l.path, l.path+".1")
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, maxFiles+1))
	_ = l.open()
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

func (l *Logger) Debugf(format string, args ...any) { l.log(LevelDebug, "DEBUG", format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.log(LevelInfo, "INFO", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.log(LevelWarn, "WARN", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.log(LevelError, "ERROR", format, args...) }

// Package-level convenience wrappers around Default().
func Debugf(format string, args ...any) { Default().Debugf(format, args...) }
func Infof(format string, args ...any)  { Default().Infof(format, args...) }
func Warnf(format string, args ...any)  { Default().Warnf(format, args...) }
func Errorf(format string, args ...any) { Default().Errorf(format, args...) }

func (l *Logger) log(lvl Level, tag, format string, args ...any) {
	if lvl < l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("2006-01-02T15:04:05.000Z07:00")
	line := fmt.Sprintf("%s [%s] %s\n", ts, tag, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotateIfNeeded()
	if l.f != nil {
		l.f.WriteString(line)
	}
	if l.o != nil {
		l.o.Write([]byte(line))
	}
	// Ring buffer.
	if len(l.ring) < ringSize {
		l.ring = append(l.ring, Entry{TS: ts, Level: tag, Msg: msg})
	} else {
		l.ring[l.ringI] = Entry{TS: ts, Level: tag, Msg: msg}
		l.ringI = (l.ringI + 1) % ringSize
	}
}

// Recent returns up to n most-recent entries at or above the given level.
// level is matched case-insensitively by name: DEBUG/INFO/WARN/ERROR; an empty
// or unknown level means INFO.
func (l *Logger) Recent(level string, n int) []Entry {
	if n <= 0 || n > ringSize {
		if n <= 0 {
			n = 200
		}
		if n > ringSize {
			n = ringSize
		}
	}
	min := LevelInfo
	switch strings.ToUpper(level) {
	case "DEBUG":
		min = LevelDebug
	case "WARN":
		min = LevelWarn
	case "ERROR":
		min = LevelError
	case "INFO":
		min = LevelInfo
	}
	l.mu.Lock()
	total := len(l.ring)
	ordered := make([]Entry, 0, total)
	if total < ringSize {
		ordered = append(ordered, l.ring...)
	} else {
		ordered = append(ordered, l.ring[l.ringI:]...)
		ordered = append(ordered, l.ring[:l.ringI]...)
	}
	l.mu.Unlock()
	out := make([]Entry, 0, n)
	weight := func(name string) Level {
		switch name {
		case "DEBUG":
			return LevelDebug
		case "INFO":
			return LevelInfo
		case "WARN":
			return LevelWarn
		case "ERROR":
			return LevelError
		}
		return LevelInfo
	}
	// Walk newest-first to collect, then reverse to chronological order.
	for i := len(ordered) - 1; i >= 0 && len(out) < n; i-- {
		if weight(ordered[i].Level) >= min {
			out = append(out, ordered[i])
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
