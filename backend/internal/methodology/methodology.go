// Package methodology implements the methodology library (DESIGN §5 / §14#9):
// the "how to work" half of the four-library split. It stores good/bad
// experience entries plus a small set of standing behavioural principles, and
// activates them at assembly time.
//
// Three activation layers:
//  1. Standing principles (Always=true): always injected, no matching needed.
//  2. Keyword activation: Match() scores every active entry against the user
//     query by keyword命中, so "先估算一下工期" pulls in the estimating entries.
//     Bad entries are phrased as things to avoid rather than things to do.
//  3. Explicit binding: a role or skill may pin specific ids.
//
// Retired entries stay on disk (SupersededBy / Status) so the reasoning trail
// behind a change is auditable instead of silently deleted.
package methodology

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

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/llm"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Kind distinguishes experience entries.
const (
	KindGood      = "good"      // 正面经验：推荐做法
	KindBad       = "bad"       // 反面经验：应避免的做法
	KindPrinciple = "principle" // 常驻行为准则
)

// Status is the lifecycle state of an entry.
const (
	StatusActive  = "active"
	StatusRetired = "retired"
)

// Injection budget (DESIGN §10.2 / §17 原则六).
//
// Standing rules are injected into *every* turn and keyword entries on every
// hit, so an unbounded library is not a storage problem but a prompt problem:
// past a point the rules crowd out the request the user actually made, the
// model starts ignoring the lot, and each turn gets slower and dearer. A user
// reaches this by hand or by importing someone else's bundle, so the limits
// are enforced here rather than left to good intentions.
const (
	// MaxStandingEntries caps Always=true rules, each of which fires every turn.
	MaxStandingEntries = 10
	// MaxContentRunes caps one entry (runes, not bytes: a byte cap would cut
	// Chinese entries at a third of the length a user reads as "the same").
	MaxContentRunes = 4000
	// MaxTotalEntries caps the library as a whole.
	MaxTotalEntries = 200
)

// Methodology is a persisted methodology entry.
type Methodology struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Description  string   `json:"description,omitempty"`
	Content      string   `json:"content"`
	Keywords     []string `json:"keywords,omitempty"`
	Enabled      bool     `json:"enabled"`
	Always       bool     `json:"always"`
	Status       string   `json:"status"`
	Supersedes   string   `json:"supersedes,omitempty"`
	SupersededBy string   `json:"supersededBy,omitempty"`
	Hits         int      `json:"hits"`
	LastHitAt    int64    `json:"lastHitAt,omitempty"`
	Version      int      `json:"version"`
	Builtin      bool     `json:"builtin"`
	CreatedAt    int64    `json:"createdAt"`
	UpdatedAt    int64    `json:"updatedAt"`
}

// MatchHit is one activated entry returned by Match.
type MatchHit struct {
	Methodology
	Score int      `json:"score"`
	Hits  []string `json:"matchedKeywords"`
}

// Service manages methodologies.
type Service struct {
	store storage.Store
	mu    sync.RWMutex
	cache map[string]*Methodology
	// audit records who changed which rule. The rules are injected into the
	// system prompt, so "where did this behaviour come from" has to be
	// answerable without diffing the data directory by hand.
	audit *audit.Audit
}

// SetAudit attaches the audit logger. Optional: nil disables logging.
func (s *Service) SetAudit(a *audit.Audit) { s.audit = a }

// logAudit records a mutation. Callers hold s.mu; the write is a single small
// append and these paths are cold (a human editing a rule), so it stays inside
// the lock rather than complicating every caller with an unlock-then-log step.
func (s *Service) logAudit(event, id string, fields map[string]string) {
	if s.audit == nil {
		return
	}
	if fields == nil {
		fields = map[string]string{}
	}
	fields["id"] = id
	s.audit.Log(event, "", fields)
}

// standingCount counts the entries that fire on every turn, ignoring the one
// being written so that re-saving a standing rule is not blocked by its own
// existence.
func (s *Service) standingCount(exceptID string) int {
	n := 0
	for id, m := range s.cache {
		if id == exceptID || !m.Enabled || m.Status != StatusActive || !m.Always {
			continue
		}
		n++
	}
	return n
}

// New constructs the methodology Service and seeds built-ins when empty.
func New(store storage.Store) *Service {
	s := &Service{store: store, cache: map[string]*Methodology{}}
	s.load()
	if len(s.cache) == 0 {
		s.seed()
	}
	return s
}

