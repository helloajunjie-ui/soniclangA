// Package provider defines the BYOK multi-vendor abstraction: provider entities
// (data), the runtime Provider interface (Chat/ChatStream/Embeddings/Models),
// the Service (CRUD + key rotation + connectivity test) and preset vendor
// templates. OpenAI-compatible vendors share one adapter; Anthropic native is a
// stub in M2 (DESIGN §2/§5/§6.2).
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/provider/health"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/util"
)

// ---- runtime interface (consumed by orchestrator) ----

// Message is a chat message in provider-agnostic form.
type Message struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a model-issued tool invocation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSpec describes a tool the model may call.
type ToolSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// Usage is token accounting for one call.
type Usage struct {
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	TotalTokens      int     `json:"totalTokens"`
	Cost             float64 `json:"cost"`
}

// CallOptions parameterizes a chat completion call.
type CallOptions struct {
	Model       string
	Temperature float64
	MaxTokens   int
	TopP        float64
	Tools       []ToolSpec
	Stream      bool
}

// ChatResponse is a non-streaming completion result.
type ChatResponse struct {
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	Usage        Usage      `json:"usage"`
	FinishReason string     `json:"finishReason"`
	Model        string     `json:"model"`
}

// StreamChunk is one item emitted on the chat stream.
type StreamChunk struct {
	Delta        string     `json:"delta,omitempty"`
	Reasoning    string     `json:"reasoning,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"` // full tool calls, delivered on Done
	Done         bool       `json:"done"`
	Usage        Usage      `json:"usage"`
	Model        string     `json:"model"`
	FinishReason string     `json:"finishReason"`
	Err          error      `json:"-"`
}

// Runtime is the runtime contract for a vendor (OpenAI-compatible or native).
type Runtime interface {
	ID() string
	Chat(ctx context.Context, messages []Message, opts CallOptions) (*ChatResponse, error)
	ChatStream(ctx context.Context, messages []Message, opts CallOptions) (<-chan StreamChunk, error)
	Embeddings(ctx context.Context, model string, texts []string) ([][]float32, error)
	Models(ctx context.Context) ([]ModelInfo, error)
}

// ---- entities (persisted) ----

// KeyRecord is a reference to a sealed secret in the vault (no plaintext here).
type KeyRecord struct {
	ID string `json:"id"`
	// Label is the user's own note ("主号" / "备用"), shown instead of the mask
	// when several keys are configured. Not a secret.
	Label         string `json:"label,omitempty"`
	Ref           string `json:"ref"`
	Mask          string `json:"mask"`
	Status        string `json:"status"` // active | cooldown | error
	CooldownUntil int64  `json:"cooldownUntil"`
}

