package provider

import (
	"fmt"
	"sync"
	"time"

	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/provider/health"
)

// defaultCooldownSeconds benches a failing key for a minute unless the provider
// overrides it (DESIGN §6.6).
const defaultCooldownSeconds = 60

// keyring owns in-vendor key rotation and cooldown state. Every adapter embeds
// one instead of reimplementing rotation: a third copy of the cooldown rules
// would inevitably drift from the other two, and the failure mode is silent —
// a key that should be benched keeps burning quota on 401s.
//
// Concurrency: the adapter holds a *clone* of the provider entity, so writes to
// Keys here are private to the adapter and safe behind mu. The service's own
// entity is never touched by this path.
type keyring struct {
	entity *Provider
	vault  *keyvault.Vault
	health *health.Health

	mu     sync.Mutex
	keyIdx int
}

// selectKey returns the next usable vault ref, skipping keys that are cooling
// down. It advances the round-robin cursor so consecutive calls spread load
// across keys rather than hammering the first one until it fails.
func (k *keyring) selectKey() (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.entity.Keys) == 0 {
		return "", fmt.Errorf("provider %s has no keys", k.entity.ID)
	}
	now := time.Now().UnixMilli()
	for i := 0; i < len(k.entity.Keys); i++ {
		idx := (k.keyIdx + i) % len(k.entity.Keys)
		key := k.entity.Keys[idx]
		if key.CooldownUntil > now {
			continue
		}
		k.keyIdx = (idx + 1) % len(k.entity.Keys)
		return key.Ref, nil
	}
	return "", fmt.Errorf("all keys of provider %s are in cooldown", k.entity.ID)
}

// markCooldown benches a key for the provider's cooldown window and mirrors the
// state into the health table so the orchestrator's failover can see it.
func (k *keyring) markCooldown(ref string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	secs := k.entity.CooldownSeconds
	if secs <= 0 {
		secs = defaultCooldownSeconds
	}
	until := time.Now().UnixMilli() + int64(secs)*1000
	for i := range k.entity.Keys {
		if k.entity.Keys[i].Ref == ref {
			k.entity.Keys[i].CooldownUntil = until
			k.entity.Keys[i].Status = "cooldown"
		}
	}
	if k.health != nil {
		k.health.SetCooldown(until)
	}
}

// secret resolves a vault ref to its plaintext. Used by the non-streaming and
// auxiliary paths, which do not retry across keys: a failure there surfaces to
// the caller (and, in the orchestrator, to the cross-vendor failover chain).
func (k *keyring) secret(ref string) (string, error) {
	if k.vault == nil {
		return "", fmt.Errorf("key vault unavailable for provider %s", k.entity.ID)
	}
	return k.vault.Get(ref)
}

// firstSecret returns the first key's plaintext without consulting cooldown.
// Only for one-shot calls (Models/Embeddings/Chat non-stream) where retrying
// across keys would just multiply latency on an already-failing provider.
func (k *keyring) firstSecret() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.entity.Keys) == 0 {
		return ""
	}
	if s, err := k.vault.Get(k.entity.Keys[0].Ref); err == nil {
		return s
	}
	return ""
}
