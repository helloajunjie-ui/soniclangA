package knowledge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
)

// The knowledge service had no tests at all, while every chat turn with a
// role-bound collection goes through it. What matters here is the lifecycle:
// a document is created empty and pending, ingestion is what makes it
// retrievable, and deleting it has to take its chunks out of the index too —
// a citation pointing at a deleted document is worse than no citation.

func newKnowledge(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	open := func(ns string) storage.Store {
		st, err := storage.Open(dir, ns, filepath.Join(dir, filepath.FromSlash(ns)), storage.DriverJSON)
		if err != nil {
			t.Fatalf("storage %s: %v", ns, err)
		}
		return st
	}
	return New(
		open("knowledge/collections"),
		open("knowledge/index"),
		filepath.Join(dir, "knowledge"),
		dir,
		storage.DriverJSON,
		retrieval.New(),
		nil, // no providers: BM25-only, which is the documented degradation path
	)
}

func newCollection(t *testing.T, k *Service) *Collection {
	t.Helper()
	c := &Collection{Name: "测试库"}
	if err := k.Create(c); err != nil {
		t.Fatalf("create collection: %v", err)
	}
	return c
}

// ingest is the whole pipeline (parse → chunk → index) for one document.
func ingest(t *testing.T, k *Service, collID, name, body string) *Doc {
	t.Helper()
	data := []byte(body)
	d, err := k.CreateDoc(collID, name, "", data)
	if err != nil {
		t.Fatalf("create doc: %v", err)
	}
	if err := k.Ingest(context.Background(), collID, d, data, nil); err != nil {
		t.Fatalf("ingest %s: %v", name, err)
	}
	return d
}

func TestCreateCollectionSetsDefaults(t *testing.T) {
	k := newKnowledge(t)
	c := &Collection{Name: "默认参数"}
	if err := k.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.ID == "" {
		t.Error("collection has no id")
	}
	// A zero chunk size would make chunk.Split return nothing, so the default
	// has to be applied at creation rather than at ingest time.
	if c.ChunkSize != DefaultChunkSize {
		t.Errorf("ChunkSize = %d, want %d", c.ChunkSize, DefaultChunkSize)
	}
	if c.IndexStatus != StatusReady {
		t.Errorf("IndexStatus = %q, want ready", c.IndexStatus)
	}

	got, err := k.Get(c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "默认参数" {
		t.Errorf("name = %q", got.Name)
	}
	if len(k.List()) != 1 {
		t.Errorf("List() returned %d collections, want 1", len(k.List()))
	}
}

func TestCreateDocStartsPendingAndCounts(t *testing.T) {
	k := newKnowledge(t)
	col := newCollection(t, k)

	body := "# 标题\n\n正文内容。\n"
	d, err := k.CreateDoc(col.ID, "notes.md", "", []byte(body))
	if err != nil {
		t.Fatalf("create doc: %v", err)
	}
	if d.IndexStatus != StatusPending {
		t.Errorf("IndexStatus = %q, want pending (ingestion has not run)", d.IndexStatus)
	}
	if d.Trust != TrustUnverified {
		t.Errorf("Trust = %q, want unverified by default", d.Trust)
	}
	if d.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", d.Size, len(body))
	}

	// DocCount is what the UI shows next to the collection; it has to move with
	// the document store, not with ingestion.
	got, err := k.Get(col.ID)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if got.DocCount != 1 {
		t.Errorf("DocCount = %d, want 1", got.DocCount)
	}
	if docs := k.ListDocs(col.ID); len(docs) != 1 {
		t.Errorf("ListDocs returned %d docs, want 1", len(docs))
	}
	if _, err := k.GetDoc(col.ID, d.ID); err != nil {
		t.Errorf("GetDoc: %v", err)
	}
}

