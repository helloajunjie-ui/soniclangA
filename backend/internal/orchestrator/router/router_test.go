package router

import (
	"context"
	"errors"
	"testing"

	"github.com/agenthub/agent-proxy/internal/provider"
)

func TestRuleClassify(t *testing.T) {
	cases := []struct {
		msg      string
		wantMode string
		why      string
	}{
		{"你好", "chat", "短消息无动作词"},
		{"帮我写一份季度总结报告", "task", "命中动作词"},
		{"请调研一下 AgentHub 的模块化设计，并生成报告", "task", "长消息+动作词"},
	}
	for _, c := range cases {
		got := ruleClassify(c.msg)
		if got.Mode != c.wantMode {
			t.Errorf("%s: mode=%s want %s (%s)", c.why, got.Mode, c.wantMode, got.Reason)
		}
	}
}

// A threshold above the rule's task confidence must keep the turn in chat —
// over-orchestrating a simple question is worse than under-orchestrating.
func TestThresholdKeepsChat(t *testing.T) {
	d := ClassifyWith(context.Background(), nil, "帮我写一份报告", nil, Options{Threshold: 0.95})
	if d.Mode != "chat" {
		t.Fatalf("threshold not honoured: %+v", d)
	}
	if d.Source != "rule" {
		t.Fatalf("source = %q", d.Source)
	}

	d2 := ClassifyWith(context.Background(), nil, "帮我写一份报告", nil, Options{Threshold: 0.5})
	if d2.Mode != "task" {
		t.Fatalf("low threshold should allow task: %+v", d2)
	}
}

// stubClassifier lets the tests drive the model strategy without a provider.
type stubClassifier struct {
	content string
	err     error
}

func (s stubClassifier) Chat(context.Context, []provider.Message, provider.CallOptions) (*provider.ChatResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &provider.ChatResponse{Content: s.content}, nil
}

func TestModelStrategy(t *testing.T) {
	d := ClassifyWith(context.Background(), stubClassifier{content: `{"mode":"task","confidence":0.9}`},
		"把它整理成清单", nil, Options{Strategy: "model"})
	if d.Mode != "task" || d.Source != "llm" {
		t.Fatalf("model strategy ignored: %+v", d)
	}

	// Prose around the JSON must not stop it from being parsed.
	d2 := ClassifyWith(context.Background(), stubClassifier{content: "模型回答后面还有解释 {\"mode\":\"chat\"}"},
		"这段话是什么意思", nil, Options{Strategy: "model"})
	if d2.Mode != "chat" || d2.Source != "llm" {
		t.Fatalf("embedded JSON not parsed: %+v", d2)
	}

	// When the model says chat but the rule screen hit an explicit action
	// word, the rule wins: the classifier must not silently downgrade a request.
	d3 := ClassifyWith(context.Background(), stubClassifier{content: `{"mode":"chat"}`},
		"把它整理成清单", nil, Options{Strategy: "model"})
	if d3.Mode != "task" || d3.Source != "rule" {
		t.Fatalf("action word should survive a chat verdict: %+v", d3)
	}
}

// A classifier outage must degrade to the rule decision, never block the turn.
func TestModelStrategyFallsBackOnError(t *testing.T) {
	d := ClassifyWith(context.Background(), stubClassifier{err: errors.New("boom")},
		"帮我写一份报告", nil, Options{Strategy: "model"})
	if d.Mode != "task" || d.Source != "rule" {
		t.Fatalf("classifier error should fall back to rule: %+v", d)
	}

	d2 := ClassifyWith(context.Background(), stubClassifier{content: "完全不是 JSON"},
		"你好", nil, Options{Strategy: "model"})
	if d2.Mode != "chat" {
		t.Fatalf("unparsable classifier output should stay chat: %+v", d2)
	}
}

// A nil classifier means the model strategy is unavailable — the rule decides.
func TestModelStrategyWithoutClassifier(t *testing.T) {
	d := ClassifyWith(context.Background(), nil, "帮我做一份对标分析", nil, Options{Strategy: "model"})
	if d.Source != "rule" {
		t.Fatalf("nil classifier should use rule, got %+v", d)
	}
}
