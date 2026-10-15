package provider

import (
	"context"
	"testing"

	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/storage"
)

// newTestService builds a Service over a temp dir with a working vault.
func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	v, err := keyvault.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(storage.NewJSON(dir), v)
}

// The adapter caches its own copy of the entity, so an edit that does not drop
// the cached adapter is silently ignored: the old baseUrl keeps being used
// until the process restarts.
func TestUpdateInvalidatesCachedAdapter(t *testing.T) {
	s := newTestService(t)
	p := &Provider{ID: "p1", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"}
	if err := s.Create(p, []string{"sk-test-key-123456"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.GetRuntime("p1")
	if err != nil {
		t.Fatal(err)
	}
	if second, _ := s.GetRuntime("p1"); first != second {
		t.Fatal("adapter should be cached between calls")
	}

	updated, _ := s.Get("p1")
	updated.BaseURL = "https://b.example/v1"
	if err := s.Update("p1", updated); err != nil {
		t.Fatal(err)
	}
	third, err := s.GetRuntime("p1")
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("Update did not invalidate the cached adapter: the old base URL stays in effect")
	}
	ad, ok := third.(*openAIAdapter)
	if !ok {
		t.Fatalf("unexpected adapter type %T", third)
	}
	if ad.entity.BaseURL != "https://b.example/v1" {
		t.Fatalf("adapter still points at %q", ad.entity.BaseURL)
	}
}

// Adding a key must also invalidate: otherwise the new key is never selected.
func TestAddKeyInvalidatesCachedAdapter(t *testing.T) {
	s := newTestService(t)
	p := &Provider{ID: "p2", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"}
	if err := s.Create(p, []string{"sk-first-key-000001"}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetRuntime("p2")
	if err := s.AddKey("p2", "sk-second-key-000002", "备用"); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetRuntime("p2")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("AddKey did not invalidate the adapter: the new key would be ignored")
	}
	ad := after.(*openAIAdapter)
	if len(ad.entity.Keys) != 2 {
		t.Fatalf("adapter sees %d keys, want 2", len(ad.entity.Keys))
	}
}

// List/Get must hand out copies: the caller serialises them without the lock.
func TestListReturnsDetachedCopies(t *testing.T) {
	s := newTestService(t)
	p := &Provider{ID: "p3", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"}
	if err := s.Create(p, []string{"sk-key-0000001"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("p3")
	if err != nil {
		t.Fatal(err)
	}
	got.Name = "mutated by caller"
	got.Keys[0].Status = "tampered"

	fresh, _ := s.Get("p3")
	if fresh.Name == "mutated by caller" {
		t.Fatal("Get returned the live cached pointer")
	}
	if fresh.Keys[0].Status == "tampered" {
		t.Fatal("Get shares the Keys slice with the cache")
	}
}

// The health map is read on every GetRuntime; a concurrent Create must not race
// with it (run with -race to see the effect).
func TestGetRuntimeConcurrentWithCreate(t *testing.T) {
	s := newTestService(t)
	base := &Provider{ID: "p4", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"}
	if err := s.Create(base, []string{"sk-key-0000001"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_, _ = s.GetRuntime("p4")
		}
	}()
	for i := 0; i < 50; i++ {
		p := &Provider{ID: "churn", Name: "C", Type: "openai", BaseURL: "https://c.example/v1"}
		_ = s.Create(p, []string{"sk-key-0000002"})
		_ = s.Delete("churn")
	}
	<-done
	_ = context.Background()
}

// A partial PUT must not destroy the provider's keys. Keys are vault-backed and
// managed only through Create/AddKey/RemoveKey, so a client that PUTs just a
// changed baseUrl used to leave a provider that still existed, still showed its
// name — and could no longer authenticate at all. The failure is silent: the
// next call simply 401s.
func TestUpdatePreservesKeys(t *testing.T) {
	s := newTestService(t)
	p := &Provider{ID: "p9", Name: "P", Type: "anthropic", BaseURL: "https://a.example/v1"}
	if err := s.Create(p, []string{"sk-key-aaaaaaaa", "sk-key-bbbbbbbb"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get("p9")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Keys) != 2 {
		t.Fatalf("expected 2 keys after create, got %d", len(before.Keys))
	}
	// A minimal edit, exactly what a CLI or a script would send.
	if err := s.Update("p9", &Provider{BaseURL: "https://b.example/v1"}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get("p9")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Keys) != 2 {
		t.Fatalf("a partial update wiped the keys: %d left", len(after.Keys))
	}
	if after.BaseURL != "https://b.example/v1" {
		t.Fatalf("baseUrl not applied: %q", after.BaseURL)
	}
	// The sealed secrets must still resolve, i.e. this was a real wipe.
	for _, k := range after.Keys {
		if _, err := s.vault.Get(k.Ref); err != nil {
			t.Fatalf("key %s no longer resolves: %v", k.ID, err)
		}
	}
}

// Updating a nonexistent provider must report not-found rather than creating a
// ghost entry that can never authenticate.
func TestUpdateUnknownProvider(t *testing.T) {
	s := newTestService(t)
	if err := s.Update("nope", &Provider{BaseURL: "https://x.example"}); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Cooldown lives on the adapter's private clone of the entity, never in the
// service cache — so editing a provider hands the keys a fresh chance. That is
// the intended behaviour: the operator changed the configuration, and retrying
// immediately is the point. This test pins it so the choice is deliberate rather
// than accidental.
func TestEditGivesBenchedKeysAFreshChance(t *testing.T) {
	s := newTestService(t)
	if err := s.Create(&Provider{ID: "p10", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"},
		[]string{"sk-key-cccccccc"}); err != nil {
		t.Fatal(err)
	}
	rt, err := s.GetRuntime("p10")
	if err != nil {
		t.Fatal(err)
	}
	ad := rt.(*openAIAdapter)
	ref, err := ad.selectKey()
	if err != nil {
		t.Fatal(err)
	}
	ad.markCooldown(ref)
	if _, err := ad.selectKey(); err == nil {
		t.Fatal("a benched key must not be handed out again")
	}

	if err := s.Update("p10", &Provider{BaseURL: "https://c.example/v1"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.GetRuntime("p10")
	if err != nil {
		t.Fatal(err)
	}
	if fresh == rt {
		t.Fatal("the edit did not invalidate the adapter, so the cooldown survived")
	}
	if _, err := fresh.(*openAIAdapter).selectKey(); err != nil {
		t.Fatalf("after an edit the key should be usable again: %v", err)
	}
}

// Update must hand the caller back the stored view, otherwise an immediate
// read-modify-write from the same response loses the keys all over again.
func TestUpdateReflectsStoredKeysOnTheInput(t *testing.T) {
	s := newTestService(t)
	if err := s.Create(&Provider{ID: "p11", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"},
		[]string{"sk-key-dddddddd"}); err != nil {
		t.Fatal(err)
	}
	in := &Provider{BaseURL: "https://b.example/v1"}
	if err := s.Update("p11", in); err != nil {
		t.Fatal(err)
	}
	if len(in.Keys) != 1 {
		t.Fatalf("input struct not refreshed with stored keys: %d", len(in.Keys))
	}
}

// Enabled is owned by SetEnabled. A full-object edit from the UI carries the
// whole provider, so a stale copy used to be able to switch a provider off —
// or silently switch a deliberately disabled one back on. Both directions are
// silent: the next call just fails, or a provider the user parked starts
// serving traffic again.
func TestUpdateDoesNotChangeEnabled(t *testing.T) {
	s := newTestService(t)
	p := &Provider{ID: "p10", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"}
	if err := s.Create(p, []string{"sk-key-aaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled("p10", false); err != nil {
		t.Fatal(err)
	}

	// An edit that sends enabled:true (as a stale UI copy would) must not
	// re-enable the provider.
	if err := s.Update("p10", &Provider{Name: "renamed", BaseURL: "https://b.example/v1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("p10")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" {
		t.Fatalf("the edit did not apply: %q", got.Name)
	}
	if got.Enabled {
		t.Fatalf("an edit re-enabled a disabled provider")
	}
	if s.IsEnabled("p10") {
		t.Fatalf("IsEnabled disagrees with the stored flag")
	}
}

// IsEnabled must fail closed on an unknown id: it guards a call path, so
// "unknown" has to mean "do not call".
func TestIsEnabledFailsClosedOnUnknown(t *testing.T) {
	s := newTestService(t)
	if s.IsEnabled("nope") {
		t.Fatalf("an unregistered provider reported as enabled")
	}
}

// SetEnabled must survive a reload, otherwise the switch only holds until the
// process restarts — the classic "it worked when I tried it" bug.
func TestSetEnabledPersists(t *testing.T) {
	dir := t.TempDir()
	v, err := keyvault.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(storage.NewJSON(dir), v)
	p := &Provider{ID: "p11", Name: "P", Type: "openai", BaseURL: "https://a.example/v1"}
	if err := s.Create(p, []string{"sk-key-aaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if !s.IsEnabled("p11") {
		t.Fatalf("a newly created provider should be enabled")
	}
	if err := s.SetEnabled("p11", false); err != nil {
		t.Fatal(err)
	}

	reloaded := New(storage.NewJSON(dir), v)
	if reloaded.IsEnabled("p11") {
		t.Fatalf("the switch did not survive a reload")
	}
}
