package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Both backends must satisfy the same contract. Note the sqlite driver keeps a
// process-wide shared connection (dbOnce), so all sqlite subtests deliberately
// share one temp database and are separated by namespace.
func TestBackends(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "test.db")
	// release the shared sqlite handle so TempDir can be removed
	defer resetSharedForTest()

	t.Run("json", func(t *testing.T) {
		dir := filepath.Join(root, "json_ns")
		exerciseStore(t, NewJSON(dir), DriverJSON)
	})

	t.Run("sqlite", func(t *testing.T) {
		dir := filepath.Join(root, "sqlite_ns")
		st, err := NewSQLite(dbPath, "ns_basic", dir)
		if err != nil {
			t.Fatalf("NewSQLite: %v", err)
		}
		exerciseStore(t, st, DriverSQLite)
	})

	// Regression: knowledge/index documents are JSON ARRAYS. Migrating them by
	// decoding into map[string]interface{} silently dropped them.
	t.Run("sqlite_array_document", func(t *testing.T) {
		dir := filepath.Join(root, "sqlite_arr")
		st, err := NewSQLite(dbPath, "ns_array", dir)
		if err != nil {
			t.Fatalf("NewSQLite: %v", err)
		}
		records := []map[string]interface{}{
			{"chunkId": "a#0", "text": "第一条"},
			{"chunkId": "a#1", "text": "第二条"},
		}
		if err := st.Save("kb1", records); err != nil {
			t.Fatalf("save array: %v", err)
		}
		var got []map[string]interface{}
		if err := st.Load("kb1", &got); err != nil {
			t.Fatalf("load array: %v", err)
		}
		if len(got) != 2 || got[1]["chunkId"] != "a#1" {
			t.Fatalf("array round-trip broken: %#v", got)
		}
	})

	t.Run("migration_preserves_array_document", func(t *testing.T) {
		jsonDir := filepath.Join(root, "migrate_src")
		src := NewJSON(jsonDir)
		// an array document plus an object document plus a stream
		if err := src.Save("kb1", []map[string]interface{}{{"chunkId": "x#0"}}); err != nil {
			t.Fatal(err)
		}
		if err := src.Save("obj", map[string]string{"k": "v"}); err != nil {
			t.Fatal(err)
		}
		if err := src.AppendRaw("conv1", []byte("{\"n\":1}\n")); err != nil {
			t.Fatal(err)
		}
		if err := src.AppendRaw("conv1", []byte("{\"n\":2}\n")); err != nil {
			t.Fatal(err)
		}

		dst, err := NewSQLite(dbPath, "ns_migrated", root)
		if err != nil {
			t.Fatalf("NewSQLite: %v", err)
		}
		n, err := migrateJSONToSQLite(jsonDir, "ns_migrated", dst)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		if n != 4 { // 1 array + 1 object + 2 stream lines
			t.Fatalf("migrated %d items, want 4", n)
		}
		var arr []map[string]interface{}
		if err := dst.Load("kb1", &arr); err != nil {
			t.Fatalf("array doc lost in migration: %v", err)
		}
		if len(arr) != 1 || arr[0]["chunkId"] != "x#0" {
			t.Fatalf("array doc corrupted: %#v", arr)
		}
		var obj map[string]string
		if err := dst.Load("obj", &obj); err != nil || obj["k"] != "v" {
			t.Fatalf("object doc lost: %v %#v", err, obj)
		}
		lines, err := dst.ReadJSONL("conv1")
		if err != nil || len(lines) != 2 {
			t.Fatalf("stream lost: %v lines=%d", err, len(lines))
		}
	})

	t.Run("migration_is_idempotent", func(t *testing.T) {
		jsonDir := filepath.Join(root, "migrate_src")
		dst, err := NewSQLite(dbPath, "ns_migrated", root)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := migrateJSONToSQLite(jsonDir, "ns_migrated", dst)
		if n != 0 {
			t.Fatalf("second migration should be a no-op, got %d", n)
		}
	})
}

func exerciseStore(t *testing.T, s Store, wantBackend string) {
	t.Helper()
	if s.Backend() != wantBackend {
		t.Fatalf("Backend()=%s want %s", s.Backend(), wantBackend)
	}
	type doc struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	// save + load
	if err := s.Save("a", doc{ID: "a", Name: "alpha"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	var got doc
	if err := s.Load("a", &got); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Name != "alpha" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// exists + ids
	if !s.Exists("a") {
		t.Error("Exists(a) should be true")
	}
	ids, err := s.IDs()
	if err != nil {
		t.Fatalf("IDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("IDs=%v want [a]", ids)
	}
	// missing document
	var missing doc
	if err := s.Load("nope", &missing); err == nil {
		t.Error("Loading a missing document must fail")
	}
	if s.Exists("nope") {
		t.Error("Exists(nope) should be false")
	}
	// stream order must be preserved
	for i := 1; i <= 3; i++ {
		b, _ := json.Marshal(map[string]int{"i": i})
		if err := s.AppendRaw("st", append(b, '\n')); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	lines, err := s.ReadJSONL("st")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("stream len=%d want 3", len(lines))
	}
	for i, l := range lines {
		var m map[string]int
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if m["i"] != i+1 {
			t.Fatalf("stream order broken at %d: %v", i, m)
		}
	}
	// remove clears both document and stream
	if err := s.Remove("a"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if s.Exists("a") {
		t.Error("document should be gone after Remove")
	}
	// auxiliary dir must exist for job status files
	if d := s.Dir(); d == "" {
		t.Error("Dir() must not be empty")
	} else if _, err := os.Stat(d); errors.Is(err, os.ErrNotExist) {
		t.Errorf("Dir()=%s does not exist", d)
	}
}
