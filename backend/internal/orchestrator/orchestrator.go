// Package orchestrator is the dual-mode main loop (DESIGN §5 orchestrator / §6.1).
// It loads the session + role, routes (auto), assembles the prompt, streams the
// model response, runs the tool loop, applies failover, and persists messages.
// Skill/knowledge/memory injection and the full task state machine are wired in
// later milestones; M2 delivers a working chat + tool-loop path.
package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/brief"
	"github.com/agenthub/agent-proxy/internal/config"
	"github.com/agenthub/agent-proxy/internal/control"
	"github.com/agenthub/agent-proxy/internal/conversation"
	"github.com/agenthub/agent-proxy/internal/envprobe"
	"github.com/agenthub/agent-proxy/internal/knowledge"
	"github.com/agenthub/agent-proxy/internal/memory"
	"github.com/agenthub/agent-proxy/internal/methodology"
	"github.com/agenthub/agent-proxy/internal/module"
	"github.com/agenthub/agent-proxy/internal/orchestrator/assemble"
	"github.com/agenthub/agent-proxy/internal/orchestrator/failover"
	"github.com/agenthub/agent-proxy/internal/orchestrator/loop"
	"github.com/agenthub/agent-proxy/internal/orchestrator/loopguard"
	"github.com/agenthub/agent-proxy/internal/orchestrator/router"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/role"
	"github.com/agenthub/agent-proxy/internal/skill"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
	"github.com/agenthub/agent-proxy/internal/util"
)

// MemoryService is the optional recall backend (nil before M3b).
// Recall is query-driven; Guidance returns the always-on L3 persona + L2 scene
// block that steers every turn without a retrieval round-trip.
type MemoryService interface {
	Retrieve(ctx context.Context, query string, topK int) []memory.MemoryResult
	Guidance(maxChars int) string
	// Compact evicts old whole turns to disk when the history outgrows the
	// budget, returning the messages that should stay in context.
	Compact(convID string, msgs []provider.Message, budgetTokens int) (*memory.CompactResult, error)
	// OffloadPointer renders the node_id index for anything already evicted.
	OffloadPointer(convID string) string
}

// MemoryExtractor distills L1 atomic memories once a turn has settled (M3b).
// Declared here rather than importing memory/extract so the orchestrator stays
// free of the LLM plumbing that extraction pulls in.
type MemoryExtractor interface {
	Extract(ctx context.Context, convID, providerID, model string,
		inputs []memory.ExtractInput, force bool) (*memory.ExtractResult, error)
}

// KnowledgeService is the optional RAG backend (nil until M3a is wired).
type KnowledgeService interface {
	SearchCollections(ids []string, query string, topK int) []knowledge.Citation
}

// SkillService resolves the skills bound to a role (M4).
type SkillService interface {
	GetMany(ids []string) []*skill.Skill
}

// MethodologyService activates methodology entries for a query (M4).
type MethodologyService interface {
	Match(query string, pinned []string) []methodology.MatchHit
}

// Event is one SSE event emitted on the chat stream (DESIGN §15.1).
type Event struct {
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`
	RequestID string                 `json:"requestId"`
}

// ChatInput is the request to Chat.
type ChatInput struct {
	ConversationID string `json:"conversationId,omitempty"`
	RoleID         string `json:"roleId,omitempty"`
	Message        string `json:"message"`
	Mode           string `json:"mode,omitempty"`
	Model          string `json:"model,omitempty"`
	// Temperature/MaxTokens let a caller (notably the OpenAI-compatible
	// endpoint) override the role defaults for a single request. Zero means
	// "use the role's value".
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"maxTokens,omitempty"`
	// Reasoning overrides the thinking effort (low|medium|high) for models that
	// expose it (OpenAI o-series). Empty means the model's default.
	Reasoning string `json:"reasoning,omitempty"`
}

