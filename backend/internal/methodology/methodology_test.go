package methodology

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/storage"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	s := New(storage.NewJSON(t.TempDir()))
	if got := len(s.List()); got != len(Builtins()) {
		t.Fatalf("seed count = %d, want %d", got, len(Builtins()))
	}
	return s
}

func TestBuiltinsShape(t *testing.T) {
	principles := 0
	for _, m := range Builtins() {
		if m.Name == "" || m.Content == "" {
			t.Fatalf("builtin %s missing name or content", m.ID)
		}
		if !m.Enabled {
			t.Fatalf("builtin %s should be enabled", m.ID)
		}
		if m.Status != StatusActive {
			t.Fatalf("builtin %s status = %q", m.ID, m.Status)
		}
		switch m.Kind {
		case KindGood, KindBad, KindPrinciple:
		default:
			t.Fatalf("builtin %s has invalid kind %q", m.ID, m.Kind)
		}
		if m.Always {
			principles++
			if len(m.Keywords) != 0 {
				t.Errorf("standing principle %s should not need keywords", m.ID)
			}
		}
	}
	if principles != 5 {
		t.Fatalf("standing principles = %d, want 5 (DESIGN §14#9)", principles)
	}
}

// TestMatchActivatesKeywords is the core activation contract: a query naming a
// concept pulls in both the good and the bad entry for it.
func TestMatchActivatesKeywords(t *testing.T) {
	s := newTestService(t)
	hits := s.Match("这个需求大概要多久，帮我估时排期", nil)

	var good, bad bool
	for _, h := range hits {
		switch h.ID {
		case "m-good-decompose-estimate":
			good = true
		case "m-bad-guess-estimate":
			bad = true
		}
	}
	if !good || !bad {
		t.Fatalf("estimation query did not activate both entries: %+v", hitIDs(hits))
	}
	// Standing principles always fire, regardless of the query.
	for _, id := range []string{"m-principle-confirm", "m-principle-cite"} {
		if !contains(hitIDs(hits), id) {
			t.Errorf("standing principle %s missing from match", id)
		}
	}
	// Unrelated entries must not fire.
	for _, h := range hits {
		if h.ID == "m-good-api-first" {
			t.Error("unrelated entry activated")
		}
	}
}

func TestMatchRecordsHits(t *testing.T) {
	s := newTestService(t)
	s.Match("重构一下这个模块", nil)
	m, err := s.Get("m-bad-big-bang")
	if err != nil {
		t.Fatal(err)
	}
	if m.Hits != 1 {
		t.Fatalf("hits = %d, want 1", m.Hits)
	}
	if m.LastHitAt == 0 {
		t.Error("lastHitAt not recorded")
	}
}

func TestMatchSkipsDisabledAndRetired(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Toggle("m-bad-big-bang"); err != nil {
		t.Fatal(err)
	}
	if contains(hitIDs(s.Match("重构", nil)), "m-bad-big-bang") {
		t.Fatal("disabled entry should be excluded")
	}

	if _, err := s.Retire("m-bad-guess-estimate", ""); err != nil {
		t.Fatal(err)
	}
	if contains(hitIDs(s.Match("估时", nil)), "m-bad-guess-estimate") {
		t.Fatal("retired entry should be excluded")
	}
}

// TestMatchPinnedBindings covers the explicit-binding activation layer: a role
// may pin an entry even when the query has no keyword hit.
func TestMatchPinnedBindings(t *testing.T) {
	s := newTestService(t)
	hits := s.Match("完全不相关的查询内容", []string{"m-good-api-first"})
	if !contains(hitIDs(hits), "m-good-api-first") {
		t.Fatal("pinned entry not activated")
	}
}

func TestMatchStandingFirst(t *testing.T) {
	s := newTestService(t)
	hits := s.Match("重构 估时 接口", nil)
	if len(hits) < 5 {
		t.Fatalf("too few hits: %d", len(hits))
	}
	for i := 0; i < 5; i++ {
		if !hits[i].Always {
			t.Fatalf("hit %d (%s) is not a standing principle; ordering wrong", i, hits[i].ID)
		}
	}
}

