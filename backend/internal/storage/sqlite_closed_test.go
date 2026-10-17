package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

// After CloseShared the sync.Once prevents the shared handle from ever being
// opened again, so sharedDB used to return (nil, nil). Every caller treats a nil
// *sql.DB as usable, which turned "the handle is gone" into a nil-pointer panic
// several frames away (observed as `database/sql.(*DB).conn(0x0 …)` inside the
// JSON→SQLite migration). It must be an error at the call site instead.
func TestOpenAfterCloseReportsAClosedHandle(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "closed.db")
	if _, err := NewSQLite(dbPath, "ns_close", dir); err != nil {
		t.Fatalf("first open: %v", err)
	}

	CloseShared() // no reset: exactly the shutdown-then-use state
	defer resetSharedForTest()

	_, err := NewSQLite(dbPath, "ns_close", dir)
	if err == nil {
		t.Fatal("reopening after CloseShared returned no error; callers get a nil *sql.DB and panic")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error should say the handle is closed, got: %v", err)
	}
}
