// This file implements symbolic short-term memory (DESIGN §6.5 ① / §15.4
// memory/offload/).
//
// When a conversation outgrows the context budget, old turns and bulky tool
// logs are moved to disk and the context keeps only a compressed summary plus
// a node_id pointer, so the model can still ask for the original text via
// memory_search-style retrieval instead of carrying it forever.
//
// Two boundary rules matter here (and are covered by tests):
//   - eviction happens on whole-turn boundaries, so a tool result is never
//     separated from the assistant message that requested it (a dangling tool
//     result breaks the OpenAI wire format);
//   - a stray leading user message (the long-task goal) is pinned to the head
//     so a long run can never forget what it set out to do.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Offload kinds.
const (
	KindTurn    = "turn"
	KindToolLog = "tool_log"
)

// OffloadBlock is one evicted block under memory/offload/{convId}/.
type OffloadBlock struct {
	NodeID   string `json:"nodeId"`
	Kind     string `json:"kind"`
	Summary  string `json:"summary"`
	File     string `json:"file"`
	Chars    int    `json:"chars"`
	Messages int    `json:"messages"`
	// Hash dedupes repeats: compaction runs on every turn and would otherwise
	// re-evict (and re-write) the same old turns each time, growing offload
	// storage quadratically with conversation length.
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"createdAt"`
}

// CompactResult reports what Compact did.
type CompactResult struct {
	Keep    []provider.Message
	Evicted []OffloadBlock
}

func offloadDir(base, convID string) string {
	return filepath.Join(base, "offload", safeName(convID))
}

func safeName(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_", "..", "_")
	n := r.Replace(s)
	if n == "" {
		n = "unknown"
	}
	return n
}

// splitTurns groups messages into whole turns. A turn starts at a user message
// (or at the leading system message) and owns every assistant/tool message
// that follows it, which guarantees tool results stay paired with their caller.
func splitTurns(msgs []provider.Message) [][]provider.Message {
	var turns [][]provider.Message
	for _, m := range msgs {
		if m.Role == "user" || len(turns) == 0 {
			turns = append(turns, []provider.Message{m})
			continue
		}
		turns[len(turns)-1] = append(turns[len(turns)-1], m)
	}
	return turns
}

func (s *Service) estimate(text string) int {
	if s.retrieval != nil {
		return s.retrieval.Estimate(text)
	}
	return len([]rune(text))
}

// Compact keeps the goal plus as many recent whole turns as fit the budget and
// offloads the rest to disk. It returns the messages to send and the blocks
// that were written, so the caller can inject the pointer summary.
func (s *Service) Compact(convID string, msgs []provider.Message, budgetTokens int) (*CompactResult, error) {
	if budgetTokens <= 0 {
		budgetTokens = 4000
	}
	turns := splitTurns(msgs)
	if len(turns) == 0 {
		return &CompactResult{Keep: msgs}, nil
	}
	// --- head protection: system prompt + first user goal ---
	head := []provider.Message{}
	headCost := 0
	i := 0
	if turns[0][0].Role == "system" {
		head = append(head, turns[0]...)
		for _, m := range turns[0] {
			headCost += s.estimate(m.Content)
		}
		i = 1
	}
	if i < len(turns) && turns[i][0].Role == "user" {
		head = append(head, turns[i]...)
		for _, m := range turns[i] {
			headCost += s.estimate(m.Content)
		}
		i++
	}
	rest := turns[i:]
	if len(rest) == 0 {
		return &CompactResult{Keep: msgs}, nil
	}
	// --- keep the most recent whole turns that fit ---
	used := headCost
	keepIdx := make([]int, 0, len(rest))
	for j := len(rest) - 1; j >= 0; j-- {
		cost := 0
		for _, m := range rest[j] {
			cost += s.estimate(m.Content)
		}
		if used+cost > budgetTokens && len(keepIdx) > 0 {
			break
		}
		used += cost
		keepIdx = append(keepIdx, j)
	}
	sort.Ints(keepIdx)
	tailStart := len(rest)
	if len(keepIdx) > 0 {
		tailStart = keepIdx[0]
	}
	evictedTurns := rest[:tailStart]
	keep := append([]provider.Message{}, head...)
	for _, j := range keepIdx {
		keep = append(keep, rest[j]...)
	}
	if len(evictedTurns) == 0 {
		return &CompactResult{Keep: keep}, nil
	}
	// --- offload the evicted turns, one block per turn ---
	blocks := make([]OffloadBlock, 0, len(evictedTurns))
	for _, turn := range evictedTurns {
		kind := KindTurn
		hasTool := false
		var b strings.Builder
		for _, m := range turn {
			if m.Role == "tool" {
				hasTool = true
			}
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			fmt.Fprintf(&b, "### %s\n%s\n\n", m.Role, m.Content)
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "- tool_call %s(%s)\n", tc.Name, tc.Arguments)
			}
		}
		if hasTool {
			kind = KindToolLog
		}
		body := b.String()
		if strings.TrimSpace(body) == "" {
			continue
		}
		blk, err := s.Offload(convID, kind, body)
		if err != nil {
			return &CompactResult{Keep: keep}, err
		}
		blocks = append(blocks, *blk)
	}
	return &CompactResult{Keep: keep, Evicted: blocks}, nil
}

// Offload writes text to memory/offload/{convId}/{nodeId}.md and registers it
// in that conversation's index. It returns the block with its summary.
func (s *Service) Offload(convID, kind, text string) (*OffloadBlock, error) {
	if convID == "" {
		return nil, fmt.Errorf("conversation id required for offload")
	}
	if kind == "" {
		kind = KindTurn
	}
	dir := offloadDir(s.baseDir, convID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	hash := hashText(text)
	if existing := s.OffloadBlocks(convID); len(existing) > 0 {
		for _, b := range existing {
			if b.Hash == hash {
				same := b
				return &same, nil
			}
		}
	}
	nodeID := "node_" + util.NewID()[:10]
	file := nodeID + ".md"
	if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o644); err != nil {
		return nil, err
	}
	blk := OffloadBlock{
		NodeID:    nodeID,
		Kind:      kind,
		Summary:   summarize(text, 140),
		File:      file,
		Chars:     len([]rune(text)),
		Messages:  strings.Count(text, "### "),
		Hash:      hash,
		CreatedAt: time.Now().UnixMilli(),
	}
	blocks := append(s.OffloadBlocks(convID), blk)
	if err := s.writeOffloadIndex(convID, blocks); err != nil {
		return nil, err
	}
	return &blk, nil
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// OffloadBlocks lists the offloaded blocks of one conversation.
func (s *Service) OffloadBlocks(convID string) []OffloadBlock {
	b, err := os.ReadFile(filepath.Join(offloadDir(s.baseDir, convID), "index.json"))
	if err != nil {
		return nil
	}
	var out []OffloadBlock
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (s *Service) writeOffloadIndex(convID string, blocks []OffloadBlock) error {
	data, err := json.MarshalIndent(blocks, "", "  ")
	if err != nil {
		return err
	}
	dir := offloadDir(s.baseDir, convID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return s.writeFile(filepath.Join("offload", safeName(convID), "index.json"), data)
}

// OffloadGet retrieves the original text behind a node_id.
func (s *Service) OffloadGet(convID, nodeID string) (string, error) {
	for _, b := range s.OffloadBlocks(convID) {
		if b.NodeID != nodeID {
			continue
		}
		data, err := os.ReadFile(filepath.Join(offloadDir(s.baseDir, convID), b.File))
		if err != nil {
			return "", fmt.Errorf("offload %s missing: %w", nodeID, err)
		}
		return string(data), nil
	}
	return "", fmt.Errorf("offload %s not found", nodeID)
}

// OffloadPointer renders the in-context replacement for evicted content:
// a compressed summary plus the node_id needed to pull the original back.
func (s *Service) OffloadPointer(convID string) string {
	blocks := s.OffloadBlocks(convID)
	if len(blocks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<offload>\n")
	b.WriteString("以下较早的对话/工具日志已卸载到磁盘，上下文中只保留摘要与指针；\n")
	b.WriteString("需要原文时用 memory_search 或 /api/v1/memories/offload 按 node_id 取回。\n")
	for _, blk := range blocks {
		fmt.Fprintf(&b, "- %s [%s] %s\n", blk.NodeID, blk.Kind, blk.Summary)
	}
	b.WriteString("</offload>\n")
	return b.String()
}

// summarize builds a deterministic extractive summary (no LLM in the hot path:
// offload can run on every turn, and a network call there would be a trap).
func summarize(text string, n int) string {
	t := strings.TrimSpace(strings.Join(strings.Fields(text), " "))
	if t == "" {
		return "(空)"
	}
	runes := []rune(t)
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n]) + "…"
}
