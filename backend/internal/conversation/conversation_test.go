package conversation

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/agenthub/agent-proxy/internal/storage"
)

// The message stream is a JSONL append log with a seq counter kept on the
// conversation document. That split is where the bugs live: seq assignment must
// stay gap-free and monotonic under concurrent turns, and every tail operation
// (trim / edit / continue) has to keep the log and MessageCount in agreement.
// None of this had a test before.

func newService(t *testing.T) *Service {
	t.Helper()
	return New(storage.NewJSON(t.TempDir()))
}

func mustCreate(t *testing.T, s *Service) *Conversation {
	t.Helper()
	c, err := s.Create("role-1", ModeChat)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.ID == "" {
		t.Fatal("created conversation has no id")
	}
	return c
}

func appendMsg(t *testing.T, s *Service, convID, role, content string) string {
	t.Helper()
	if err := s.AppendMessage(convID, Message{Role: role, Content: content, Status: "sent"}); err != nil {
		t.Fatalf("append %q: %v", content, err)
	}
	msgs, err := s.Messages(convID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	return msgs[len(msgs)-1].ID
}

func TestAppendMessageAssignsOrderedSeq(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)

	appendMsg(t, s, c.ID, "user", "第一句")
	appendMsg(t, s, c.ID, "assistant", "第二句")
	appendMsg(t, s, c.ID, "user", "第三句")

	msgs, err := s.Messages(c.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	for i, m := range msgs {
		if want := int64(i + 1); m.Seq != want {
			t.Errorf("message %d has seq %d, want %d", i, m.Seq, want)
		}
		if m.ID == "" {
			t.Errorf("message %d has no id", i)
		}
		if m.CreatedAt == 0 {
			t.Errorf("message %d has no timestamp", i)
		}
	}

	// History replay reads the conversation document's counter, so it has to
	// track the log exactly or the UI's pagination totals drift.
	got, err := s.Get(c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MessageCount != 3 {
		t.Errorf("MessageCount = %d, want 3", got.MessageCount)
	}
}

// Two turns landing at once (a user typing while a tool result is appended) must
// not share a seq: a duplicate seq makes TrimMessages delete the wrong tail, and
// the client's dedupe-by-seq silently drops a message.
func TestConcurrentAppendsGetDistinctSeq(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if err := s.AppendMessage(c.ID, Message{Role: "user", Content: "m"}); err != nil {
				t.Errorf("append: %v", err)
			}
		}(i)
	}
	wg.Wait()

	msgs, err := s.Messages(c.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != n {
		t.Fatalf("got %d messages, want %d", len(msgs), n)
	}
	seen := map[int64]bool{}
	for _, m := range msgs {
		if seen[m.Seq] {
			t.Fatalf("duplicate seq %d", m.Seq)
		}
		seen[m.Seq] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct seqs, want %d", len(seen), n)
	}
}

// Trim is the primitive behind edit/delete/regenerate: the target and everything
// after it go away, and the counter follows the log.
func TestTrimDropsTargetAndEverythingAfter(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)

	appendMsg(t, s, c.ID, "user", "1")
	second := appendMsg(t, s, c.ID, "assistant", "2")
	appendMsg(t, s, c.ID, "user", "3")

	if err := s.TrimMessages(c.ID, second); err != nil {
		t.Fatalf("trim: %v", err)
	}
	msgs, err := s.Messages(c.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "1" {
		t.Fatalf("after trim = %+v, want only the first message", msgs)
	}
	got, err := s.Get(c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1", got.MessageCount)
	}

	// Trimming an id that is not in the log must be an error, not a no-op: the
	// UI treats success as "safe to resend".
	if err := s.TrimMessages(c.ID, "no-such-message"); err == nil {
		t.Error("trimming an unknown message id returned nil")
	}
}

func TestUpdateMessageRewritesInPlace(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)

	appendMsg(t, s, c.ID, "user", "原文")
	target := appendMsg(t, s, c.ID, "user", "要改的")

	if err := s.UpdateMessage(c.ID, target, "改好了"); err != nil {
		t.Fatalf("update: %v", err)
	}
	msgs, _ := s.Messages(c.ID)
	if len(msgs) != 2 {
		t.Fatalf("update changed the message count: %+v", msgs)
	}
	if msgs[0].Content != "原文" || msgs[1].Content != "改好了" {
		t.Errorf("after update = %q / %q", msgs[0].Content, msgs[1].Content)
	}
	if err := s.UpdateMessage(c.ID, "nope", "x"); err == nil {
		t.Error("updating an unknown message id returned nil")
	}
}

