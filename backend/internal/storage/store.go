// Package storage provides the persistence abstraction used by every module.
// It is a backend-neutral interface (DESIGN §7.1): callers only see namespaced
// JSON documents and append-only JSONL streams, while the implementation is
// either the flat-file JSONStore (default up to ~10k chunks) or the SQLiteStore
// (modernc.org/sqlite, pure Go, no CGO). Switching backends is a config flag —
// call sites are unaffected.
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNotFound is returned by Load when a document does not exist.
var ErrNotFound = errors.New("storage: document not found")

// Store is the persistence contract implemented by every backend.
type Store interface {
	// Load unmarshals the document id into out.
	Load(id string, out interface{}) error
	// Save marshals in and stores it under id (atomic / upsert).
	Save(id string, in interface{}) error
	// Remove deletes the document id and any stream attached to it.
	Remove(id string) error
	// IDs lists all document ids in this namespace.
	IDs() ([]string, error)
	// AppendJSONL marshals v and appends it as one stream line.
	AppendJSONL(id string, v interface{}) error
	// AppendRaw appends a raw line (must end with '\n') to the stream id.
	AppendRaw(id string, line []byte) error
	// ReadJSONL returns all raw lines of the stream id, in append order.
	ReadJSONL(id string) ([][]byte, error)
	// Exists reports whether the document id is present.
	Exists(id string) bool
	// Dir returns the directory used for auxiliary files of this namespace.
	Dir() string
	// Backend reports the driver name: "json" or "sqlite".
	Backend() string
}

// ---- JSON backend ----

// JSONStore keeps one directory per namespace holding {id}.json documents and
// {id}.jsonl append-only streams. Writes are atomic (tmp + rename).
type JSONStore struct {
	dir string
	mu  sync.Mutex
}

// NewJSON creates (if needed) the namespace directory and returns a JSONStore.
func NewJSON(baseDir string) *JSONStore {
	_ = os.MkdirAll(baseDir, 0o755)
	return &JSONStore{dir: baseDir}
}

func (s *JSONStore) jsonPath(id string) string  { return filepath.Join(s.dir, id+".json") }
func (s *JSONStore) jsonlPath(id string) string { return filepath.Join(s.dir, id+".jsonl") }

func (s *JSONStore) Load(id string, out interface{}) error {
	data, err := os.ReadFile(s.jsonPath(id))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (s *JSONStore) Save(id string, in interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.jsonPath(id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.jsonPath(id))
}

func (s *JSONStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(s.jsonPath(id))
	_ = os.Remove(s.jsonlPath(id))
	return nil
}

func (s *JSONStore) IDs() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".json") {
			ids = append(ids, strings.TrimSuffix(n, ".json"))
		}
	}
	return ids, nil
}

func (s *JSONStore) AppendJSONL(id string, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.AppendRaw(id, append(b, '\n'))
}

func (s *JSONStore) AppendRaw(id string, line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.jsonlPath(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// ReadJSONL reads all appended lines. It takes the write lock so a reader can
// never observe a half-written final line (AppendRaw holds this lock while
// writing), and skips malformed lines instead of failing the whole read — one
// corrupt record must not hide an entire message history.
func (s *JSONStore) ReadJSONL(id string) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.jsonlPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var lines [][]byte
	for _, l := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		// Guard against a truncated tail; anything unparsable is dropped here
		// rather than handed to the caller as corrupt JSON.
		if !json.Valid([]byte(t)) {
			continue
		}
		lines = append(lines, []byte(t))
	}
	return lines, nil
}

func (s *JSONStore) Exists(id string) bool {
	_, err := os.Stat(s.jsonPath(id))
	return err == nil
}

func (s *JSONStore) Dir() string { return s.dir }

func (s *JSONStore) Backend() string { return DriverJSON }

// ---- backend factory ----

// Driver names.
const (
	DriverJSON   = "json"
	DriverSQLite = "sqlite"
)

// Open returns a Store for a namespace, honouring the configured driver.
//
// When driver is "sqlite" the single dataDir/agenthub.db file is used and any
// pre-existing JSON data in dir is migrated across on first run, so enabling
// SQLite never loses existing configuration (providers/keys, roles, corpora).
// If the driver cannot initialise, Open falls back to JSON rather than blocking
// startup — the failure is reported to stderr and via the returned warning.
func Open(dataDir, ns, dir, driver string) (Store, error) {
	if driver == DriverSQLite {
		dbPath := filepath.Join(dataDir, "agenthub.db")
		st, err := NewSQLite(dbPath, ns, dir)
		if err == nil {
			if n, mErr := migrateJSONToSQLite(dir, ns, st); mErr == nil && n > 0 {
				fmt.Fprintf(os.Stderr, "storage: migrated %d items from JSON into sqlite (ns=%s)\n", n, ns)
			}
			return st, nil
		}
		// sqlite unavailable: keep serving from JSON
		return NewJSON(dir), fmt.Errorf("sqlite unavailable for ns %s (%v), using json fallback", ns, err)
	}
	return NewJSON(dir), nil
}
