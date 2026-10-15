// Package failover executes the role fallback chain (DESIGN §6.6 priority ①).
// In-vendor key rotation + cooldown live in provider/health; this selects the
// next (provider, model) candidate from role.Fallbacks[] when a call fails.
package failover

import "github.com/agenthub/agent-proxy/internal/role"

// Chain tracks which targets have already been tried in this turn.
//
// Without it, SelectFallback only skips the *current* target: failing over from
// A to B and then failing again would select A once more, ping-ponging until the
// step budget ran out instead of exhausting the chain.
type Chain struct {
	tried map[string]bool
}

// NewChain creates an empty chain.
func NewChain() *Chain { return &Chain{tried: map[string]bool{}} }

// Mark records a target as tried.
func (c *Chain) Mark(provider, model string) {
	if c.tried == nil {
		c.tried = map[string]bool{}
	}
	c.tried[provider+"/"+model] = true
}

// Exhausted reports whether every configured fallback has been attempted.
func (c *Chain) Exhausted() bool {
	return len(c.tried) > 0
}

// SelectFallback returns the first fallback target that has not been tried yet,
// or nil when the chain is exhausted. A nil role has no chain.
func SelectFallback(r *role.Role, chain *Chain, curProvider, curModel string) *role.Fallback {
	return SelectFallbackFiltered(r, chain, curProvider, curModel, nil)
}

// SelectFallbackFiltered is SelectFallback with an extra usability gate.
//
// usable is consulted for each candidate's provider id; a candidate it rejects
// is skipped. Without the gate a chain whose first entry points at a
// switched-off provider would be selected, fail, and only then move on — the
// turn burns a fallback slot on a target that was never callable. A nil usable
// accepts everything.
func SelectFallbackFiltered(r *role.Role, chain *Chain, curProvider, curModel string, usable func(providerID string) bool) *role.Fallback {
	if r == nil {
		return nil
	}
	// The starting target counts as tried so we never bounce straight back.
	if chain != nil {
		chain.Mark(curProvider, curModel)
	}
	for i := range r.Fallbacks {
		fb := &r.Fallbacks[i]
		if fb.ProviderId == curProvider && fb.Model == curModel {
			continue
		}
		if chain != nil && chain.tried[fb.ProviderId+"/"+fb.Model] {
			continue
		}
		if usable != nil && !usable(fb.ProviderId) {
			continue
		}
		return fb
	}
	return nil
}
