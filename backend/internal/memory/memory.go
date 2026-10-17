// Package memory implements the local memory subsystem (DESIGN §5 memory /
// §6.5 / §15.4). It follows the L0-L3 pyramid:
//
//	L0 raw conversation  — conversations/{id}.jsonl (the source of truth)
//	L1 atomic memory     — memories/{id}.json (facts/preferences/tasks, with links)
//	L2 scene memory      — memory/scenes/{scene}.md + memory/index.json
//	L3 persona           — memory/persona.md (human-readable, hand-editable)
//
// Vectors never live inside the entity: they are kept in
// memory/index/{scope}.json and handed to the shared retrieval kernel, so the
// same BM25 ∥ embedding → RRF pipeline used by knowledge is reused verbatim.
//
// Two invariants are enforced structurally, not by convention:
//   - extraction passes the triple gate (cadence / input size / output size)
//     and never distills secrets (see package extract);
//   - consolidation only *registers* contradictions in conflicts.json and
//     never rewrites an existing memory (auto-merge is a poisoning vector).
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/conversation"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Memory pyramid levels. L0 is not stored here — the conversation itself is L0.
const (
	LevelL0 = "L0"
	LevelL1 = "L1"
	LevelL2 = "L2" // scene (memory/scenes/*.md)
	LevelL3 = "L3" // persona (memory/persona.md)
)

// Memory categories (DESIGN §15.4).
const (
	CatProfile    = "profile"
	CatPreference = "preference"
	CatFact       = "fact"
	CatTask       = "task"
	CatInterest   = "interest"
)

// Memory scopes.
const (
	ScopeGlobal = "global"
	ScopeRole   = "role"
)

// Origin prefixes recorded on every memory so its provenance stays auditable.
const (
	OriginUser       = "user:"
	OriginWeb        = "web:"
	OriginTool       = "tool:"
	OriginDoc        = "doc:"
	OriginSelf       = "self"
	OriginLLMExtract = "llm_extract"
)

// Virtual ids used for the L2/L3 layers, which live in Markdown rather than
// in memories/*.json. Drilldown resolves them back to their L1 members.
const (
	PersonaID   = "persona"
	ScenePrefix = "scene:"
)