// Orchestrator wires the main loop together.
type Orchestrator struct {
	Providers     *provider.Service
	Conversations *conversation.Service
	Roles         *role.Service
	Tools         *registry.Registry
	Config        *config.Config
	Audit         *audit.Audit
	Memory        MemoryService      // optional
	Knowledge     KnowledgeService   // optional (M3a)
	Extractor     MemoryExtractor    // optional (M3b): L1 distillation after a turn
	Skills        SkillService       // optional (M4): resolve role-bound skills
	Methodologies MethodologyService // optional (M4): keyword-activated entries
	// Modules is the switch registry (DESIGN §5). A tool whose module is off
	// is never advertised to the model, and a disabled memory/rag module
	// suppresses their prompt blocks.
	Modules *module.Registry
	// ControlGate is the extra check applied to every control_* tool call. It
	// is separate from the permission tier on purpose: desktop operations never
	// ride on the general tier (see permissionGate), so this is the one place
	// that can refuse the whole family.
	ControlGate loop.Gate
	// OnModulesChanged is invoked after a switch flips so main can persist it.
	OnModulesChanged func()
}

// ModuleGate is the narrow view of the module registry the orchestrator needs;
// it keeps the dependency optional in tests.
type ModuleGate interface {
	IsEnabled(id string) bool
}

// toolSpecs returns the tool schemas the model may call, honouring module
// switches. A disabled tool must be invisible to the model, otherwise it would
// still emit calls the loop cannot satisfy.
func (o *Orchestrator) toolSpecs() []provider.ToolSpec {
	specs := o.Tools.Schemas()
	if o.Modules == nil {
		return specs
	}
	out := make([]provider.ToolSpec, 0, len(specs))
	for _, s := range specs {
		if o.Modules.IsEnabled("tool." + s.Name) {
			out = append(out, s)
		}
	}
	return out
}

func (o *Orchestrator) moduleEnabled(id string) bool {
	if o.Modules == nil {
		return true
	}
	return o.Modules.IsEnabled(id)
}

// New constructs an Orchestrator.
func New(providers *provider.Service, conversations *conversation.Service, roles *role.Service, tools *registry.Registry, cfg *config.Config, aud *audit.Audit) *Orchestrator {
	return &Orchestrator{
		Providers:     providers,
		Conversations: conversations,
		Roles:         roles,
		Tools:         tools,
		Config:        cfg,
		Audit:         aud,
	}
}

// Chat streams a conversation turn as SSE events.
//
// The returned channel is closed when the turn ends. If the caller stops reading
// (browser tab closed, client timeout), ctx is cancelled and emit drops
// subsequent events instead of blocking forever — otherwise the worker goroutine
// (and the provider stream behind it) would leak on every abandoned turn.
func (o *Orchestrator) Chat(ctx context.Context, in ChatInput) (<-chan Event, error) {
	ch := make(chan Event)
	go func() {
		defer close(ch)
		o.run(ctx, ch, in)
	}()
	return ch, nil
}

// emit sends one event unless the consumer is gone. It reports whether the
// event was delivered so callers can stop a long-running turn early.
func (o *Orchestrator) emit(ctx context.Context, ch chan<- Event, rid string, t string, d map[string]interface{}) bool {
	if d == nil {
		d = map[string]interface{}{}
	}
	d["_t"] = t
	select {
	case ch <- Event{Type: t, Data: d, RequestID: rid}:
		return true
	case <-ctx.Done():
		return false
	}
}

// resolveRoleID picks the role for one turn, in precedence order:
//
//  1. an explicit role in the request (a deliberate per-turn override),
//  2. the conversation's own roleId — a conversation created against a role
//     must keep using it,
//  3. the global default role,
//  4. the built-in "general".
//
// Step 2 is the one that is easy to omit and impossible for the user to
// diagnose: without it every existing conversation answers with the default
// role's provider, model, tools, skills and methodology, with no error shown.
func (o *Orchestrator) resolveRoleID(in ChatInput, conv *conversation.Conversation) string {
	if in.RoleID != "" {
		return in.RoleID
	}
	if conv != nil && conv.RoleId != "" {
		return conv.RoleId
	}
	if d := o.Config.GetSettings().DefaultRoleId; d != "" {
		return d
	}
	return "general"
}

