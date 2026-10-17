package memory

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
)

// The context-memory path had three defects that all fail silently: the model
// simply stops being reminded of things, with nothing in the logs.
//
//  1. Update() re-registered the retrieval row with a nil embedding, and Refresh
//     replaces rather than merges — so any flag-only edit removed the memory from
//     semantic recall until it was embedded again.
//  2. Embed() merged the previously persisted index unconditionally, so records
//     for deleted/invalidated memories were never dropped: they kept occupying
//     recall candidate slots, and Recall throws those hits away on the cache
//     lookup, so live memories eventually stopped being returned.
//  3. truncate() sliced by bytes, so a long Chinese persona was cut mid-rune and
//     invalid UTF-8 went into the system prompt.

// embedServer answers /v1/embeddings with a constant vector. A constant vector
// makes the vector path trivially verifiable: after embedding, *any* query
// matches, so a query that BM25 cannot match isolates the vector path.
func embedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		// One embedding per input string.
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(strings.NewReader(`{"input":["x"]}`)).Decode(&body)
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2,0.3]}],"model":"mock-embed"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newMemoryService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	upstream := embedServer(t)

	vault, err := keyvault.New(dir)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	provs := provider.New(storage.NewJSON(dir+"/providers"), vault)
	if err := provs.Create(&provider.Provider{
		ID: "p-embed", Name: "mock", Type: "openai",
		BaseURL: upstream.URL + "/v1", Enabled: true,
	}, []string{"sk-test"}); err != nil {
		t.Fatalf("provider: %v", err)
	}

	s := New(
		storage.NewJSON(dir+"/mem"),
		storage.NewJSON(dir+"/memidx"),
		dir+"/memory",
		retrieval.New(),
		provs,
		false,
	)
	s.SetEmbedding("p-embed", "mock-embed", 3)
	return s, dir
}

// A query with no 2-gram overlap can only be answered by the vector path.
const nonBM25Query = "zzz"

func TestUpdateKeepsTheVector(t *testing.T) {
	s, _ := newMemoryService(t)
	m, err := s.Add(&Memory{Level: LevelL1, Content: "用户偏好简洁的回答，不要客套。"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.Embed([]string{m.ID}, "p-embed", "mock-embed"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if hits, _ := s.Recall(nonBM25Query, 5, ScopeGlobal); len(hits) == 0 {
		t.Fatal("precondition: the embedded memory is not reachable through the vector path")
	}

	// A flag-only edit (what the UI does when you toggle importance).
	m.Importance = 0.9
	if err := s.Update(m); err != nil {
		t.Fatalf("update: %v", err)
	}

	if hits, _ := s.Recall(nonBM25Query, 5, ScopeGlobal); len(hits) == 0 {
		t.Error("a flag-only update dropped the memory out of semantic recall")
	}

	// A text edit *should* invalidate the old vector (it no longer describes the
	// new text), so the semantic path may legitimately go quiet until re-embedding.
	m.Content = "用户现在喜欢详细的解释。"
	if err := s.Update(m); err != nil {
		t.Fatalf("update text: %v", err)
	}
	if hits, _ := s.Recall(nonBM25Query, 5, ScopeGlobal); len(hits) != 0 {
		t.Errorf("the stale vector survived a content change: %d hits", len(hits))
	}
}

// Deleted memories must leave the persisted index, otherwise every re-embed
// resurrects them and they crowd out live results.
func TestEmbedDropsDeletedRecords(t *testing.T) {
	s, _ := newMemoryService(t)
	keep, err := s.Add(&Memory{Level: LevelL1, Content: "要留下的记忆。"})
	if err != nil {
		t.Fatalf("add keep: %v", err)
	}
	gone, err := s.Add(&Memory{Level: LevelL1, Content: "要删除的记忆。"})
	if err != nil {
		t.Fatalf("add gone: %v", err)
	}
	if err := s.Embed(nil, "p-embed", "mock-embed"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if err := s.Delete(gone.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The next embedding pass rewrites the persisted index.
	if err := s.Embed(nil, "p-embed", "mock-embed"); err != nil {
		t.Fatalf("re-embed: %v", err)
	}

	var persisted persistedIndex
	if err := s.indexes.Load(scopeKey(ScopeGlobal), &persisted); err != nil {
		t.Fatalf("load index: %v", err)
	}
	for _, r := range persisted.Records {
		if r.ID == gone.ID {
			t.Error("the deleted memory is still in the persisted vector index")
		}
	}
	found := false
	for _, r := range persisted.Records {
		if r.ID == keep.ID {
			found = true
		}
	}
	if !found {
		t.Error("the surviving memory fell out of the index")
	}
}

// The persona is injected into every turn; cutting it mid-rune puts invalid
// UTF-8 into the prompt.
func TestGuidanceTruncatesOnARuneBoundary(t *testing.T) {
	s, _ := newMemoryService(t)
	// Long Chinese text: 2 bytes per rune in UTF-8 (3 for most of these), so a
	// byte limit almost always lands inside a character.
	body := strings.Repeat("用户偏好简洁直接的中文回答，不接受冗长的客套与铺垫。\n", 40)
	if err := s.SetPersona(body); err != nil {
		t.Fatalf("set persona: %v", err)
	}

	g := s.Guidance(400)
	if !utf8.ValidString(g) {
		t.Fatal("guidance contains invalid UTF-8: the persona was cut mid-rune")
	}
	if len(g) > 400+64 {
		t.Errorf("guidance ignored the character budget: %d bytes", len(g))
	}
	if !strings.HasPrefix(g, "<persona>") {
		t.Errorf("guidance lost its boundary marker: %q", g[:20])
	}
}