// The point of ingestion is that the text becomes findable.
func TestIngestMakesDocumentSearchable(t *testing.T) {
	k := newKnowledge(t)
	col := newCollection(t, k)

	ingest(t, k, col.ID, "运维手册.md", strings.Repeat(
		"本手册说明发布流程。发布前需要冻结主干，口令是 KESTREL-7734。\n", 20))

	got := k.Search(col.ID, "KESTREL-7734", 5)
	if len(got) == 0 {
		t.Fatal("ingested document is not retrievable by its own distinctive term")
	}
	if !strings.Contains(got[0].Text, "KESTREL-7734") {
		t.Errorf("top hit does not contain the query term: %q", got[0].Text)
	}
	// Citations carry the document name so the UI can say where the answer came
	// from; a chunk id alone is unreadable.
	if got[0].DocName == "" || got[0].DocName == got[0].Doc {
		t.Errorf("citation has no human-readable document name: %+v", got[0])
	}

	// Chunk counters feed the collection card.
	colNow, err := k.Get(col.ID)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if colNow.ChunkCount == 0 {
		t.Error("ChunkCount is 0 after a successful ingest")
	}
}

// Deleting a document must take its chunks out of the retrieval index, or the
// agent keeps citing something the user removed.
func TestDeleteDocRemovesItFromSearch(t *testing.T) {
	k := newKnowledge(t)
	col := newCollection(t, k)

	kept := ingest(t, k, col.ID, "保留.md", strings.Repeat("这条要留下，词是 ALPHA-1。\n", 20))
	gone := ingest(t, k, col.ID, "删除.md", strings.Repeat("这条要删掉，词是 BRAVO-2。\n", 20))

	if len(k.Search(col.ID, "BRAVO-2", 5)) == 0 {
		t.Fatal("precondition: deleted-candidate is not retrievable before deletion")
	}

	if err := k.DeleteDoc(col.ID, gone.ID); err != nil {
		t.Fatalf("delete doc: %v", err)
	}
	if hits := k.Search(col.ID, "BRAVO-2", 5); len(hits) != 0 {
		t.Errorf("deleted document still returned %d hits", len(hits))
	}
	// The surviving document is untouched — a delete must not clear the index.
	if len(k.Search(col.ID, "ALPHA-1", 5)) == 0 {
		t.Error("deleting one document broke retrieval for another")
	}
	_ = kept

	colNow, err := k.Get(col.ID)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	if colNow.DocCount != 1 {
		t.Errorf("DocCount = %d after deleting one of two, want 1", colNow.DocCount)
	}
}

// Deleting the collection drops everything with it.
func TestDeleteCollectionRemovesDocumentsAndIndex(t *testing.T) {
	k := newKnowledge(t)
	col := newCollection(t, k)
	ingest(t, k, col.ID, "a.md", strings.Repeat("内容 CHARLIE-3。\n", 20))

	if err := k.Delete(col.ID); err != nil {
		t.Fatalf("delete collection: %v", err)
	}
	if _, err := k.Get(col.ID); err == nil {
		t.Error("collection still readable after delete")
	}
	if len(k.List()) != 0 {
		t.Errorf("List() still reports %d collections", len(k.List()))
	}
	if hits := k.Search(col.ID, "CHARLIE-3", 5); len(hits) != 0 {
		t.Errorf("deleted collection still returns %d hits", len(hits))
	}
}

// Searching a collection that does not exist must return nothing rather than
// panic: the module switch lets a role keep a stale collection id.
func TestSearchOnMissingCollectionIsEmpty(t *testing.T) {
	k := newKnowledge(t)
	if hits := k.Search("no-such-collection", "任何词", 5); len(hits) != 0 {
		t.Errorf("got %d hits for a missing collection", len(hits))
	}
	if hits := k.SearchCollections([]string{"a", "b"}, "任何词", 5); len(hits) != 0 {
		t.Errorf("got %d hits across missing collections", len(hits))
	}
}

// An unparsable document must be marked as failed with the reason attached —
// "pending forever" is what makes a broken upload look like a stuck queue.
func TestIngestFailureMarksDocument(t *testing.T) {
	k := newKnowledge(t)
	col := newCollection(t, k)

	// kind "pdf" with bytes that are not a PDF.
	data := []byte("this is definitely not a pdf")
	d, err := k.CreateDoc(col.ID, "broken.pdf", "pdf", data)
	if err != nil {
		t.Fatalf("create doc: %v", err)
	}
	if err := k.Ingest(context.Background(), col.ID, d, data, nil); err == nil {
		t.Fatal("ingesting garbage as a PDF returned no error")
	}

	got, err := k.GetDoc(col.ID, d.ID)
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	if got.IndexStatus != StatusError {
		t.Errorf("IndexStatus = %q, want error", got.IndexStatus)
	}
	if got.Error == "" {
		t.Error("failed document has no error message")
	}
}
