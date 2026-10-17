package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCreatesPerTaskDir(t *testing.T) {
	dataDir := t.TempDir()
	got, err := Resolve(dataDir, "conv-123", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := filepath.Join(dataDir, "workspace", "conv-123")
	if got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("workspace was not created: %v", err)
	}
}

// The user's directory wins, and is returned as an absolute path so every tool
// agrees on it.
func TestUserDirectoryWins(t *testing.T) {
	dataDir := t.TempDir()
	userDir := t.TempDir()
	got, err := Resolve(dataDir, "conv-123", userDir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != userDir {
		t.Errorf("dir = %q, want the user's %q", got, userDir)
	}
}

// A missing directory is a typo, not an instruction to invent one: creating it
// would hide the mistake behind an empty folder that looks right.
func TestMissingUserDirectoryIsAnError(t *testing.T) {
	dataDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := Resolve(dataDir, "c", missing); err == nil {
		t.Fatal("a missing workspace was accepted")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("the missing workspace was created anyway")
	}
}

func TestUserPathMustBeDirectory(t *testing.T) {
	dataDir := t.TempDir()
	file := filepath.Join(dataDir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(dataDir, "c", file); err == nil {
		t.Fatal("a file was accepted as a workspace")
	}
}

// A conversation id must not be able to escape the data directory.
func TestConversationIDCannotEscape(t *testing.T) {
	dataDir := t.TempDir()
	got, err := Resolve(dataDir, "../../etc", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.HasPrefix(got, filepath.Join(dataDir, "workspace")) {
		t.Errorf("escaped the data dir: %q", got)
	}
	if strings.Contains(got, "..") {
		t.Errorf("path still contains a traversal: %q", got)
	}
}

func TestContextRoundTrip(t *testing.T) {
	if got := FromContext(context.Background()); got != "" {
		t.Errorf("empty context returned %q", got)
	}
	ctx := WithWorkspace(context.Background(), `E:\work`)
	if got := FromContext(ctx); got != `E:\work` {
		t.Errorf("FromContext = %q", got)
	}
	// A nil context must not panic: tools are called from tests and background
	// jobs alike.
	if got := FromContext(nil); got != "" {
		t.Errorf("nil context returned %q", got)
	}
}

func TestSubdirPerArtifactKind(t *testing.T) {
	ws := t.TempDir()
	for _, kind := range []string{KindImages, KindScripts, KindDocs, KindDownloads, KindLogs} {
		dir, err := Subdir(ws, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if dir != filepath.Join(ws, kind) {
			t.Errorf("%s -> %q", kind, dir)
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Errorf("%s was not created", kind)
		}
	}
}

// A typo'd kind must not quietly create a new folder.
func TestSubdirRejectsUnknownKind(t *testing.T) {
	ws := t.TempDir()
	for _, bad := range []string{"", "imagez", "../escape", "Images"} {
		if _, err := Subdir(ws, bad); err == nil {
			t.Errorf("kind %q was accepted", bad)
		}
	}
	if _, err := Subdir("", KindImages); err == nil {
		t.Error("an empty workspace was accepted")
	}
}
