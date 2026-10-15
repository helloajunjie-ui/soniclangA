package skill

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/storage"
)

const sampleSkillMD = `---
name: 会议纪要整理
description: 把会议记录整理为结构化纪要
tools: [timer, note]
tags:
  - 会议
  - 整理
---

# 会议纪要整理

1. 提取参会人、时间、议题。
2. 按议题归纳讨论要点与结论。
3. 列出行动项，带责任人与截止时间。
`

// failStore is a Store whose Save always fails, used to assert that in-memory
// mutation is rolled back when persistence fails.
type failStore struct{ storage.Store }

func (failStore) Save(id string, in interface{}) error { return errors.New("boom") }
func (failStore) Backend() string                      { return "fail" }
func (failStore) Dir() string                          { return "" }

func newTestService(t *testing.T) *Service {
	t.Helper()
	s := New(storage.NewJSON(t.TempDir()))
	if got := len(s.List()); got != len(Builtins()) {
		t.Fatalf("seed count = %d, want %d", got, len(Builtins()))
	}
	return s
}

func TestSeedBuiltins(t *testing.T) {
	for _, sk := range Builtins() {
		if sk.Name == "" || sk.Instructions == "" {
			t.Fatalf("builtin %s missing name or instructions", sk.ID)
		}
		if !sk.Enabled {
			t.Fatalf("builtin %s should be enabled by default", sk.ID)
		}
	}
}

// TestScanIdempotent verifies the core scan invariant: the source path is the
// dedup key, so repeated scans update in place instead of duplicating entries.
func TestScanIdempotent(t *testing.T) {
	s := newTestService(t)
	dir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(sampleSkillMD), 0o644); err != nil {
		t.Fatal(err)
	}

	base := len(s.List())
	for i := 1; i <= 2; i++ {
		res, err := s.Scan([]string{dir})
		if err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
		if i == 1 {
			if res.Added != 1 || res.Update != 0 {
				t.Fatalf("first scan: added=%d updated=%d, want 1/0", res.Added, res.Update)
			}
		} else if res.Added != 0 || res.Update != 0 {
			// An unchanged file must not be counted as updated, nor rewritten.
			t.Fatalf("unchanged rescan %d: added=%d updated=%d, want 0/0", i, res.Added, res.Update)
		}
		if got := len(s.List()); got != base+1 {
			t.Fatalf("scan %d created duplicates: %d entries, want %d", i, got, base+1)
		}
	}
	// Changing the file body must be detected as an update.
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(sampleSkillMD+"\n- extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 || res.Update != 1 {
		t.Fatalf("changed rescan: added=%d updated=%d, want 0/1", res.Added, res.Update)
	}
}

func TestParseSkillMDFrontmatter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(p, []byte(sampleSkillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	sk, err := ParseSkillMD(p)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "会议纪要整理" {
		t.Errorf("name = %q", sk.Name)
	}
	if sk.Description != "把会议记录整理为结构化纪要" {
		t.Errorf("description = %q", sk.Description)
	}
	if len(sk.Tools) != 2 || sk.Tools[0] != "timer" || sk.Tools[1] != "note" {
		t.Errorf("tools = %v", sk.Tools)
	}
	// The list is written in block form and must not swallow the "---" of the
	// frontmatter terminator.
	if len(sk.Tags) != 2 || sk.Tags[0] != "会议" || sk.Tags[1] != "整理" {
		t.Errorf("tags = %v", sk.Tags)
	}
	if !strings.Contains(sk.Instructions, "行动项") {
		t.Errorf("instructions lost the body: %q", sk.Instructions)
	}
	if strings.Contains(sk.Instructions, "---") {
		t.Errorf("frontmatter delimiters leaked into the body: %q", sk.Instructions)
	}
}

func TestParseSkillMDNoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "SKILL.md")
	body := "# 纯正文技能\n\n直接给出步骤。\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sk, err := ParseSkillMD(p)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "纯正文技能" {
		t.Errorf("name = %q, want the H1 heading", sk.Name)
	}
	if !strings.Contains(sk.Instructions, "直接给出步骤") {
		t.Errorf("instructions = %q", sk.Instructions)
	}
}

// TestScanSkipsHiddenAndNoise makes sure a SKILL.md buried in node_modules or a
// dot-directory is not imported.
func TestScanSkipsHiddenAndNoise(t *testing.T) {
	s := newTestService(t)
	root := t.TempDir()
	base := len(s.List())
	for _, sub := range []string{"node_modules/pkg", ".git/hooks", ".vscode"} {
		d := filepath.Join(root, sub)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(sampleSkillMD), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Scan([]string{root}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.List()); got != base {
		t.Fatalf("scan imported %d hidden entries, want 0", got-base)
	}
}

func TestScanMissingDirIsNotError(t *testing.T) {
	s := newTestService(t)
	got, err := s.ScanDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d skills from a missing dir", len(got))
	}
	if _, err := s.Scan([]string{filepath.Join(t.TempDir(), "nope")}); err != nil {
		t.Fatalf("Scan over a missing dir should not error: %v", err)
	}
}

func TestToggleRollsBackOnWriteError(t *testing.T) {
	s := newTestService(t)
	sk := s.List()[0]
	before := sk.Enabled
	// Swap in a store whose Save always fails, then assert rollback.
	s.store = failStore{storage.NewJSON(t.TempDir())}
	if _, err := s.Toggle(sk.ID); err == nil {
		t.Fatal("Toggle should surface the persistence error")
	}
	if got := s.cache[sk.ID].Enabled; got != before {
		t.Fatalf("toggle left the in-memory flag flipped after a failed write")
	}
}

