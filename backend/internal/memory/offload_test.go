package memory

import (
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	return New(storage.NewJSON(root+"/memories"), storage.NewJSON(root+"/index"), root+"/memory",
		retrieval.New(), nil, false)
}

// TestSplitTurnsKeepsToolResultsPaired guards the wire-format invariant: a
// tool message must never be separated from the assistant turn that requested
// it, or the provider rejects the request.
func TestSplitTurnsKeepsToolResultsPaired(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "calc"}}},
		{Role: "tool", ToolCallID: "c1", Content: "60"},
		{Role: "assistant", Content: "答案是 60"},
		{Role: "user", Content: "u2"},
	}
	turns := splitTurns(msgs)
	// [system] | [u1, assistant(call), tool(c1), assistant] | [u2]
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d: %#v", len(turns), turns)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant"}
	if len(turns[1]) != len(wantRoles) {
		t.Fatalf("turn 2 should hold user+call+result+answer, got %d: %#v", len(turns[1]), turns[1])
	}
	for i, r := range wantRoles {
		if turns[1][i].Role != r {
			t.Fatalf("turn 2[%d] = %s, want %s (%#v)", i, turns[1][i].Role, r, turns[1])
		}
	}
	// the decisive check: no turn may begin with an orphaned tool result
	for _, turn := range turns {
		if turn[0].Role == "tool" {
			t.Fatalf("a turn starts with a tool result, which would break the wire format: %#v", turn)
		}
	}
}

// TestCompactProtectsGoal ensures a long run never forgets what it set out to
// do: the leading user goal stays in context even when everything else is gone.
func TestCompactProtectsGoal(t *testing.T) {
	s := newTestService(t)
	msgs := []provider.Message{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "目标：把 AgentHub 的记忆模块做完"},
	}
	// 20 filler turns, each long enough to blow any reasonable budget
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "问题" + strings.Repeat("长", 60)},
			provider.Message{Role: "assistant", Content: strings.Repeat("答", 120)},
		)
	}
	res, err := s.Compact("conv-1", msgs, 400)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(res.Evicted) == 0 {
		t.Fatal("expected some turns to be evicted")
	}
	var hasSystem, hasGoal bool
	for _, m := range res.Keep {
		if m.Role == "system" {
			hasSystem = true
		}
		if strings.Contains(m.Content, "目标：") {
			hasGoal = true
		}
	}
	if !hasSystem {
		t.Error("system prompt was dropped")
	}
	if !hasGoal {
		t.Error("leading user goal was evicted — a long task would lose its objective")
	}
}

// TestOffloadRoundTrip verifies evicted text is recoverable by node_id.
func TestOffloadRoundTrip(t *testing.T) {
	s := newTestService(t)
	original := "### user\n原始问题\n\n### assistant\n很长的回答……\n"
	blk, err := s.Offload("conv-2", KindTurn, original)
	if err != nil {
		t.Fatalf("offload: %v", err)
	}
	if blk.NodeID == "" || blk.Summary == "" {
		t.Fatalf("incomplete block: %+v", blk)
	}
	got, err := s.OffloadGet("conv-2", blk.NodeID)
	if err != nil {
		t.Fatalf("offload get: %v", err)
	}
	if got != original {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, original)
	}
	ptr := s.OffloadPointer("conv-2")
	if !strings.Contains(ptr, blk.NodeID) {
		t.Errorf("pointer block missing node id: %s", ptr)
	}
	if _, err := s.OffloadGet("conv-2", "node_missing"); err == nil {
		t.Error("expected an error for an unknown node id")
	}
}

// TestOffloadDedupe guards against quadratic growth: compaction runs on every
// turn, so re-evicting an already-evicted turn must reuse the existing node
// instead of writing another copy.
func TestOffloadDedupe(t *testing.T) {
	s := newTestService(t)
	text := "### user\n重复出现的旧轮次内容\n\n### assistant\n回答\n"
	first, err := s.Offload("conv-dedupe", KindTurn, text)
	if err != nil {
		t.Fatalf("offload: %v", err)
	}
	second, err := s.Offload("conv-dedupe", KindTurn, text)
	if err != nil {
		t.Fatalf("offload: %v", err)
	}
	if first.NodeID != second.NodeID {
		t.Errorf("identical content produced two nodes: %s vs %s", first.NodeID, second.NodeID)
	}
	if got := len(s.OffloadBlocks("conv-dedupe")); got != 1 {
		t.Errorf("index holds %d blocks, want 1", got)
	}
	// different content must still get its own node
	if other, err := s.Offload("conv-dedupe", KindTurn, text+"\n差异\n"); err != nil {
		t.Fatalf("offload: %v", err)
	} else if other.NodeID == first.NodeID {
		t.Error("distinct content reused the same node id")
	}
}

// TestCompactNoEvictionWhenItFits makes sure a short conversation is untouched.
func TestCompactNoEvictionWhenItFits(t *testing.T) {
	s := newTestService(t)
	msgs := []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "你好，有什么可以帮你？"},
	}
	res, err := s.Compact("conv-3", msgs, 8000)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(res.Evicted) != 0 {
		t.Errorf("nothing should be evicted, got %d blocks", len(res.Evicted))
	}
	if len(res.Keep) != len(msgs) {
		t.Errorf("keep = %d, want %d", len(res.Keep), len(msgs))
	}
}

// TestGuidanceEmptyWhenNoLayers keeps the always-on block out of the prompt
// until there is actually a persona or a scene to show.
func TestGuidanceEmptyWhenNoLayers(t *testing.T) {
	s := newTestService(t)
	if g := s.Guidance(1500); g != "" {
		t.Errorf("guidance should be empty on a fresh store, got %q", g)
	}
	if err := s.SetPersona("# 用户画像\n- 偏好中文"); err != nil {
		t.Fatalf("set persona: %v", err)
	}
	if err := s.SaveScene("记忆模块", "正在实现 L0-L3", []string{"m1"}); err != nil {
		t.Fatalf("save scene: %v", err)
	}
	g := s.Guidance(1500)
	if !strings.Contains(g, "<persona>") || !strings.Contains(g, "记忆模块") {
		t.Errorf("guidance missing layers: %q", g)
	}
}
