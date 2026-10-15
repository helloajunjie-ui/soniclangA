package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/agenthub/agent-proxy/internal/tool/schema"
)

// anthropicVersion is the API version header value. Anthropic pins behaviour to
// this string, so it is sent on every request rather than left to the default.
const anthropicVersion = "2023-06-01"

// defaultAnthropicMaxTokens is used when the caller passes no max_tokens.
// Anthropic REQUIRES the field — omitting it returns 400 — so unlike the
// OpenAI adapter (where max_tokens is optional) we cannot simply skip it.
const defaultAnthropicMaxTokens = 4096

// errNoAnthropicEmbeddings: Anthropic publishes no /embeddings endpoint. The
// local RAG and memory engines call Embeddings on whatever provider is
// configured, so returning an empty slice here would look like "this text has
// no vector" and silently degrade recall to BM25-only with no error anywhere.
// A clear error lets the caller fall back deliberately.
var errNoAnthropicEmbeddings = errors.New("Anthropic 不提供 embeddings 端点；请为资料库/记忆配置一个 OpenAI 兼容的 embedding 服务商")

// anthropicAdapter speaks the native Messages API (POST /v1/messages).
// Everything that differs from OpenAI — auth header, top-level system field,
// mandatory max_tokens, tool_use/tool_result content blocks, the SSE event
// vocabulary — is contained in this file (DESIGN §2/§5).
type anthropicAdapter struct {
	*keyring
}

func (a *anthropicAdapter) ID() string { return a.entity.ID }

// ---- wire types ----

type anContentBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result. Content is accepted as a plain string on the way in; the API
	// also allows a list of blocks, which we never send (our tools return text).
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type anMessage struct {
	Role    string           `json:"role"`
	Content []anContentBlock `json:"content"`
}

type anTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type anRequest struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	System    string      `json:"system,omitempty"`
	Messages  []anMessage `json:"messages"`
	Tools     []anTool    `json:"tools,omitempty"`
	Stream    bool        `json:"stream,omitempty"`
	// Temperature and TopP are mutually discouraged by Anthropic ("we recommend
	// altering either, not both"); sending both is legal but pointless. Only one
	// is sent, preferring temperature because that is what the orchestrator
	// exposes to users.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
}

// ---- request translation ----

// buildAnthropicRequest translates the provider-agnostic message list into the
// Messages API shape. The three non-obvious rules, all of which produce a 400
// if violated:
//
//  1. `system` is a top-level field, not a message.
//  2. `max_tokens` is mandatory.
//  3. Tool results ride in a **user** message, and consecutive same-role
//     messages must be merged into one.
func buildAnthropicRequest(messages []Message, opts CallOptions) (anRequest, error) {
	req := anRequest{Model: opts.Model, MaxTokens: opts.MaxTokens, Stream: opts.Stream}
	if req.MaxTokens <= 0 {
		req.MaxTokens = defaultAnthropicMaxTokens
	}
	if opts.Temperature != 0 {
		t := opts.Temperature
		req.Temperature = &t
	} else if opts.TopP != 0 {
		p := opts.TopP
		req.TopP = &p
	}

	var sysParts []string
	for _, m := range messages {
		switch m.Role {
		case "system":
			if strings.TrimSpace(m.Content) != "" {
				sysParts = append(sysParts, m.Content)
			}
		case "tool":
			// A tool result is machine output, but Anthropic requires it inside
			// a user-role turn.
			appendAnBlock(&req.Messages, "user", anContentBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})
		case "assistant":
			// Replay the assistant turn verbatim: its tool_use blocks carry the
			// ids the tool_result blocks must reference.
			blocks := make([]anContentBlock, 0, 1+len(m.ToolCalls))
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, anContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				raw := json.RawMessage(tc.Arguments)
				if len(bytes.TrimSpace(raw)) == 0 {
					raw = json.RawMessage("{}")
				}
				blocks = append(blocks, anContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: raw})
			}
			if len(blocks) > 0 {
				appendAnBlock(&req.Messages, "assistant", blocks...)
			}
		default: // user
			if strings.TrimSpace(m.Content) != "" {
				appendAnBlock(&req.Messages, "user", anContentBlock{Type: "text", Text: m.Content})
			}
		}
	}
	req.System = strings.Join(sysParts, "\n\n")

	if len(req.Messages) == 0 {
		return req, errors.New("没有可发送的消息：system 提示词不能作为唯一输入")
	}
	// Anthropic requires the first turn to be user. A history that opens with an
	// assistant turn (or an empty leading system) is rejected with 400.
	if req.Messages[0].Role != "user" {
		return req, errors.New("Anthropic 要求首条消息为 user 角色")
	}

	for _, t := range opts.Tools {
		params := t.Parameters
		if params == nil {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		req.Tools = append(req.Tools, anTool{Name: t.Name, Description: t.Description, InputSchema: params})
	}
	return req, nil
}

