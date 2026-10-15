package provider

import (
	"context"
	"errors"
	"testing"
)

// fakeRuntime scripts a model catalogue so discovery can be exercised without a
// network call.
type fakeRuntime struct {
	id     string
	models []ModelInfo
	err    error
}

func (f fakeRuntime) ID() string { return f.id }

func (f fakeRuntime) Chat(context.Context, []Message, CallOptions) (*ChatResponse, error) {
	return nil, errors.New("not implemented")
}

func (f fakeRuntime) ChatStream(context.Context, []Message, CallOptions) (<-chan StreamChunk, error) {
	return nil, errors.New("not implemented")
}

func (f fakeRuntime) Embeddings(context.Context, string, []string) ([][]float32, error) {
	return nil, errors.New("not implemented")
}

func (f fakeRuntime) Models(context.Context) ([]ModelInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

// seedCatalogue writes a discovered catalogue straight into the cache, standing
// in for a completed probe.
func seedCatalogue(s *Service, id string, ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	models := make([]ModelInfo, 0, len(ids))
	for _, m := range ids {
		models = append(models, ModelInfo{ID: m, Name: m, Capabilities: ModelCaps{Chat: true}})
	}
	s.cache[id].Models = models
}

func useRuntime(s *Service, id string, rt Runtime) {
	s.adaptersMu.Lock()
	defer s.adaptersMu.Unlock()
	s.adapters[id] = rt
}

func newProvider(t *testing.T, s *Service) {
	t.Helper()
	p := &Provider{ID: "p1", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"}
	if err := s.Create(p, []string{"sk-test-key-123456"}); err != nil {
		t.Fatal(err)
	}
}

// The catalogue is discovered, not authored. When it followed the request body a
// client could PUT its own list — and since EnsureModels treats a non-empty list
// as "already known", a single hand-written entry would shadow the vendor's real
// catalogue for good, leaving /v1/models and every role's model picker showing a
// fiction that no refresh could clear.
func TestUpdateCannotAuthorModelCatalogue(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "real-1", "real-2")

	s.mu.Lock()
	s.cache["p1"].LastError = "prior probe failed"
	s.mu.Unlock()

	edited, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	edited.Name = "Renamed"
	edited.Models = []ModelInfo{{ID: "made-up"}}
	edited.LastError = ""
	if err := s.Update("p1", edited); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" {
		t.Fatalf("the edit itself did not stick: name=%q", got.Name)
	}
	if len(got.Models) != 2 || got.Models[0].ID != "real-1" {
		t.Fatalf("client-authored catalogue leaked into storage: %+v", got.Models)
	}
	if got.LastError != "prior probe failed" {
		t.Fatalf("an edit cleared runtime state: LastError=%q", got.LastError)
	}
}

// RefreshModels must replace the cached list rather than merge into it: a model
// retired upstream has to disappear from the picker, not linger forever.
func TestRefreshModelsReplacesCachedCatalogue(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "stale-1", "stale-2", "stale-3")
	useRuntime(s, "p1", fakeRuntime{id: "p1", models: []ModelInfo{{ID: "fresh"}}})

	got, err := s.RefreshModels("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("refresh returned %+v", got)
	}
	stored, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Models) != 1 || stored.Models[0].ID != "fresh" {
		t.Fatalf("refresh did not replace the catalogue: %+v", stored.Models)
	}
}

// A failed refresh is a transient network condition, not evidence that the
// provider has no models. Blanking the list would empty every role's picker and
// make working bindings render as unset.
func TestFailedRefreshKeepsCatalogue(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "real-1", "real-2")
	useRuntime(s, "p1", fakeRuntime{id: "p1", err: errors.New("dial tcp 1.2.3.4:443: i/o timeout")})

	if _, err := s.RefreshModels("p1"); err == nil {
		t.Fatal("a failing probe reported success")
	}
	stored, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Models) != 2 {
		t.Fatalf("a failed refresh blanked the catalogue: %+v", stored.Models)
	}
}

// EnsureModels is the lazy path used by /v1/models: it must fetch when the list
// is empty and leave a populated list alone.
func TestEnsureModelsFetchesOnlyWhenEmpty(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	useRuntime(s, "p1", fakeRuntime{id: "p1", models: []ModelInfo{{ID: "discovered"}}})

	got, err := s.EnsureModels("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "discovered" {
		t.Fatalf("EnsureModels did not fetch into an empty list: %+v", got)
	}

	useRuntime(s, "p1", fakeRuntime{id: "p1", models: []ModelInfo{{ID: "would-overwrite"}}})
	again, err := s.EnsureModels("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != "discovered" {
		t.Fatalf("EnsureModels re-fetched over a populated list: %+v", again)
	}
}
