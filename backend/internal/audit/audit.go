// Package audit provides the cross-module, desensitized audit log. Callers must
// never pass secrets/tokens into Fields; only non-sensitive metadata is logged
// (DESIGN §10.3 / §10.4).
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one append-only audit record.
type Entry struct {
	TS        string            `json:"ts"`
	Event     string            `json:"event"`
	RequestID string            `json:"requestId,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// Audit writes entries to data/audit.log.
type Audit struct {
	path string
	mu   sync.Mutex
}

// New constructs an Audit logger for the given data directory.
func New(dataDir string) *Audit {
	return &Audit{path: filepath.Join(dataDir, "audit.log")}
}

// Log appends a desensitized audit entry.
func (a *Audit) Log(event, requestID string, fields map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := Entry{
		TS:        time.Now().UTC().Format(time.RFC3339),
		Event:     event,
		RequestID: requestID,
		Fields:    fields,
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