// appendAnBlock appends blocks to msgs, merging into the previous message when
// the role repeats. Merging is mandatory, not cosmetic: two consecutive user
// messages are a 400.
func appendAnBlock(msgs *[]anMessage, role string, blocks ...anContentBlock) {
	if n := len(*msgs); n > 0 && (*msgs)[n-1].Role == role {
		(*msgs)[n-1].Content = append((*msgs)[n-1].Content, blocks...)
		return
	}
	*msgs = append(*msgs, anMessage{Role: role, Content: blocks})
}

// ---- transport ----

func (a *anthropicAdapter) newRequest(ctx context.Context, path string, body []byte, secret string) (*http.Request, error) {
	url := strings.TrimRight(a.entity.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", secret)
	req.Header.Set("anthropic-version", anthropicVersion)
	for k, v := range a.entity.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// upstreamErr renders a non-200 response. Anthropic wraps the detail in
// {"error":{"type":..,"message":..}} — surfacing the raw body would bury the
// actual reason ("credit balance is too low") behind JSON punctuation.
func upstreamErr(status int, body []byte) error {
	var out struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.Error.Message != "" {
		return fmt.Errorf("upstream %d %s: %s", status, out.Error.Type, out.Error.Message)
	}
	return fmt.Errorf("upstream %d: %s", status, strings.TrimSpace(string(body)))
}

// ---- ChatStream ----

func (a *anthropicAdapter) ChatStream(ctx context.Context, messages []Message, opts CallOptions) (<-chan StreamChunk, error) {
	reqBody, err := buildAnthropicRequest(messages, opts)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		var lastErr error
		tries := len(a.entity.Keys) + 1
		for attempt := 0; attempt < tries; attempt++ {
			ref, err := a.selectKey()
			if err != nil {
				lastErr = err
				break
			}
			secret, err := a.secret(ref)
			if err != nil {
				lastErr = err
				a.markCooldown(ref)
				continue
			}
			ok, streamErr := a.streamOnce(ctx, ch, secret, payload)
			if ok {
				return
			}
			lastErr = streamErr
			a.markCooldown(ref)
		}
		ch <- StreamChunk{Done: true, Err: lastErr}
	}()
	return ch, nil
}

func (a *anthropicAdapter) streamOnce(ctx context.Context, ch chan StreamChunk, secret string, payload []byte) (bool, error) {
	req, err := a.newRequest(ctx, "/messages", payload, secret)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, upstreamErr(resp.StatusCode, b)
	}
	a.parseAnthropicSSE(resp.Body, ch)
	return true, nil
}

// anBlock accumulates one content block across its start/delta/stop events.
type anBlock struct {
	kind   string // text | tool_use | thinking | ...
	id     string
	name   string
	text   string
	reason string
}

