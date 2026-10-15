package role

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/storage"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	return New(storage.NewJSON(t.TempDir()))
}

// --- template vars (M4c) ---

func TestRenderPromptBasics(t *testing.T) {
	r := &Role{
		SystemPrompt: "你是 {{项目}} 的 {{角色}}，风格：{{风格}}",
		Vars: []RoleVar{
			{Name: "项目", Required: true},
			{Name: "角色", Required: false, Default: "助手"},
		},
	}
	got := r.RenderPrompt(map[string]string{"项目": "AgentHub", "风格": "简洁"})
	want := "你是 AgentHub 的 助手，风格：简洁"
	if got != want {
		t.Fatalf("render = %q, want %q", got, want)
	}
}

func TestRenderPromptKeepsUnknownPlaceholder(t *testing.T) {
	r := &Role{SystemPrompt: "语言：{{语言}}"}
	got := r.RenderPrompt(nil)
	if got != "语言：{{语言}}" {
		t.Fatalf("undeclared placeholder should stay visible, got %q", got)
	}
}

func TestRenderPromptIgnoresLiteralBraces(t *testing.T) {
	r := &Role{
		SystemPrompt: `输出 JSON：{"name": "{{name}}", "ok": true}`,
		Vars:         []RoleVar{{Name: "name"}},
	}
	got := r.RenderPrompt(map[string]string{"name": "bob"})
	if !strings.Contains(got, `"name": "bob"`) || !strings.Contains(got, `"ok": true`) {
		t.Fatalf("literal braces mangled: %q", got)
	}
}

func TestApplyVarsPolicies(t *testing.T) {
	base := Role{
		SystemPrompt: "{{必填}}",
		Vars:         []RoleVar{{Name: "必填", Required: true}},
	}

	err := base
	err.MissingVarPolicy = MissingError
	_, _, e := err.ApplyVars(nil)
	if _, ok := e.(*MissingVarError); !ok {
		t.Fatalf("policy=error should return MissingVarError, got %v", e)
	}

	ask := base
	ask.MissingVarPolicy = MissingAsk
	_, missing, e := ask.ApplyVars(nil)
	if e != nil || len(missing) != 1 || missing[0] != "必填" {
		t.Fatalf("policy=ask should report missing without error, got missing=%v err=%v", missing, e)
	}

	skip := base // no policy => skip
	prompt, missing, e := skip.ApplyVars(nil)
	// skip does not block generation; missing is still reported for the UI,
	// and the placeholder stays visible in the rendered prompt.
	if e != nil || len(missing) != 1 || prompt != "{{必填}}" {
		t.Fatalf("policy=skip should render with placeholder intact, got %q missing=%v err=%v", prompt, missing, e)
	}
}

func TestMissingVarsForHonoursDefaults(t *testing.T) {
	r := Role{
		Vars: []RoleVar{
			{Name: "a", Required: true, Default: "x"},
			{Name: "b", Required: true},
		},
	}
	missing := r.MissingVarsFor(nil)
	if len(missing) != 1 || missing[0] != "b" {
		t.Fatalf("defaulted var must not be missing, got %v", missing)
	}
}

func TestUnknownVars(t *testing.T) {
	r := Role{
		SystemPrompt: "{{known}} {{unknown1}} {{unknown2}}",
		Vars:         []RoleVar{{Name: "known"}},
	}
	got := r.UnknownVars()
	if len(got) != 2 {
		t.Fatalf("want 2 unknown vars, got %v", got)
	}
}

// --- duplicate (M4c) ---

func TestDuplicateIsDeepCopy(t *testing.T) {
	s := newTestService(t)
	src := &Role{
		Name:         "源",
		SystemPrompt: "p",
		Tools:        []string{"calc"},
		Skills:       []string{"research"},
	}
	if err := s.Create(src); err != nil {
		t.Fatal(err)
	}
	cp, err := s.Duplicate(src.ID, "副本名")
	if err != nil {
		t.Fatal(err)
	}
	if cp.ID == src.ID || cp.Name != "副本名" {
		t.Fatalf("bad duplicate: id=%s name=%s", cp.ID, cp.Name)
	}
	// Mutate the copy's slices; the source must not see it.
	cp.Tools[0] = "mutated"
	fresh, err := s.Get(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Tools[0] != "calc" {
		t.Fatal("Duplicate shared slice backing arrays with source")
	}
}

func TestDuplicateNotFound(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Duplicate("nope", ""); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

// --- export / import bundle ---

func TestExportAllRoundTrip(t *testing.T) {
	s := newTestService(t)
	b, err := s.ExportAll()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int    `json:"schemaVersion"`
		Kind          string `json:"kind"`
		Items         []Role `json:"items"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "roles" || len(doc.Items) == 0 {
		t.Fatalf("bad bundle: kind=%q items=%d", doc.Kind, len(doc.Items))
	}
}

func TestExportImportSingleRole(t *testing.T) {
	s := newTestService(t)
	src := &Role{Name: "导出者", SystemPrompt: "p", Vars: []RoleVar{{Name: "v", Required: true}}}
	if err := s.Create(src); err != nil {
		t.Fatal(err)
	}
	b, err := s.Export(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	dst := newTestService(t)
	rl, err := dst.Import(b)
	if err != nil {
		t.Fatal(err)
	}
	if rl.Name != "导出者" || len(rl.Vars) != 1 || rl.ID == src.ID {
		t.Fatalf("import lost data: %+v", rl)
	}
}
