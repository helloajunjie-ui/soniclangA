package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/config"
	"github.com/agenthub/agent-proxy/internal/conversation"
	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/logx"
	"github.com/agenthub/agent-proxy/internal/orchestrator"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/role"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
)

// This is the one test that walks the whole chat path the UI depends on:
// HTTP POST → orchestrator → provider adapter → upstream SSE → events back out
// as SSE frames → assistant message persisted. Everything else in this package
// tests a seam; this tests the wire contract, which is what a frontend breaks on.
//
// The upstream is a real HTTP server speaking OpenAI's chunk format, so the
// adapter's SSE parsing is exercised rather than stubbed. The services are real
// too (JSON stores in t.TempDir) — the only fake is the vendor.

// upstreamAnswer is what the fake vendor streams back.
const upstreamAnswer = "你好，我是 mock。"

// newFakeUpstream serves POST /v1/chat/completions as an OpenAI-compatible
// stream, splitting the answer into a few chunks so the delta path is real.
// A non-zero status makes it answer with an error instead.
func newFakeUpstream(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		// The adapter must be sending the sealed key it was given.
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"bad key"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		chunks := []string{"你好，", "我是 ", "mock。"}
		for i, c := range chunks {
			payload := map[string]interface{}{
				"model": "mock-model",
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]interface{}{"content": c},
					"finish_reason": nil,
				}},
			}
			if i == len(chunks)-1 {
				payload["choices"].([]map[string]interface{})[0]["finish_reason"] = "stop"
			}
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// A usage-only frame (no choices) is what real vendors send last; the
		// adapter has to keep it out of the content stream.
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type chatApp struct {
	cfg   *config.Config
	convs *conversation.Service
	h     *Handler
	srv   *httptest.Server
}