func (o *Orchestrator) run(ctx context.Context, ch chan Event, in ChatInput) {
	rid := util.NewID()
	emit := func(t string, d map[string]interface{}) bool { return o.emit(ctx, ch, rid, t, d) }

	// Resolve the conversation first: its roleId decides which role runs this
	// turn, and looking it up twice would be a wasted read on the hot path.
	var conv *conversation.Conversation
	if in.ConversationID != "" {
		c, cerr := o.Conversations.Get(in.ConversationID)
		if cerr != nil {
			emit("error", map[string]interface{}{"code": "conv_not_found", "message": cerr.Error()})
			return
		}
		conv = c
	}
	role, err := o.Roles.Get(o.resolveRoleID(in, conv))
	if err != nil {
		emit("error", map[string]interface{}{"code": "role_not_found", "message": err.Error()})
		return
	}

	if conv == nil {
		mode := conversation.Mode(in.Mode)
		if mode == "" {
			mode = conversation.Mode(role.DefaultMode)
		}
		if mode == "" {
			mode = conversation.ModeAuto
		}
		conv, err = o.Conversations.Create(o.resolveRoleID(in, nil), mode)
		if err != nil {
			emit("error", map[string]interface{}{"code": "conv_create_failed", "message": err.Error()})
			return
		}
	}

	providerID, model, err := o.resolveTarget(role, conv, in)
	if err != nil {
		emit("error", map[string]interface{}{"code": "no_target", "message": err.Error()})
		return
	}

	// Auto routing (DESIGN §6.8). Runs after target resolution so the model
	// strategy has a runtime to classify with. The role's autoRoute block
	// overrides the global config; when routing is off, the conversation mode
	// stands as-is.
	mode := conv.Mode
	if mode == conversation.ModeAuto || o.autoRouteEnabled(role) {
		opts := o.routeOptions(role)
		var cls router.RuntimeClassifier
		if strings.EqualFold(opts.Strategy, "model") {
			if rt, rerr := o.Providers.GetRuntime(providerID); rerr == nil {
				cls = rt
			}
		}
		d := router.ClassifyWith(ctx, cls, in.Message, o.toolSpecs(), opts)
		emit("route", map[string]interface{}{
			"mode": d.Mode, "confidence": d.Confidence, "reason": d.Reason, "source": d.Source,
		})
		if conversation.Mode(d.Mode) != conv.Mode {
			mode = conversation.Mode(d.Mode)
			_ = o.Conversations.SetMode(conv.ID, mode)
		}
	}
	if mode == conversation.ModeTask {
		emit("plan", map[string]interface{}{"goal": in.Message, "steps": []map[string]interface{}{{"id": "s1", "title": in.Message, "dependsOn": []string{}}}})
	}

	// persist user message
	if err := o.Conversations.AppendMessage(conv.ID, conversation.Message{Role: "user", Content: in.Message, Status: "sent"}); err != nil {
		emit("error", map[string]interface{}{"code": "append_failed", "message": err.Error()})
		return
	}

	// knowledge recall (M3a): role-bound collections, BM25+vector fused by RRF
	var cites []knowledge.Citation
	if o.Knowledge != nil && o.moduleEnabled("rag.knowledge") && len(role.KnowledgeCollectionIds) > 0 {
		topK := role.RetrievalTopK
		if topK <= 0 {
			topK = 5
		}
		cites = o.Knowledge.SearchCollections(role.KnowledgeCollectionIds, in.Message, topK)
	}

	// build initial messages (knowledge is injected into the system prompt)
	msgs := o.buildMessages(conv, role, cites, in.Message)
	msgs = append(msgs, provider.Message{Role: "user", Content: in.Message})

	// context event: injected knowledge + recalled memory (DESIGN §15.1)
	{
		kn := make([]map[string]interface{}, 0, len(cites))
		for _, c := range cites {
			kn = append(kn, map[string]interface{}{
				"collection": c.Collection, "doc": c.Doc, "docName": c.DocName,
				"chunk": c.Chunk, "text": c.Text, "score": c.Score, "trust": c.Trust,
			})
		}
		mem := []map[string]interface{}{}
		if o.Memory != nil && o.moduleEnabled("memory.engine") {
			for _, r := range o.Memory.Retrieve(ctx, in.Message, role.RetrievalTopK) {
				mem = append(mem, map[string]interface{}{"id": r.ID, "level": r.Level, "content": r.Content, "score": r.Score})
			}
		}
		if len(kn) > 0 || len(mem) > 0 {
			emit("context", map[string]interface{}{"knowledge": kn, "memory": mem})
		}
	}

	guard := &loopguard.State{}
	maxSteps := role.MaxSteps
	if maxSteps == 0 {
		maxSteps = 30
	}
	if mode == conversation.ModeTask {
		_ = o.Conversations.UpdateMeta(conv.ID, conversation.MetaPatch{})
	}

	assistantID := util.NewID()
	chain := failover.NewChain()
	for step := 0; step < maxSteps; step++ {
		guard.Steps++
		if stop, why := loopguard.Check(guard); stop {
			emit("error", map[string]interface{}{"code": "loopguard", "message": why})
			return
		}
		rt, err := o.Providers.GetRuntime(providerID)
		if err != nil {
			emit("error", map[string]interface{}{"code": "provider_unavailable", "message": err.Error()})
			return
		}
		opts := provider.CallOptions{
			Model:           model,
			Temperature:     o.temperature(role, in),
			MaxTokens:       o.maxTokens(role, in),
			TopP:            role.TopP,
			Tools:           o.toolSpecs(),
			Stream:          true,
			ReasoningEffort: in.Reasoning,
		}
		stream, err := rt.ChatStream(ctx, msgs, opts)
		if err != nil {
			fb := o.selectFallback(role, chain, providerID, model)
			if fb != nil {
				fromP, fromM := providerID, model
				providerID, model = fb.ProviderId, fb.Model
				emit("fallback", map[string]interface{}{
					"from":   map[string]string{"providerId": fromP, "model": fromM},
					"to":     map[string]string{"providerId": providerID, "model": model},
					"reason": fb.Reason, "kind": "roleFallback",
				})
				continue
			}
			emit("error", map[string]interface{}{"code": "stream_failed", "message": err.Error()})
			return
		}

		var content, reasoning string
		var toolCalls []provider.ToolCall
		var usage provider.Usage
		var gotModel string
		done := false
		// A provider can fail after the stream opened (every key benched, a
		// mid-stream network drop). That failure arrives as chunk.Err and used
		// to bypass the fallback chain entirely, so a role with fallbacks still
		// died with "all keys in cooldown".
		switched := false
		for chunk := range stream {
			// The consumer may have gone away mid-stream; stop consuming so the
			// provider goroutine can exit instead of blocking on a full pipe.
			if ctx.Err() != nil {
				return
			}
			if chunk.Err != nil {
				fb := o.selectFallback(role, chain, providerID, model)
				if fb != nil {
					fromP, fromM := providerID, model
					providerID, model = fb.ProviderId, fb.Model
					emit("fallback", map[string]interface{}{
						"from":   map[string]string{"providerId": fromP, "model": fromM},
						"to":     map[string]string{"providerId": providerID, "model": model},
						"reason": fb.Reason, "kind": "roleFallback",
						"cause": chunk.Err.Error(),
					})
					switched = true
					break
				}
				emit("error", map[string]interface{}{"code": "stream_error", "message": chunk.Err.Error()})
				return
			}
			if chunk.Delta != "" {
				content += chunk.Delta
				emit("delta", map[string]interface{}{"messageId": assistantID, "delta": chunk.Delta})
			}
			if chunk.Reasoning != "" {
				reasoning += chunk.Reasoning
				emit("reasoning", map[string]interface{}{"messageId": assistantID, "delta": chunk.Reasoning})
			}
			if chunk.Done {
				toolCalls = chunk.ToolCalls
				usage = chunk.Usage
				gotModel = chunk.Model
				done = true
			}
		}
		if switched {
			// Retry on the fallback target; msgs/assistantID are untouched, so
			// the model re-answers the same request there.
			continue
		}
		if !done {
			emit("error", map[string]interface{}{"code": "incomplete", "message": "stream ended without done"})
			return
		}
		if gotModel == "" {
			gotModel = model
		}

		// persist assistant message (citations travel with the turn)
		_ = o.Conversations.AppendMessage(conv.ID, conversation.Message{
			ID: assistantID, Role: "assistant", Content: content, Reasoning: reasoning,
			ToolCalls: toolCalls, ProviderId: providerID, Model: gotModel,
			Usage: usage, FinishReason: "stop", Status: "done",
			Citations: toConvCitations(cites),
		})

		if len(toolCalls) == 0 {
			emit("done", map[string]interface{}{
				"messageId": assistantID,
				"usage":     map[string]interface{}{"promptTokens": usage.PromptTokens, "completionTokens": usage.CompletionTokens, "totalTokens": usage.TotalTokens, "cost": usage.Cost},
				"model":     gotModel, "providerId": providerID,
				"finishReason": "stop", "citations": cites,
			})
			o.distillMemories(conv.ID, providerID, gotModel)
			return
		}

		// tool loop
		results := loop.Execute(ctx, o.Tools, toolCalls, o.permissionGate(role, conv))
		var toolMsgs []provider.Message
		for _, tc := range toolCalls {
			emit("tool_call", map[string]interface{}{"messageId": assistantID, "toolCall": map[string]interface{}{"id": tc.ID, "name": tc.Name, "argumentsDelta": tc.Arguments}})
		}
		assistantMsg := provider.Message{Role: "assistant", Content: content, ToolCalls: toolCalls}
		toolMsgs = append(toolMsgs, assistantMsg)
		for _, r := range results {
			emit("tool_result", map[string]interface{}{"messageId": assistantID, "toolCallId": r.ID, "name": r.Name, "result": r.Result, "truncated": false})
			toolMsgs = append(toolMsgs, provider.Message{Role: "tool", ToolCallID: r.ID, Content: r.Result})
			_ = o.Conversations.AppendMessage(conv.ID, conversation.Message{Role: "tool", ToolCallId: r.ID, Content: r.Result, Status: "done"})
		}
		msgs = append(msgs, toolMsgs...)
		assistantID = util.NewID() // next assistant turn
	}

	emit("done", map[string]interface{}{"messageId": assistantID, "model": model, "providerId": providerID, "finishReason": "length", "citations": cites})
	o.distillMemories(conv.ID, providerID, model)
}