func (s *Service) load() {
	ids, err := s.store.IDs()
	if err != nil {
		return
	}
	for _, id := range ids {
		var m Methodology
		if err := s.store.Load(id, &m); err == nil {
			// Documents written before the good/bad split carried `prompt`
			// instead of `content`; adopt it rather than dropping the entry.
			if m.Content == "" {
				var legacy struct {
					Prompt string `json:"prompt"`
				}
				if err := s.store.Load(id, &legacy); err == nil {
					m.Content = legacy.Prompt
				}
			}
			if m.Kind == "" {
				if m.Always {
					m.Kind = KindPrinciple
				} else {
					m.Kind = KindGood
				}
			}
			if m.Status == "" {
				m.Status = StatusActive
			}
			if !m.Enabled {
				m.Enabled = true // pre-Enabled documents default to on
			}
			s.cache[id] = &m
		}
	}
}

// List returns all methodologies, standing principles first then by name.
func (s *Service) List() []*Methodology {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Methodology, 0, len(s.cache))
	for _, m := range s.cache {
		cp := *m
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Always != out[j].Always {
			return out[i].Always
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns a methodology by id.
func (s *Service) Get(id string) (*Methodology, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.cache[id]
	if !ok {
		return nil, fmt.Errorf("methodology %s not found", id)
	}
	cp := *m
	return &cp, nil
}

// Create persists a new methodology. A brand-new entry is enabled unless the
// caller asked for a different lifecycle state: an entry that silently never
// fires is a worse failure than one you have to switch off. Import uses
// createPreserving so a bundle round-trip keeps its switches.
func (s *Service) Create(m *Methodology) error {
	fresh := m.Status == ""
	return s.create(m, fresh)
}

// createPersisting stores the entry without applying the "new entry" defaults.
func (s *Service) createPreserving(m *Methodology) error {
	return s.create(m, false)
}

func (s *Service) create(m *Methodology, applyNewDefaults bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = util.NewID()
	}
	if err := m.normalise(); err != nil {
		return err
	}
	if applyNewDefaults && !m.Enabled {
		m.Enabled = true
	}
	if err := s.checkBudget(m.ID, m.Always); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	m.CreatedAt, m.UpdatedAt = now, now
	if m.Version == 0 {
		m.Version = 1
	}
	if err := s.store.Save(m.ID, m); err != nil {
		return err
	}
	cp := *m
	s.cache[m.ID] = &cp
	s.logAudit("methodology.create", m.ID, map[string]string{
		"kind": m.Kind, "always": boolStr(m.Always), "enabled": boolStr(m.Enabled), "builtin": boolStr(m.Builtin),
	})
	return nil
}

// checkBudget refuses a write that would blow the per-turn injection budget.
// Callers hold s.mu.
func (s *Service) checkBudget(id string, always bool) error {
	if _, exists := s.cache[id]; !exists && len(s.cache) >= MaxTotalEntries {
		return fmt.Errorf("行为规则总数已达上限 %d 条：请先删掉不再需要的条目", MaxTotalEntries)
	}
	if !always {
		return nil
	}
	if n := s.standingCount(id); n >= MaxStandingEntries {
		return fmt.Errorf(
			"常驻原则最多 %d 条（当前已有 %d 条）：每一条都会在每轮对话中生效，条数太多会挤占你的实际提问。"+
				"可以把其中一条改成关键词触发，或删掉多余的", MaxStandingEntries, n)
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Update replaces a methodology.
func (s *Service) Update(id string, m *Methodology) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cache[id]; !ok {
		return fmt.Errorf("methodology %s not found", id)
	}
	m.ID = id
	if err := m.normalise(); err != nil {
		return err
	}
	if err := s.checkBudget(id, m.Always); err != nil {
		return err
	}
	m.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, m); err != nil {
		return err
	}
	cp := *m
	s.cache[id] = &cp
	s.logAudit("methodology.update", id, map[string]string{
		"kind": m.Kind, "always": boolStr(m.Always), "enabled": boolStr(m.Enabled),
	})
	return nil
}

// Delete removes a methodology. Unlike Retire there is no trace left in the
// library itself, so the audit entry is the only record that it existed.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.cache[id]
	if !ok {
		return fmt.Errorf("methodology %s not found", id)
	}
	_ = s.store.Remove(id)
	delete(s.cache, id)
	s.logAudit("methodology.delete", id, map[string]string{
		"kind": old.Kind, "always": boolStr(old.Always), "builtin": boolStr(old.Builtin),
	})
	return nil
}