func TestImportBundleAndSingle(t *testing.T) {
	s := newTestService(t)
	base := len(s.List())

	bundle, err := s.ExportAll()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Import(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != base {
		t.Fatalf("bundle import produced %d, want %d", len(got), base)
	}
	// Imported copies must get fresh ids and lose their source path.
	for _, sk := range got {
		if sk.SourcePath != "" {
			t.Errorf("imported skill kept sourcePath %q", sk.SourcePath)
		}
	}
	if _, err := s.Get("skill-doc-qa"); err != nil {
		t.Error("bundle import clobbered the original seed")
	}

	one, _ := s.Export("skill-brainstorm")
	single, err := s.Import(one)
	if err != nil {
		t.Fatal(err)
	}
	if len(single) != 1 || single[0].Name != "头脑风暴" {
		t.Fatalf("single import = %+v", single)
	}

	if _, err := s.Import([]byte(`{"name":""}`)); err == nil {
		t.Error("skill without a name should be rejected")
	}
	if _, err := s.Import([]byte(`not json`)); err == nil {
		t.Error("malformed document should be rejected")
	}
}

// TestImportPreservesEnabled makes sure a bundle round-trip does not silently
// switch skills back on — Create forces the flag on, Import must not.
func TestImportPreservesEnabled(t *testing.T) {
	src := newTestService(t)
	target := src.List()[0]
	if _, err := src.Toggle(target.ID); err != nil {
		t.Fatal(err)
	}
	data, err := src.ExportAll()
	if err != nil {
		t.Fatal(err)
	}
	dst := New(storage.NewJSON(t.TempDir()))
	got, err := dst.Import(data)
	if err != nil {
		t.Fatal(err)
	}
	// Check only the imported copies: dst also holds its own enabled seeds.
	for _, sk := range got {
		if sk.Name == target.Name && sk.Enabled {
			t.Fatalf("imported disabled skill %q was switched back on", sk.Name)
		}
	}
}

// TestCreateDefaultsEnabled guards the availability bug: a POST that omits
// "enabled" must produce a usable skill.
func TestCreateDefaultsEnabled(t *testing.T) {
	s := newTestService(t)
	sk := &Skill{Name: "新技能", Instructions: "步骤"}
	if err := s.Create(sk); err != nil {
		t.Fatal(err)
	}
	if !sk.Enabled {
		t.Fatal("new skill should default to enabled")
	}
	if len(s.ListEnabled()) != len(s.List()) {
		t.Fatal("newly created skill is not enabled")
	}
}

// TestGetReturnsCopy guards against a caller mutating the cached entity.
func TestGetReturnsCopy(t *testing.T) {
	s := newTestService(t)
	sk, err := s.Get("skill-doc-qa")
	if err != nil {
		t.Fatal(err)
	}
	sk.Name = "被改坏了"
	again, _ := s.Get("skill-doc-qa")
	if again.Name == "被改坏了" {
		t.Fatal("Get returned a pointer into the cache")
	}
}

func TestGetManySkipsMissing(t *testing.T) {
	s := newTestService(t)
	got := s.GetMany([]string{"skill-doc-qa", "does-not-exist", "skill-brainstorm"})
	if len(got) != 2 {
		t.Fatalf("GetMany = %d, want 2 (unknown ids skipped)", len(got))
	}
}

// TestListEnabledFilter checks the assemble-path filter.
func TestListEnabledFilter(t *testing.T) {
	s := newTestService(t)
	all := s.List()
	if _, err := s.Toggle(all[0].ID); err != nil {
		t.Fatal(err)
	}
	enabled := s.ListEnabled()
	if len(enabled) != len(all)-1 {
		t.Fatalf("ListEnabled = %d, want %d", len(enabled), len(all)-1)
	}
	for _, sk := range enabled {
		if sk.ID == all[0].ID {
			t.Fatal("disabled skill still returned by ListEnabled")
		}
	}
}

// TestDirsIncludesUserDir guards the two-channel scan contract (DESIGN §14#3).
func TestDirsIncludesUserDir(t *testing.T) {
	dirs := Dirs(t.TempDir())
	if len(dirs) < 1 {
		t.Fatal("no scan dirs")
	}
	if !strings.HasSuffix(dirs[0], filepath.Join("skills")) {
		t.Errorf("first dir = %q, want <data>/skills", dirs[0])
	}
	joined := strings.Join(dirs, "|")
	if !strings.Contains(joined, ".agenthub") {
		t.Errorf("user dir missing: %v", dirs)
	}
}

// TestPersistedRoundTrip verifies skills survive a reload through the store.
func TestPersistedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(storage.NewJSON(dir))
	if err := s.Create(&Skill{Name: "持久化技能", Instructions: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	reloaded := New(storage.NewJSON(dir))
	var found bool
	for _, sk := range reloaded.List() {
		if sk.Name == "持久化技能" {
			found = true
			if !sk.Enabled {
				t.Error("reloaded skill lost its enabled flag")
			}
		}
	}
	if !found {
		t.Fatal("skill did not survive the reload")
	}
}

// TestLegacySkillDefaultsEnabled covers skills stored before the Enabled field.
func TestLegacySkillDefaultsEnabled(t *testing.T) {
	dir := t.TempDir()
	st := storage.NewJSON(dir)
	// Simulate a legacy document with no "enabled" key at all.
	legacy := map[string]interface{}{
		"id": "legacy-1", "name": "旧技能", "instructions": "x",
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "legacy-1.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = st
	s := New(storage.NewJSON(dir))
	sk, err := s.Get("legacy-1")
	if err != nil {
		t.Fatal(err)
	}
	if !sk.Enabled {
		t.Fatal("legacy skill should default to enabled")
	}
}