// distillMemories runs L1 extraction after a turn settles. It runs in the
// background: extraction costs an extra LLM call, and a slow or failed one
// must never hold up the reply the user is waiting on.
// resolveSkills expands the role's skill ids into instructions. Only enabled
// skills are injected, and unknown ids are skipped so deleting a skill can
// never break a role that still references it.
func (o *Orchestrator) resolveSkills(r *role.Role) []*skill.Skill {
	if o.Skills == nil || r == nil || len(r.Skills) == 0 {
		return nil
	}
	all := o.Skills.GetMany(r.Skills)
	out := make([]*skill.Skill, 0, len(all))
	for _, sk := range all {
		if sk != nil && sk.Enabled {
			out = append(out, sk)
		}
	}
	return out
}

// resolveMethodologies activates methodology entries for this query: standing
// principles plus keyword hits, with any ids the role pins explicitly.
func (o *Orchestrator) resolveMethodologies(r *role.Role, query string) []methodology.MatchHit {
	if o.Methodologies == nil {
		return nil
	}
	var pinned []string
	if r != nil {
		pinned = r.MethodologyIds
	}
	hits := o.Methodologies.Match(query, pinned)
	// Cap the block so a broad query cannot bloat the system prompt.
	if len(hits) > 12 {
		hits = hits[:12]
	}
	return hits
}

