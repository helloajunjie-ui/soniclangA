package extract

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/memory"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
)

func newMemService(t *testing.T) *memory.Service {
	t.Helper()
	root := t.TempDir()
	return memory.New(storage.NewJSON(root+"/memories"), storage.NewJSON(root+"/index"),
		root+"/memory", retrieval.New(), nil, false)
}

// TestSecretRedaction covers the rule that outranks everything else: a secret
// must never leave the machine in a distillation prompt.
func TestSecretRedaction(t *testing.T) {
	in := []memory.ExtractInput{
		{Role: "user", Content: "我的 key 是 sk-abcdefghijklmnopqrstuvwxyz012345，帮我在记忆里记一下"},
		{Role: "user", Content: "api_key: hunter2supersecret"},
		{Role: "user", Content: "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9xxxx"},
		{Role: "user", Content: "我喜欢用中文交流"},
	}
	kept, filtered, redacted := sanitize(in)
	if redacted != 3 {
		t.Errorf("redacted = %d, want 3", redacted)
	}
	if filtered != 0 {
		t.Errorf("filtered = %d, want 0", filtered)
	}
	for _, k := range kept {
		if SecretPatterns.MatchString(k.Content) {
			t.Errorf("secret survived sanitization: %q", k.Content)
		}
	}
	if len(kept) != 4 || kept[3].Content != "我喜欢用中文交流" {
		t.Errorf("clean message should pass through untouched: %#v", kept)
	}
}

// TestInternalMessagesFiltered stops our own plumbing (tool echoes, scheduler
// notices) from being distilled as if it were something the user said.
func TestInternalMessagesFiltered(t *testing.T) {
	in := []memory.ExtractInput{
		{Role: "tool", Content: "[memory_search] 未检索到相关记忆。"},
		{Role: "tool", Content: "[conversation_search] 命中 3 条"},
		{Role: "assistant", Content: "<knowledge>资料片段</knowledge>"},
		{Role: "user", Content: "我下周要去上海出差"},
	}
	kept, filtered, _ := sanitize(in)
	if filtered != 3 {
		t.Errorf("filtered = %d, want 3", filtered)
	}
	if len(kept) != 1 || !strings.Contains(kept[0].Content, "上海") {
		t.Errorf("only the human message should survive: %#v", kept)
	}
}

// TestOutputSecretGuard is the second line of defence: even if a model echoes
// a secret back inside an extracted memory, the item is dropped on the way in.
func TestOutputSecretGuard(t *testing.T) {
	cases := []string{
		"用户的 API 密钥是 sk-abcdefghijklmnopqrstuvwxyz012345",
		"记得保存 password: mysecretpassword",
	}
	for _, c := range cases {
		if !SecretPatterns.MatchString(c) {
			t.Errorf("guard failed to catch: %q", c)
		}
	}
}

// TestCadenceGate makes extraction cheap: a second run inside the window is
// skipped instead of firing another LLM call.
func TestCadenceGate(t *testing.T) {
	s := newMemService(t)
	// a real (empty) provider service: with a nil one the run would legitimately
	// stop at "not_configured" and never reach the cadence gate under test.
	prov := provider.New(storage.NewJSON(t.TempDir()+"/providers"), nil)
	e := New(s, prov, Gates{MinIntervalMinutes: 30, MaxInputMessages: 40, MaxOutputItems: 8})
	convID := "conv-gate"
	ctx := context.Background()
	// no provider/model configured: the run must stop at the first gate
	res, err := e.Extract(ctx, convID, "", "", nil, false)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.Skipped != "not_configured" {
		t.Fatalf("skipped = %q, want not_configured", res.Skipped)
	}
	// mark as just-distilled: a second automatic run must stop at the cadence
	// gate rather than firing another LLM call.
	s.MarkExtracted(convID, time.Now().UnixMilli())
	inputs := []memory.ExtractInput{{Role: "user", Content: "你好"}}
	res2, err := e.Extract(ctx, convID, "p", "m", inputs, false)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res2.Skipped != "cadence" {
		t.Fatalf("second run should hit the cadence gate, got %q", res2.Skipped)
	}
	// force is reserved for the manual API and must get past the gate. There is
	// no provider behind "p", so it has to fail at the provider lookup instead
	// of being silently skipped.
	if _, err := e.Extract(ctx, convID, "p", "m", inputs, true); err == nil {
		t.Error("force should bypass the cadence gate and reach the provider")
	}
}

func TestNormalizeCategory(t *testing.T) {
	cases := map[string]string{
		"preference": memory.CatPreference,
		"偏好":         memory.CatPreference,
		"fact（事实）":   memory.CatFact,
		"":           memory.CatFact,
		"nonsense":   memory.CatFact,
	}
	for in, want := range cases {
		if got := normalizeCategory(in); got != want {
			t.Errorf("normalizeCategory(%q) = %q, want %q", in, got, want)
		}
	}
}