// Toggle flips the enabled flag.
func (s *Service) Toggle(id string) (*Methodology, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.cache[id]
	if !ok {
		return nil, fmt.Errorf("methodology %s not found", id)
	}
	m.Enabled = !m.Enabled
	m.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, m); err != nil {
		m.Enabled = !m.Enabled
		return nil, err
	}
	s.logAudit("methodology.toggle", id, map[string]string{"enabled": boolStr(m.Enabled)})
	cp := *m
	return &cp, nil
}

// Retire marks an entry as superseded by another one instead of deleting it,
// keeping the reasoning trail auditable (DESIGN §5 退役链).
func (s *Service) Retire(id, byID string) (*Methodology, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.cache[id]
	if !ok {
		return nil, fmt.Errorf("methodology %s not found", id)
	}
	m.Status = StatusRetired
	m.SupersededBy = byID
	m.Enabled = false
	m.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, m); err != nil {
		return nil, err
	}
	if byID != "" {
		if heir, ok := s.cache[byID]; ok {
			heir.Supersedes = id
			heir.UpdatedAt = m.UpdatedAt
			_ = s.store.Save(byID, heir)
		}
	}
	cp := *m
	s.logAudit("methodology.retire", id, map[string]string{"by": byID, "kind": m.Kind})
	return &cp, nil
}

func (m *Methodology) normalise() error {
	m.Name = strings.TrimSpace(m.Name)
	m.Content = strings.TrimSpace(m.Content)
	if m.Name == "" || m.Content == "" {
		return fmt.Errorf("methodology name and content are required")
	}
	// Count runes, not bytes: a byte cap would cut a Chinese entry at roughly a
	// third of the length an English one gets, and the limit is about how much
	// the model reads, which is tokens.
	if n := len([]rune(m.Content)); n > MaxContentRunes {
		return fmt.Errorf("条目内容 %d 字，超过单条上限 %d 字：请拆成多条，或删掉与规则无关的举例",
			n, MaxContentRunes)
	}
	switch m.Kind {
	case KindGood, KindBad, KindPrinciple:
	default:
		if m.Always {
			m.Kind = KindPrinciple
		} else {
			m.Kind = KindGood
		}
	}
	if m.Status == "" {
		m.Status = StatusActive
	}
	if m.Always {
		m.Kind = KindPrinciple
	}
	seen := map[string]bool{}
	kw := make([]string, 0, len(m.Keywords))
	for _, k := range m.Keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		kw = append(kw, k)
	}
	m.Keywords = kw
	return nil
}

// Match returns the entries activated by query: standing principles always,
// keyword hits scored by the number of distinct matched keywords (longer
// keywords weigh more because they are more specific). Hits are recorded so
// the UI can surface which entries actually fire.
func (s *Service) Match(query string, pinned []string) []MatchHit {
	q := strings.ToLower(strings.TrimSpace(query))
	pinnedSet := map[string]bool{}
	for _, id := range pinned {
		pinnedSet[id] = true
	}

	s.mu.Lock()
	now := time.Now().UnixMilli()
	out := make([]MatchHit, 0)
	for _, m := range s.cache {
		if !m.Enabled || m.Status != StatusActive {
			continue
		}
		hit := MatchHit{Methodology: *m}
		switch {
		case m.Always:
			hit.Score = 1 << 20 // standing principles sort first
		default:
			for _, k := range m.Keywords {
				if k != "" && strings.Contains(q, k) {
					hit.Score += 10 + len([]rune(k))
					hit.Hits = append(hit.Hits, k)
				}
			}
		}
		if hit.Score == 0 && !pinnedSet[m.ID] {
			continue
		}
		if pinnedSet[m.ID] && hit.Score == 0 {
			hit.Score = 1 << 19 // explicit binding, just below standing rules
		}
		out = append(out, hit)
		// Persist the usage signal without blocking the turn on IO errors.
		m.Hits++
		m.LastHitAt = now
		_ = s.store.Save(m.ID, m)
	}
	s.mu.Unlock()

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Export returns a methodology as JSON.
func (s *Service) Export(id string) ([]byte, error) {
	m, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	doc := map[string]interface{}{"schemaVersion": 1}
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, &doc)
	return json.MarshalIndent(doc, "", "  ")
}

