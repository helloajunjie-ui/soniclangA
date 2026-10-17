package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/config"
	"github.com/agenthub/agent-proxy/internal/conversation"
	"github.com/agenthub/agent-proxy/internal/logx"
	"github.com/agenthub/agent-proxy/internal/memory"
	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
)

// These cover request-shape bugs that silently destroyed user data instead of
// failing: decoding into a non-pointer string turned "field absent" into "field
// is empty", and the handler then wrote that empty value and answered 200.

func newWriteHandler(t *testing.T) (*Handler, *memory.Service, *conversation.Service) {
	t.Helper()
	dir := t.TempDir()
	open := func(ns string) storage.Store {
		return storage.NewJSON(filepath.Join(dir, ns))
	}
	mem := memory.New(open("mem"), open("memidx"), filepath.Join(dir, "memory"), retrieval.New(), nil, false)
	convs := conversation.New(open("convs"))
	return &Handler{
		Cfg: &config.Config{DataDir: dir, Settings: config.Defaults()},
		log: logx.Default(),
	}, mem, convs
}

func post(h *Handler, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+target, strings.NewReader(body))
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// The UI's 保存画像 button posts {markdown}; the handler only read {content}, so
// every save wrote "" over the L3 profile and reported success.
func TestPersonaSaveAcceptsTheUIField(t *testing.T) {
	h, mem, _ := newWriteHandler(t)
	h.Memory = mem

	if err := mem.SetPersona("# 画像\n\n用户偏好简洁的回答。\n"); err != nil {
		t.Fatalf("seed persona: %v", err)
	}

	// Exactly what the frontend sends.
	rec := post(h, http.MethodPut, "/api/v1/memories/persona", `{"markdown":"# 画像\n\n用户偏好中文回答。\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := mem.Persona(); !strings.Contains(got, "中文") {
		t.Errorf("persona was not updated, it is now: %q", got)
	}

	// The documented field still works.
	rec = post(h, http.MethodPut, "/api/v1/memories/persona", `{"content":"# 画像\n\n用表格。\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("content field: status = %d", rec.Code)
	}
	if got := mem.Persona(); !strings.Contains(got, "表格") {
		t.Errorf("content field was ignored: %q", got)
	}
}

// A body with neither key (or a malformed one) must be refused, never treated as
// "clear the profile".
func TestPersonaSaveRefusesMissingContent(t *testing.T) {
	h, mem, _ := newWriteHandler(t)
	h.Memory = mem
	const seeded = "# 画像\n\n不要被清空。\n"
	if err := mem.SetPersona(seeded); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, body := range []string{`{}`, `{"unrelated":1}`, `not json`} {
		rec := post(h, http.MethodPut, "/api/v1/memories/persona", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
		if got := mem.Persona(); got != seeded {
			t.Errorf("body %q changed the persona to %q", body, got)
		}
	}
}

// Editing a message bubble with a body that has no content used to blank it and
// answer {"ok":true}.
func TestMessageEditRequiresContent(t *testing.T) {
	h, _, convs := newWriteHandler(t)
	h.Convs = convs

	c, err := convs.Create("general", conversation.ModeChat)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := convs.AppendMessage(c.ID, conversation.Message{Role: "user", Content: "原文"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	msgs, err := convs.Messages(c.ID)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages: %v %d", err, len(msgs))
	}
	mid := msgs[0].ID
	path := "/api/v1/conversations/" + c.ID + "/messages/" + mid

	if rec := post(h, http.MethodPut, path, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing content: status = %d, want 400", rec.Code)
	}
	if rec := post(h, http.MethodPut, path, `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want 400", rec.Code)
	}
	if got, _ := convs.Messages(c.ID); got[0].Content != "原文" {
		t.Errorf("message content changed to %q", got[0].Content)
	}

	// A real edit still works, including an explicit empty string.
	if rec := post(h, http.MethodPut, path, `{"content":"改好了"}`); rec.Code != http.StatusOK {
		t.Fatalf("valid edit: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, _ := convs.Messages(c.ID); got[0].Content != "改好了" {
		t.Errorf("edit did not apply: %q", got[0].Content)
	}
}