// MemoryResult is one recalled item handed to the orchestrator. It lives here
// rather than in the orchestrator on purpose: memory is the lower layer, and
// letting it import its consumer would make the orchestrator un-importable
// (it drives extraction, so it has to depend on this package).
type MemoryResult struct {
	ID      string  `json:"id"`
	Level   string  `json:"level"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// RecallHit is a memory plus the relevance score the retrieval kernel assigned
// to it for one query. Memory is embedded (not nested) so the JSON stays flat
// and clients can sort by score without unwrapping anything.
type RecallHit struct {
	*Memory
	Score float64 `json:"score"`
}

// ExtractInput is one message handed to the L1 distiller.
type ExtractInput struct {
	ID      string
	Role    string
	Content string
}

// ExtractResult reports what one distillation run produced (and why it may
// have produced nothing at all).
type ExtractResult struct {
	Memories []*Memory `json:"memories"`
	Skipped  string    `json:"skipped,omitempty"` // gate that stopped the run
	Filtered int       `json:"filtered"`          // plumbing messages removed
	Redacted int       `json:"redacted"`          // secret spans masked
	Dropped  int       `json:"dropped"`           // output items rejected
}

// Links is the provenance chain that makes every memory traceable back to L0.
type Links struct {
	SourceConversationId string   `json:"sourceConversationId,omitempty"`
	SourceMessageIds     []string `json:"sourceMessageIds,omitempty"`
	MemberIds            []string `json:"memberIds,omitempty"` // L2 -> L1 members
}

// Memory is one stored item (DESIGN §15.4 memories/{id}.json).
type Memory struct {
	ID                  string   `json:"id"`
	Level               string   `json:"level"`
	Scope               string   `json:"scope"`
	RoleId              string   `json:"roleId,omitempty"`
	Category            string   `json:"category,omitempty"`
	Scene               string   `json:"scene,omitempty"`
	Content             string   `json:"content"`
	EmbeddingProviderId string   `json:"embeddingProviderId,omitempty"`
	EmbeddingModel      string   `json:"embeddingModel,omitempty"`
	EmbeddingDim        int      `json:"embeddingDim,omitempty"`
	Tags                []string `json:"tags,omitempty"`
	Origin              string   `json:"origin,omitempty"`
	Links               Links    `json:"links"`
	Confidence          float64  `json:"confidence"`
	Importance          float64  `json:"importance"`
	HitCount            int      `json:"hitCount"`
	LastHitAt           int64    `json:"lastHitAt,omitempty"`
	Supersedes          string   `json:"supersedes,omitempty"`
	SupersededBy        string   `json:"superseded_by,omitempty"`
	ValidFrom           int64    `json:"valid_from,omitempty"`
	InvalidAt           int64    `json:"invalid_at,omitempty"`
	Pinned              bool     `json:"pinned"`
	Enabled             bool     `json:"enabled"`
	Source              string   `json:"source"` // auto | manual
	CreatedAt           int64    `json:"createdAt"`
	UpdatedAt           int64    `json:"updatedAt"`
}

// Conflict is a pending contradiction (DESIGN §15.4 memory/conflicts.json).
// Consolidation only registers these — it never rewrites a memory.
type Conflict struct {
	ID         string   `json:"id"`
	ClaimA     string   `json:"claimA"`
	ClaimB     string   `json:"claimB"`
	SourceIds  []string `json:"sourceIds"`
	Conflict   string   `json:"conflict,omitempty"`
	Status     string   `json:"status"` // open | resolved
	ResolvedBy string   `json:"resolvedBy,omitempty"`
	Resolution string   `json:"resolution,omitempty"`
	CreatedAt  int64    `json:"createdAt"`
	ResolvedAt int64    `json:"resolvedAt,omitempty"`
}

// ListFilter narrows a listing query.
type ListFilter struct {
	Level    string
	Scope    string
	Category string
	Scene    string
	Include  bool // include disabled/superseded items
}

// ConversationSource supplies L0 originals for drilldown. Implemented by
// conversation.Service; kept as an interface so memory stays decoupled.
type ConversationSource interface {
	Messages(convID string) ([]conversation.Message, error)
}

// Service stores, recalls and consolidates memories.
type Service struct {
	entities  storage.Store // memories/{id}.json
	indexes   storage.Store // memory/index/{scope}.json (vectors, kept apart)
	baseDir   string        // data/memory: persona.md, scenes/, conflicts.json, offload/
	retrieval *retrieval.Service
	providers *provider.Service
	convs     ConversationSource // optional: enables L0 drilldown
	auto      bool

	mu sync.RWMutex
	// audit records destructive or corrective operations (delete, invalidate,
	// conflict ruling). Memories are personal data, so only ids and shapes are
	// logged, never content: the point is to be able to answer "who removed
	// this, and when", not to duplicate the memory into a second file.
	audit *audit.Audit
	// cache stores values, not pointers: handing out a *Memory would let any
	// caller mutate shared state without touching the store.
	cache     map[string]Memory
	emb       embeddingConfig
	scenes    map[string][]string // scene -> L1 memberIds
	conflicts []Conflict
	gate      map[string]int64 // conversation -> last extraction time
}

type embeddingConfig struct {
	ProviderId string `json:"providerId"`
	Model      string `json:"model"`
	Dim        int    `json:"dim"`
}

type persistedIndex struct {
	Scope      string             `json:"scope"`
	ProviderId string             `json:"embeddingProviderId,omitempty"`
	Model      string             `json:"embeddingModel,omitempty"`
	Dim        int                `json:"embeddingDim,omitempty"`
	Records    []retrieval.Record `json:"records"`
}

// New constructs the memory Service, loads entities into the recall index and
// reads the Markdown layers (persona / scenes / conflicts) from disk.
func New(entities, indexes storage.Store, baseDir string,
	retr *retrieval.Service, prov *provider.Service, autoExtract bool) *Service {
	s := &Service{
		entities:  entities,
		indexes:   indexes,
		baseDir:   baseDir,
		retrieval: retr,
		providers: prov,
		auto:      autoExtract,
		cache:     map[string]Memory{},
		scenes:    map[string][]string{},
		gate:      map[string]int64{},
	}
	_ = os.MkdirAll(filepath.Join(baseDir, "scenes"), 0o755)
	_ = os.MkdirAll(filepath.Join(baseDir, "offload"), 0o755)
	s.load()
	return s
}

// SetConversationSource wires the L0 lookup used by drilldown.
func (s *Service) SetConversationSource(c ConversationSource) { s.convs = c }

// AutoExtract reports whether automatic L1 extraction is enabled.
func (s *Service) AutoExtract() bool { return s.auto }

func (s *Service) load() {
	ids, err := s.entities.IDs()
	if err != nil {
		return
	}
	byScope := map[string][]retrieval.Record{}
	var all []*Memory
	for _, id := range ids {
		var m Memory
		if err := s.entities.Load(id, &m); err != nil {
			continue
		}
		all = append(all, &m)
	}
	// load persisted vectors per scope (kept in a separate store)
	for _, m := range all {
		s.cache[m.ID] = *m
		sk := scopeKey(m.Scope)
		var pi persistedIndex
		if err := s.indexes.Load(sk, &pi); err == nil && pi.Records != nil {
			for _, r := range pi.Records {
				if r.ID == m.ID {
					byScope[sk] = append(byScope[sk], r)
				}
			}
			if pi.ProviderId != "" {
				s.emb = embeddingConfig{ProviderId: pi.ProviderId, Model: pi.Model, Dim: pi.Dim}
			}
		}
	}
	// rebuild BM25 for every scope, attaching whatever vectors we found
	for _, m := range all {
		sk := scopeKey(m.Scope)
		recs := byScope[sk]
		found := false
		for i := range recs {
			if recs[i].ID == m.ID {
				found = true
				break
			}
		}
		if !found {
			byScope[sk] = append(byScope[sk], retrieval.Record{ID: m.ID, Text: m.Content})
		}
	}
	if s.retrieval != nil {
		for sk, recs := range byScope {
			_ = s.retrieval.BuildIndex(nsOfScope(sk), recs)
		}
	}
	s.loadScenes()
	s.loadConflicts()
}

func nsOfScope(sk string) string { return "mem:" + sk }

func scopeKey(scope string) string {
	if scope == "" {
		scope = ScopeGlobal
	}
	r := strings.NewReplacer("/", "_", ":", "_", " ", "_", "\\", "_")
	return r.Replace(scope)
}

// ---- persistence helpers ----

func (s *Service) writeFile(rel string, data []byte) error {
	p := filepath.Join(s.baseDir, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Service) readFile(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.baseDir, rel))
}

// persistVectors writes the vector index for one scope.
func (s *Service) persistVectors(scope string, recs []retrieval.Record) error {
	sk := scopeKey(scope)
	pi := persistedIndex{Scope: scope, Records: recs}
	s.mu.RLock()
	pi.ProviderId, pi.Model, pi.Dim = s.emb.ProviderId, s.emb.Model, s.emb.Dim
	s.mu.RUnlock()
	data, err := json.Marshal(pi)
	if err != nil {
		return err
	}
	return s.indexes.Save(sk, json.RawMessage(data))
}

// ---- CRUD ----

// Add stores a memory (L1) and refreshes its scope index.
func (s *Service) Add(m *Memory) (*Memory, error) {
	now := time.Now().UnixMilli()
	if m.ID == "" {
		m.ID = util.NewID()
	}
	if m.Level == "" {
		m.Level = LevelL1
	}
	if m.Scope == "" {
		m.Scope = ScopeGlobal
	}
	if m.Source == "" {
		m.Source = "manual"
	}
	m.Enabled = true
	m.CreatedAt, m.UpdatedAt = now, now
	if m.ValidFrom == 0 {
		m.ValidFrom = now
	}
	if m.Confidence == 0 {
		m.Confidence = 0.6
	}
	if m.Importance == 0 {
		m.Importance = 0.5
	}
	if err := s.entities.Save(m.ID, m); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[m.ID] = *m
	s.mu.Unlock()
	if s.retrieval != nil {
		_ = s.retrieval.Refresh(nsOfScope(scopeKey(m.Scope)), m.ID, m.Content, nil)
	}
	return m, nil
}

// Get returns one memory.
func (s *Service) Get(id string) (*Memory, error) {
	s.mu.RLock()
	m, ok := s.cache[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("memory %s not found", id)
	}
	cp := m
	return &cp, nil
}

// Update replaces a stored memory.
func (s *Service) Update(m *Memory) error {
	if m.ID == "" {
		return fmt.Errorf("memory id required")
	}
	// The previous copy decides whether the retrieval row needs rewriting. A
	// flag-only update used to re-register the row with a nil embedding, which
	// clears the stored vector (Refresh replaces, it does not merge) — so
	// toggling a memory's importance silently dropped it out of semantic recall
	// until something embedded it again.
	s.mu.RLock()
	prev, existed := s.cache[m.ID]
	s.mu.RUnlock()

	m.UpdatedAt = time.Now().UnixMilli()
	if err := s.entities.Save(m.ID, m); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[m.ID] = *m
	s.mu.Unlock()
	if s.retrieval != nil {
		// A changed text does invalidate the old vector, so re-registering (and
		// dropping the stale vector) is correct there.
		textChanged := !existed || prev.Content != m.Content
		scopeChanged := existed && scopeKey(prev.Scope) != scopeKey(m.Scope)
		if textChanged || scopeChanged {
			_ = s.retrieval.Refresh(nsOfScope(scopeKey(m.Scope)), m.ID, m.Content, nil)
		}
	}
	return nil
}

// SetAudit attaches the audit logger used for destructive and corrective
// operations. Optional: nil (tests, embedded use) disables logging.
func (s *Service) SetAudit(a *audit.Audit) { s.audit = a }

func (s *Service) logAudit(event string, fields map[string]string) {
	if s.audit != nil {
		s.audit.Log(event, "", fields)
	}
}

// Invalidate retires a memory without destroying it: the record stays visible
// so the user can see what was dropped, but it is marked invalid and pulled
// from the recall index, so it can no longer be recalled. This is the
// reversible counterpart to Delete and the action behind the 失效 button.
//
// Editing the entry afterwards restores it: Update re-saves and re-registers
// the index row, which is the only undo path and is deliberate — a wrong fact
// should be corrected, not silently resurrected.
func (s *Service) Invalidate(id string) error {
	s.mu.Lock()
	m, ok := s.cache[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("memory %s not found", id)
	}
	now := time.Now().UnixMilli()
	m.InvalidAt = now
	m.UpdatedAt = now
	s.cache[id] = m
	scope := m.Scope
	s.mu.Unlock()

	if err := s.entities.Save(id, m); err != nil {
		return err
	}
	// Dropping the index row is what makes the promise structural rather than
	// conventional: recall runs off the index, so an invalidated memory cannot
	// come back through BM25 or the vector path even if a caller forgets to
	// re-check the flag.
	if s.retrieval != nil {
		_ = s.retrieval.Remove(nsOfScope(scopeKey(scope)), id)
	}
	s.logAudit("memory.invalidate", map[string]string{"id": id, "level": m.Level, "scope": scope})
	return nil
}

// Delete removes a memory and its vector, irreversibly. Every call is audited:
// "the assistant forgot something" is otherwise indistinguishable from a bug,
// and with a local-only store there is no second copy to consult.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	m, ok := s.cache[id]
	delete(s.cache, id)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("memory %s not found", id)
	}
	if s.retrieval != nil {
		_ = s.retrieval.Remove(nsOfScope(scopeKey(m.Scope)), id)
	}
	err := s.entities.Remove(id)
	s.logAudit("memory.delete", map[string]string{"id": id, "level": m.Level, "scope": m.Scope})
	return err
}

// List returns memories matching the filter.
func (s *Service) List(f ListFilter) []*Memory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Memory, 0, len(s.cache))
	for _, m := range s.cache {
		c := m
		if f.Level != "" && c.Level != f.Level {
			continue
		}
		if f.Scope != "" && c.Scope != f.Scope {
			continue
		}
		if f.Category != "" && c.Category != f.Category {
			continue
		}
		if f.Scene != "" && c.Scene != f.Scene {
			continue
		}
		if !f.Include && (!c.Enabled || c.SupersededBy != "") {
			continue
		}
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out
}

// ---- recall ----

// Recall runs dual-path retrieval (BM25 ∥ embedding → RRF) over one scope and
// applies the hit-count bookkeeping used to rank recurring memories.
func (s *Service) Recall(query string, topK int, scope string) ([]RecallHit, error) {
	if s.retrieval == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if topK <= 0 {
		topK = 5
	}
	sk := scopeKey(scope)
	ns := nsOfScope(sk)
	bm := s.retrieval.SearchBM25(ns, query, topK*2)
	var vec []retrieval.Hit
	if emb, err := s.embedQuery(query); err == nil && len(emb) > 0 {
		vec = s.retrieval.SearchVector(ns, emb, topK*2)
	}
	fused := s.retrieval.RRF(bm, vec, topK)
	out := make([]RecallHit, 0, len(fused))
	now := time.Now().UnixMilli()
	s.mu.Lock()
	for _, h := range fused {
		m, ok := s.cache[h.ID]
		if !ok {
			continue
		}
		if !m.recallable(now) {
			continue
		}
		cp := m
		cp.HitCount++
		cp.LastHitAt = now
		s.cache[h.ID] = cp
		_ = s.entities.Save(cp.ID, cp)
		out = append(out, RecallHit{Memory: &cp, Score: h.Score})
	}
	s.mu.Unlock()
	return out, nil
}

// recallable reports whether this memory may be returned by recall. It is the
// single predicate behind every recall path, so the rules cannot drift apart.
//
// The boundary is <=, not <: a memory marked invalid *now* has to be out of the
// very next recall. With < the explicit 失效 action left the entry live for
// another millisecond, which is invisible in use but wrong in principle — and
// "it stays until something else happens to move the clock" is exactly the kind
// of behaviour that is impossible to reason about later.
func (m Memory) recallable(now int64) bool {
	if !m.Enabled || m.SupersededBy != "" {
		return false
	}
	return m.InvalidAt == 0 || m.InvalidAt > now
}

func (s *Service) embedQuery(query string) ([]float32, error) {
	s.mu.RLock()
	cfg := s.emb
	s.mu.RUnlock()
	if cfg.ProviderId == "" || cfg.Model == "" || s.providers == nil {
		return nil, fmt.Errorf("no embedding provider configured")
	}
	rt, err := s.providers.GetRuntime(cfg.ProviderId)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	embs, err := rt.Embeddings(ctx, cfg.Model, []string{query})
	if err != nil || len(embs) == 0 {
		if err == nil {
			err = fmt.Errorf("empty embedding")
		}
		return nil, err
	}
	return embs[0], nil
}

// SetEmbedding records which provider/model produced the stored vectors so
// recall embeds the query with the same model.
func (s *Service) SetEmbedding(providerID, model string, dim int) {
	s.mu.Lock()
	s.emb = embeddingConfig{ProviderId: providerID, Model: model, Dim: dim}
	s.mu.Unlock()
}

// Embed indexes the given memories (or all L1 when ids is empty) with the
// configured embedding model, writing vectors to memory/index/{scope}.json.
func (s *Service) Embed(ids []string, providerID, model string) error {
	if s.providers == nil || providerID == "" || model == "" {
		return fmt.Errorf("embedding provider required")
	}
	rt, err := s.providers.GetRuntime(providerID)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	s.mu.RLock()
	items := make([]*Memory, 0, len(s.cache))
	for _, m := range s.cache {
		if len(want) > 0 && !want[m.ID] {
			continue
		}
		c := m
		items = append(items, &c)
	}
	s.mu.RUnlock()
	if len(items) == 0 {
		return nil
	}
	texts := make([]string, 0, len(items))
	for _, m := range items {
		texts = append(texts, m.Content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	embs, err := rt.Embeddings(ctx, model, texts)
	if err != nil {
		return err
	}
	dim := 0
	byScope := map[string][]retrieval.Record{}
	for i, m := range items {
		if i >= len(embs) {
			break
		}
		sk := scopeKey(m.Scope)
		byScope[sk] = append(byScope[sk], retrieval.Record{ID: m.ID, Text: m.Content, Embedding: embs[i]})
		m.EmbeddingProviderId = providerID
		m.EmbeddingModel = model
		m.EmbeddingDim = len(embs[i])
		dim = len(embs[i])
		_ = s.entities.Save(m.ID, m)
		s.mu.Lock()
		s.cache[m.ID] = *m
		s.mu.Unlock()
	}
	if dim > 0 {
		s.SetEmbedding(providerID, model, dim)
	}
	for sk, recs := range byScope {
		// Merge with pre-existing records of the same scope, but drop the ones
		// whose memory no longer exists, was invalidated, or moved to another
		// scope. Without this the vector file only ever grew: deleted entries keep
		// occupying recall candidate slots (Recall then throws the hit away on the
		// cache lookup), and after enough deletions live memories stop being
		// returned at all.
		kept := map[string]retrieval.Record{}
		var prev persistedIndex
		if err := s.indexes.Load(sk, &prev); err == nil {
			now := time.Now().UnixMilli()
			s.mu.RLock()
			for _, r := range prev.Records {
				if m, ok := s.cache[r.ID]; ok && m.recallable(now) && scopeKey(m.Scope) == sk {
					kept[r.ID] = r
				}
			}
			s.mu.RUnlock()
		}
		for _, r := range recs {
			kept[r.ID] = r
		}
		merged := make([]retrieval.Record, 0, len(kept))
		for _, r := range kept {
			merged = append(merged, r)
		}
		if err := s.persistVectors(sk, merged); err != nil {
			return err
		}
		if s.retrieval != nil {
			_ = s.retrieval.BuildIndex(nsOfScope(sk), merged)
		}
	}
	return nil
}

// Retrieve satisfies orchestrator.MemoryService.
func (s *Service) Retrieve(_ context.Context, query string, topK int) []MemoryResult {
	mem, _ := s.Recall(query, topK, ScopeGlobal)
	if len(mem) == 0 {
		return nil
	}
	out := make([]MemoryResult, 0, len(mem))
	for _, m := range mem {
		out = append(out, MemoryResult{ID: m.ID, Level: m.Level, Content: m.Content, Score: 1.0})
	}
	return out
}

// Searcher returns a builtin.MemorySearcher closure for the memory_search tool.
func (s *Service) Searcher() func(query string, topK int) string {
	return func(query string, topK int) string {
		hits, err := s.Recall(query, topK, ScopeGlobal)
		if err != nil || len(hits) == 0 {
			return "[memory_search] 未检索到相关记忆。"
		}
		var b strings.Builder
		for i, h := range hits {
			fmt.Fprintf(&b, "[%s/%s] %s\n", h.Level, h.Category, h.Content)
			if i >= topK-1 {
				break
			}
		}
		return b.String()
	}
}

// ---- L3 persona (memory/persona.md) ----

// Persona returns the L3 profile document (human-readable Markdown).
func (s *Service) Persona() string {
	b, err := s.readFile("persona.md")
	if err != nil {
		return ""
	}
	return string(b)
}

// SetPersona overwrites the L3 profile document.
func (s *Service) SetPersona(md string) error {
	// keep one backup so a bad consolidation run is recoverable by hand
	if cur := s.Persona(); cur != "" {
		_ = s.writeFile("persona.md.bak", []byte(cur))
	}
	return s.writeFile("persona.md", []byte(md))
}

// ---- L2 scenes (memory/scenes/{scene}.md + memory/index.json) ----

func (s *Service) loadScenes() {
	b, err := s.readFile("index.json")
	if err != nil {
		return
	}
	var idx struct {
		Scenes map[string][]string `json:"scenes"`
	}
	if err := json.Unmarshal(b, &idx); err == nil && idx.Scenes != nil {
		s.scenes = idx.Scenes
	}
}

func (s *Service) saveScenes() error {
	// Copy the map under the read lock. Handing the live map to json.Marshal
	// after unlocking would iterate it concurrently with a scene write, which
	// is an unrecoverable "concurrent map iteration and map write" crash.
	s.mu.RLock()
	idx := struct {
		Scenes map[string][]string `json:"scenes"`
	}{Scenes: make(map[string][]string, len(s.scenes))}
	for k, v := range s.scenes {
		cp := make([]string, len(v))
		copy(cp, v)
		idx.Scenes[k] = cp
	}
	s.mu.RUnlock()
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return s.writeFile("index.json", data)
}

// Scenes lists known scene names.
func (s *Service) Scenes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.scenes))
	for k := range s.scenes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Scene returns the Markdown body of one scene plus its L1 member ids.
func (s *Service) Scene(name string) (string, []string, error) {
	b, err := s.readFile(filepath.Join("scenes", sceneFileName(name)))
	if err != nil {
		return "", nil, fmt.Errorf("scene %s not found", name)
	}
	s.mu.RLock()
	members := append([]string(nil), s.scenes[name]...)
	s.mu.RUnlock()
	return string(b), members, nil
}

// SaveScene writes a scene document and updates the scene→memberIds index.
func (s *Service) SaveScene(name, markdown string, memberIds []string) error {
	if name == "" {
		return fmt.Errorf("scene name required")
	}
	if err := s.writeFile(filepath.Join("scenes", sceneFileName(name)), []byte(markdown)); err != nil {
		return err
	}
	s.mu.Lock()
	s.scenes[name] = memberIds
	s.mu.Unlock()
	return s.saveScenes()
}

// DeleteScene removes a scene document and its index entry.
func (s *Service) DeleteScene(name string) error {
	_ = os.Remove(filepath.Join(s.baseDir, "scenes", sceneFileName(name)))
	s.mu.Lock()
	delete(s.scenes, name)
	s.mu.Unlock()
	return s.saveScenes()
}

func sceneFileName(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return r.Replace(name) + ".md"
}

// Guidance returns the always-on steering block: L3 persona plus L2 scene
// headings (DESIGN §6.5 ② defaults). It is deliberately cheap so it can be
// injected into every turn without blowing the context budget.
func (s *Service) Guidance(maxChars int) string {
	if maxChars <= 0 {
		maxChars = 1500
	}
	var b strings.Builder
	if p := strings.TrimSpace(s.Persona()); p != "" {
		b.WriteString("<persona>\n")
		b.WriteString(truncate(p, maxChars/2))
		b.WriteString("\n</persona>\n")
	}
	scenes := s.Scenes()
	if len(scenes) > 0 {
		b.WriteString("<scenes>\n")
		left := maxChars - b.Len()
		for _, name := range scenes {
			if left <= 0 {
				break
			}
			line := fmt.Sprintf("- %s\n", name)
			b.WriteString(line)
			left -= len(line)
		}
		b.WriteString("</scenes>\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// Cut on a rune boundary. Slicing bytes in the middle of a multi-byte rune
	// puts invalid UTF-8 into the system prompt — the same defect as the
	// desktop-control Truncate that produced half a Chinese character
	// (DESIGN §16 缺陷表), in a new place.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// ---- contradictions (memory/conflicts.json) ----

func (s *Service) loadConflicts() {
	b, err := s.readFile("conflicts.json")
	if err != nil {
		return
	}
	var list []Conflict
	if err := json.Unmarshal(b, &list); err == nil {
		s.conflicts = list
	}
}

func (s *Service) saveConflicts() error {
	// Marshal inside the lock: the slice may be appended to concurrently and
	// encoding the shared backing array would race.
	s.mu.RLock()
	data, err := json.MarshalIndent(s.conflicts, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return s.writeFile("conflicts.json", data)
}

// Conflicts returns pending contradictions (status=open when openOnly).
func (s *Service) Conflicts(openOnly bool) []Conflict {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Conflict, 0, len(s.conflicts))
	for _, c := range s.conflicts {
		if openOnly && c.Status != "open" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// AddConflict registers a contradiction. It never touches existing memories:
// resolving one requires an explicit call to ResolveConflict.
func (s *Service) AddConflict(c Conflict) (Conflict, error) {
	if c.ID == "" {
		c.ID = util.NewID()
	}
	if c.Status == "" {
		c.Status = "open"
	}
	c.CreatedAt = time.Now().UnixMilli()
	// de-duplicate: same claim pair already open -> keep the first one
	s.mu.Lock()
	for _, e := range s.conflicts {
		if e.Status == "open" && sameClaimPair(e, c) {
			s.mu.Unlock()
			return e, nil
		}
	}
	s.conflicts = append(s.conflicts, c)
	s.mu.Unlock()
	return c, s.saveConflicts()
}

func sameClaimPair(a, b Conflict) bool {
	return (a.ClaimA == b.ClaimA && a.ClaimB == b.ClaimB) ||
		(a.ClaimA == b.ClaimB && a.ClaimB == b.ClaimA)
}

// ResolveConflict closes a contradiction and, when a memory id is named,
// marks it superseded. This is the only path that mutates a memory based on
// a conflict, and it always requires an explicit human decision.
func (s *Service) ResolveConflict(id, resolution, resolvedBy, supersedeID string) error {
	now := time.Now().UnixMilli()
	s.mu.Lock()
	found := false
	for i := range s.conflicts {
		if s.conflicts[i].ID != id {
			continue
		}
		found = true
		s.conflicts[i].Status = "resolved"
		s.conflicts[i].Resolution = resolution
		s.conflicts[i].ResolvedBy = resolvedBy
		s.conflicts[i].ResolvedAt = now
	}
	s.mu.Unlock()
	if !found {
		return fmt.Errorf("conflict %s not found", id)
	}
	if supersedeID != "" {
		s.mu.RLock()
		m, ok := s.cache[supersedeID]
		s.mu.RUnlock()
		if ok {
			c := m
			c.SupersededBy = "resolved:" + id
			c.InvalidAt = now
			c.UpdatedAt = now
			_ = s.Update(&c)
		}
	}
	s.logAudit("memory.conflict.resolve", map[string]string{
		"conflict": id, "resolution": resolution, "by": resolvedBy, "supersede": supersedeID,
	})
	return s.saveConflicts()
}

// ---- drilldown (L3 → L2 → L1 → L0) ----

// DrillStep is one node in a provenance chain.
type DrillStep struct {
	Level   string  `json:"level"`
	ID      string  `json:"id"`
	Title   string  `json:"title,omitempty"`
	Content string  `json:"content,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

// Drilldown is a full trace from one node back to the raw conversation.
type Drilldown struct {
	Root  string      `json:"root"`
	Chain []DrillStep `json:"chain"`
}

// Drilldown resolves an id (persona / scene:{name} / memory id) down to L0.
func (s *Service) Drilldown(id string) (*Drilldown, error) {
	d := &Drilldown{Root: id}
	switch {
	case id == PersonaID || id == LevelL3:
		p := s.Persona()
		if p == "" {
			return nil, fmt.Errorf("persona is empty")
		}
		d.Chain = append(d.Chain, DrillStep{Level: LevelL3, ID: PersonaID, Title: "persona", Content: p})
		for _, name := range s.Scenes() {
			body, members, err := s.Scene(name)
			if err != nil {
				continue
			}
			step := DrillStep{Level: LevelL2, ID: ScenePrefix + name, Title: name, Content: body}
			d.Chain = append(d.Chain, step)
			s.appendL1(d, members)
		}
	case strings.HasPrefix(id, ScenePrefix):
		name := strings.TrimPrefix(id, ScenePrefix)
		body, members, err := s.Scene(name)
		if err != nil {
			return nil, err
		}
		d.Chain = append(d.Chain, DrillStep{Level: LevelL2, ID: id, Title: name, Content: body})
		s.appendL1(d, members)
	default:
		m, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		d.Chain = append(d.Chain, DrillStep{Level: m.Level, ID: m.ID, Title: m.Category, Content: m.Content})
		s.appendL1(d, []string{m.ID})
	}
	return d, nil
}

// appendL1 adds the L1 members of a scene, then resolves their L0 sources.
// The two passes must stay separate: dedupe guards the L1 insertion, but an id
// already present in the chain (the drilldown root, say) still needs its L0
// originals resolved — folding the passes together silently dropped them.
func (s *Service) appendL1(d *Drilldown, ids []string) {
	seen := map[string]bool{}
	for _, c := range d.Chain {
		seen[c.ID] = true
	}
	var resolved []string
	for _, mid := range ids {
		if !seen[mid] {
			seen[mid] = true
			m, err := s.Get(mid)
			if err != nil {
				continue
			}
			d.Chain = append(d.Chain, DrillStep{Level: m.Level, ID: m.ID, Title: m.Category, Content: m.Content})
		}
		resolved = append(resolved, mid)
	}
	s.appendL0(d, resolved)
}

// appendL0 pulls the originating conversation messages behind each L1 memory.
func (s *Service) appendL0(d *Drilldown, l1IDs []string) {
	if s.convs == nil {
		return
	}
	seen := map[string]bool{}
	for _, c := range d.Chain {
		seen[c.ID] = true
	}
	for _, mid := range l1IDs {
		m, err := s.Get(mid)
		if err != nil || m.Links.SourceConversationId == "" {
			continue
		}
		msgs, err := s.convs.Messages(m.Links.SourceConversationId)
		if err != nil {
			continue
		}
		want := map[string]bool{}
		for _, smid := range m.Links.SourceMessageIds {
			want[smid] = true
		}
		for _, msg := range msgs {
			if (len(want) > 0 && !want[msg.ID]) || seen[msg.ID] {
				continue
			}
			seen[msg.ID] = true
			d.Chain = append(d.Chain, DrillStep{
				Level:   LevelL0,
				ID:      msg.ID,
				Title:   msg.Role,
				Content: msg.Content,
			})
		}
	}
}

// ---- extraction cadence gate (see package extract for the full gate) ----

// LastExtractAt returns the last extraction timestamp for a conversation.
func (s *Service) LastExtractAt(convID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gate[convID]
}

// MarkExtracted records that a conversation was distilled at time t.
func (s *Service) MarkExtracted(convID string, t int64) {
	s.mu.Lock()
	s.gate[convID] = t
	s.mu.Unlock()
}
