// SQLite backend for the Store contract (DESIGN §7.1 escape hatch). It uses
// modernc.org/sqlite — a transpiled, pure-Go SQLite — so the build stays
// CGO-free and cross-compilable while gaining transactions, durable appends
// and indexed lookups for large corpora (knowledge chunks, message streams).
//
// All namespaces share one database file (dataDir/agenthub.db); rows are
// partitioned by an "ns" column, mirroring the per-directory JSON layout.
package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ---- shared connection ----

var (
	dbOnce sync.Once
	dbRef  *sql.DB
	dbErr  error
)

// sharedDB opens (once) the single SQLite file used by every namespace.
func sharedDB(path string) (*sql.DB, error) {
	dbOnce.Do(func() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			dbErr = err
			return
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			dbErr = err
			return
		}
		// SQLite serialises writers; a small pool avoids lock contention while
		// still allowing concurrent reads.
		db.SetMaxOpenConns(4)
		if err := db.Ping(); err != nil {
			dbErr = err
			_ = db.Close()
			return
		}
		if err := initSchema(db); err != nil {
			dbErr = err
			_ = db.Close()
			return
		}
		dbRef = db
	})
	return dbRef, dbErr
}

func initSchema(db *sql.DB) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA synchronous=NORMAL`,
		`CREATE TABLE IF NOT EXISTS documents (
			ns         TEXT NOT NULL,
			id         TEXT NOT NULL,
			data       BLOB NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (ns, id)
		)`,
		`CREATE TABLE IF NOT EXISTS streams (
			ns   TEXT NOT NULL,
			id   TEXT NOT NULL,
			seq  INTEGER NOT NULL,
			line BLOB NOT NULL,
			PRIMARY KEY (ns, id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_streams_ns_id ON streams(ns, id, seq)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("schema %q: %w", s, err)
		}
	}
	return nil
}

// CloseShared releases the shared database handle (best-effort, at shutdown).
func CloseShared() {
	if dbRef != nil {
		_ = dbRef.Close()
		dbRef = nil
	}
}

// resetSharedForTest closes the shared handle and allows a new one to be
// opened. Tests need it to release the file before their TempDir is removed
// (Windows refuses to delete an open file).
func resetSharedForTest() {
	CloseShared()
	dbOnce = sync.Once{}
	dbErr = nil
}

// ---- SQLiteStore ----

// SQLiteStore is a namespace view over the shared database.
type SQLiteStore struct {
	ns     string
	auxDir string // directory for auxiliary files (job status, upload staging)
	db     *sql.DB
	mu     sync.Mutex
}

// NewSQLite returns a Store backed by the shared database file.
func NewSQLite(dbPath, ns, auxDir string) (*SQLiteStore, error) {
	db, err := sharedDB(dbPath)
	if err != nil {
		return nil, err
	}
	if auxDir != "" {
		_ = os.MkdirAll(auxDir, 0o755)
	}
	return &SQLiteStore{ns: ns, auxDir: auxDir, db: db}, nil
}

func (s *SQLiteStore) Load(id string, out interface{}) error {
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM documents WHERE ns=? AND id=?`, s.ns, id).Scan(&data)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return json.Unmarshal(data, out)
}

func (s *SQLiteStore) Save(id string, in interface{}) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO documents(ns, id, data, updated_at) VALUES(?, ?, ?, ?)
		 ON CONFLICT(ns, id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		s.ns, id, data, time.Now().UnixMilli(),
	)
	return err
}

func (s *SQLiteStore) Remove(id string) error {
	if _, err := s.db.Exec(`DELETE FROM documents WHERE ns=? AND id=?`, s.ns, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM streams WHERE ns=? AND id=?`, s.ns, id)
	return err
}

func (s *SQLiteStore) IDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM documents WHERE ns=? ORDER BY id`, s.ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *SQLiteStore) AppendJSONL(id string, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.AppendRaw(id, append(b, '\n'))
}

// AppendRaw appends one stream line. seq is computed under a mutex so
// concurrent appends keep a deterministic order.
func (s *SQLiteStore) AppendRaw(id string, line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var seq int
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(seq), 0) FROM streams WHERE ns=? AND id=?`, s.ns, id,
	).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO streams(ns, id, seq, line) VALUES(?, ?, ?, ?)`,
		s.ns, id, seq+1, line,
	)
	return err
}

func (s *SQLiteStore) ReadJSONL(id string) ([][]byte, error) {
	rows, err := s.db.Query(
		`SELECT line FROM streams WHERE ns=? AND id=? ORDER BY seq`, s.ns, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines [][]byte
	for rows.Next() {
		var l []byte
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		t := string(l)
		if len(t) == 0 {
			continue
		}
		lines = append(lines, []byte(t))
	}
	return lines, rows.Err()
}

func (s *SQLiteStore) Exists(id string) bool {
	var n int
	err := s.db.QueryRow(`SELECT 1 FROM documents WHERE ns=? AND id=?`, s.ns, id).Scan(&n)
	return err == nil
}

func (s *SQLiteStore) Dir() string { return s.auxDir }

func (s *SQLiteStore) Backend() string { return DriverSQLite }

// ---- JSON -> SQLite migration ----

// migrateJSONToSQLite copies any pre-existing flat-file data for a namespace
// into SQLite. It is a no-op once the namespace already has rows, so it is safe
// to call on every start. Returns the number of items migrated.
func migrateJSONToSQLite(jsonDir, ns string, dst Store) (int, error) {
	entries, err := os.ReadDir(jsonDir)
	if err != nil {
		return 0, nil // nothing to migrate
	}
	// destination already populated => migration already done
	if ids, err := dst.IDs(); err == nil && len(ids) > 0 {
		return 0, nil
	}
	src := NewJSON(jsonDir)
	migrated := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		switch {
		case strings.HasSuffix(n, ".json"):
			id := strings.TrimSuffix(n, ".json")
			// Documents may be objects OR arrays (e.g. knowledge/index holds a
			// JSON array of chunk records), so decode into interface{} and let
			// Save re-encode whatever shape it was. Decoding into map[string]
			// would silently drop array documents.
			var v interface{}
			if err := src.Load(id, &v); err != nil {
				fmt.Fprintf(os.Stderr, "storage: skip %s/%s (%v)\n", ns, id, err)
				continue
			}
			if err := dst.Save(id, v); err != nil {
				return migrated, err
			}
			migrated++
		case strings.HasSuffix(n, ".jsonl"):
			id := strings.TrimSuffix(n, ".jsonl")
			lines, err := src.ReadJSONL(id)
			if err != nil {
				continue
			}
			for _, l := range lines {
				if err := dst.AppendRaw(id, append(l, '\n')); err != nil {
					return migrated, err
				}
				migrated++
			}
		}
	}
	return migrated, nil
}