// distillMemories runs L1 extraction after a turn settles. It runs in the
// background: extraction costs an extra LLM call, and a slow or failed one
// must never hold up the reply the user is waiting on.
func (o *Orchestrator) distillMemories(convID, providerID, model string) {
	if o.Extractor == nil || !o.Config.GetSettings().MemoryAutoExtract || !o.moduleEnabled("memory.engine") {
		return
	}
	go func() {
		msgs, err := o.Conversations.Messages(convID)
		if err != nil {
			return
		}
		inputs := make([]memory.ExtractInput, 0, len(msgs))
		for _, m := range msgs {
			inputs = append(inputs, memory.ExtractInput{ID: m.ID, Role: m.Role, Content: m.Content})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		if _, err := o.Extractor.Extract(ctx, convID, providerID, model, inputs, false); err != nil && o.Audit != nil {
			o.Audit.Log("memory_extract_failed", "", map[string]string{"conversationId": convID, "error": err.Error()})
		}
	}()
}

// toConvCitations maps knowledge citations onto the persisted message shape.
func toConvCitations(cites []knowledge.Citation) []conversation.Citation {
	if len(cites) == 0 {
		return nil
	}
	out := make([]conversation.Citation, 0, len(cites))
	for _, c := range cites {
		out = append(out, conversation.Citation{Collection: c.Collection, Doc: c.Doc, Chunk: fmt.Sprint(c.Chunk), Score: c.Score})
	}
	return out
}

// BuildMessages exposes the assembled turn messages (system prompt + history)
// for inspection. The /debug/system-prompt endpoint uses it so an operator can
// see exactly what the model receives — the fastest way to diagnose "why did
// it not use my skill/persona/vars".
func (o *Orchestrator) BuildMessages(convID string, query string) ([]provider.Message, error) {
	conv, err := o.Conversations.Get(convID)
	if err != nil {
		return nil, err
	}
	rl, err := o.Roles.Get(conv.RoleId)
	if err != nil {
		return nil, err
	}
	var cites []knowledge.Citation
	if o.Knowledge != nil {
		cites = o.Knowledge.SearchCollections(rl.KnowledgeCollectionIds, query, rl.RetrievalTopK)
	}
	return o.buildMessages(conv, rl, cites, query), nil
}

func (o *Orchestrator) buildMessages(conv *conversation.Conversation, role *role.Role, cites []knowledge.Citation, query string) []provider.Message {
	var msgs []provider.Message
	var guidance string
	// L3 persona + L2 scenes steer every turn; the query-driven recall below
	// only adds the specific facts this message actually needs.
	if o.Memory != nil && o.moduleEnabled("memory.engine") {
		guidance = o.Memory.Guidance(1500)
	}
	sys := assemble.Assemble(assemble.Context{
		Role:           role,
		Citations:      cites,
		Skills:         o.resolveSkills(role),
		Methodologies:  o.resolveMethodologies(role, query),
		MemoryGuidance: guidance,
		Vars:           conv.Vars,
	})
	if sys != "" {
		msgs = append(msgs, provider.Message{Role: "system", Content: sys})
	}
	hist, _ := o.Conversations.Messages(conv.ID)
	pending := make([]provider.Message, 0, len(hist))
	for _, m := range hist {
		if m.Role == "system" {
			continue
		}
		pending = append(pending, provider.Message{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallId})
	}
	// Symbolic short-term memory: once the history outgrows the role's soft
	// limit, evict old whole turns to disk and keep only a pointer to them.
	if o.Memory != nil && o.moduleEnabled("memory.engine") {
		if res, err := o.Memory.Compact(conv.ID, pending, contextBudget(role)); err == nil && res != nil && len(res.Keep) > 0 {
			pending = res.Keep
		}
		if ptr := o.Memory.OffloadPointer(conv.ID); ptr != "" {
			msgs = append(msgs, provider.Message{Role: "system", Content: ptr})
		}
	}
	return append(msgs, pending...)
}

// contextBudget is the token allowance for conversation history. The role's
// soft limit wins; the default leaves plenty of headroom under typical 32k
// context windows while still keeping a useful amount of history live.
func contextBudget(r *role.Role) int {
	if r != nil && r.Budget.TokensSoftLimit != nil && *r.Budget.TokensSoftLimit > 0 {
		return *r.Budget.TokensSoftLimit
	}
	return 12000
}

// temperature/maxTokens resolve request-level overrides against the role.
// A caller that explicitly sends 0 (a legitimate temperature) still wins, so
// the check is "was the pointer supplied", not "is it non-zero".
func (o *Orchestrator) temperature(r *role.Role, in ChatInput) float64 {
	if in.Temperature != nil {
		return *in.Temperature
	}
	if r != nil {
		return r.Temperature
	}
	return 0.7
}

func (o *Orchestrator) maxTokens(r *role.Role, in ChatInput) int {
	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		return *in.MaxTokens
	}
	if r != nil && r.MaxTokens > 0 {
		return r.MaxTokens
	}
	return 4096
}