func TestAppendToMessageGrowsTheTail(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)

	appendMsg(t, s, c.ID, "user", "问题")
	id := appendMsg(t, s, c.ID, "assistant", "前半")

	if err := s.AppendToMessage(c.ID, id, "后半"); err != nil {
		t.Fatalf("append-to: %v", err)
	}
	msgs, _ := s.Messages(c.ID)
	if len(msgs) != 2 {
		t.Fatalf("continue opened a new message instead of growing: %+v", msgs)
	}
	if msgs[1].Content != "前半后半" {
		t.Errorf("content = %q, want 前半后半", msgs[1].Content)
	}
}

// Export is what the user copies out of the app; dropping system turns and
// keeping order is the whole contract.
func TestExportMarkdownSkipsSystemTurns(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)

	appendMsg(t, s, c.ID, "system", "你是助手")
	appendMsg(t, s, c.ID, "user", "你好")
	appendMsg(t, s, c.ID, "assistant", "在的")

	out, err := s.Export(c.ID, "md")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	text := string(out)
	if strings.Contains(text, "你是助手") {
		t.Error("system turn leaked into the markdown export")
	}
	if !strings.Contains(text, "你好") || !strings.Contains(text, "在的") {
		t.Errorf("export lost content:\n%s", text)
	}
	if strings.Index(text, "你好") > strings.Index(text, "在的") {
		t.Error("export is out of order")
	}

	// The JSON export is the archival form: it must round-trip through
	// encoding/json and carry both the conversation and its messages.
	js, err := s.Export(c.ID, "json")
	if err != nil {
		t.Fatalf("export json: %v", err)
	}
	var doc struct {
		SchemaVersion int             `json:"schemaVersion"`
		Conversation  json.RawMessage `json:"conversation"`
		Messages      []Message       `json:"messages"`
	}
	if err := json.Unmarshal(js, &doc); err != nil {
		t.Fatalf("json export is not valid JSON: %v", err)
	}
	if doc.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", doc.SchemaVersion)
	}
	if len(doc.Messages) != 3 {
		t.Errorf("json export has %d messages, want all 3 (including system)", len(doc.Messages))
	}
}

func TestDeleteRemovesConversationAndHistory(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)
	appendMsg(t, s, c.ID, "user", "hi")

	if err := s.Delete(c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(c.ID); err == nil {
		t.Error("conversation still readable after delete")
	}
	msgs, err := s.Messages(c.ID)
	if err == nil && len(msgs) != 0 {
		t.Errorf("history survived delete: %+v", msgs)
	}
}

// Switching role / mode must persist: a reload reads them back from the store,
// and the next turn picks provider and prompt from them.
func TestMetaUpdatesPersist(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)

	if err := s.SwitchRole(c.ID, "role-2"); err != nil {
		t.Fatalf("switch role: %v", err)
	}
	if err := s.SetMode(c.ID, ModeTask); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	got, err := s.Get(c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RoleId != "role-2" {
		t.Errorf("RoleId = %q, want role-2", got.RoleId)
	}
	if got.Mode != ModeTask {
		t.Errorf("Mode = %q, want task", got.Mode)
	}
}

// A task plan is upserted by step id: re-reporting the same step must update it
// rather than append a duplicate, or the UI grows a step per progress tick.
func TestUpsertStepIsIdempotentByID(t *testing.T) {
	s := newService(t)
	c := mustCreate(t, s)

	if err := s.UpsertStep(c.ID, TaskStep{ID: "s1", Title: "第一步", Status: StepRunning}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertStep(c.ID, TaskStep{ID: "s1", Title: "第一步", Status: StepDone}); err != nil {
		t.Fatalf("upsert again: %v", err)
	}
	plan, err := s.GetTask(c.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("got %d steps, want 1 (upsert duplicated)", len(plan.Steps))
	}
	if plan.Steps[0].Status != StepDone {
		t.Errorf("step status = %q, want done", plan.Steps[0].Status)
	}
}