func TestCreateNormalises(t *testing.T) {
	s := newTestService(t)
	m := &Methodology{
		Name:     "  契约优先  ",
		Content:  "  先定契约  ",
		Keywords: []string{"契约", "契约", " ", "接口"},
	}
	if err := s.Create(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "契约优先" || got.Content != "先定契约" {
		t.Errorf("not trimmed: %q / %q", got.Name, got.Content)
	}
	if len(got.Keywords) != 2 {
		t.Errorf("keywords = %v, want deduped+trimmed [契约 接口]", got.Keywords)
	}
	if got.Kind != KindGood {
		t.Errorf("kind = %q, want good", got.Kind)
	}
	if got.Status != StatusActive {
		t.Errorf("status = %q", got.Status)
	}
	if err := s.Create(&Methodology{Name: "x"}); err == nil {
		t.Error("content is required")
	}
	if err := s.Create(&Methodology{Content: "x"}); err == nil {
		t.Error("name is required")
	}
}

func TestAlwaysForcesPrincipleKind(t *testing.T) {
	s := newTestService(t)
	if err := s.Create(&Methodology{Name: "常驻", Content: "c", Always: true, Kind: KindBad}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range s.List() {
		if m.Name == "常驻" {
			found = true
			if m.Kind != KindPrinciple {
				t.Errorf("kind = %q, want principle", m.Kind)
			}
		}
	}
	if !found {
		t.Fatal("entry not created")
	}
}

func TestRetireKeepsTrail(t *testing.T) {
	s := newTestService(t)
	heir := &Methodology{Name: "新版估算方法", Content: "c", Kind: KindGood}
	if err := s.Create(heir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retire("m-bad-guess-estimate", heir.ID); err != nil {
		t.Fatal(err)
	}
	old, _ := s.Get("m-bad-guess-estimate")
	if old.Status != StatusRetired || old.SupersededBy != heir.ID {
		t.Fatalf("retired entry = %+v", old)
	}
	if old.Enabled {
		t.Error("retired entry must be switched off")
	}
	newer, _ := s.Get(heir.ID)
	if newer.Supersedes != "m-bad-guess-estimate" {
		t.Errorf("heir supersedes = %q", newer.Supersedes)
	}
	// The retired entry is still readable — retirement is not deletion.
	if _, err := s.Get("m-bad-guess-estimate"); err != nil {
		t.Error("retirement must not delete the entry")
	}
}

func TestUpdateAndDeleteMissing(t *testing.T) {
	s := newTestService(t)
	if err := s.Update("nope", &Methodology{Name: "x", Content: "y"}); err == nil {
		t.Error("update of a missing entry should fail")
	}
	if err := s.Delete("nope"); err == nil {
		t.Error("delete of a missing entry should fail")
	}
}

func TestToggleRollsBackOnWriteError(t *testing.T) {
	s := newTestService(t)
	before := s.cache["m-bad-big-bang"].Enabled
	s.store = failStore{storage.NewJSON(t.TempDir())}
	if _, err := s.Toggle("m-bad-big-bang"); err == nil {
		t.Fatal("Toggle should surface the persistence error")
	}
	if s.cache["m-bad-big-bang"].Enabled != before {
		t.Fatal("toggle left the in-memory flag flipped after a failed write")
	}
}

// failStore is a Store whose Save always fails.
type failStore struct{ storage.Store }

func (failStore) Save(id string, in interface{}) error { return errBoom }
func (failStore) Backend() string                      { return "fail" }
func (failStore) Dir() string                          { return "" }

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

func TestImportLegacyPromptField(t *testing.T) {
	s := newTestService(t)
	base := len(s.List())
	legacy := `{"schemaVersion":1,"name":"旧条目","prompt":"用五问法找根因","tags":["分析"]}`
	got, err := s.Import([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "用五问法找根因" {
		t.Fatalf("legacy import produced %+v", got)
	}
	if len(s.List()) != base+1 {
		t.Error("entry not added")
	}
	if _, err := s.Import([]byte(`{"items":[{"name":"批量","content":"c"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import([]byte(`{"items":[{}]}`)); err == nil {
		t.Error("entry without name/content should be rejected")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	s := newTestService(t)
	data, err := s.ExportAll()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Import(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(Builtins()) {
		t.Fatalf("round trip produced %d, want %d", len(got), len(Builtins()))
	}
	for _, m := range got {
		if m.ID == "" {
			t.Fatal("imported entry has no id")
		}
	}
	// Originals survive the bundle import.
	for _, want := range []string{"m-principle-confirm", "m-good-api-first", "m-bad-big-bang"} {
		if _, err := s.Get(want); err != nil {
			t.Errorf("original %s clobbered", want)
		}
	}
}

// TestCreateDefaultsEnabled guards the availability bug that motivated the
// explicit default: a POST that omits "enabled" must produce a live entry, not
// one that silently never fires.
func TestCreateDefaultsEnabled(t *testing.T) {
	s := newTestService(t)
	m := &Methodology{Name: "新约定", Content: "先跑测试", Kind: KindGood, Keywords: []string{"测试"}}
	if err := s.Create(m); err != nil {
		t.Fatal(err)
	}
	if !m.Enabled {
		t.Fatal("new entry should default to enabled")
	}
	if !contains(hitIDs(s.Match("跑测试", nil)), m.ID) {
		t.Fatal("newly created entry did not activate")
	}
	// An entry explicitly created as retired keeps its disabled state.
	r := &Methodology{Name: "已退役", Content: "x", Status: StatusRetired}
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	if r.Enabled {
		t.Fatal("retired entry must not be force-enabled")
	}
}

// TestImportPreservesEnabled makes sure a bundle round-trip does not silently
// switch entities back on (or off).
func TestImportPreservesEnabled(t *testing.T) {
	src := newTestService(t)
	if _, err := src.Toggle("m-good-api-first"); err != nil {
		t.Fatal(err)
	}
	data, err := src.ExportAll()
	if err != nil {
		t.Fatal(err)
	}
	dst := New(storage.NewJSON(t.TempDir()))
	if _, err := dst.Import(data); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range dst.List() {
		if m.Name == "接口先行" && !m.Enabled {
			found = true
		}
	}
	if !found {
		t.Fatal("imported disabled entry was switched back on")
	}
}

func TestCompatFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newTestService(t)
	if err := s.ExportFile(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "methodology", "methodology.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("compat file not written: %v", err)
	}
	var doc struct {
		Kind  string `json:"kind"`
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "methodologies" || len(doc.Items) != len(Builtins()) {
		t.Fatalf("compat file content = kind:%s items:%d", doc.Kind, len(doc.Items))
	}

	// A missing file is a no-op, not an error.
	fresh := New(storage.NewJSON(t.TempDir()))
	n, err := fresh.ImportFile(filepath.Join(dir, "nothing-here"))
	if err != nil || n != 0 {
		t.Fatalf("ImportFile on a missing dir = %d, %v", n, err)
	}
}

// TestPersistedRoundTrip verifies entries and their activation state survive a
// reload, including the hit counter.
func TestPersistedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(storage.NewJSON(dir))
	if err := s.Create(&Methodology{
		Name: "团队约定", Content: "提交前跑单测", Kind: KindGood,
		Keywords: []string{"提交", "单测"},
	}); err != nil {
		t.Fatal(err)
	}
	s.Match("提交前跑单测", nil)

	reloaded := New(storage.NewJSON(dir))
	var found *Methodology
	for _, m := range reloaded.List() {
		if m.Name == "团队约定" {
			found = m
		}
	}
	if found == nil {
		t.Fatal("entry did not survive the reload")
	}
	if found.Hits != 1 {
		t.Errorf("hits = %d, want 1", found.Hits)
	}
	if !found.Enabled || found.Kind != KindGood {
		t.Errorf("reloaded entry = %+v", found)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s := newTestService(t)
	m, err := s.Get("m-good-api-first")
	if err != nil {
		t.Fatal(err)
	}
	m.Name = "被改坏了"
	again, _ := s.Get("m-good-api-first")
	if again.Name == "被改坏了" {
		t.Fatal("Get returned a pointer into the cache")
	}
}

func TestLearnRequiresProvider(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Learn(nil, nil, "m", "text"); err == nil {
		t.Fatal("Learn without a provider should fail")
	}
	if _, err := s.Learn(nil, nil, "m", "   "); err == nil {
		t.Fatal("Learn with empty text should fail")
	}
}

// TestListStandingFirst checks list ordering used by the UI.
func TestListStandingFirst(t *testing.T) {
	list := newTestService(t).List()
	seenNon := false
	for _, m := range list {
		if m.Always {
			if seenNon {
				t.Fatal("standing principles must come first")
			}
			continue
		}
		seenNon = true
	}
}

func hitIDs(hits []MatchHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// unused guard for the strings import in helper-only contexts
var _ = strings.TrimSpace