// Permission levels (DESIGN §6.7): ask = only the read-only tools run on their
// own, auto/full = every tool of the agent's own runs.
//
// Software control sits outside this scale: it answers to the module switch and
// the deny list, not to how far the agent is trusted with its own tools. See
// permissionGateWith.
const (
	PermAsk  = "ask"
	PermAuto = "auto"
	PermFull = "full"
)

// readOnlyTools are safe to run without asking: they read state and return
// text, never mutate the machine or the user's data.
var readOnlyTools = map[string]bool{
	"calculator": true, "memory_search": true, "conversation_search": true,
	"knowledge_search": true, "note": true, "timer": true,
	// env_probe only reads directory names and PATH (DESIGN §9.3). It is the
	// step that has to work at every tier: "what can this machine do the job
	// with" is the question the very first sentence of a task turns on, and a
	// probe that asked for permission would make the default tier unusable.
	envprobe.ToolName: true,
	// The brief store writes Markdown into the product's own data directory —
	// the same place conversation turns are written, and not the user's files.
	// Gating it would mean the agent cannot take notes during the one situation
	// the notebook exists for: while waiting for something to be installed.
	brief.ToolCreate: true, brief.ToolAppend: true, brief.ToolRead: true, brief.ToolList: true,
}

// controlToolPrefix matches every software-control tool (DESIGN §9.1). Kept in
// step with control.ToolPrefix by the test in this package.
const controlToolPrefix = control.ToolPrefix

