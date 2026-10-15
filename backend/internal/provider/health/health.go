// Package health holds the per-provider health table, latency probe results and
// cooldown deadlines consumed directly by orchestrator/failover (DESIGN §5
// provider.health / §6.6).
package health

import (
	"sync"
	"time"
)

// ModelHealth is the recorded status of one model.
type ModelHealth struct {
	OK        bool  `json:"ok"`
	LatencyMs int64 `json:"latency"`
	CheckedAt int64 `json:"checkedAt"`
}

// Health is the per-provider health table.
type Health struct {
	mu                    sync.RWMutex
	models                map[string]ModelHealth
	providerCooldownUntil int64
}

// New constructs an empty Health table.
func New() *Health {
	return &Health{models: map[string]ModelHealth{}}
}

// SetModelOK records a successful probe.
func (h *Health) SetModelOK(name string, latencyMs int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.models[name] = ModelHealth{OK: true, LatencyMs: latencyMs, CheckedAt: time.Now().UnixMilli()}
}

// SetModelDown records a failed probe.
func (h *Health) SetModelDown(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.models[name]
	m.OK = false
	m.CheckedAt = time.Now().UnixMilli()
	h.models[name] = m
}

// IsModelOK reports whether a model is currently healthy.
func (h *Health) IsModelOK(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	m, ok := h.models[name]
	return ok && m.OK
}

// Snapshot returns a copy of the model health map.
func (h *Health) Snapshot() map[string]ModelHealth {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]ModelHealth, len(h.models))
	for k, v := range h.models {
		out[k] = v
	}
	return out
}

// SetCooldown marks the whole provider as cooled-down until the given unix-ms.
func (h *Health) SetCooldown(until int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.providerCooldownUntil = until
}

// CooldownUntil returns the provider-level cooldown deadline (unix-ms).
func (h *Health) CooldownUntil() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.providerCooldownUntil
}