// newChatApp wires the minimum real services for a chat turn. Optional
// collaborators (memory, knowledge, skills, methodologies) are left nil on
// purpose — the orchestrator guards every one of them.
func newChatApp(t *testing.T, upstreamURL string) *chatApp {
	t.Helper()
	dir := t.TempDir()

	cfg := &config.Config{DataDir: dir, Addr: "127.0.0.1:0", Settings: config.Defaults()}
	// JSON, not the sqlite default: the settings store would otherwise hold a
	// process-wide handle to a file inside t.TempDir, and Windows refuses to
	// remove the directory while it is open. (Closing the shared handle instead
	// would leave the next test with a closed database — it is a shutdown-time
	// API, not a per-test one.)
	cfg.Settings.StorageDriver = storage.DriverJSON
	if err := cfg.Init(); err != nil {
		t.Fatalf("config init: %v", err)
	}
	// Deterministic routing: the rule classifier is cheap and keeps the turn
	// inside chat mode instead of firing the task planner.
	cfg.Settings.AutoRoute.Enabled = false

	open := func(ns string) storage.Store {
		st, err := storage.Open(dir, ns, filepath.Join(dir, ns), storage.DriverJSON)
		if err != nil {
			t.Fatalf("storage %s: %v", ns, err)
		}
		return st
	}

	vault, err := keyvault.New(dir)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	providers := provider.New(open("providers"), vault)
	prov := &provider.Provider{
		Type: "openai", Name: "mock vendor", BaseURL: upstreamURL + "/v1", Enabled: true,
		Models: []provider.ModelInfo{{ID: "mock-model", Name: "mock-model"}},
	}
	if err := providers.Create(prov, []string{"sk-test"}); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	roles := role.New(open("roles"))
	if err := roles.Create(&role.Role{
		ID: "test-role", Name: "测试角色", SystemPrompt: "你是测试助手。",
		DefaultMode: "chat", ProviderId: prov.ID, Model: "mock-model",
		// "full" (not the orchestrator's PermFull constant, which is internal to
		// that package): the fixture only needs the tier that never gates a tool.
		Permission: "full", Temperature: 0.2, MaxTokens: 256,
	}); err != nil {
		t.Fatalf("create role: %v", err)
	}

	convs := conversation.New(open("conversations"))
	orch := orchestrator.New(providers, convs, roles, registry.New(), cfg, audit.New(dir))

	h := &Handler{
		Cfg: cfg, Providers: providers, Roles: roles, Convs: convs,
		Tools: registry.New(), Orch: orch, log: logx.Default(),
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &chatApp{cfg: cfg, convs: convs, h: h, srv: srv}
}

type sseFrame struct {
	Type      string                 `json:"type"`
	RequestID string                 `json:"requestId"`
	Data      map[string]interface{} `json:"data"`
}

// chat posts a message and returns the parsed SSE frames.
func (a *chatApp) chat(t *testing.T, convID, message string) (int, string, []sseFrame) {
	t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"message":%q}`, message))
	req, err := http.NewRequest(http.MethodPost,
		a.srv.URL+"/api/v1/conversations/"+convID+"/messages", body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	frames := parseSSE(t, resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), frames
}

func parseSSE(t *testing.T, r io.Reader) []sseFrame {
	t.Helper()
	var out []sseFrame
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var f sseFrame
		if err := json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &f); err != nil {
			t.Fatalf("frame is not valid JSON (%v): %s", err, line)
		}
		out = append(out, f)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	return out
}

func (a *chatApp) newConversation(t *testing.T) string {
	t.Helper()
	c, err := a.convs.Create("test-role", conversation.Mode("chat"))
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	return c.ID
}

func TestChatSSEStreamsDeltasAndPersists(t *testing.T) {
	upstream := newFakeUpstream(t, http.StatusOK)
	app := newChatApp(t, upstream.URL)
	convID := app.newConversation(t)

	status, ctype, frames := app.chat(t, convID, "你好")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ctype, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ctype)
	}
	if len(frames) == 0 {
		t.Fatal("no SSE frames at all")
	}

	// Every frame must carry a type, a requestId (the UI correlates the whole
	// turn by it) and the _t marker the frontend switches on. A frame missing
	// any of these is silently dropped by the client, so it would look like a
	// stalled answer rather than a bug.
	var requestID string
	var delta strings.Builder
	var sawDone bool
	for _, f := range frames {
		if f.Type == "" {
			t.Fatalf("frame without a type: %+v", f)
		}
		if f.RequestID == "" {
			t.Errorf("frame %q has no requestId", f.Type)
		} else if requestID == "" {
			requestID = f.RequestID
		} else if f.RequestID != requestID {
			t.Errorf("requestId changed mid-turn: %q -> %q", requestID, f.RequestID)
		}
		if got, _ := f.Data["_t"].(string); got != f.Type {
			t.Errorf("frame type %q but data._t = %q", f.Type, got)
		}
		switch f.Type {
		case "error":
			t.Fatalf("unexpected error event: %+v", f.Data)
		case "delta":
			s, _ := f.Data["delta"].(string)
			delta.WriteString(s)
			if id, _ := f.Data["messageId"].(string); id == "" {
				t.Error("delta frame has no messageId")
			}
		case "done":
			sawDone = true
			if m, _ := f.Data["model"].(string); m != "mock-model" {
				t.Errorf("done.model = %q, want mock-model", m)
			}
		}
	}

	if got := delta.String(); got != upstreamAnswer {
		t.Errorf("streamed text = %q, want %q", got, upstreamAnswer)
	}
	if !sawDone {
		t.Fatal("stream ended without a done event")
	}

	// The turn must survive the stream: history is what the next turn and the
	// reload-after-refresh path read from.
	msgs, err := app.convs.Messages(convID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want user+assistant", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "你好" {
		t.Errorf("first message = %+v, want the user turn", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != upstreamAnswer {
		t.Errorf("assistant message = %+v, want %q", msgs[1], upstreamAnswer)
	}
	if msgs[1].Status != "done" {
		t.Errorf("assistant status = %q, want done", msgs[1].Status)
	}
}

// A vendor failure must arrive as an error event on the same stream, not as a
// broken connection or an empty 200: the UI shows the message and keeps the
// turn visible.
func TestChatSSESurfacesUpstreamFailure(t *testing.T) {
	upstream := newFakeUpstream(t, http.StatusInternalServerError)
	app := newChatApp(t, upstream.URL)
	convID := app.newConversation(t)

	status, ctype, frames := app.chat(t, convID, "你好")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE headers are already sent)", status)
	}
	if !strings.HasPrefix(ctype, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ctype)
	}

	var errFrames []sseFrame
	for _, f := range frames {
		if f.Type == "error" {
			errFrames = append(errFrames, f)
		}
		if f.Type == "done" {
			t.Error("a failed turn must not report done")
		}
	}
	if len(errFrames) == 0 {
		t.Fatalf("no error event: %+v", frames)
	}
	// The reason has to reach the user; "stream_failed" with an empty message is
	// what makes a failure look like a hang.
	if msg, _ := errFrames[0].Data["message"].(string); msg == "" {
		t.Error("error event has no message")
	}
	// Two legitimate shapes: a failure before the stream opens ("stream_failed")
	// and one that arrives as a chunk after it opened ("stream_error"). What the
	// UI must never see is silence, so either code is fine but the event is not
	// optional.
	if code, _ := errFrames[0].Data["code"].(string); code != "stream_failed" && code != "stream_error" {
		t.Errorf("error code = %q, want stream_failed or stream_error", code)
	}

	// The user's message is kept even though the answer failed — that is what
	// lets them retry without retyping.
	msgs, err := app.convs.Messages(convID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Errorf("history = %+v, want just the user turn", msgs)
	}
}

// An empty message is a client bug and must be refused before the stream opens,
// so the caller gets a status code instead of an event stream that says nothing.
func TestChatSSERejectsEmptyMessage(t *testing.T) {
	upstream := newFakeUpstream(t, http.StatusOK)
	app := newChatApp(t, upstream.URL)
	convID := app.newConversation(t)

	resp, err := http.Post(app.srv.URL+"/api/v1/conversations/"+convID+"/messages",
		"application/json", strings.NewReader(`{"message":""}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Streaming is only possible if the response writer exposes Flusher. ServeHTTP
// wraps it for request logging, so the wrapper has to forward Flush or every
// chat turn fails with "streaming unsupported".
func TestChatSSEReachesTheWriterThroughTheLogWrapper(t *testing.T) {
	upstream := newFakeUpstream(t, http.StatusOK)
	app := newChatApp(t, upstream.URL)
	convID := app.newConversation(t)

	resp, err := http.Post(app.srv.URL+"/api/v1/conversations/"+convID+"/messages",
		"application/json", strings.NewReader(`{"message":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "streaming unsupported") {
		t.Fatal("the logging wrapper swallowed Flush: chat SSE cannot work")
	}
	if !strings.Contains(string(b), "data:") {
		t.Fatalf("no frames written through the wrapper: %q", string(b))
	}
}
