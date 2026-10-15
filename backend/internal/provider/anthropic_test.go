package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/keyvault"
)

// ---- request translation ----

func TestBuildAnthropicRequestExtractsSystemToTopLevel(t *testing.T) {
	req, err := buildAnthropicRequest([]Message{
		{Role: "system", Content: "你是助手"},
		{Role: "system", Content: "第二段"},
		{Role: "user", Content: "你好"},
	}, CallOptions{Model: "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	// system must NOT appear as a message; Anthropic rejects role:"system".
	for _, m := range req.Messages {
		if m.Role == "system" {
			t.Fatal("system must be a top-level field, not a message")
		}
	}
	if req.System != "你是助手\n\n第二段" {
		t.Fatalf("system not joined: %q", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("unexpected messages: %+v", req.Messages)
	}
}

func TestBuildAnthropicRequestInjectsMaxTokensWhenAbsent(t *testing.T) {
	// Anthropic returns 400 when max_tokens is missing, so the adapter must
	// never let a zero value through.
	req, err := buildAnthropicRequest([]Message{{Role: "user", Content: "hi"}}, CallOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != defaultAnthropicMaxTokens {
		t.Fatalf("max_tokens must be defaulted, got %d", req.MaxTokens)
	}
	// and an explicit value must survive
	req2, _ := buildAnthropicRequest([]Message{{Role: "user", Content: "hi"}}, CallOptions{Model: "m", MaxTokens: 77})
	if req2.MaxTokens != 77 {
		t.Fatalf("explicit max_tokens overwritten: %d", req2.MaxTokens)
	}
}

func TestBuildAnthropicRequestMergesConsecutiveSameRole(t *testing.T) {
	// Two adjacent user messages are a 400 upstream; they must be merged.
	req, err := buildAnthropicRequest([]Message{
		{Role: "user", Content: "第一段"},
		{Role: "user", Content: "第二段"},
		{Role: "assistant", Content: "回复"},
		{Role: "user", Content: "追问"},
	}, CallOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("same-role turns were not merged: %d messages", len(req.Messages))
	}
	if len(req.Messages[0].Content) != 2 {
		t.Fatalf("expected 2 blocks in the merged user turn, got %d", len(req.Messages[0].Content))
	}
}

func TestBuildAnthropicRequestToolResultBecomesUserTurn(t *testing.T) {
	req, err := buildAnthropicRequest([]Message{
		{Role: "user", Content: "查一下"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_01", Name: "calc", Arguments: `{"a":1}`}}},
		{Role: "tool", ToolCallID: "toolu_01", Content: "42"},
	}, CallOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("want user/assistant/user, got %d", len(req.Messages))
	}
	if req.Messages[2].Role != "user" {
		t.Fatalf("tool_result must live in a user turn, got %q", req.Messages[2].Role)
	}
	blk := req.Messages[2].Content[0]
	if blk.Type != "tool_result" || blk.ToolUseID != "toolu_01" || blk.Content != "42" {
		t.Fatalf("bad tool_result block: %+v", blk)
	}
	// The assistant turn must replay its tool_use verbatim, ids included.
	au := req.Messages[1].Content[0]
	if au.Type != "tool_use" || au.ID != "toolu_01" || au.Name != "calc" {
		t.Fatalf("assistant tool_use not replayed: %+v", au)
	}
	if string(au.Input) != `{"a":1}` {
		t.Fatalf("tool input mangled: %s", au.Input)
	}
}

func TestBuildAnthropicRequestEmptyToolArgsBecomeEmptyObject(t *testing.T) {
	// An empty tool_use input is invalid JSON for the upstream parser.
	req, _ := buildAnthropicRequest([]Message{
		{Role: "user", Content: "x"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Name: "n", Arguments: ""}}},
	}, CallOptions{Model: "m"})
	if got := string(req.Messages[1].Content[0].Input); got != "{}" {
		t.Fatalf("empty arguments must become {}, got %q", got)
	}
}

func TestBuildAnthropicRequestRejectsAssistantFirstHistory(t *testing.T) {
	_, err := buildAnthropicRequest([]Message{{Role: "assistant", Content: "开头"}}, CallOptions{Model: "m"})
	if err == nil {
		t.Fatal("a history opening with an assistant turn must be rejected, not sent")
	}
}

func TestBuildAnthropicRequestRejectsEmptyConversation(t *testing.T) {
	_, err := buildAnthropicRequest([]Message{{Role: "system", Content: "只有系统提示"}}, CallOptions{Model: "m"})
	if err == nil {
		t.Fatal("system-only history must be rejected")
	}
}

func TestBuildAnthropicRequestSendsOnlyOneSamplingKnob(t *testing.T) {
	// Anthropic recommends altering either temperature or top_p, not both.
	req, _ := buildAnthropicRequest([]Message{{Role: "user", Content: "x"}},
		CallOptions{Model: "m", Temperature: 0.3, TopP: 0.9})
	if req.Temperature == nil || req.TopP != nil {
		t.Fatalf("expected temperature only, got temp=%v topP=%v", req.Temperature, req.TopP)
	}
	req2, _ := buildAnthropicRequest([]Message{{Role: "user", Content: "x"}},
		CallOptions{Model: "m", TopP: 0.9})
	if req2.TopP == nil || req2.Temperature != nil {
		t.Fatalf("expected top_p only, got temp=%v topP=%v", req2.Temperature, req2.TopP)
	}
}

func TestBuildAnthropicRequestToolSchemaMapping(t *testing.T) {
	req, _ := buildAnthropicRequest([]Message{{Role: "user", Content: "x"}}, CallOptions{
		Model: "m",
		Tools: []ToolSpec{{Name: "calc", Description: "算数", Parameters: map[string]interface{}{"type": "object"}}},
	})
	if len(req.Tools) != 1 {
		t.Fatalf("tools not forwarded: %+v", req.Tools)
	}
	if req.Tools[0].Name != "calc" || req.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("tool not mapped to input_schema: %+v", req.Tools[0])
	}
}

// ---- SSE parsing ----

// anthropicSSE builds a Messages-API event stream for the parser.
func anthropicSSE(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func TestParseAnthropicSSEAssemblesTextAndStops(t *testing.T) {
	stream := anthropicSSE(
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":120,"output_tokens":1}}}`,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"，世界"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":18}}`,
		`data: {"type":"message_stop"}`,
	)
	ch := make(chan StreamChunk, 16)
	a := &anthropicAdapter{}
	a.parseAnthropicSSE(strings.NewReader(stream), ch)
	close(ch)

	var text string
	var done *StreamChunk
	for c := range ch {
		if c.Done {
			cc := c
			done = &cc
			continue
		}
		text += c.Delta
	}
	if text != "你好，世界" {
		t.Fatalf("text deltas not concatenated: %q", text)
	}
	if done == nil {
		t.Fatal("no Done chunk emitted")
	}
	if done.Usage.PromptTokens != 120 || done.Usage.CompletionTokens != 18 || done.Usage.TotalTokens != 138 {
		t.Fatalf("usage wrong: %+v", done.Usage)
	}
	if done.FinishReason != "stop" {
		t.Fatalf("stop_reason not normalised: %q", done.FinishReason)
	}
	if done.Model != "claude-opus-5" {
		t.Fatalf("model not captured: %q", done.Model)
	}
}

func TestParseAnthropicSSEConcatenatesPartialToolJSON(t *testing.T) {
	// input_json_delta arrives as JSON *fragments*. Parsing per-delta would blow
	// up on any mid-object prefix; the adapter must join them and hand over one
	// string.
	stream := anthropicSSE(
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":10}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"算一下"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"calculator","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"expr"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ession\":\"1+1\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}`,
		`data: {"type":"message_stop"}`,
	)
	ch := make(chan StreamChunk, 32)
	a := &anthropicAdapter{}
	a.parseAnthropicSSE(strings.NewReader(stream), ch)
	close(ch)

	var last StreamChunk
	for c := range ch {
		last = c
	}
	if len(last.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(last.ToolCalls))
	}
	tc := last.ToolCalls[0]
	if tc.Name != "calculator" || tc.ID != "toolu_9" {
		t.Fatalf("tool call identity wrong: %+v", tc)
	}
	if tc.Arguments != `{"expression":"1+1"}` {
		t.Fatalf("partial JSON not assembled: %q", tc.Arguments)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Arguments), &obj); err != nil {
		t.Fatalf("assembled arguments are not valid JSON: %v", err)
	}
	if last.FinishReason != "tool_calls" {
		t.Fatalf("tool_use must normalise to tool_calls, got %q", last.FinishReason)
	}
}