// ExportAll exports every entry as a bundle, optionally including retired ones.
func (s *Service) ExportAll() ([]byte, error) {
	doc := map[string]interface{}{
		"schemaVersion": 1,
		"kind":          "methodologies",
		"exportedAt":    time.Now().UnixMilli(),
		"items":         s.List(),
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Import creates entries from exported JSON (single doc or {items:[...]}).
func (s *Service) Import(data []byte) ([]*Methodology, error) {
	var probe struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("invalid methodology document: %w", err)
	}
	raws := probe.Items
	if len(raws) == 0 {
		raws = []json.RawMessage{data}
	}
	out := make([]*Methodology, 0, len(raws))
	for _, raw := range raws {
		var m Methodology
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("invalid methodology entry: %w", err)
		}
		// A legacy export carries `prompt` instead of `content`.
		if m.Content == "" {
			var legacy struct {
				Prompt string `json:"prompt"`
			}
			_ = json.Unmarshal(raw, &legacy)
			m.Content = legacy.Prompt
		}
		m.ID = ""
		if err := s.createPreserving(&m); err != nil {
			return nil, err
		}
		cp := m
		out = append(out, &cp)
	}
	return out, nil
}

// ExportFile writes the compatibility bundle at
// {dataDir}/methodology/methodology.json (DESIGN §14#9 / 契约补遗 #9).
func (s *Service) ExportFile(dataDir string) error {
	doc := map[string]interface{}{
		"schemaVersion": 1,
		"kind":          "methodologies",
		"exportedAt":    time.Now().UnixMilli(),
		"items":         s.List(),
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(dataDir, "methodology")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "methodology.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "methodology.json"))
}

// ImportFile reads the compatibility bundle if it exists. A missing file is not
// an error — it simply means nothing has been exported yet.
func (s *Service) ImportFile(dataDir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "methodology", "methodology.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	got, err := s.Import(b)
	return len(got), err
}

// ---- learning ----

// LearnProposal is one entry distilled from a retrospective text.
type LearnProposal struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description,omitempty"`
	Content     string   `json:"content"`
	Keywords    []string `json:"keywords"`
}

const learnPrompt = `你是方法论提炼助手。从下面这段复盘/反馈文本中，提炼出可复用的工作方法条目。

要求：
- 只提炼真正可复用的做法，不要复述具体业务细节。
- 每条区分 good（应当坚持的做法）或 bad（应当避免的做法）。
- keywords 给 2-4 个触发关键词（小写、短词），用于将来按关键词激活；必须是原文里出现过的词或紧密相关的词。
- 最多 5 条；文本没有可提炼内容时返回空数组。
- 只输出 JSON：{"items":[{"name":"","kind":"good|bad","description":"","content":"","keywords":[""]}]}

文本：
`

