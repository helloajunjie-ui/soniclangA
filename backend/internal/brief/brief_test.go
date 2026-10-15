package brief

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newSvc(t *testing.T) *Service {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// The whole reason this store exists is that the user keeps talking while
// something is being prepared. Appending must not lose what was written before,
// and must not require the caller to re-send the existing note.
func TestAppendKeepsEarlierNotes(t *testing.T) {
	s := newSvc(t)
	b, err := s.Create("2.5D 游戏")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Append(b.ID, "主角是个会滑铲的邮差。"); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	got, err := s.Append(b.ID, "整体色调偏黄昏。")
	if err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	for _, want := range []string{"2.5D 游戏", "邮差", "黄昏"} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("brief lost %q after appending: %q", want, got.Content)
		}
	}
	if strings.Count(got.Content, "# 2.5D 游戏") != 1 {
		t.Errorf("appending duplicated the title: %q", got.Content)
	}
}

// An id becomes a file name. A traversal attempt must be refused rather than
// quietly rewritten, otherwise the caller cannot find the note it just wrote.
func TestIDsCannotEscapeTheDirectory(t *testing.T) {
	s := newSvc(t)
	for _, bad := range []string{"../escape", `..\escape`, "a/b", "", strings.Repeat("x", 65), "有中文"} {
		if _, err := s.Write(bad, "x"); err != ErrBadID {
			t.Errorf("Write(%q) must be refused, got %v", bad, err)
		}
	}
	if _, err := s.Get("../settings"); err != ErrBadID {
		t.Errorf("Get must refuse a traversal id, got %v", err)
	}
}

func TestMissingBriefIsReportedNotCreated(t *testing.T) {
	s := newSvc(t)
	if _, err := s.Get("20260101-000000"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.Append("20260101-000000", "x"); err != ErrNotFound {
		t.Fatalf("append to a missing brief must fail rather than create one, got %v", err)
	}
}

// Writing a body by hand should not leave the file with two titles, and writing
// a bare body should still keep the note identifiable.
func TestWriteNormalisesTheTitle(t *testing.T) {
	s := newSvc(t)
	b, _ := s.Create("灵感")
	got, err := s.Write(b.ID, "只写了一句话")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasPrefix(got.Content, "# 灵感") {
		t.Errorf("a bare body must keep the original title, got %q", got.Content)
	}
	got, err = s.Write(b.ID, "# 换个名字\n\n正文")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Count(got.Content, "# ") != 1 || !strings.Contains(got.Content, "换个名字") {
		t.Errorf("a body with its own heading must replace the title, got %q", got.Content)
	}
}

// Files are the interface: they must be readable and editable without the
// product, so the store has to pick up changes made outside it.
func TestExternallyEditedBriefIsPickedUp(t *testing.T) {
	s := newSvc(t)
	b, _ := s.Create("点子")
	p := filepath.Join(s.dir, b.ID+".md")
	if err := os.WriteFile(p, []byte("# 手改的标题\n\n外面写的正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "手改的标题" || !strings.Contains(got.Content, "外面写的正文") {
		t.Errorf("external edit not visible: %+v", got)
	}
	if got.CreatedAt == "" {
		t.Error("creation time must survive an external edit (it comes from the id)")
	}
}

func TestListIsNewestFirstAndSkipsContent(t *testing.T) {
	s := newSvc(t)
	a, _ := s.Create("早")
	if _, err := s.Append(a.ID, "早的内容"); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Create("晚")
	if _, err := s.Append(b.ID, "晚的内容"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 briefs, got %d", len(list))
	}
	if list[0].ID < list[1].ID {
		t.Errorf("list must be newest first, got %s then %s", list[0].ID, list[1].ID)
	}
	for _, it := range list {
		if it.Content != "" {
			t.Errorf("listing must not carry content: %+v", it)
		}
	}
}

func TestDisabledStoreRefusesWithoutPanicking(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if s.Enabled() {
		t.Fatal("an empty dir must mean disabled")
	}
	if _, err := s.Create("x"); err == nil {
		t.Error("Create must fail when disabled")
	}
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Errorf("List must be empty and error-free when disabled, got %v %v", list, err)
	}
}