func TestParseAnthropicSSEMultipleToolCallsKeepBlockOrder(t *testing.T) {
	stream := anthropicSSE(
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tA","name":"first"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tB","name":"second"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_stop"}`,
	)
	ch := make(chan StreamChunk, 16)
	a := &anthropicAdapter{}
	a.parseAnthropicSSE(strings.NewReader(stream), ch)
	close(ch)
	var last StreamChunk
	for c := range ch {
		last = c
	}
	// Map iteration order is random in Go; emitting them out of order would make
	// multi-tool turns non-reproducible between identical runs.
	if len(last.ToolCalls) != 2 || last.ToolCalls[0].Name != "first" || last.ToolCalls[1].Name != "second" {
		t.Fatalf("tool call order not preserved: %+v", last.ToolCalls)
	}
}

func TestParseAnthropicSSEThinkingSurfacesAsReasoning(t *testing.T) {
	stream := anthropicSSE(
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先分解问题"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig=="}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}`,
		`data: {"type":"message_stop"}`,
	)
	ch := make(chan StreamChunk, 16)
	a := &anthropicAdapter{}
	a.parseAnthropicSSE(strings.NewReader(stream), ch)
	close(ch)
	var reason, text string
	for c := range ch {
		reason += c.Reasoning
		text += c.Delta
	}
	if reason != "先分解问题" {
		t.Fatalf("thinking_delta not surfaced as reasoning: %q", reason)
	}
	if text != "答案" {
		t.Fatalf("text after thinking lost: %q", text)
	}
}

func TestParseAnthropicSSEStreamErrorIsReported(t *testing.T) {
	// A mid-stream error event must become an error, not a silent empty answer:
	// otherwise the user sees a blank reply and thinks the model refused.
	stream := anthropicSSE(
		`data: {"type":"message_start","message":{"model":"m"}}`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	)
	ch := make(chan StreamChunk, 8)
	a := &anthropicAdapter{}
	a.parseAnthropicSSE(strings.NewReader(stream), ch)
	close(ch)
	var last StreamChunk
	for c := range ch {
		last = c
	}
	if last.Err == nil {
		t.Fatal("stream error event was swallowed")
	}
	if !strings.Contains(last.Err.Error(), "Overloaded") {
		t.Fatalf("error detail lost: %v", last.Err)
	}
}

func TestParseAnthropicSSEPingAndUnknownEventsIgnored(t *testing.T) {
	stream := anthropicSSE(
		`data: {"type":"ping"}`,
		`data: {"type":"some_future_event","foo":1}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`data: {"type":"message_stop"}`,
	)
	ch := make(chan StreamChunk, 16)
	a := &anthropicAdapter{}
	a.parseAnthropicSSE(strings.NewReader(stream), ch)
	close(ch)
	var text string
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("unknown event caused failure: %v", c.Err)
		}
		text += c.Delta
	}
	if text != "ok" {
		t.Fatalf("text lost around unknown events: %q", text)
	}
}

