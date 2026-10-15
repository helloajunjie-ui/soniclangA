// Package extract distills L1 atomic memories from a finished conversation
// turn (DESIGN §6.5 ① / §15.4). It is deliberately conservative:
//
//   - the triple gate (30min cadence / ≤40 input messages / ≤8 output items)
//     keeps extraction cheap and prevents the memory store from flooding;
//   - internal plumbing messages (tool plumbing, scheduler echoes) are filtered
//     out, so the model distills what the human said rather than our own noise;
//   - secrets are redacted before the text leaves the machine, and any item
//     that still looks like a secret is dropped on the way back in.
//
// Extraction never deletes or rewrites an existing memory. Contradictions are
// left for consolidation to *register* (never auto-resolve).
package extract

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agenthub/agent-proxy/internal/llm"
	"github.com/agenthub/agent-proxy/internal/llmjson"
	"github.com/agenthub/agent-proxy/internal/memory"
	"github.com/agenthub/agent-proxy/internal/provider"
)

// Gates are the three extraction limits (DESIGN §6.5 ④).
type Gates struct {
	MinIntervalMinutes int `json:"minIntervalMinutes"` // cadence per conversation
	MaxInputMessages   int `json:"maxInputMessages"`   // messages distilled per run
	MaxOutputItems     int `json:"maxOutputItems"`     // memories produced per run
}

// DefaultGates returns the documented defaults (30min / 40 / 8).
func DefaultGates() Gates {
	return Gates{MinIntervalMinutes: 30, MaxInputMessages: 40, MaxOutputItems: 8}
}

// FromMessages adapts provider-shaped messages when ids are unavailable.
func FromMessages(msgs []provider.Message) []memory.ExtractInput {
	out := make([]memory.ExtractInput, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, memory.ExtractInput{Role: m.Role, Content: m.Content})
	}
	return out
}

// Extractor distills L1 memories through an LLM.
type Extractor struct {
	mem   *memory.Service
	prov  *provider.Service
	gates Gates
}

// New constructs an Extractor.
func New(mem *memory.Service, prov *provider.Service, g Gates) *Extractor {
	if g.MinIntervalMinutes <= 0 && g.MaxInputMessages <= 0 && g.MaxOutputItems <= 0 {
		g = DefaultGates()
	}
	return &Extractor{mem: mem, prov: prov, gates: g}
}

// Extract distills L1 memories for one conversation. force bypasses only the
// cadence gate (used by the manual API), never the input/output limits.
func (e *Extractor) Extract(ctx context.Context, convID, providerID, model string,
	inputs []memory.ExtractInput, force bool) (*memory.ExtractResult, error) {
	res := &memory.ExtractResult{}
	// e.prov must be checked here as well: a fresh install has auto-extract on
	// by default but no provider yet, and calling GetRuntime on a nil service
	// panics rather than erroring.
	if e.mem == nil || e.prov == nil || providerID == "" || model == "" {
		res.Skipped = "not_configured"
		return res, nil
	}
	// --- gate 1: cadence ---
	if !force && e.gates.MinIntervalMinutes > 0 {
		if last := e.mem.LastExtractAt(convID); last > 0 {
			if time.Since(time.UnixMilli(last)) < time.Duration(e.gates.MinIntervalMinutes)*time.Minute {
				res.Skipped = "cadence"
				return res, nil
			}
		}
	}
	// --- filter internal plumbing + mask secrets ---
	clean, filtered, redacted := sanitize(inputs)
	res.Filtered, res.Redacted = filtered, redacted
	if len(clean) == 0 {
		res.Skipped = "empty_after_filter"
		return res, nil
	}
	// --- gate 2: input size ---
	if e.gates.MaxInputMessages > 0 && len(clean) > e.gates.MaxInputMessages {
		clean = clean[len(clean)-e.gates.MaxInputMessages:]
	}
	// --- LLM distillation ---
	items, err := e.call(ctx, providerID, model, clean)
	if err != nil {
		return res, err
	}
	// --- gate 3: output size ---
	if e.gates.MaxOutputItems > 0 && len(items) > e.gates.MaxOutputItems {
		items = items[:e.gates.MaxOutputItems]
	}
	// --- persist, dropping anything that still smells like a secret ---
	srcIDs := make([]string, 0, len(clean))
	for _, in := range clean {
		if in.ID != "" {
			srcIDs = append(srcIDs, in.ID)
		}
	}
	now := time.Now().UnixMilli()
	for _, it := range items {
		content := strings.TrimSpace(it.Content)
		if content == "" || len([]rune(content)) < 4 {
			res.Dropped++
			continue
		}
		if SecretPatterns.MatchString(content) {
			res.Dropped++
			continue
		}
		if e.duplicate(content) {
			res.Dropped++
			continue
		}
		m := &memory.Memory{
			Level:      memory.LevelL1,
			Scope:      memory.ScopeGlobal,
			Category:   normalizeCategory(it.Category),
			Scene:      strings.TrimSpace(it.Scene),
			Content:    content,
			Tags:       it.Tags,
			Origin:     memory.OriginLLMExtract,
			Links:      memory.Links{SourceConversationId: convID, SourceMessageIds: srcIDs},
			Confidence: clamp01(it.Confidence, 0.6),
			Importance: clamp01(it.Importance, 0.5),
			Source:     "auto",
			CreatedAt:  now,
			UpdatedAt:  now,
			ValidFrom:  now,
		}
		saved, err := e.mem.Add(m)
		if err != nil {
			continue
		}
		res.Memories = append(res.Memories, saved)
	}
	e.mem.MarkExtracted(convID, now)
	return res, nil
}

