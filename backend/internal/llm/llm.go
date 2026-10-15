// Package llm wraps the provider runtime for the internal, non-conversational
// LLM calls the product makes on its own behalf (memory extraction,
// consolidation). These runs are unattended: nobody is watching the answer, so
// the failure mode to avoid is silent loss, not a slow reply.
package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/agenthub/agent-proxy/internal/llmjson"
	"github.com/agenthub/agent-proxy/internal/provider"
)

// the correction appended when the first reply is not parseable
const fixupInstruction = "上一次回复无法解析为 JSON。请严格只输出一个 JSON 对象，不要任何解释、前后缀或代码块标记。"

// ChatObject asks the model for JSON and returns raw object text.
//
// Models answer structured-output prompts with prose often enough that a single
// parse failure would silently discard an entire unattended run. On a failure we
// retry exactly once with the bad reply shown back for correction — a second
// attempt usually fixes it, while a third would only burn tokens on a model
// that cannot comply.
func ChatObject(ctx context.Context, rt provider.Runtime, model string,
	msgs []provider.Message, opts provider.CallOptions) (string, error) {
	opts.Model = model
	opts.Stream = false
	resp, err := rt.Chat(ctx, msgs, opts)
	if err != nil {
		return "", err
	}
	first, ferr := llmjson.Object(resp.Content)
	if ferr == nil {
		return first, nil
	}
	retry := append(append([]provider.Message{}, msgs...),
		provider.Message{Role: "assistant", Content: clip(resp.Content, 400)},
		provider.Message{Role: "user", Content: fixupInstruction},
	)
	resp2, err := rt.Chat(ctx, retry, opts)
	if err != nil {
		return "", ferr
	}
	second, serr := llmjson.Object(resp2.Content)
	if serr != nil {
		return "", fmt.Errorf("%v (retry: %v)", ferr, serr)
	}
	return second, nil
}

// Chat runs a plain (non-JSON) completion, used where the answer is the payload.
func Chat(ctx context.Context, rt provider.Runtime, model string,
	msgs []provider.Message, opts provider.CallOptions) (string, error) {
	opts.Model = model
	opts.Stream = false
	resp, err := rt.Chat(ctx, msgs, opts)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func clip(s string, n int) string {
	t := strings.TrimSpace(s)
	r := []rune(t)
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