func (a *anthropicAdapter) parseAnthropicSSE(r io.Reader, ch chan StreamChunk) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	blocks := map[int]*anBlock{}
	var order []int
	var finishReason, model string
	usage := Usage{}
	var streamErr error

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // event:/id:/comment lines carry nothing we need
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			// message_start
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			// content_block_start
			ContentBlock anContentBlock `json:"content_block"`
			// One struct serves both content_block_delta and message_delta: they share the
			// "delta" JSON key, and declaring two fields with the same tag makes
			// encoding/json silently drop both. message_delta carries stop_reason
			// (nested, not top level) plus cumulative usage.
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			// error
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "message_start":
			if ev.Message.Model != "" {
				model = ev.Message.Model
			}
			// Input tokens are only reported here; output arrives progressively
			// in message_delta. message_delta's usage is cumulative.
			usage.PromptTokens = ev.Message.Usage.InputTokens
			if ev.Message.Usage.OutputTokens > 0 {
				usage.CompletionTokens = ev.Message.Usage.OutputTokens
			}
		case "content_block_start":
			b := &anBlock{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			if _, seen := blocks[ev.Index]; !seen {
				order = append(order, ev.Index)
			}
			blocks[ev.Index] = b
		case "content_block_delta":
			b, ok := blocks[ev.Index]
			if !ok {
				// A delta for a block we never saw start: treat it as text of the
				// kind implied by the delta, rather than dropping content.
				b = &anBlock{kind: "text"}
				blocks[ev.Index] = b
				order = append(order, ev.Index)
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text += ev.Delta.Text
				ch <- StreamChunk{Delta: ev.Delta.Text}
			case "thinking_delta":
				b.reason += ev.Delta.Thinking
				ch <- StreamChunk{Reasoning: ev.Delta.Thinking}
			case "input_json_delta":
				// Partial JSON is a fragment of a string, never a value. It must
				// be concatenated and handed over whole on Done; parsing here
				// would fail on any mid-object prefix.
				b.text += ev.Delta.PartialJSON
			case "signature_delta":
				// Verifies thinking-block integrity; nothing to surface.
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				finishReason = ev.Delta.StopReason
			}
			if ev.Usage.InputTokens > 0 {
				usage.PromptTokens = ev.Usage.InputTokens
			}
			usage.CompletionTokens = ev.Usage.OutputTokens
		case "error":
			streamErr = fmt.Errorf("upstream stream error %s: %s", ev.Error.Type, ev.Error.Message)
		case "ping", "content_block_stop", "message_stop":
			// keep-alive / block boundary / stream end
		}
	}
	if err := sc.Err(); err != nil && streamErr == nil {
		streamErr = err
	}
	if streamErr != nil {
		ch <- StreamChunk{Done: true, Err: streamErr}
		return
	}

	// Assemble tool calls in the order the blocks were opened. Ranging over a map
	// would emit tool calls in random order, which makes multi-tool turns
	// non-deterministic between runs.
	var tcs []ToolCall
	for _, idx := range order {
		b := blocks[idx]
		if b.kind != "tool_use" {
			continue
		}
		args := strings.TrimSpace(b.text)
		if args == "" {
			args = "{}"
		}
		tcs = append(tcs, ToolCall{ID: b.id, Name: b.name, Arguments: args})
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	ch <- StreamChunk{Done: true, ToolCalls: tcs, Usage: usage, Model: model, FinishReason: normaliseAnthropicStop(finishReason)}
}

// normaliseAnthropicStop maps Anthropic's stop_reason onto the OpenAI-ish
// vocabulary the orchestrator and the /v1 compatibility endpoint already speak.
// Leaving it raw would make the UI treat "tool_use" as an unknown reason.
func normaliseAnthropicStop(s string) string {
	switch s {
	case "tool_use":
		return "tool_calls"
	case "end_turn", "stop_sequence", "max_tokens":
		return "stop"
	case "":
		return ""
	default:
		return s
	}
}

// ---- Chat (non-stream) ----

