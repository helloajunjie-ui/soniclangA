package failover

import (
	"testing"

	"github.com/agenthub/agent-proxy/internal/role"
)

func TestChainWalksEachTargetOnce(t *testing.T) {
	r := &role.Role{Fallbacks: []role.Fallback{
		{ProviderId: "a", Model: "m1"},
		{ProviderId: "b", Model: "m2"},
		{ProviderId: "c", Model: "m3"},
	}}
	c := NewChain()

	fb := SelectFallback(r, c, "primary", "m0")
	if fb == nil || fb.ProviderId != "a" {
		t.Fatalf("first fallback = %+v", fb)
	}
	fb = SelectFallback(r, c, fb.ProviderId, fb.Model)
	if fb == nil || fb.ProviderId != "b" {
		t.Fatalf("second fallback = %+v", fb)
	}
	fb = SelectFallback(r, c, fb.ProviderId, fb.Model)
	if fb == nil || fb.ProviderId != "c" {
		t.Fatalf("third fallback = %+v", fb)
	}
	if got := SelectFallback(r, c, "c", "m3"); got != nil {
		t.Fatalf("chain should be exhausted, got %+v", got)
	}
}

// The original bug: failing A→B then failing again selected A once more,
// ping-ponging instead of exhausting the chain.
func TestChainDoesNotPingPong(t *testing.T) {
	r := &role.Role{Fallbacks: []role.Fallback{
		{ProviderId: "a", Model: "m1"},
		{ProviderId: "b", Model: "m2"},
	}}
	c := NewChain()

	first := SelectFallback(r, c, "primary", "m0")
	if first.ProviderId != "a" {
		t.Fatalf("first = %+v", first)
	}
	second := SelectFallback(r, c, "a", "m1")
	if second.ProviderId != "b" {
		t.Fatalf("second = %+v", second)
	}
	// B fails: A must not be selected again.
	if third := SelectFallback(r, c, "b", "m2"); third != nil {
		t.Fatalf("ping-pong back to %+v", third)
	}
}

func TestSelectFallbackNilRole(t *testing.T) {
	if got := SelectFallback(nil, NewChain(), "a", "m"); got != nil {
		t.Fatalf("nil role should have no fallback, got %+v", got)
	}
}

func TestNoFallbacksConfigured(t *testing.T) {
	r := &role.Role{}
	if got := SelectFallback(r, NewChain(), "a", "m"); got != nil {
		t.Fatalf("empty chain should return nil, got %+v", got)
	}
}

// A fallback aimed at a switched-off provider must be skipped, not selected and
// then failed. Otherwise the turn spends a chain slot on a target that was
// never callable, and with a long chain it can exhaust every step before
// reaching a provider that actually works.
func TestSelectFallbackSkipsDisabledProviders(t *testing.T) {
	r := &role.Role{Fallbacks: []role.Fallback{
		{ProviderId: "off", Model: "m1"},
		{ProviderId: "on", Model: "m2"},
	}}
	c := NewChain()

	usable := func(id string) bool { return id != "off" }
	fb := SelectFallbackFiltered(r, c, "primary", "m0", usable)
	if fb == nil {
		t.Fatal("expected the enabled fallback to be selected")
	}
	if fb.ProviderId != "on" {
		t.Fatalf("selected a disabled provider: %+v", fb)
	}
}

// With every candidate disabled the chain reports nothing left, so the caller
// surfaces a real error instead of looping on unusable targets.
func TestSelectFallbackAllDisabledReturnsNil(t *testing.T) {
	r := &role.Role{Fallbacks: []role.Fallback{
		{ProviderId: "off1", Model: "m1"},
		{ProviderId: "off2", Model: "m2"},
	}}
	fb := SelectFallbackFiltered(r, NewChain(), "primary", "m0", func(string) bool { return false })
	if fb != nil {
		t.Fatalf("expected nil when nothing is usable, got %+v", fb)
	}
}