// Learn distills methodology entries from a retrospective text and appends them
// as new entries (they are never auto-merged into existing ones — merging is a
// human decision, same invariant as memory conflict resolution).
//
// Every entry it creates lands with Enabled=false: distillation is driven by
// text the app did not author, so its output is a *proposal* until a human
// switches it on. Nothing currently calls Learn, and this default has to hold
// before anything does.
func (s *Service) Learn(ctx context.Context, rt provider.Runtime, model, text string) ([]*Methodology, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("empty text")
	}
	if rt == nil {
		return nil, fmt.Errorf("no provider configured")
	}
	raw, err := llm.ChatObject(ctx, rt, model, []provider.Message{
		{Role: "user", Content: learnPrompt + text},
	}, provider.CallOptions{Temperature: 0.2, MaxTokens: 1600})
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []LearnProposal `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("unparsable learn result: %w", err)
	}
	created := make([]*Methodology, 0, len(out.Items))
	for _, p := range out.Items {
		m := &Methodology{
			Name:        strings.TrimSpace(p.Name),
			Kind:        strings.ToLower(strings.TrimSpace(p.Kind)),
			Description: strings.TrimSpace(p.Description),
			Content:     strings.TrimSpace(p.Content),
			Keywords:    p.Keywords,
			// Landed switched off, on purpose. A distilled rule is written into
			// the system prompt, so anything that can steer the distillation
			// text — a conversation, a scraped page, a document — could
			// otherwise promote its own instructions into every later turn.
			// Off means a human turns it on deliberately; that step is the
			// whole guardrail, and defaulting to on would remove it silently.
			Enabled: false,
			Status:  StatusActive,
		}
		if m.Name == "" || m.Content == "" {
			continue
		}
		if err := s.Create(m); err != nil {
			return created, err
		}
		cp := *m
		created = append(created, &cp)
	}
	return created, nil
}

// ---- seeds ----

func (s *Service) seed() {
	now := time.Now().UnixMilli()
	for _, m := range Builtins() {
		m.CreatedAt, m.UpdatedAt = now, now
		_ = s.store.Save(m.ID, m)
		cp := *m
		s.cache[m.ID] = &cp
	}
}

// Builtins returns the seeded library: five standing principles (DESIGN §14#9)
// plus good/bad experience entries with their activation keywords.
func Builtins() []*Methodology {
	always := func(id, name, content string) *Methodology {
		return &Methodology{
			ID: id, Name: name, Kind: KindPrinciple, Content: content,
			Always: true, Enabled: true, Status: StatusActive,
			Builtin: true, Version: 1,
		}
	}
	good := func(id, name, desc, content string, kw []string) *Methodology {
		return &Methodology{
			ID: id, Name: name, Kind: KindGood, Description: desc, Content: content,
			Keywords: kw, Enabled: true, Status: StatusActive,
			Builtin: true, Version: 1,
		}
	}
	bad := func(id, name, desc, content string, kw []string) *Methodology {
		return &Methodology{
			ID: id, Name: name, Kind: KindBad, Description: desc, Content: content,
			Keywords: kw, Enabled: true, Status: StatusActive,
			Builtin: true, Version: 1,
		}
	}
	return []*Methodology{
		always("m-principle-confirm", "先确认后行动",
			"信息不足时先提出澄清问题，而不是假设前提后继续推进。"),
		always("m-principle-fact-inference", "区分事实与推断",
			"明确区分「已确认的事实」「合理推断」「待验证假设」三类陈述，不要把推断写成结论。"),
		always("m-principle-cite", "给出依据与不确定处",
			"重要结论给出依据来源；对不确定的部分显式标注，不要含糊带过。"),
		always("m-principle-next-step", "落到可执行的最小下一步",
			"每次输出都给出可执行的最小下一步，而不是停留在泛泛建议。"),
		always("m-principle-risk-confirm", "不可逆操作先确认",
			"涉及删除、覆盖、外发、付费等不可逆或高风险动作时，先说明影响并征得确认。"),

		good("m-good-minimal-verify", "最小可行验证", "先跑通最小闭环再扩展",
			"先设计一个能在最短时间内跑通的最小验证，确认关键假设成立后再投入扩展实现。",
			[]string{"验证", "试一下", "可行", "试点", "验证一下"}),
		good("m-good-api-first", "接口先行", "先定契约再写实现",
			"跨模块/跨端协作时先定接口契约（输入、输出、错误码、边界），各方按契约并行开发。",
			[]string{"接口", "契约", "对接", "联调", "api"}),
		good("m-good-reproduce-first", "复现优先", "定位问题先复现",
			"处理缺陷时先构造可稳定复现的最小场景，再谈根因与修复；无法复现的问题先补观测。",
			[]string{"报错", "异常", "bug", "崩溃", "失败", "复现"}),
		good("m-good-decompose-estimate", "拆分后再估算", "先拆任务再给工期",
			"估算前先把任务拆到可独立完成的小项，逐项估算并显式留出联调与返工余量。",
			[]string{"估时", "排期", "工期", "工作量", "多久"}),
		good("m-good-rollback-plan", "灰度与回滚", "变更必须有回退路径",
			"任何上线/迁移/大改动都要预先设计灰度节奏与回滚方案，并明确回滚触发条件。",
			[]string{"上线", "发布", "灰度", "回滚", "迁移"}),

		bad("m-bad-guess-estimate", "拍脑袋估算", "禁止无依据的工期承诺",
			"不要在未拆解、未参考历史数据的情况下给出确定性工期；应给出区间与前提条件。",
			[]string{"估时", "排期", "工期", "工作量", "多久"}),
		bad("m-bad-big-bang", "一次性大改", "避免无保护的大范围重写",
			"不要在没有测试与灰度保护的情况下一次性大范围重写；应小步重构，每步可验证。",
			[]string{"重构", "重写", "大改", "推倒"}),
		bad("m-bad-ignore-edge", "忽略边界情况", "别跳过边界与异常路径",
			"不要只覆盖理想路径；空值、超长、并发、超时、权限不足等边界必须显式处理。",
			[]string{"边界", "异常", "空值", "并发", "超时"}),
		bad("m-bad-silent-swallow", "静默吞错", "禁止吞掉错误",
			"不要用空 catch / 忽略返回值的方式静默吞掉错误；至少记录日志并向上传递可诊断信息。",
			[]string{"catch", "忽略", "吞", "try", "异常"}),
		bad("m-bad-copy-paste", "复制粘贴式复用", "复用前先理解上下文",
			"不要在不理解原上下文的情况下复制代码或方案；先确认前置条件与差异，再决定是否复用。",
			[]string{"复用", "抄", "copy", "照搬"}),
	}
}