// ModelCaps is the capability set of a model.
type ModelCaps struct {
	Chat      bool `json:"chat"`
	Embedding bool `json:"embedding"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

// ModelInfo is a model advertised by a provider.
type ModelInfo struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Capabilities  ModelCaps `json:"capabilities"`
	ContextWindow int       `json:"contextWindow"`
}

// ModelPref is the user's decision about one discovered model.
//
// Both fields mean "depart from the default", and the default is enabled and
// not a favourite, so the zero value is the right thing to store for a model
// nobody has touched. Written through SetModelPref only.
type ModelPref struct {
	Disabled bool `json:"disabled,omitempty"`
	Favorite bool `json:"favorite,omitempty"`
}

// Provider is the persisted provider entity (DESIGN §7).
type Provider struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"` // openai|deepseek|moonshot|zhipu|tongyi|ollama|vllm|anthropic|custom
	Name       string            `json:"name"`
	BaseURL    string            `json:"baseUrl"`
	Headers    map[string]string `json:"headers,omitempty"`
	TimeoutMs  int               `json:"timeout"`
	Proxy      string            `json:"proxy,omitempty"`
	APIVersion string            `json:"apiVersion,omitempty"`
	Keys       []KeyRecord       `json:"keys"`
	Models     []ModelInfo       `json:"models"`
	// ModelPrefs is the user's curation of the discovered catalogue. It is a
	// separate map rather than extra fields on ModelInfo because RefreshModels
	// replaces Models wholesale every time the vendor is re-read: a flag living
	// in that slice would be wiped by the next refresh, silently discarding a
	// choice made by hand. Sparse by design — an absent entry means "enabled,
	// not a favourite" — so a vendor publishing 500 models adds nothing.
	ModelPrefs map[string]ModelPref `json:"modelPrefs,omitempty"`
	Enabled    bool                 `json:"enabled"`
	// CooldownSeconds is how long a failing key is benched before it is tried
	// again (DESIGN §6.6). 0 means the 60s default.
	CooldownSeconds int    `json:"cooldownSeconds,omitempty"`
	LastError       string `json:"lastError,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// ---- templates ----

// Template is preset metadata for a vendor type.
type Template struct {
	Type           string `json:"type"`
	Name           string `json:"name"`
	DefaultBaseURL string `json:"defaultBaseURL"`
	Protocol       string `json:"protocol"` // openai | anthropic
}

var templates = map[string]Template{
	"openai":    {Type: "openai", Name: "OpenAI", DefaultBaseURL: "https://api.openai.com/v1", Protocol: "openai"},
	"deepseek":  {Type: "deepseek", Name: "DeepSeek", DefaultBaseURL: "https://api.deepseek.com/v1", Protocol: "openai"},
	"moonshot":  {Type: "moonshot", Name: "Moonshot (Kimi)", DefaultBaseURL: "https://api.moonshot.cn/v1", Protocol: "openai"},
	"zhipu":     {Type: "zhipu", Name: "智谱 GLM", DefaultBaseURL: "https://open.bigmodel.cn/api/paas/v4", Protocol: "openai"},
	"tongyi":    {Type: "tongyi", Name: "通义千问", DefaultBaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Protocol: "openai"},
	"ollama":    {Type: "ollama", Name: "Ollama", DefaultBaseURL: "http://localhost:11434/v1", Protocol: "openai"},
	"vllm":      {Type: "vllm", Name: "vLLM", DefaultBaseURL: "http://localhost:8000/v1", Protocol: "openai"},
	"anthropic": {Type: "anthropic", Name: "Anthropic", DefaultBaseURL: "https://api.anthropic.com/v1", Protocol: "anthropic"},
	"custom":    {Type: "custom", Name: "自定义 (OpenAI 兼容)", DefaultBaseURL: "", Protocol: "openai"},
}

// TemplateTypes returns all preset vendor templates.
func TemplateTypes() []Template {
	out := make([]Template, 0, len(templates))
	for _, t := range templates {
		out = append(out, t)
	}
	return out
}

// ApplyTemplate fills BaseURL/Type/Name from a preset when the type is known.
func ApplyTemplate(p *Provider) {
	if t, ok := templates[p.Type]; ok {
		if p.BaseURL == "" {
			p.BaseURL = t.DefaultBaseURL
		}
		if p.Name == "" {
			p.Name = t.Name
		}
	}
}

// ---- service ----

// ErrNotFound is returned when a provider id is unknown.
var ErrNotFound = errors.New("provider not found")

// Service manages provider entities, key rotation and runtime adapters.
type Service struct {
	store      storage.Store
	vault      *keyvault.Vault
	mu         sync.RWMutex
	cache      map[string]*Provider
	health     map[string]*health.Health
	adaptersMu sync.RWMutex
	adapters   map[string]Runtime
}

// New constructs the provider Service and loads persisted entities.
func New(store storage.Store, vault *keyvault.Vault) *Service {
	s := &Service{
		store:    store,
		vault:    vault,
		cache:    map[string]*Provider{},
		health:   map[string]*health.Health{},
		adapters: map[string]Runtime{},
	}
	s.load()
	return s
}

func (s *Service) load() {
	ids, err := s.store.IDs()
	if err != nil {
		return
	}
	for _, id := range ids {
		var p Provider
		if err := s.store.Load(id, &p); err == nil {
			s.cache[id] = &p
			s.health[id] = health.New()
		}
	}
}

// List returns all provider entities as detached copies.
//
// Copying matters: the handler JSON-encodes these after the lock is released,
// while AddKey/Update mutate the cached entity (notably appending to Keys).
// Handing out the live pointer would let an encode race with a write and emit a
// torn response — or panic mid-slice-growth.
func (s *Service) List() []*Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Provider, 0, len(s.cache))
	for _, p := range s.cache {
		out = append(out, cloneProvider(p))
	}
	return out
}

// Get returns a detached copy of a provider entity by id.
func (s *Service) Get(id string) (*Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.cache[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneProvider(p), nil
}

// cloneProvider deep-copies a provider. A JSON round trip is used so that any
// field added later is copied automatically; on failure we fall back to a
// shallow copy, which is still safer than returning the live pointer.
func cloneProvider(p *Provider) *Provider {
	if p == nil {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		cp := *p
		return &cp
	}
	var out Provider
	if err := json.Unmarshal(b, &out); err != nil {
		cp := *p
		return &cp
	}
	return &out
}

// Create persists a new provider and seals its secrets. secrets are plaintext
// keys (sealed into the vault); the entity's Keys slice is populated with refs.
func (s *Service) Create(p *Provider, secrets []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = util.NewID()
	}
	ApplyTemplate(p)
	for _, sec := range secrets {
		if sec == "" {
			continue
		}
		kid := util.NewID()
		if err := s.vault.Set(kid, sec); err != nil {
			return err
		}
		p.Keys = append(p.Keys, KeyRecord{ID: kid, Ref: kid, Mask: util.MaskSecret(sec), Status: "active"})
	}
	now := time.Now().UnixMilli()
	p.CreatedAt, p.UpdatedAt = now, now
	p.Enabled = true
	if err := s.store.Save(p.ID, p); err != nil {
		return err
	}
	s.cache[p.ID] = p
	s.health[p.ID] = health.New()
	return nil
}

// Update replaces a persisted provider entity.
// invalidateAdapter drops the cached runtime for a provider. Adapters hold
// their own copy of the entity, so editing base URL, type or keys must rebuild
// the adapter — otherwise the change silently has no effect until restart.
func (s *Service) invalidateAdapter(id string) {
	s.adaptersMu.Lock()
	delete(s.adapters, id)
	s.adaptersMu.Unlock()
}

// Update applies an edit to a provider.
//
// Secrets and runtime state are NOT taken from the request body: keys live in
// the vault and are managed exclusively through Create/AddKey/RemoveKey, and
// cooldown is per-adapter runtime state. A client that PUTs only a changed
// baseUrl must not silently lose every key — the provider would keep its id and
// name but stop being able to authenticate at all.
func (s *Service) Update(id string, p *Provider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.cache[id]
	if !ok {
		return ErrNotFound
	}
	// Keys are vault-backed and cooldown is per-adapter runtime state; neither
	// may be reset by an edit.
	saved := *p
	saved.ID = id
	saved.Keys = old.Keys
	saved.CreatedAt = old.CreatedAt
	// The model catalogue is discovered, never authored. It used to follow the
	// request body, which meant a client could PUT an arbitrary list — and
	// because EnsureModels treats "non-empty" as "already known", one
	// hand-written entry would shadow the vendor's real catalogue for good,
	// leaving /v1/models and every role's model picker showing a fiction.
	// RefreshModels is the single writer now.
	saved.Models = old.Models
	// Curation is the user's, not the editor's — the same rule as Enabled and
	// Keys. A PUT that renames a provider must not quietly re-enable the 500
	// models that were switched off.
	saved.ModelPrefs = old.ModelPrefs
	// LastError is runtime state written by Test/RefreshModels, not a field an
	// edit should clear.
	saved.LastError = old.LastError
	// Enabled is owned by SetEnabled, not by whoever edits the entity. An
	// ordinary edit (rename, add a model) must not be able to silently take a
	// provider out of service, nor quietly put a deliberately disabled one back
	// in — the UI sends the whole object, so a stale copy would do exactly that.
	saved.Enabled = old.Enabled
	saved.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, &saved); err != nil {
		return err
	}
	s.cache[id] = &saved
	s.invalidateAdapter(id)
	// Give the caller back the same view the API returns, so an immediate
	// follow-up PUT does not read as "my keys vanished".
	*p = saved
	return nil
}

// SetEnabled flips a provider's enable switch.
//
// This is the only writer of the flag: Update deliberately preserves the stored
// value, so the switch has exactly one path and cannot be changed as a side
// effect of editing something else.
func (s *Service) SetEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.cache[id]
	if !ok {
		return ErrNotFound
	}
	if p.Enabled == enabled {
		return nil
	}
	next := *p
	next.Enabled = enabled
	next.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, &next); err != nil {
		return err
	}
	s.cache[id] = &next
	s.invalidateAdapter(id)
	return nil
}

// IsEnabled reports whether a provider may be used for a call. An unknown id
// counts as disabled: it could not be called anyway, and callers use this to
// refuse a request, so failing closed is the safe direction.
func (s *Service) IsEnabled(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.cache[id]
	return ok && p.Enabled
}

// Delete removes a provider, its sealed keys and its health table.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.cache[id]; ok {
		for _, k := range p.Keys {
			_ = s.vault.Delete(k.Ref)
		}
	}
	_ = s.store.Remove(id)
	delete(s.cache, id)
	delete(s.health, id)
	s.adaptersMu.Lock()
	delete(s.adapters, id)
	s.adaptersMu.Unlock()
	return nil
}

// AddKey seals an additional secret for a provider. The label is the user's
// note for telling several keys apart; it is not part of the secret.
func (s *Service) AddKey(id, secret, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.cache[id]
	if !ok {
		return ErrNotFound
	}
	kid := util.NewID()
	if err := s.vault.Set(kid, secret); err != nil {
		return err
	}
	p.Keys = append(p.Keys, KeyRecord{ID: kid, Label: label, Ref: kid, Mask: util.MaskSecret(secret), Status: "active"})
	p.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, p); err != nil {
		return err
	}
	// A newly added key must be visible to the next request: the adapter keeps
	// its own copy of the key list.
	s.invalidateAdapter(id)
	return nil
}

// RemoveKey deletes one sealed key from a provider.
func (s *Service) RemoveKey(id, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.cache[id]
	if !ok {
		return ErrNotFound
	}
	filtered := p.Keys[:0]
	for _, k := range p.Keys {
		if k.ID == keyID {
			_ = s.vault.Delete(k.Ref)
			continue
		}
		filtered = append(filtered, k)
	}
	p.Keys = filtered
	p.UpdatedAt = time.Now().UnixMilli()
	s.invalidateAdapter(id)
	return s.store.Save(id, p)
}

// EnsureModels returns a provider's model list, fetching it once if the entity
// has never been tested. Without this, /v1/models reports an empty catalogue for
// a freshly configured provider and third-party clients see no models at all.
func (s *Service) EnsureModels(id string) ([]ModelInfo, error) {
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if len(p.Models) > 0 {
		return p.Models, nil
	}
	return s.RefreshModels(id)
}

// RefreshModels asks the vendor for its model catalogue and caches the result.
//
// Unlike EnsureModels it never short-circuits on the cached list, so it is the
// way to pick up models added upstream after the first probe. It is also the
// only writer of Provider.Models: the catalogue is discovered, not authored,
// because a hand-written entry would shadow the real one indefinitely.
//
// It touches Models only. ModelPrefs lives beside the catalogue precisely so a
// refresh cannot discard curation, and entries are deliberately not pruned when
// a vendor stops advertising a model: a vendor having a bad day must not erase a
// decision the user made by hand. A stale entry costs nothing — it names a model
// that is not in the list either way.
func (s *Service) RefreshModels(id string) ([]ModelInfo, error) {
	rt, err := s.GetRuntime(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	models, err := rt.Models(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if cur, ok := s.cache[id]; ok {
		cur.Models = models
		cur.LastError = ""
		cur.UpdatedAt = time.Now().UnixMilli()
		_ = s.store.Save(id, cur)
	}
	s.mu.Unlock()
	return models, nil
}

// SetModelPref records the user's decision about one discovered model. Pass nil
// for a half that should stay as it is.
//
// This is the only writer of ModelPrefs, for the same reason SetEnabled is the
// only writer of Enabled: an edit that PUTs the whole entity must not be able
// to reset curation as a side effect, and the two directions of that mistake
// are both silent.
//
// A preference that matches the default is erased rather than stored, so the
// map stays the size of the user's deviations and not of the catalogue.
func (s *Service) SetModelPref(id, modelID string, disabled, favorite *bool) error {
	if modelID == "" {
		return errors.New("model id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.cache[id]
	if !ok {
		return ErrNotFound
	}
	next := *p
	pref := next.ModelPrefs[modelID]
	if disabled != nil {
		pref.Disabled = *disabled
	}
	if favorite != nil {
		pref.Favorite = *favorite
	}
	if next.ModelPrefs == nil {
		next.ModelPrefs = map[string]ModelPref{}
	}
	if pref == (ModelPref{}) {
		delete(next.ModelPrefs, modelID)
	} else {
		next.ModelPrefs[modelID] = pref
	}
	next.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, &next); err != nil {
		return err
	}
	s.cache[id] = &next
	return nil
}

// SetAllModelPrefs applies one decision to the whole current catalogue.
//
// Server-side on purpose: an aggregator advertises several hundred models, so
// the alternative is one request per row and a UI that hangs for a minute on
// "switch everything off". Only ids the provider actually advertises are
// touched — the catalogue is what "all" means, not the pref map — so an entry
// kept for a model the vendor has temporarily dropped is left alone.
func (s *Service) SetAllModelPrefs(id string, disabled, favorite *bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.cache[id]
	if !ok {
		return ErrNotFound
	}
	if len(p.Models) == 0 {
		return nil
	}
	next := *p
	prefs := make(map[string]ModelPref, len(next.ModelPrefs))
	for k, v := range next.ModelPrefs {
		prefs[k] = v
	}
	next.ModelPrefs = prefs
	for _, m := range p.Models {
		pref := prefs[m.ID]
		if disabled != nil {
			pref.Disabled = *disabled
		}
		if favorite != nil {
			pref.Favorite = *favorite
		}
		if pref == (ModelPref{}) {
			delete(prefs, m.ID)
		} else {
			prefs[m.ID] = pref
		}
	}
	next.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, &next); err != nil {
		return err
	}
	s.cache[id] = &next
	return nil
}

// UsableModels returns the models a caller may actually pick: the vendor's
// catalogue minus the ones the user switched off.
//
// This is what /v1/models and every model picker read. The full catalogue is
// not a usable list — an aggregator advertises several hundred models, of which
// anyone uses a handful — so the curation the user did on the 服务商 page is the
// thing that makes the downstream pickers navigable at all. Narrowing to one
// provider is not enough on its own; one provider can carry the whole 563.
//
// Deliberately no fallback: if every model is switched off the result is empty,
// because that is what the user asked for and re-adding them silently would
// make the switch a decoration.
func (s *Service) UsableModels(id string) ([]ModelInfo, error) {
	models, err := s.EnsureModels(id)
	if err != nil {
		return nil, err
	}
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if len(p.ModelPrefs) == 0 {
		return models, nil
	}
	out := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		if p.ModelPrefs[m.ID].Disabled {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Test performs a connectivity probe and, on success, refreshes the model
// catalogue as a side effect. It reports only whether the provider answered;
// callers that want the catalogue itself use RefreshModels.
func (s *Service) Test(id string) error {
	rt, err := s.GetRuntime(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := rt.Models(ctx)
	if err != nil {
		s.mu.Lock()
		if p, ok := s.cache[id]; ok {
			p.LastError = err.Error()
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	if p, ok := s.cache[id]; ok {
		p.Models = models
		p.LastError = ""
		p.UpdatedAt = time.Now().UnixMilli()
		_ = s.store.Save(id, p)
	}
	s.mu.Unlock()
	return nil
}

// Health returns the health table for a provider (may be nil).
func (s *Service) Health(id string) *health.Health {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health[id]
}

// GetRuntime returns (building if needed) the runtime adapter for a provider.
func (s *Service) GetRuntime(id string) (Runtime, error) {
	s.mu.RLock()
	p, ok := s.cache[id]
	// The health map is written under s.mu by Create/Delete, so it must be
	// read inside the same critical section — reading it after unlocking races
	// with a concurrent provider edit and can crash the process.
	var hl *health.Health
	if ok {
		hl = s.health[p.ID]
	}
	s.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	s.adaptersMu.RLock()
	a, ok := s.adapters[id]
	s.adaptersMu.RUnlock()
	if ok {
		return a, nil
	}
	// The adapter keeps its own copy of the entity: it writes cooldown state
	// into Keys on every failure, and mutating the shared cache from a
	// streaming goroutine would race with the service's own writes.
	a = buildAdapter(cloneProvider(p), s.vault, hl)
	s.adaptersMu.Lock()
	s.adapters[id] = a
	s.adaptersMu.Unlock()
	return a, nil
}

func buildAdapter(p *Provider, vault *keyvault.Vault, h *health.Health) Runtime {
	// A pointer, not a value: keyring carries a mutex, and copying it into the
	// adapter would create a second lock guarding the same cooldown state.
	kr := &keyring{entity: p, vault: vault, health: h}
	if p.Type == "anthropic" {
		return &anthropicAdapter{keyring: kr}
	}
	return &openAIAdapter{keyring: kr}
}

var _ = fmt.Sprintf
