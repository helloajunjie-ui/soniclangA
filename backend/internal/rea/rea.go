// Package rea builds the "port" for software that has none: a recorded,
// parameterised action list that turns a GUI-only app into something the model
// can call again without rediscovering its controls every time.
//
// The division of labour is deliberate. REA does not click anything itself — the
// control_* tools already do that, under the module switch, the allow list and the
// audit log. REA only remembers *what to do*: which window, which action, which
// steps, which parts are parameters. Execution stays with the tools that already
// have the safeguards.
package rea

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Step is one recorded operation. Ops map onto the control_* tools:
//
//	focus  -> control_focus        (target: window)
//	click  -> control_click        (target: control id or x,y)
//	type   -> control_type_text    (text, may contain ${var})
//	hotkey -> control_hotkey       (keys, e.g. "ctrl+s")
//	launch -> control_launch       (target: executable)
//	http   -> http_fetch           (url, may contain ${var}) — for apps that do
//	                                  expose a local port; probing first is the
//	                                  whole point, so this op exists.
type Step struct {
	Op     string `json:"op"`
	Target string `json:"target,omitempty"`
	Text   string `json:"text,omitempty"`
	Keys   string `json:"keys,omitempty"`
	URL    string `json:"url,omitempty"`
	// Note is shown to the model when it plans a replay: it is the place to say
	// "queue 按钮在右上角" without hard-coding coordinates in the step.
	Note string `json:"note,omitempty"`
}

// Profile is the adapter for one application.
type Profile struct {
	App        string `json:"app"` // stable id, e.g. "comfy-desktop"
	Name       string `json:"name,omitempty"`
	MatchClass string `json:"matchClass,omitempty"` // window class / process name
	MatchTitle string `json:"matchTitle,omitempty"` // window title substring
	// Probe records what recon found, so the next session does not repeat it:
	// "port 8188" beats "no API" for deciding whether to drive the GUI at all.
	// AppVersion is the software version this adapter was recorded against.
	AppVersion string            `json:"appVersion,omitempty"`
	Probe      []string          `json:"probe,omitempty"`
	Actions    map[string]Action `json:"actions"`
	// Rev increments on every save; history keeps the previous revisions.
	Rev int `json:"rev"`
	// VerifiedAt is the last time an action was confirmed to work.
	VerifiedAt int64 `json:"verifiedAt"`
	UpdatedAt  int64 `json:"updatedAt"`
}

// Action is one parameterised operation, e.g. "txt2img" with a ${prompt} step.
type Action struct {
	Description string   `json:"description,omitempty"`
	Params      []string `json:"params,omitempty"`
	Steps       []Step   `json:"steps"`
	// Verified means this action was confirmed to work at least once. An action
	// that was never confirmed is a guess; rea_plan says so instead of letting the
	// model treat a draft as a working adapter.
	Verified bool `json:"verified,omitempty"`
	// Stale marks an action whose replay failed — usually because the software
	// updated and the controls moved. The next attempt should re-record it rather
	// than retry the same steps.
	Stale bool `json:"stale,omitempty"`
	// LastNote carries the last observation ("窗口标题变了"), shown by rea_plan.
	LastNote string `json:"lastNote,omitempty"`
}

// Service stores profiles under data/rea and resolves their parameters.
type Service struct {
	dir   string
	mu    sync.RWMutex
	cache map[string]*Profile
}

// New loads the profiles already on disk.
func New(dir string) *Service {
	s := &Service{dir: dir, cache: map[string]*Profile{}}
	_ = os.MkdirAll(dir, 0o755)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return s
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var p Profile
		if json.Unmarshal(b, &p) == nil && p.App != "" {
			s.cache[p.App] = &p
		}
	}
	return s
}

// List returns the known profiles, sorted by app id.
func (s *Service) List() []Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Profile, 0, len(s.cache))
	for _, p := range s.cache {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out
}

// Get returns one profile.
func (s *Service) Get(app string) (*Profile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.cache[app]
	if !ok {
		return nil, fmt.Errorf("没有 %q 的 REA 档案；先用 rea_save 录一份", app)
	}
	return cloneProfile(p), nil
}

