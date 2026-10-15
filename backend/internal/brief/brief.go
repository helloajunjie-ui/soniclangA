// Package brief keeps the sheet of paper the user and the agent write on while
// nothing can be built yet.
//
// The flow it serves (DESIGN §9.3) has a waiting room: the request matched
// something that has to be installed, or something that has to be worked out
// before it can be driven, and that takes time. Rather than leave the user
// staring at a spinner, the agent keeps talking to them and writes down what
// they want — the characters, the look, the features, the names. When the
// software is ready the notes already exist, so work starts from a brief
// instead of from a blank page.
//
// Files are plain Markdown under <data>/briefs/<id>.md. That is deliberate: the
// user can open them in any editor, and nothing about the product has to be
// running for the notes to survive.
package brief

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned when no brief carries the requested id.
var ErrNotFound = errors.New("没有这份简报")

// ErrBadID is returned for an id that is not a plain file name. Ids come from
// callers and end up as path segments, so anything that could climb out of the
// directory is refused rather than sanitised — a silently rewritten id would
// write the brief somewhere the caller cannot find again.
var ErrBadID = errors.New("简报 id 不合法")

// Brief is one note. Content is only populated by Get; List omits it so that a
// long note does not ride along in every listing.
type Brief struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Path      string `json:"path"`
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Bytes     int    `json:"bytes"`
	Content   string `json:"content,omitempty"`
}

// Service owns the briefs directory.
type Service struct {
	dir string
}

// New prepares the store under dir. An empty dir disables the store, which is
// what the module wiring passes when the feature is off.
func New(dir string) (*Service, error) {
	if dir == "" {
		return &Service{}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建简报目录失败：%w", err)
	}
	return &Service{dir: dir}, nil
}

// Enabled reports whether the store is usable.
func (s *Service) Enabled() bool { return s != nil && s.dir != "" }

func (s *Service) path(id string) (string, error) {
	if !validID(id) {
		return "", ErrBadID
	}
	return filepath.Join(s.dir, id+".md"), nil
}

// validID accepts the ids this package issues: digits, dashes, and the letters
// used by hand-written ones.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// Create starts a note and returns it. The id is derived from the clock so the
// directory sorts chronologically, with a suffix only when two land in the same
// second.
func (s *Service) Create(title string) (Brief, error) {
	if !s.Enabled() {
		return Brief{}, errors.New("简报存储未启用")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "未命名"
	}
	base := time.Now().Format("20060102-150405")
	id := base
	for i := 2; ; i++ {
		p, err := s.path(id)
		if err != nil {
			return Brief{}, err
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			break
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
	path, _ := s.path(id)
	if err := os.WriteFile(path, []byte("# "+title+"\n"), 0o644); err != nil {
		return Brief{}, fmt.Errorf("写入简报失败：%w", err)
	}
	return s.Get(id)
}

// Get reads one brief.
func (s *Service) Get(id string) (Brief, error) {
	if !s.Enabled() {
		return Brief{}, errors.New("简报存储未启用")
	}
	p, err := s.path(id)
	if err != nil {
		return Brief{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Brief{}, ErrNotFound
		}
		return Brief{}, err
	}
	st, _ := os.Stat(p)
	out := Brief{ID: id, Path: p, Content: string(b), Title: titleOf(string(b)), Bytes: len(b)}
	if st != nil {
		out.UpdatedAt = st.ModTime().Format(time.RFC3339)
	}
	// The id is the creation timestamp, so the created date survives edits
	// without being written into the file. A hand-made id simply has none.
	out.CreatedAt = createdFromID(id)
	return out, nil
}

// createdFromID recovers the creation time from an id this package issued.
func createdFromID(id string) string {
	base := id
	if i := strings.LastIndex(base, "-"); i > 0 {
		// Only strip the de-duplication suffix, which is short and numeric; a
		// date-time id keeps its single dash.
		if n := base[i+1:]; len(n) <= 2 && allDigits(n) {
			base = base[:i]
		}
	}
	t, err := time.ParseInLocation("20060102-150405", base, time.Local)
	if err != nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Write replaces the body, keeping the title line. Passing a body that already
// begins with the title heading replaces the file wholesale, so a caller that
// rewrote everything by hand does not end up with two titles.
func (s *Service) Write(id, content string) (Brief, error) {
	if !s.Enabled() {
		return Brief{}, errors.New("简报存储未启用")
	}
	p, err := s.path(id)
	if err != nil {
		return Brief{}, err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return Brief{}, ErrNotFound
		}
		return Brief{}, err
	}
	body := strings.TrimRight(content, "\n") + "\n"
	if !strings.HasPrefix(strings.TrimSpace(body), "#") {
		cur, _ := s.Get(id)
		body = "# " + cur.Title + "\n\n" + body
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return Brief{}, fmt.Errorf("写入简报失败：%w", err)
	}
	return s.Get(id)
}

// Append adds a section to the end of a brief.
//
// Appending rather than rewriting is the normal case while collecting ideas:
// the user keeps talking, and each thought should land under the previous one
// instead of making the agent re-send the whole note it may have truncated.
func (s *Service) Append(id, text string) (Brief, error) {
	cur, err := s.Get(id)
	if err != nil {
		return Brief{}, err
	}
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return cur, nil
	}
	return s.Write(id, strings.TrimRight(cur.Content, "\n")+"\n\n"+text+"\n")
}

// List returns every brief, newest first.
func (s *Service) List() ([]Brief, error) {
	if !s.Enabled() {
		return []Brief{}, nil
	}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return []Brief{}, err
	}
	out := make([]Brief, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		b, err := s.Get(strings.TrimSuffix(e.Name(), ".md"))
		if err != nil {
			continue
		}
		b.Content = ""
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// titleOf reads the first heading, falling back to the first non-empty line.
func titleOf(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
		return line
	}
	return ""
}