func (a *anthropicAdapter) Chat(ctx context.Context, messages []Message, opts CallOptions) (*ChatResponse, error) {
	reqBody, err := buildAnthropicRequest(messages, opts)
	if err != nil {
		return nil, err
	}
	reqBody.Stream = false
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	ref, err := a.selectKey()
	if err != nil {
		return nil, err
	}
	secret, err := a.secret(ref)
	if err != nil {
		return nil, err
	}
	req, err := a.newRequest(ctx, "/messages", payload, secret)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, upstreamErr(resp.StatusCode, b)
	}
	var out struct {
		Model      string           `json:"model"`
		Content    []anContentBlock `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	res := &ChatResponse{
		Model:        out.Model,
		FinishReason: normaliseAnthropicStop(out.StopReason),
		Usage: Usage{
			PromptTokens:     out.Usage.InputTokens,
			CompletionTokens: out.Usage.OutputTokens,
			TotalTokens:      out.Usage.InputTokens + out.Usage.OutputTokens,
		},
	}
	var text, reasoning strings.Builder
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			raw := strings.TrimSpace(string(b.Input))
			if raw == "" {
				raw = "{}"
			}
			res.ToolCalls = append(res.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: raw})
		}
	}
	res.Content = schema.SanitizeResult(text.String())
	if r := reasoning.String(); r != "" {
		res.Content = strings.TrimSpace(r + "\n" + res.Content)
	}
	return res, nil
}

// ---- Models ----

func (a *anthropicAdapter) Models(ctx context.Context) ([]ModelInfo, error) {
	url := strings.TrimRight(a.entity.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", a.firstSecret())
	req.Header.Set("anthropic-version", anthropicVersion)
	for k, v := range a.entity.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, upstreamErr(resp.StatusCode, b)
	}
	var out struct {
		Data []struct {
			ID             string                 `json:"id"`
			DisplayName    string                 `json:"display_name"`
			MaxInputTokens int                    `json:"max_input_tokens"`
			MaxTokens      int                    `json:"max_tokens"`
			Capabilities   map[string]interface{} `json:"capabilities"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	infos := make([]ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		caps := ModelCaps{Chat: true, Vision: true, Reasoning: true}
		if m.Capabilities != nil {
			// The API reports vision support as image_input; absence of the key
			// means unsupported rather than false-by-default.
			if _, ok := m.Capabilities["image_input"]; !ok {
				caps.Vision = false
			}
			if _, ok := m.Capabilities["thinking"]; !ok {
				caps.Reasoning = false
			}
		}
		infos = append(infos, ModelInfo{
			ID: m.ID, Name: orString(m.DisplayName, m.ID),
			Capabilities: caps, ContextWindow: m.MaxInputTokens,
		})
	}
	// Anthropic paginates (default 20 per page). Without following has_more the
	// UI would show a truncated catalogue and the user would think those are the
	// only models available.
	if out.HasMore && out.LastID != "" {
		if more, err := a.modelsPage(ctx, url, out.LastID); err == nil {
			infos = append(infos, more...)
		}
	}
	return infos, nil
}

// modelsPage fetches the pages after `afterID`, bounded so a misbehaving
// endpoint cannot spin here forever.
//
// The cursor must come from the caller's already-fetched page: starting from an
// empty cursor re-requests page 1 and duplicates every model already listed.
func (a *anthropicAdapter) modelsPage(ctx context.Context, base, afterID string) ([]ModelInfo, error) {
	var all []ModelInfo
	after := afterID
	for page := 0; page < 10; page++ {
		u := base + "?limit=1000&after_id=" + neturl.QueryEscape(after)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return all, err
		}
		req.Header.Set("x-api-key", a.firstSecret())
		req.Header.Set("anthropic-version", anthropicVersion)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			return all, err
		}
		var out struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			LastID  string `json:"last_id"`
			HasMore bool   `json:"has_more"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		status := resp.StatusCode
		resp.Body.Close()
		if err != nil {
			return all, err
		}
		if status != 200 {
			return all, fmt.Errorf("upstream %d", status)
		}
		for _, m := range out.Data {
			all = append(all, ModelInfo{ID: m.ID, Name: orString(m.DisplayName, m.ID), Capabilities: ModelCaps{Chat: true}})
		}
		// A server that keeps claiming more pages while returning the same
		// cursor would loop forever; the fixed page cap is the backstop, but
		// stopping on a non-advancing cursor is cheaper and clearer.
		if !out.HasMore || out.LastID == "" || out.LastID == after {
			break
		}
		after = out.LastID
	}
	return all, nil
}

func orString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// ---- Embeddings ----

// Embeddings always fails: Anthropic has no such endpoint. See
// errNoAnthropicEmbeddings for why this must not be a silent empty result.
func (a *anthropicAdapter) Embeddings(ctx context.Context, model string, texts []string) ([][]float32, error) {
	return nil, errNoAnthropicEmbeddings
}