// cloneProfile deep-copies a profile. The struct copy alone is not enough: Actions
// is a map and Step slices are shared, so a caller editing a profile it "read"
// would silently rewrite the cache — and the revision archived by the next Save
// would hold the new content instead of the old one.
func cloneProfile(p *Profile) *Profile {
	cp := *p
	cp.Probe = append([]string(nil), p.Probe...)
	cp.Actions = make(map[string]Action, len(p.Actions))
	for k, a := range p.Actions {
		a.Steps = append([]Step(nil), a.Steps...)
		a.Params = append([]string(nil), a.Params...)
		cp.Actions[k] = a
	}
	return &cp
}

// Save stores a profile. App must be a plain id: it becomes a file name.
func (s *Service) Save(p *Profile) error {
	if !validApp(p.App) {
		return fmt.Errorf("app 只能是字母、数字、- 和 _：%q", p.App)
	}
	if len(p.Actions) == 0 {
		return fmt.Errorf("至少要有 1 个 action")
	}
	for name, a := range p.Actions {
		if len(a.Steps) == 0 {
			return fmt.Errorf("action %q 没有步骤", name)
		}
		for i, st := range a.Steps {
			if !validOp(st.Op) {
				return fmt.Errorf("action %q 第 %d 步的 op %q 不支持（可用：focus/click/type/hotkey/launch/http）", name, i+1, st.Op)
			}
		}
	}
	// Archive the revision being replaced before writing the new one. Kept short on
	// purpose: this is a history of "what changed when a release broke the
	// adapter", not a version control system.
	if prev, ok := s.cache[p.App]; ok {
		_ = os.MkdirAll(filepath.Join(s.dir, "history", p.App), 0o755)
		if b, err := json.MarshalIndent(prev, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(s.dir, "history", p.App, fmt.Sprintf("rev-%03d.json", prev.Rev)), b, 0o644)
		}
		p.Rev = prev.Rev + 1
	} else {
		p.Rev = 1
	}
	if p.VerifiedAt == 0 {
		p.VerifiedAt = time.Now().UnixMilli()
	}
	p.UpdatedAt = time.Now().UnixMilli()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, p.App+".json.tmp")
	final := filepath.Join(s.dir, p.App+".json")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[p.App] = cloneProfile(p)
	s.mu.Unlock()
	return nil
}

// Mark records the outcome of a replay: ok=true confirms the action, ok=false
// marks it stale. This is the feedback that keeps recorded adapters honest —
// without it a profile keeps looking valid long after the app changed.
func (s *Service) Mark(app, action string, ok bool, note string) error {
	s.mu.Lock()
	p, exists := s.cache[app]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("没有 %q 的档案", app)
	}
	a, hasAction := p.Actions[action]
	if !hasAction {
		s.mu.Unlock()
		return fmt.Errorf("%s 没有 action %q", app, action)
	}
	a.Verified = ok
	a.Stale = !ok
	a.LastNote = note
	p.Actions[action] = a
	cp := cloneProfile(p)
	if ok {
		cp.VerifiedAt = time.Now().UnixMilli()
	}
	s.mu.Unlock()
	return s.Save(cp)
}

// Resolve substitutes ${var} placeholders and returns the concrete steps to run.
//
// Missing parameters are an error rather than an empty string: a click with an
// empty text box looks like it worked and produces the wrong result.
func (s *Service) Resolve(app, action string, vars map[string]string) ([]Step, error) {
	p, err := s.Get(app)
	if err != nil {
		return nil, err
	}
	a, ok := p.Actions[action]
	if !ok {
		names := make([]string, 0, len(p.Actions))
		for n := range p.Actions {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%s 没有 action %q（可用：%s）", app, action, strings.Join(names, ", "))
	}
	out := make([]Step, 0, len(a.Steps))
	for _, st := range a.Steps {
		st.Target = subst(st.Target, vars)
		st.Text = subst(st.Text, vars)
		st.Keys = subst(st.Keys, vars)
		st.URL = subst(st.URL, vars)
		if strings.Contains(st.Target+st.Text+st.Keys+st.URL, "${") {
			return nil, fmt.Errorf("参数没给全：步骤还残留占位符（%s）", strings.Join(a.Params, ", "))
		}
		out = append(out, st)
	}
	return out, nil
}

func subst(s string, vars map[string]string) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}

func validApp(app string) bool {
	if app == "" || len(app) > 48 {
		return false
	}
	for _, c := range app {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func validOp(op string) bool {
	switch op {
	case "focus", "click", "type", "hotkey", "launch", "http":
		return true
	}
	return false
}