// ---- plumbing / secret filtering ----

var internalMarkers = []string{
	"[memory_search]",
	"[conversation_search]",
	"[scheduler]",
	"[system_internal]",
	"<offload>",
	"<knowledge>",
}

var SecretPatterns = regexp.MustCompile(strings.Join([]string{
	`sk-[A-Za-z0-9_\-]{16,}`,
	`\bAKIA[0-9A-Z]{16}\b`,
	`\bghp_[A-Za-z0-9]{20,}\b`,
	`\bgsk_[A-Za-z0-9]{20,}\b`,
	`\bxox[baprs]-[A-Za-z0-9\-]{10,}`,
	`(?i)\b(api[_-]?key|access[_-]?token|secret[_-]?key|client[_-]?secret|password|passwd)\b\s*[:=]\s*["']?[^\s"',]{6,}`,
	`(?i)\bbearer\s+[A-Za-z0-9_\-\.=]{16,}`,
}, "|"))

func isInternal(role, content string) bool {
	if content == "" {
		return true
	}
	for _, m := range internalMarkers {
		if strings.Contains(content, m) {
			return true
		}
	}
	// tool plumbing carries our own scaffolding rather than user meaning
	if role == "tool" && !strings.Contains(content, "\n") && len([]rune(content)) < 200 {
		return true
	}
	return false
}

func sanitize(inputs []memory.ExtractInput) (kept []memory.ExtractInput, filtered, redacted int) {
	for _, in := range inputs {
		if isInternal(in.Role, in.Content) {
			filtered++
			continue
		}
		content := in.Content
		if SecretPatterns.MatchString(content) {
			content = SecretPatterns.ReplaceAllString(content, "[REDACTED]")
			redacted++
		}
		kept = append(kept, memory.ExtractInput{ID: in.ID, Role: in.Role, Content: strings.TrimSpace(content)})
	}
	return kept, filtered, redacted
}

// ---- model I/O ----

type item struct {
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Scene      string   `json:"scene,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
	Importance float64  `json:"importance,omitempty"`
}

const systemPrompt = `你是记忆提炼器。从对话中提取值得长期保留的原子记忆。

规则：
1. 只提取稳定、可复用的信息：用户身份/画像、偏好、硬性约束、长期事实、持续中的任务、兴趣领域。
2. 不要提取：一次性问答、寒暄、临时计算结果、重复内容、推理过程。
3. 绝不提取任何密钥、令牌、密码、API Key；若内容含此类信息直接丢弃。
4. 每条记忆一句话，独立可理解，不依赖上下文代词。
5. category 只能是 profile / preference / fact / task / interest 之一。
6. 最多 8 条；宁缺毋滥。

只输出如下 JSON，不要额外文字：
{"memories":[{"category":"preference","content":"...","scene":"...","tags":["..."],"confidence":0.8,"importance":0.7}]}`

func (e *Extractor) call(ctx context.Context, providerID, model string, inputs []memory.ExtractInput) ([]item, error) {
	rt, err := e.prov.GetRuntime(providerID)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, in := range inputs {
		who := in.Role
		switch who {
		case "user":
			who = "用户"
		case "assistant":
			who = "助手"
		case "system":
			who = "系统"
		}
		if in.Content == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", who, collapse(in.Content, 800))
	}
	body := b.String()
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	msgs := []provider.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "对话记录：\n\n" + body},
	}
	ctx2, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	raw, err := llm.ChatObject(ctx2, rt, model, msgs,
		provider.CallOptions{Temperature: 0.2, MaxTokens: 1200})
	if err != nil {
		return nil, err
	}
	return parseItems(raw)
}

// parseItems tolerates a model that wraps JSON in prose, a code fence, or
// appends commentary after the closing brace.
func parseItems(raw string) ([]item, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out struct {
		Memories []item `json:"memories"`
	}
	if err := llmjson.UnmarshalInto(raw, &out); err != nil {
		return nil, fmt.Errorf("parse extraction: %w", err)
	}
	return out.Memories, nil
}

func collapse(s string, n int) string {
	t := strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	r := []rune(t)
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// ---- helpers ----

var categories = map[string]string{
	"profile": memory.CatProfile, "preference": memory.CatPreference,
	"fact": memory.CatFact, "task": memory.CatTask, "interest": memory.CatInterest,
	"画像": memory.CatProfile, "偏好": memory.CatPreference,
	"事实": memory.CatFact, "任务": memory.CatTask, "兴趣": memory.CatInterest,
}

func normalizeCategory(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if v, ok := categories[c]; ok {
		return v
	}
	// tolerate "preference（偏好）" style output
	for k, v := range categories {
		if strings.Contains(c, k) {
			return v
		}
	}
	return memory.CatFact
}

func clamp01(v, def float64) float64 {
	if v <= 0 || v > 1 {
		return def
	}
	return v
}

func (e *Extractor) duplicate(content string) bool {
	key := strings.ToLower(strings.Join(strings.Fields(content), ""))
	for _, m := range e.mem.List(memory.ListFilter{Level: memory.LevelL1, Include: true}) {
		if strings.ToLower(strings.Join(strings.Fields(m.Content), "")) == key {
			return true
		}
	}
	return false
}