// permissionGate returns the gate for a turn of this orchestrator.
//
// Use this rather than the package-level helper: it carries the wired
// ControlGate, and the package-level one only exists for tests that exercise
// the tier logic on its own.
func (o *Orchestrator) permissionGate(r *role.Role, conv *conversation.Conversation) loop.Gate {
	return permissionGateWith(r, conv, o.ControlGate)
}

// permissionGate returns the gate for this turn, or nil when everything may run.
//
// Without this the permission field was decorative: tools executed regardless of
// what the role/conversation asked for. Note that "ask" still lets read-only
// tools through — a level that blocked the calculator outright would make the
// product unusable out of the box.
func permissionGate(r *role.Role, conv *conversation.Conversation) loop.Gate {
	return permissionGateWith(r, conv, nil)
}

// permissionGateWith builds the gate, additionally routing every control_*
// tool through controlGate.
//
// Software control does not ride on the general permission tier, and that cuts
// both ways. The tier answers one question: how much does the user trust the
// agent with *the agent's own* tools — files, shell, network. Driving the
// desktop is a different question, and it is not settled per call. The user
// settles it once, by switching the module on: "help me build a 2.5D game" is
// the task, and opening Blender is part of carrying it out. Asking again before
// each click would be a sign-off queue rather than a capability — and since a
// fresh install runs at "ask", gating the family there meant the product's
// first capability did nothing out of the box.
//
// So the rules are:
//   - every control_* call goes through controlGate whatever the tier: the
//     module switch and the deny list (both re-checked inside Run) are the
//     boundary, and they are the user's own words, not a review of the agent's
//     request;
//   - nothing else about control_* depends on the tier. Reading window titles
//     is the discovery step of that same task, so it follows the same switch.
func permissionGateWith(r *role.Role, conv *conversation.Conversation, controlGate loop.Gate) loop.Gate {
	level := permLevel(r, conv)
	return func(name string, args map[string]interface{}) error {
		if strings.HasPrefix(name, controlToolPrefix) {
			return controlGateCheck(controlGate, name, args)
		}
		if level == PermFull || level == PermAuto {
			return nil
		}
		if readOnlyTools[name] {
			return nil
		}
		return fmt.Errorf("工具 %s 需要授权（当前权限：ask，仅只读工具自动放行）", name)
	}
}

// controlGateCheck applies the only two things that bound software control: the
// module switch (the user's own opt-in) and the deny list. Neither depends on
// the permission tier — see permissionGateWith for why.
func controlGateCheck(controlGate loop.Gate, name string, args map[string]interface{}) error {
	if controlGate == nil {
		// Not wired means "cannot be verified here", not "no gate needed".
		// Refusing is the fail-closed reading and is what the module switch's
		// own default-off posture assumes will happen.
		return fmt.Errorf("工具 %s 未启用：软件控制未接线（请在「软件控制」页打开开关后重启）", name)
	}
	return controlGate(name, args)
}

// permLevel resolves the effective permission tier for a turn: an explicit
// conversation setting wins over the role, which wins over the strict default.
func permLevel(r *role.Role, conv *conversation.Conversation) string {
	level := PermAsk
	if r != nil && r.Permission != "" {
		level = r.Permission
	}
	if conv != nil && conv.Permission != "" {
		level = conv.Permission
	}
	return level
}

