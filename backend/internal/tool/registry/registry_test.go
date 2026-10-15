package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/tool"
)

type fake struct {
	name string
	out  string
}

func (f fake) Name() string        { return f.name }
func (f fake) Description() string { return "d" }
func (f fake) Schema() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}
func (f fake) Execute(context.Context, map[string]interface{}) (string, error) { return f.out, nil }

func TestBuiltinCannotBeUnregistered(t *testing.T) {
	r := New()
	r.Register(fake{name: "calculator"})
	if err := r.Unregister("calculator"); err == nil {
		t.Fatal("built-in tools must not be removable — they would just return at next boot")
	}
	if _, ok := r.Get("calculator"); !ok {
		t.Fatal("built-in disappeared anyway")
	}
}

func TestDynamicRegisterAndUnregister(t *testing.T) {
	r := New()
	r.RegisterDynamic(SourceScript, "script-1", fake{name: "my_tool"})
	if _, ok := r.Get("my_tool"); !ok {
		t.Fatal("dynamic tool not registered")
	}
	if src, owner, ok := r.Owner("my_tool"); !ok || src != SourceScript || owner != "script-1" {
		t.Fatalf("provenance wrong: %q/%q ok=%v", src, owner, ok)
	}
	if n := r.UnregisterOwner("script-1"); n != 1 {
		t.Fatalf("expected 1 removed, got %d", n)
	}
	if _, ok := r.Get("my_tool"); ok {
		t.Fatal("tool survived UnregisterOwner")
	}
}

func TestUnregisterOwnerLeavesOtherOwners(t *testing.T) {
	r := New()
	r.RegisterDynamic(SourceScript, "a", fake{name: "tool_a"})
	r.RegisterDynamic(SourceScript, "b", fake{name: "tool_b"})
	r.Register(fake{name: "builtin_x"})
	if n := r.UnregisterOwner("a"); n != 1 {
		t.Fatalf("expected 1, got %d", n)
	}
	if _, ok := r.Get("tool_b"); !ok {
		t.Fatal("a different owner's tool was removed")
	}
	if _, ok := r.Get("builtin_x"); !ok {
		t.Fatal("built-in removed by owner sweep")
	}
}

func TestReRegisterReplacesEntry(t *testing.T) {
	r := New()
	r.RegisterDynamic(SourceScript, "a", fake{name: "t", out: "old"})
	r.RegisterDynamic(SourceScript, "a", fake{name: "t", out: "new"})
	got, err := r.Execute(context.Background(), "t", nil)
	if err != nil || got != "new" {
		t.Fatalf("re-register must replace, got %q err=%v", got, err)
	}
	if len(r.List()) != 1 {
		t.Fatalf("re-register duplicated the entry: %d", len(r.List()))
	}
}

func TestListSortsBuiltinsFirst(t *testing.T) {
	r := New()
	r.Register(fake{name: "z_builtin"})
	r.RegisterDynamic(SourceScript, "s", fake{name: "a_script"})
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	if list[0].Source != SourceBuiltin || list[1].Source != SourceScript {
		t.Fatalf("built-ins must sort first, got %+v", list)
	}
}

func TestExecuteSanitisesResult(t *testing.T) {
	r := New()
	r.Register(fake{name: "noisy", out: "ok\x00\x07\n"})
	got, err := r.Execute(context.Background(), "noisy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, "\x00\x07") {
		t.Fatalf("control chars survived sanitisation: %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Fatal("newline should survive")
	}
}

func TestExecuteSanitisesErrorText(t *testing.T) {
	r := New()
	r.Register(errTool{name: "boom"})
	_, err := r.Execute(context.Background(), "boom", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.ContainsAny(err.Error(), "\x00\x1b") {
		t.Fatalf("error text must be sanitised too (it reaches the model): %q", err.Error())
	}
	// The real reason must survive sanitisation, otherwise a failure becomes
	// an empty error the user cannot act on.
	if !strings.Contains(err.Error(), "exploded") {
		t.Fatalf("original error message lost: %q", err.Error())
	}
}

type errTool struct{ name string }

func (e errTool) Name() string        { return e.name }
func (e errTool) Description() string { return "d" }
func (e errTool) Schema() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}
func (e errTool) Execute(context.Context, map[string]interface{}) (string, error) {
	return "", errBoom
}

var errBoom = errTest("exploded")

type errTest string

func (e errTest) Error() string { return string(e) }

var _ tool.Tool = fake{}