func TestNormaliseAnthropicStop(t *testing.T) {
	cases := map[string]string{
		"tool_use":   "tool_calls",
		"end_turn":   "stop",
		"max_tokens": "stop",
		"":           "",
		"refusal":    "refusal",
	}
	for in, want := range cases {
		if got := normaliseAnthropicStop(in); got != want {
			t.Fatalf("stop_reason %q → %q, want %q", in, got, want)
		}
	}
}

// ---- live HTTP paths ----

// newAnthropicTestAdapter points an adapter at a stub server. Keys are sealed
// into a real vault so the adapter exercises its actual secret-resolution path
// (a fake here would hide a broken ref lookup).
func newAnthropicTestAdapter(t *testing.T, base string, secrets map[string]string) *anthropicAdapter {
	t.Helper()
	v, err := keyvault.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]KeyRecord, 0, len(secrets))
	for ref, plain := range secrets {
		if err := v.Set(ref, plain); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, KeyRecord{ID: ref, Ref: ref})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Ref < keys[j].Ref })
	return &anthropicAdapter{keyring: &keyring{
		entity: &Provider{
			ID: "anth", Type: "anthropic", BaseURL: base,
			Keys: keys, Enabled: true,
		},
		vault: v,
	}}
}

func TestAnthropicChatStreamSendsRequiredHeaders(t *testing.T) {
	var gotAuth, gotVer, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-api-key")
		gotVer = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"model":"claude-opus-5"}}`+"\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n")
	}))
	defer srv.Close()

	a := newAnthropicTestAdapter(t, srv.URL, map[string]string{"k1": "sk-ant-test"})
	ch, err := a.ChatStream(context.Background(), []Message{{Role: "user", Content: "你好"}}, CallOptions{Model: "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("stream error: %v", c.Err)
		}
		got += c.Delta
	}
	if got != "hi" {
		t.Fatalf("delta not delivered: %q", got)
	}
	if gotAuth != "sk-ant-test" {
		t.Fatalf("x-api-key not sent (got %q)", gotAuth)
	}
	if gotVer != anthropicVersion {
		t.Fatalf("anthropic-version header missing (got %q)", gotVer)
	}
	if gotPath != "/messages" {
		t.Fatalf("wrong endpoint: %s", gotPath)
	}
}

func TestAnthropicUpstreamErrorSurfacesMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`)
	}))
	defer srv.Close()

	a := newAnthropicTestAdapter(t, srv.URL, map[string]string{"k1": "sk-ant-test"})
	_, err := a.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, CallOptions{Model: "m"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "max_tokens: required") {
		t.Fatalf("upstream message not surfaced: %v", err)
	}
}

func TestAnthropicChatParsesToolUseAndThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
		  "model":"claude-opus-5","stop_reason":"tool_use",
		  "content":[
		    {"type":"thinking","thinking":"先算"},
		    {"type":"text","text":"我来算"},
		    {"type":"tool_use","id":"toolu_1","name":"calculator","input":{"expression":"1+1"}}
		  ],
		  "usage":{"input_tokens":77,"output_tokens":23}
		}`)
	}))
	defer srv.Close()

	a := newAnthropicTestAdapter(t, srv.URL, map[string]string{"k1": "sk-ant-test"})
	res, err := a.Chat(context.Background(), []Message{{Role: "user", Content: "1+1"}}, CallOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "calculator" {
		t.Fatalf("tool_use not parsed: %+v", res.ToolCalls)
	}
	if res.ToolCalls[0].Arguments != `{"expression":"1+1"}` {
		t.Fatalf("tool input not serialised: %q", res.ToolCalls[0].Arguments)
	}
	if !strings.Contains(res.Content, "我来算") || !strings.Contains(res.Content, "先算") {
		t.Fatalf("thinking/text not combined: %q", res.Content)
	}
	if res.Usage.TotalTokens != 100 {
		t.Fatalf("usage not totalled: %+v", res.Usage)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("stop reason not normalised: %q", res.FinishReason)
	}
}

func TestAnthropicModelsFollowsPagination(t *testing.T) {
	var cursors []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cursors = append(cursors, r.URL.Query().Get("after_id"))
		n := len(cursors)
		mu.Unlock()
		switch n {
		case 1:
			fmt.Fprint(w, `{"data":[{"id":"claude-a","display_name":"A","max_input_tokens":200000,
			  "capabilities":{"image_input":{"supported":true},"thinking":{"supported":true}}}],"has_more":true,"last_id":"claude-a"}`)
		case 2:
			fmt.Fprint(w, `{"data":[{"id":"claude-b","display_name":"B"}],"has_more":true,"last_id":"claude-b"}`)
		default:
			fmt.Fprint(w, `{"data":[{"id":"claude-c","display_name":"C"}],"has_more":false,"last_id":"claude-c"}`)
		}
	}))
	defer srv.Close()

	a := newAnthropicTestAdapter(t, srv.URL, map[string]string{"k1": "sk-ant-test"})
	models, err := a.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	// Duplicates are the failure mode here: re-requesting page 1 yields three
	// copies of the first model and the operator sees a bogus catalogue.
	want := []string{"claude-a", "claude-b", "claude-c"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %v, want %v (duplicates mean page 1 was refetched)", ids, want)
	}
	// Each page must resume from the previous page's cursor.
	if len(cursors) != 3 || cursors[0] != "" || cursors[1] != "claude-a" || cursors[2] != "claude-b" {
		t.Fatalf("pagination cursors wrong: %v", cursors)
	}
	if !models[0].Capabilities.Vision || models[0].ContextWindow != 200000 {
		t.Fatalf("capabilities/context not mapped: %+v", models[0])
	}
}

func TestAnthropicModelsPaginationTerminatesOnStuckCursor(t *testing.T) {
	// A server that always claims has_more with an unchanged cursor must not
	// spin the adapter forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"claude-x"}],"has_more":true,"last_id":"claude-x"}`)
	}))
	defer srv.Close()
	a := newAnthropicTestAdapter(t, srv.URL, map[string]string{"k1": "sk-ant-test"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := a.Models(context.Background()); err != nil {
			t.Errorf("models: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pagination did not terminate on a non-advancing cursor")
	}
}

func TestAnthropicEmbeddingsFailsLoudly(t *testing.T) {
	a := newAnthropicTestAdapter(t, "http://127.0.0.1:1", map[string]string{"k1": "sk-x"})
	_, err := a.Embeddings(context.Background(), "m", []string{"x"})
	if err == nil {
		t.Fatal("Anthropic has no embeddings endpoint; a silent empty result would degrade recall with no error")
	}
	if !errors.Is(err, errNoAnthropicEmbeddings) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAnthropicKeyCooldownRotatesToNextKey(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("x-api-key"))
		mu.Unlock()
		if r.Header.Get("x-api-key") == "bad" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`)
			return
		}
		fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n")
	}))
	defer srv.Close()

	// Key order is fixed by the helper's sort: "a-bad" then "b-good".
	a := newAnthropicTestAdapter(t, srv.URL, map[string]string{"a-bad": "bad", "b-good": "good"})

	ch, err := a.ChatStream(context.Background(), []Message{{Role: "user", Content: "x"}}, CallOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if len(seen) != 2 || seen[0] != "bad" || seen[1] != "good" {
		t.Fatalf("expected rotation after 401, got %v", seen)
	}
	// The failing key must now be benched so the next call skips it.
	if a.keyring.entity.Keys[0].Status != "cooldown" {
		t.Fatalf("failing key not benched: %+v", a.keyring.entity.Keys[0])
	}
	ref, err := a.keyring.selectKey()
	if err != nil {
		t.Fatal(err)
	}
	if ref != "b-good" {
		t.Fatalf("benched key still selected: %s", ref)
	}
}
