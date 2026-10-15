package module

import "testing"

type fake struct{ id string }

func (f fake) ID() string   { return f.id }
func (f fake) Init() error  { return nil }
func (f fake) Start() error { return nil }
func (f fake) Stop() error  { return nil }

func TestRegisterListSorted(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithMeta(fake{"tool.calculator"}, Meta{ID: "tool.calculator", Name: "计算器", Kind: KindTool})
	r.RegisterWithMeta(fake{"rag.knowledge"}, Meta{ID: "rag.knowledge", Name: "资料库", Kind: KindRAG})
	r.RegisterWithMeta(fake{"provider.deepseek"}, Meta{ID: "provider.deepseek", Name: "DeepSeek", Kind: KindProvider})

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("want 3 modules, got %d", len(list))
	}
	// provider < rag < tool alphabetically by kind.
	if list[0].ID != "provider.deepseek" || list[2].ID != "tool.calculator" {
		t.Fatalf("unsorted list: %+v", list)
	}
}

func TestToggleAndIsEnabled(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithMeta(fake{"rag.knowledge"}, Meta{ID: "rag.knowledge", Kind: KindRAG})

	if !r.IsEnabled("rag.knowledge") {
		t.Fatal("new module should be enabled")
	}
	if err := r.SetEnabled("rag.knowledge", false); err != nil {
		t.Fatal(err)
	}
	if r.IsEnabled("rag.knowledge") {
		t.Fatal("toggle off failed")
	}
	if ids := r.DisabledIDs(); len(ids) != 1 || ids[0] != "rag.knowledge" {
		t.Fatalf("DisabledIDs = %v", ids)
	}

	// Unknown ids are enabled so a stale switch cannot silence a new module.
	if !r.IsEnabled("nope") {
		t.Fatal("unknown module should default to enabled")
	}
	if err := r.SetEnabled("nope", false); err == nil {
		t.Fatal("toggling an unregistered module should error")
	}
}

func TestRestoreDisabledIgnoresUnknown(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithMeta(fake{"rag.knowledge"}, Meta{ID: "rag.knowledge", Kind: KindRAG})
	r.RestoreDisabled([]string{"rag.knowledge", "gone"})

	if r.IsEnabled("rag.knowledge") {
		t.Fatal("persisted switch not restored")
	}
	if !r.IsEnabled("gone") {
		t.Fatal("unknown persisted id should stay enabled")
	}
	if ids := r.EnabledFilter([]string{"rag.knowledge", "other"}); len(ids) != 1 || ids[0] != "other" {
		t.Fatalf("EnabledFilter = %v", ids)
	}
}

func TestRegisterDefaultsMeta(t *testing.T) {
	r := NewRegistry()
	r.Register(fake{"core"})
	m, ok := r.Get("core")
	if !ok || m.ID() != "core" {
		t.Fatal("Get failed")
	}
	if info := r.List(); len(info) != 1 || info[0].Name != "core" || info[0].Kind != KindRuntime {
		t.Fatalf("meta not defaulted: %+v", info[0])
	}
}

// Hidden modules must never reach the UI: listing one produces a switch that
// flips without changing anything observable, which a user reads as a bug.
// They must still be switchable through the disabled set so a config file can
// turn them off.
func TestHiddenNotListedButStillSwitchable(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithMeta(fake{"runtime.orchestrator"}, Meta{
		ID: "runtime.orchestrator", Name: "编排", Kind: KindRuntime, Hidden: true,
	})
	r.RegisterWithMeta(fake{"rag.knowledge"}, Meta{
		ID: "rag.knowledge", Name: "资料库检索", Kind: KindRAG,
	})

	list := r.List()
	if len(list) != 1 || list[0].ID != "rag.knowledge" {
		t.Fatalf("hidden module leaked into the listing: %+v", list)
	}
	if !list[0].Switchable {
		t.Fatalf("a plain module must be switchable")
	}

	// The hidden module is still real: disabling it must stick.
	if err := r.SetEnabled("runtime.orchestrator", false); err != nil {
		t.Fatalf("hidden module should still accept a switch: %v", err)
	}
	if r.IsEnabled("runtime.orchestrator") {
		t.Fatalf("hidden module ignored its switch")
	}
}

// A read-only entry is shown for audit but must render as information, not as
// a control the user can press.
func TestReadOnlyNotSwitchable(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithMeta(fake{"tool.get_time"}, Meta{
		ID: "tool.get_time", Name: "时间", Kind: KindTool, ReadOnly: true,
	})
	list := r.List()
	if len(list) != 1 {
		t.Fatalf("want 1 entry, got %d", len(list))
	}
	if list[0].Switchable {
		t.Fatalf("read-only entry must not report as switchable")
	}
}
