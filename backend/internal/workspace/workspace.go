// Package workspace decides where a task's files go.
//
// Local work produces artifacts (a rendered image, a script, a scraped page), and
// they need one agreed place: the tools that write files, the shell's working
// directory and REA's ${DIR} default all have to point at the same directory, or
// "it saved the file" turns into "somewhere I cannot find".
//
// Rules:
//   - the user may name a directory for the task; that directory is the workspace;
//   - otherwise each task gets its own directory under the data dir, named after
//     the conversation, created on first use;
//   - nothing here reaches outside those two places.
package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type ctxKey struct{}

// WithWorkspace attaches the resolved workspace to a tool context.
func WithWorkspace(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, dir)
}

// FromContext returns the workspace a tool should work in, or "" when none was
// resolved (tools then keep their previous behaviour).
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// Resolve returns the workspace for one task and creates it when needed.
//
// dataDir is the product's own directory; userDir is what the user asked for
// (empty when they did not). A user-named directory must already exist: silently
// creating it would turn a typo into an empty folder that looks like the real one
// and hides the mistake.
func Resolve(dataDir, convID, userDir string) (string, error) {
	if strings.TrimSpace(userDir) != "" {
		abs, err := filepath.Abs(strings.TrimSpace(userDir))
		if err != nil {
			return "", fmt.Errorf("工作空间路径无效：%w", err)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("工作空间不存在：%s", abs)
		}
		if !st.IsDir() {
			return "", fmt.Errorf("工作空间不是目录：%s", abs)
		}
		return abs, nil
	}
	if convID == "" {
		return "", fmt.Errorf("没有会话，无法确定工作空间")
	}
	dir := filepath.Join(dataDir, "workspace", safeName(convID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// safeName keeps a conversation id from turning into a path.
var unsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func safeName(id string) string {
	s := unsafe.ReplaceAllString(id, "_")
	if s == "" || s == "." || s == ".." {
		return "task"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// Artifact kinds. One directory per kind inside the workspace, so a task folder
// stays readable: images beside scripts beside downloads is how "where did that
// file go" starts.
const (
	KindImages    = "images"
	KindScripts   = "scripts"
	KindDocs      = "docs"
	KindDownloads = "downloads"
	KindLogs      = "logs"
)

var kinds = map[string]bool{
	KindImages: true, KindScripts: true, KindDocs: true,
	KindDownloads: true, KindLogs: true,
}

// Subdir returns (and creates) the directory for one artifact kind inside the
// workspace. An unknown kind is an error rather than an arbitrary new folder:
// a typo'd kind would silently scatter files across directories nobody looks in.
func Subdir(ws, kind string) (string, error) {
	if ws == "" {
		return "", fmt.Errorf("还没有工作空间")
	}
	if !kinds[kind] {
		return "", fmt.Errorf("未知的产出物类型 %q（可用：images/scripts/docs/downloads/logs）", kind)
	}
	dir := filepath.Join(ws, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
