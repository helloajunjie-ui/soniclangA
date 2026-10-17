package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Collection and document ids arrive straight from the URL path. Delete used to
// join the id into a directory path and call os.RemoveAll with no validation:
// `DELETE /api/v1/knowledge/collections/..` resolved to the library root and
// deleted every collection's documents (and staged uploads) while the collection
// rows survived. These tests pin the boundary at the service layer, which is
// where the filesystem is touched.
func TestDeleteRejectsTraversalIDs(t *testing.T) {
	k := newKnowledge(t)
	col := newCollection(t, k)
	ingest(t, k, col.ID, "keep.md", strings.Repeat("必须留下的内容 DELTA-9。\n", 20))

	for _, bad := range []string{"..", ".", "../..", "a/../..", "..%2f..", "", strings.Repeat("x", 65)} {
		if err := k.Delete(bad); err == nil {
			t.Errorf("Delete(%q) returned nil, want an error", bad)
		}
	}

	// The library must be intact: the documents are still there and searchable.
	if len(k.ListDocs(col.ID)) != 1 {
		t.Fatalf("documents were removed by a rejected delete")
	}
	if hits := k.Search(col.ID, "DELTA-9", 5); len(hits) == 0 {
		t.Error("search lost the document after a rejected delete")
	}
	// And the doc directories are still on disk.
	if _, err := os.Stat(filepath.Join(k.baseDir, "docs", col.ID)); err != nil {
		t.Errorf("docs dir for %s is gone: %v", col.ID, err)
	}
}

// Deleting a collection that does not exist must be an error, not a silent
// RemoveAll of a path built from the id.
func TestDeleteUnknownCollectionIsRefused(t *testing.T) {
	k := newKnowledge(t)
	if err := k.Delete("nosuchcollection"); err == nil {
		t.Fatal("deleting an unknown collection returned nil")
	}
}

// A caller-supplied collection id on create must be path-safe as well: docStore
// turns it into a directory.
func TestCreateRejectsUnsafeID(t *testing.T) {
	k := newKnowledge(t)
	for _, bad := range []string{"..", "../escape", "a/b", strings.Repeat("y", 65)} {
		if err := k.Create(&Collection{ID: bad, Name: "x"}); err == nil {
			t.Errorf("Create with id %q returned nil, want an error", bad)
		}
	}
	// A generated id still works.
	c := &Collection{Name: "正常"}
	if err := k.Create(c); err != nil {
		t.Fatalf("Create with a generated id: %v", err)
	}
}

// Document ids are path components too (JSON store: <dir>/<id>.json).
func TestDocOperationsRejectUnsafeIDs(t *testing.T) {
	k := newKnowledge(t)
	col := newCollection(t, k)

	if _, err := k.GetDoc(col.ID, ".."); err == nil {
		t.Error("GetDoc accepted a traversal id")
	}
	if err := k.DeleteDoc(col.ID, "../../etc/passwd"); err == nil {
		t.Error("DeleteDoc accepted a traversal id")
	}
	if _, err := k.GetDoc("..", "x"); err == nil {
		t.Error("GetDoc accepted a traversal collection id")
	}
	if docs := k.ListDocs(".."); docs != nil {
		t.Errorf("ListDocs accepted a traversal collection id and returned %d docs", len(docs))
	}
}
