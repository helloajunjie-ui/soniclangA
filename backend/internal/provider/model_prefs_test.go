package provider

import (
	"testing"

	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/storage"
)

func prefBool(b bool) *bool { return &b }

func prefIDs(models []ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func prefHas(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// This is the reason curation lives in a map beside the catalogue instead of in
// fields on ModelInfo. The vendor is re-read on every refresh and Models is
// replaced wholesale, so a flag stored in that slice would be wiped by the next
// refresh — silently, since the models would still be listed and the switch
// would simply read as "on" again.
func TestModelPrefsSurviveRefresh(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "keep", "noisy-1", "noisy-2")
	if err := s.SetModelPref("p1", "noisy-1", prefBool(true), nil); err != nil {
		t.Fatal(err)
	}

	useRuntime(s, "p1", fakeRuntime{id: "p1", models: []ModelInfo{
		{ID: "keep"}, {ID: "noisy-1"}, {ID: "noisy-2"}, {ID: "brand-new"},
	}})
	if _, err := s.RefreshModels("p1"); err != nil {
		t.Fatal(err)
	}

	got, err := s.UsableModels("p1")
	if err != nil {
		t.Fatal(err)
	}
	ids := prefIDs(got)
	if prefHas(ids, "noisy-1") {
		t.Fatalf("a refresh resurrected a switched-off model: %v", ids)
	}
	if !prefHas(ids, "brand-new") {
		t.Fatalf("a newly discovered model must be usable by default: %v", ids)
	}
}

// Narrowing a picker to one provider is not enough on its own: an aggregator
// carries hundreds of models under a single provider, so the user's curation is
// the only thing that makes the downstream list passable.
func TestUsableModelsDropsSwitchedOff(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "a", "b", "c")
	for _, id := range []string{"b", "c"} {
		if err := s.SetModelPref("p1", id, prefBool(true), nil); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.UsableModels("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("expected only the kept model, got %v", prefIDs(got))
	}
}

// Switching every model off must yield an empty list rather than falling back
// to the full catalogue. A silent fallback would make the switch a decoration,
// which is worse than having no switch at all.
func TestSwitchingEveryModelOffYieldsNothing(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "a", "b")
	for _, id := range []string{"a", "b"} {
		if err := s.SetModelPref("p1", id, prefBool(true), nil); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.UsableModels("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty list, got %v", prefIDs(got))
	}
}

// Curation is the user's, not the editor's — the same rule that keeps Update
// from resetting Enabled or dropping Keys. A rename that PUTs the whole entity
// must not quietly re-enable hundreds of models.
func TestUpdateCannotResetCuration(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "a", "b")
	if err := s.SetModelPref("p1", "b", prefBool(true), nil); err != nil {
		t.Fatal(err)
	}

	edited, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	edited.Name = "Renamed"
	edited.ModelPrefs = map[string]ModelPref{}
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
	if !got.ModelPrefs["b"].Disabled {
		t.Fatalf("an edit re-enabled the catalogue: %+v", got.ModelPrefs)
	}
}

// The two halves are independent switches. Passing nil has to mean "leave this
// one alone" — with a bare bool, toggling a favourite would carry disabled=false
// with it and re-enable a model the user had switched off.
func TestSetModelPrefLeavesTheOtherHalfAlone(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "a")
	if err := s.SetModelPref("p1", "a", prefBool(true), prefBool(true)); err != nil {
		t.Fatal(err)
	}

	if err := s.SetModelPref("p1", "a", prefBool(false), nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelPrefs["a"].Disabled {
		t.Fatalf("disabled was not cleared: %+v", got.ModelPrefs)
	}
	if !got.ModelPrefs["a"].Favorite {
		t.Fatalf("a partial update cleared the other half: %+v", got.ModelPrefs)
	}
}

// Sparse by design: a vendor publishing hundreds of models must not grow the
// record, so a model returned to its default erases its entry instead of
// keeping an explicit "false".
func TestReturningToDefaultErasesTheEntry(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "a")
	if err := s.SetModelPref("p1", "a", prefBool(true), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelPref("p1", "a", prefBool(false), nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ModelPrefs) != 0 {
		t.Fatalf("a default-valued preference was kept: %+v", got.ModelPrefs)
	}
}

// Curation is user data, so it has to come back after a restart — the store is
// the only thing carrying it across.
func TestCurationSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	v, err := keyvault.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(storage.NewJSON(dir), v)
	newProvider(t, s)
	seedCatalogue(s, "p1", "a")
	if err := s.SetModelPref("p1", "a", prefBool(true), prefBool(true)); err != nil {
		t.Fatal(err)
	}

	reloaded := New(storage.NewJSON(dir), v)
	got, err := reloaded.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ModelPrefs["a"].Disabled || !got.ModelPrefs["a"].Favorite {
		t.Fatalf("curation did not survive a reload: %+v", got.ModelPrefs)
	}
}

// An empty id would create an entry no model can ever match, so it is refused
// rather than stored.
func TestSetModelPrefRejectsEmptyID(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	if err := s.SetModelPref("p1", "", prefBool(true), nil); err == nil {
		t.Fatal("an empty model id was accepted")
	}
}

// "All" means the catalogue, not the pref map. A choice about a model the
// vendor has temporarily stopped advertising must survive a bulk operation, and
// re-enabling everything must leave the map sparse again rather than filling it
// with explicit falses.
func TestSetAllModelPrefsTouchesOnlyTheCatalogue(t *testing.T) {
	s := newTestService(t)
	newProvider(t, s)
	seedCatalogue(s, "p1", "a", "b")
	if err := s.SetModelPref("p1", "gone", prefBool(true), prefBool(true)); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAllModelPrefs("p1", prefBool(true), nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ModelPrefs["a"].Disabled || !got.ModelPrefs["b"].Disabled {
		t.Fatalf("bulk disable missed a model: %+v", got.ModelPrefs)
	}
	if !got.ModelPrefs["gone"].Favorite || !got.ModelPrefs["gone"].Disabled {
		t.Fatalf("bulk disable touched a model outside the catalogue: %+v", got.ModelPrefs)
	}

	if err := s.SetAllModelPrefs("p1", prefBool(false), nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ModelPrefs) != 1 || !got.ModelPrefs["gone"].Favorite {
		t.Fatalf("re-enabling left stale entries behind: %+v", got.ModelPrefs)
	}
}
