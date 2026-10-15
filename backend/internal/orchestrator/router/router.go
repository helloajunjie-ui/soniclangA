// Package router implements intent routing for mode=auto (DESIGN §6.8): a cheap
// rule pre-screen, then an optional LLM classification. The rule screen is the
// default; the LLM classifier is used only when strategy=model and a runtime is
// available. A conservative fallback (when no classifier is wired, unmatched
// prompts stay in chat) avoids over-orchestration.
package router

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agenthub/agent-proxy/internal/provider"
)

// RuntimeClassifier is the LLM used for classification (temperature 0).
type RuntimeClassifier interface {
	Chat(ctx context.Context, messages []provider.Message, opts provider.CallOptions) (*provider.ChatResponse, error)
}

// Decision is the routing outcome.
type Decision struct {
	Mode       string  `json:"mode"` // chat|task
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	Source     string  `json:"source"` // rule|llm
}

// Options carries the effective auto-routing settings. The role may override
// the global config (DESIGN §6.8 role.autoRoute).
type Options struct {
	// Strategy is "rule" (default) or "model".
	Strategy string
	// Threshold is the confidence a decision must reach to leave chat mode.
	// 0 disables the gate (every decision is accepted).
	Threshold float64
}

const classifyPrompt = `你是意图分类器。判断下面这条用户消息应该用哪种模式处理：
- chat：闲聊、问答、解释、咨询
- task：需要多步执行、产出文件或报告、修改代码、调研分析

只输出 JSON：{"mode":"chat|task","confidence":0.0-1.0,"reason":"不超过20字"}

消息：`

// Classify routes a user message with default (rule) options.
func Classify(ctx context.Context, cls RuntimeClassifier, msg string, specs []provider.ToolSpec) Decision {
	return ClassifyWith(ctx, cls, msg, specs, Options{})
}

// ClassifyWith routes a user message honouring strategy and threshold.
//
// Order: the rule screen runs first (free); the LLM classifier is consulted
// only when it is asked for AND available — a classification failure must never
// block the turn, so any error falls back to the rule decision.
func ClassifyWith(ctx context.Context, cls RuntimeClassifier, msg string, _ []provider.ToolSpec, opts Options) Decision {
	d := ruleClassify(msg)
	if strings.EqualFold(strings.TrimSpace(opts.Strategy), "model") && cls != nil {
		if llmDecide(ctx, cls, msg) {
			return Decision{Mode: "task", Confidence: 0.85, Reason: "模型判定为任务", Source: "llm"}
		}
		// The LLM said chat: keep it, but with the rule confidence as a floor
		// only if the rule screen disagreed on an obvious action word.
		if d.Mode == "task" {
			return d
		}
		return Decision{Mode: "chat", Confidence: 0.85, Reason: "模型判定为对话", Source: "llm"}
	}
	if opts.Threshold > 0 && d.Confidence < opts.Threshold && d.Mode == "task" {
		// Low confidence on entering task mode: stay in chat (DESIGN §6.8 ④).
		return Decision{Mode: "chat", Confidence: d.Confidence, Reason: d.Reason + "（置信度低于阈值，保持对话）", Source: d.Source}
	}
	return d
}

// llmDecide asks the model whether the message is a task. It returns false on
// any error, so a flaky classifier degrades to "chat" instead of failing.
func llmDecide(ctx context.Context, cls RuntimeClassifier, msg string) bool {
	resp, err := cls.Chat(ctx, []provider.Message{
		{Role: "user", Content: classifyPrompt + msg},
	}, provider.CallOptions{Temperature: 0, MaxTokens: 200})
	if err != nil || resp == nil {
		return false
	}
	var out struct {
		Mode string `json:"mode"`
	}
	content := resp.Content
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return false
	}
	if err := json.Unmarshal([]byte(content[start:end+1]), &out); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(out.Mode), "task")
}

func ruleClassify(msg string) Decision {
	if len([]rune(msg)) < 50 && !hasActionWord(msg) {
		return Decision{Mode: "chat", Confidence: 0.9, Reason: "短消息且无动作词，按对话处理", Source: "rule"}
	}
	if hasActionWord(msg) {
		return Decision{Mode: "task", Confidence: 0.8, Reason: "命中任务动作词", Source: "rule"}
	}
	// unmatched: conservative chat (avoid over-orchestration, DESIGN §6.8 ④)
	return Decision{Mode: "chat", Confidence: 0.6, Reason: "规则未命中，保守按对话", Source: "rule"}
}

func hasActionWord(msg string) bool {
	for _, w := range actionWords {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

var actionWords = []string{
	"调研", "分析", "执行", "批量", "整理", "生成报告", "修复", "部署",
	"计划", "任务", "步骤", "对比", "总结成", "写一份", "做一个", "实现",
	"调研一下", "帮我做", "自动",
}