// selectFallback honours the global failover switch: when disaster recovery is
// off the turn fails fast instead of silently hopping to another vendor.
func (o *Orchestrator) selectFallback(r *role.Role, chain *failover.Chain, providerID, model string) *role.Fallback {
	if o.Config != nil && !o.Config.GetSettings().Failover.Enabled {
		return nil
	}
	// Skip fallbacks that point at a switched-off provider: selecting one would
	// consume a chain slot and fail on a target that was never callable.
	return failover.SelectFallbackFiltered(r, chain, providerID, model, o.Providers.IsEnabled)
}

// autoRouteEnabled reports whether this role routes automatically. The role's
// autoRoute block can force routing on for itself; otherwise the global setting
// applies (DESIGN §6.8 role.autoRoute overrides the global config).
func (o *Orchestrator) autoRouteEnabled(r *role.Role) bool {
	if r != nil && r.AutoRoute.Enabled {
		return true
	}
	return o.Config != nil && o.Config.GetSettings().AutoRoute.Enabled
}

// routeOptions merges the role's overrides over the global auto-routing config.
func (o *Orchestrator) routeOptions(r *role.Role) router.Options {
	opts := router.Options{}
	if o.Config != nil {
		s := o.Config.GetSettings().AutoRoute
		opts.Strategy = s.Strategy
		opts.Threshold = s.Threshold
	}
	if r != nil {
		if r.AutoRoute.Strategy != "" {
			opts.Strategy = r.AutoRoute.Strategy
		}
		if r.AutoRoute.Threshold > 0 {
			opts.Threshold = r.AutoRoute.Threshold
		}
	}
	return opts
}

func (o *Orchestrator) resolveTarget(role *role.Role, conv *conversation.Conversation, in ChatInput) (string, string, error) {
	providerID := ""
	model := in.Model
	if model != "" && strings.Contains(model, "/") {
		parts := strings.SplitN(model, "/", 2)
		providerID, model = parts[0], parts[1]
	}
	if providerID == "" {
		providerID = role.ProviderId
	}
	if providerID == "" {
		providerID = conv.DefaultProviderId
	}
	if providerID == "" {
		providerID = o.Config.GetSettings().DefaultProviderId
	}
	if model == "" {
		model = role.Model
	}
	// A role is a prompt, not a model binding. When neither the role nor the
	// request names a model, fall back to the global default so a configured
	// service provider is always usable.
	if model == "" {
		model = o.Config.GetSettings().DefaultModel
	}
	// The global default model can point at a vendor the resolved provider does
	// not actually offer (e.g. defaultProvider=豆包 but defaultModel=
	// deepseek-flash). When that happens, fall back to the provider's first
	// usable model so provider/model can never be a cross-vendor pair that only
	// fails at call time with a confusing error.
	if providerID != "" && o.Providers != nil {
		if p, err := o.Providers.Get(providerID); err == nil && p != nil {
			if !providerOffers(p, model) {
				if m := firstUsableModel(p); m != "" {
					model = m
				}
			}
		}
	}
	if providerID == "" || model == "" {
		return "", "", fmt.Errorf("未配置 provider/model：请在角色或请求中指定 model（格式 provider/model）")
	}
	// A switched-off provider must be refused here rather than at call time:
	// the user needs to be told which switch to flip, and "connection failed"
	// would send them debugging the wrong thing.
	if o.Providers != nil && !o.Providers.IsEnabled(providerID) {
		return "", "", fmt.Errorf("服务商「%s」已停用：请在「服务商」页重新启用，或改选其它服务商", providerID)
	}
	return providerID, model, nil
}

// providerOffers reports whether the provider advertises the model and the
// user has not disabled it. An unlisted model means "not offered by this
// vendor" even if the string is the global default.
func providerOffers(p *provider.Provider, model string) bool {
	if p == nil || model == "" {
		return false
	}
	for _, m := range p.Models {
		if m.ID != model {
			continue
		}
		if pref, ok := p.ModelPrefs[model]; ok && pref.Disabled {
			return false
		}
		return true
	}
	return false
}

// firstUsableModel returns the provider's first advertised model that the user
// has not disabled, or "" when the catalogue is empty.
func firstUsableModel(p *provider.Provider) string {
	if p == nil {
		return ""
	}
	for _, m := range p.Models {
		if pref, ok := p.ModelPrefs[m.ID]; ok && pref.Disabled {
			continue
		}
		return m.ID
	}
	return ""
}
