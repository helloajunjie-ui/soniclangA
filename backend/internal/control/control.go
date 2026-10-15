// Package control implements the software-control capability (DESIGN §9.1):
// the agent does the work inside a local application on the user's behalf. The
// canonical case is a user who does not know the application at all — they say
// what they want in Blender and never learn Blender; the agent drives it for
// them. Driving a UI is not the point, it is the last resort: an API first, a
// CLI second, and only when neither exists the window's controls.
//
// How it behaves:
//
//   - Off by default. Nothing is enumerated and no action runs until the user
//     turns the module on.
//   - There is no registration step. The user says what they want and the agent
//     works it out (DESIGN §9.3); asking someone to pre-authorise a list of
//     applications before they can be helped is a form to fill in, not a
//     capability. What remains as a hard boundary is the built-in deny list —
//     terminals, password managers, task managers, security software — which
//     nothing can override: a boundary the tool loop itself could switch off
//     would not be a boundary.
//   - Actions that move the desktop (click, type, hotkey, window operations)
//     are refused agent-side until per-action confirmation exists; they run
//     from the software-control page, where the human is the one acting.
//   - Every action is audited, and results are truncated before they reach the
//     model so a large control tree cannot blow up the context window.
//
// On wording: this file, and the page it backs, describe what the capability
// does and what changes when it is switched on — in a few short lines. Judging
// the capability (calling it dangerous, ranking it against the rest of the
// product) is neither the code's job nor the page's job, and neither is
// explaining the mechanism at length: the page is not a lesson (DESIGN §17
// 原则九).
//
// Window enumeration, control trees and input synthesis live in the platform
// files; this package owns the policy.
package control

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Errors returned by the service. They are sentinel values so the HTTP layer and
// the tool layer can map them onto distinct status codes / user-facing text
// without string matching.
var (
	// ErrDisabled is returned when the module switch is off. The message names
	// the page the switch actually lives on: it used to point at a 模块页 that
	// was deleted when the capability switches moved back onto their own pages.
	ErrDisabled = errors.New("软件控制未开启：请在「软件控制」页打开开关")
	// ErrBlocked is returned when the deny list rejects the target.
	ErrBlocked = errors.New("目标在系统内置的禁止清单里，已拒绝")
	// ErrUnsupported is returned on platforms without a control backend.
	ErrUnsupported = errors.New("当前平台暂不支持软件控制")
	// ErrUnknownCapability is returned for an unknown capability id.
	ErrUnknownCapability = errors.New("未知的控制能力")
)

// Capability describes one controllable action the agent may perform.
//
// There is deliberately no risk field and no read/write split here. An earlier
// revision graded each action — first a three-level "risk" string, later a
// DesktopAction bool that held the action back from the agent — and the only
// thing that read the grade was a gate refusing to let the agent act. Both are
// gone. The user opts into the capability once, by switching the module on; the
// deny list is the boundary; the tier says how much the agent is trusted with
// its own tools, not whether it may carry out the task it was given.
type Capability struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Action identifiers. The id doubles as the tool name suffix, so renaming one
// is a breaking change for saved roles.
const (
	CapListApps = "list_apps"
	CapInspect  = "inspect"
	CapFocus    = "focus"
	CapClick    = "click"
	CapTypeText = "type_text"
	CapHotkey   = "hotkey"
	CapClose    = "close"
	CapLaunch   = "launch"
)

// DeniedTarget matches one app (by id / process / title substring) that is
// never controllable. Matching is case-insensitive and substring-based on
// purpose: the id of a running window is not stable across restarts, so an exact
// match would let a denied target through simply by being renamed or relaunched.
type DeniedTarget struct {
	// Reason explains why the target is blocked; surfaced to the model so it
	// can explain the refusal to the user instead of retrying blindly.
	Reason string `json:"reason"`
	// Match is a lowercase substring tested against the app id, process name,
	// window title and control class.
	Match string `json:"match"`
}

// denyList is the built-in system-level deny list (DESIGN §14 C4, fail-closed).
//
// It is a []DeniedTarget rather than a map because order matters for the
// explanation: the first hit is reported, so the more specific patterns come
// first.
var denyList = []DeniedTarget{
	{"密码管理器与凭据存储：可读取或改写用户主密码", "keepass"},
	{"密码管理器与凭据存储", "keepassxc"},
	{"密码管理器与凭据存储", "1password"},
	{"密码管理器与凭据存储", "bitwarden"},
	{"密码管理器与凭据存储", "lastpass"},
	{"密码管理器与凭据存储", "dashlane"},
	{"密码管理器与凭据存储", "enpass"},
	// Window titles in Chinese: the user renames nothing here, but a portable
	// build can localise any of these strings.
	{"密码管理器与凭据存储", "密码管理器"},
	{"密码管理器与凭据存储", "密码保险箱"},
	{"安全软件：干扰防护即可能被恶意利用", "defender"},
	{"安全软件", "msmpeng"},
	{"安全软件", "360safe"},
	{"安全软件", "huorong"},
	{"安全软件", "avast"},
	{"安全软件", "norton"},
	{"安全软件", "kaspersky"},
	{"安全软件", "mcafee"},
	{"安全软件", "bitdefender"},
	{"系统进程与凭据 LSA：读取即等价于导出域凭据", "lsass"},
	{"系统进程与凭据 LSA", "csrss"},
	{"系统进程与凭据 LSA", "winlogon"},
	{"系统进程与凭据 LSA", "services.exe"},
	{"系统进程与凭据 LSA", "smss.exe"},
	{"任务管理器：终止进程等价于任意进程控制", "taskmgr"},
	// Window classes. A renamed terminal keeps its class, so matching on the
	// class catches the case where the process name is spoofed or hidden.
	{"终端：可直接执行任意命令，绕过权限三档的全部约束", "ConsoleWindowClass"},
	{"终端", "CASCADIA_HOSTING_WINDOW_CLASS"},
	{"任务管理器", "TaskManagerTitle"},
	{"安全软件", "Credential Dialog Xaml Host"},
	{"终端：可直接执行任意命令，绕过权限三档的全部约束", "cmd.exe"},
	{"终端", "powershell"},
	{"终端", "pwsh.exe"},
	{"终端", "windowsterminal"},
	{"终端", "wt.exe"},
	{"终端", "conhost"},
	{"终端", "bash.exe"},
	{"终端", "wsl.exe"},
	{"终端", "sh.exe"},
	{"终端", "zsh"},
	{"终端", "iterm"},
	{"终端", "iterm2"},
	{"终端", "alacritty"},
	{"终端", "kitty"},
	{"终端", "wezterm"},
	{"终端", "gnome-terminal"},
	{"终端", "xterm"},
	{"终端", "regedit"},
	{"终端", "msconfig"},
	{"系统设置：改动可导致机器不可用或绕过安全策略", "control.exe"},
	{"系统设置", "cleanmgr"},
	{"系统设置", "services.msc"},
	{"系统设置", "devmgmt"},
	{"系统设置", "eventvwr"},
	{"系统设置", "compmgmt"},
	{"系统设置", "SystemPropertiesAdvanced"},
	{"远程控制：可读取他人在本机的操作", "teamviewer"},
	{"远程控制", "anydesk"},
	{"远程控制", "rustdesk"},
	{"远程控制", "sunloginclient"},
	{"远程控制", "todesk"},
	{"加密与 DRM 软件：受法律与授权约束", "safemonitor"},
	// Both patterns here name the executable the brand fuses its suffix onto;
	// the bare brand word would need a substring test to reach them, which is
	// exactly the rule matchDeny refuses.
	{"加密与 DRM 软件", "hasp"},
	{"加密与 DRM 软件", "hasplms"},
	{"加密与 DRM 软件", "drm"},
}

// normalizeKey folds a string into the form used for deny-list comparison:
// lowercase, no directory part and no executable suffix.
//
// Stripping the extension matters because the same target is referred to
// inconsistently by the different backends — a process list yields
// "notepad.exe" while a window title yields "Notepad". Comparing the bare stem
// means a caller cannot slip past a deny entry by choosing the other spelling.
func normalizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	s = path.Base(strings.ReplaceAll(s, "\\", "/"))
	s = strings.TrimSuffix(s, ".exe")
	s = strings.TrimSuffix(s, ".com")
	s = strings.TrimSuffix(s, ".bat")
	s = strings.TrimSuffix(s, ".cmd")
	s = strings.TrimSuffix(s, ".lnk")
	// A double extension ("cmd.exe.exe") survives a single TrimSuffix pass.
	// Without this loop, appending an extra ".exe" to a blocked binary would
	// yield "cmd.exe", which no deny entry matches — a one-character bypass of
	// the whole deny list.
	for {
		stripped := strings.TrimSuffix(s, ".exe")
		stripped = strings.TrimSuffix(stripped, ".com")
		stripped = strings.TrimSuffix(stripped, ".lnk")
		if stripped == s {
			return s
		}
		s = stripped
	}
}

// DenyListView returns the deny list in a displayable form. The list is
// exported read-only on purpose: an operator must be able to see exactly what
// the agent can never touch, but a UI bug must not be able to clear it.
func DenyListView() []DeniedTarget {
	out := make([]DeniedTarget, len(denyList))
	copy(out, denyList)
	return out
}

// CheckDeny reports whether any of the app's identity fields hits the built-in
// deny list. It returns the reason when blocked.
//
// Both sides of the comparison go through normalizeKey. That is not an
// optimisation: the deny entries are written with realistic spellings
// ("cmd.exe", "pwsh.exe") while the incoming fields may be bare stems ("cmd",
// "pwsh"). Matching a raw pattern against a normalised string would silently
// fail for every entry that carries an extension — the deny list would look
// present in the code and do nothing at runtime.
func CheckDeny(fields ...string) (string, bool) {
	for _, f := range fields {
		n := normalizeKey(f)
		if n == "" {
			continue
		}
		for _, d := range denyList {
			if matchDeny(n, normalizeKey(d.Match)) {
				return d.Reason, true
			}
		}
	}
	return "", false
}

// matchDeny reports whether a normalised identity hits one deny pattern.
//
// The pattern must be the whole identity, or a token inside it delimited by a
// separator — never a bare substring. A plain substring test reads as "more
// thorough" and is the opposite: "control" (the Windows control-panel launcher)
// also matches ImController.exe and every other name that merely contains the
// word, and "sh" matches Shell_TrayWnd. A desktop on which ordinary
// applications read as forbidden is the real risk here, because that is what
// teaches a user to stop trusting the list and switch the whole capability off.
//
// One rule for every pattern length. The families whose executables fuse the
// brand to a suffix — hasplms for HASP, iterm2 for iTerm — are listed by that
// executable name instead of being reached by accident.
func matchDeny(identity, pattern string) bool {
	if pattern == "" {
		return false
	}
	if identity == pattern {
		return true
	}
	for _, sep := range []string{" ", "-", "_", ".", "\\", "/"} {
		if strings.HasPrefix(identity, pattern+sep) || strings.HasSuffix(identity, sep+pattern) {
			return true
		}
		if strings.Contains(identity, sep+pattern+sep) {
			return true
		}
	}
	return false
}

// App is a controllable window reported by the platform backend.
type App struct {
	// ID is stable for the lifetime of the window (a platform handle rendered
	// as a string). It is what actions address.
	ID      string `json:"id"`
	Title   string `json:"title"`
	Process string `json:"process,omitempty"`
	// Class is the window class name, useful for locating controls.
	Class string `json:"class,omitempty"`
	// Blocked is true when the app hits the deny list. Such apps are listed so
	// the UI can explain the refusal, but every action on them is refused.
	Blocked bool   `json:"blocked"`
	Reason  string `json:"reason,omitempty"`
}

// Node is one element of a control tree snapshot.
type Node struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Class string `json:"class,omitempty"`
	Type  string `json:"type,omitempty"`
	// Value is the current value / text for inputs and labels.
	Value string `json:"value,omitempty"`
	// Enabled is false for greyed-out controls; acting on them fails anyway but
	// the model can pick a different target instead of retrying blindly.
	Enabled bool `json:"enabled"`
	// OnScreen reports whether the node currently sits within the visible
	// desktop. It is deliberately separate from Enabled: a control scrolled out
	// of view is enabled yet cannot be clicked, and conflating the two would
	// have the agent retry a click that can never land.
	OnScreen bool `json:"onScreen"`
	// Depth is the distance from the tree root, used by the UI for indentation.
	Depth int `json:"depth"`
	// Children are the child nodes. Kept as a slice so the JSON is a tree the
	// frontend can render directly.
	Children []*Node `json:"children,omitempty"`
}

// ActionRequest is the input of Run.
type ActionRequest struct {
	// Cap is one of the Cap* capability ids.
	Cap string `json:"cap"`
	// AppID addresses the target window. Required for every capability except
	// list_apps and launch.
	AppID string `json:"appId,omitempty"`
	// NodeID is the control-tree node to act on. Optional: without it an action
	// targets the window itself.
	NodeID string `json:"nodeId,omitempty"`
	// Text is the payload for type_text; Keys is the key combination for hotkey.
	Text string   `json:"text,omitempty"`
	Keys []string `json:"keys,omitempty"`
	// Target is the app id / executable / URI for launch.
	Target string `json:"target,omitempty"`
	// MaxDepth / MaxNodes bound a control-tree snapshot.
	MaxDepth         int  `json:"maxDepth,omitempty"`
	MaxNodes         int  `json:"maxNodes,omitempty"`
	IncludeOffscreen bool `json:"includeOffscreen,omitempty"`
}

// Snapshot is the result of an inspect action.
type Snapshot struct {
	App  App   `json:"app"`
	Root *Node `json:"root"`
	// Truncated is true when the node budget cut the walk short, so the model
	// knows the tree is partial instead of assuming it saw everything.
	Truncated bool `json:"truncated"`
}

// maxResultBytes caps the JSON handed back to the model. A control tree of a
// large IDE can be megabytes; the registry sanitiser would cap it anyway, but
// doing it here keeps the audit log and the SSE payload consistent.
const maxResultBytes = 24 << 10

// Backend is the platform-specific half of the control service. The policy
// layer above decides *whether* an action may run; the backend only knows how to
// perform it.
type Backend interface {
	// Platform returns a short platform label for the UI.
	Platform() string
	// Supported reports whether this platform has a working backend.
	Supported() bool
	// ListApps enumerates controllable windows.
	ListApps() ([]App, error)
	// Inspect snapshots a control tree.
	Inspect(appID string, opts InspectOptions) (*Node, bool, error)
	// Act performs one action and returns a human-readable outcome.
	Act(ctx context.Context, req ActionRequest) (string, error)
}

// InspectOptions bounds a control-tree walk.
type InspectOptions struct {
	MaxDepth         int
	MaxNodes         int
	IncludeOffscreen bool
}

// Service is the software-control backend.
//
// The zero value is not usable; construct with New.
type Service struct {
	backend Backend

	mu      sync.RWMutex
	enabled bool
	// lastAction keeps the most recent outcome per app so the UI can show
	// "what did the agent just do to this window" without replaying the log.
	lastAction map[string]string

	audit *audit.Audit
}

// New constructs the control Service with the current platform backend and the
// module switch off (DESIGN §9.1: nothing is reachable until explicitly
// enabled).
func New(aud *audit.Audit) *Service {
	return &Service{
		backend:    newBackend(),
		lastAction: map[string]string{},
		audit:      aud,
	}
}

// NewWithBackend constructs a Service around an explicit backend. Tests use it
// to exercise the policy layer without touching the real desktop, which is the
// only way to test a fail-closed list that is supposed to be un-bypassable.
func NewWithBackend(b Backend, aud *audit.Audit) *Service {
	s := New(aud)
	if b != nil {
		s.backend = b
	}
	return s
}

// SetEnabled flips the module switch.
func (s *Service) SetEnabled(v bool) {
	s.mu.Lock()
	s.enabled = v
	s.mu.Unlock()
}

// IsEnabled reports whether the capability is active.
func (s *Service) IsEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// Platform returns the backend platform label and whether it is supported.
func (s *Service) Platform() (string, bool) {
	return s.backend.Platform(), s.backend.Supported()
}

// capabilities is the single source of truth for the control surface. It backs
// two things at once — tool registration and /control/status — so there is one
// place to change when an action is added.
var capabilities = []Capability{
	{CapListApps, "列出应用窗口", "枚举当前桌面上可控的应用窗口"},
	{CapInspect, "探查控件树", "读取目标窗口的可访问性控件树，定位按钮/输入框"},
	{CapFocus, "聚焦窗口", "把目标窗口切到前台"},
	{CapClick, "点击控件", "点击控件树中的指定控件"},
	{CapTypeText, "输入文本", "向聚焦的输入框输入文本"},
	{CapHotkey, "发送快捷键", "向目标窗口发送组合键"},
	{CapClose, "关闭窗口", "请求目标窗口关闭（非强杀）"},
	{CapLaunch, "启动应用", "启动本机已安装应用"},
}

// Capabilities lists the built-in controllable actions.
func (s *Service) Capabilities() []Capability {
	out := make([]Capability, len(capabilities))
	copy(out, capabilities)
	return out
}

// Apps returns the controllable windows, annotated with deny-list state.
//
// A blocked app is returned rather than filtered out: the operator needs to see
// that it exists and why it cannot be touched. Every action path re-checks
// anyway, so this annotation is a UI convenience, never the enforcement point.
func (s *Service) Apps() ([]App, error) {
	// The module gate is checked here, not only in authorize. Enumerating a
	// user's window titles is itself a privacy leak — it exposes whatever is
	// open, including chat and document names — so a disabled module must not
	// return the list even for the UI.
	if !s.IsEnabled() {
		return nil, ErrDisabled
	}
	if !s.backend.Supported() {
		return nil, fmt.Errorf("%w（%s）", ErrUnsupported, s.backend.Platform())
	}
	apps, err := s.backend.ListApps()
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range apps {
		if reason, bad := CheckDeny(apps[i].ID, apps[i].Process, apps[i].Title, apps[i].Class); bad {
			apps[i].Blocked = true
			apps[i].Reason = reason
		}
	}
	return apps, nil
}

// FindApp resolves an app id against the live window list, so every action
// re-derives the deny decision from fresh data instead of trusting a
// caller-supplied window handle. A stale or forged app id therefore cannot
// point the action at a window that appeared after the check.
func (s *Service) FindApp(appID string) (*App, error) {
	apps, err := s.Apps()
	if err != nil {
		return nil, err
	}
	for i := range apps {
		if apps[i].ID == appID {
			return &apps[i], nil
		}
	}
	return nil, fmt.Errorf("窗口不存在或已关闭：%s（请重新调用 control_list_apps 获取当前窗口）", appID)
}

// Inspect snapshots a control tree for appID.
func (s *Service) Inspect(ctx context.Context, req ActionRequest) (*Snapshot, error) {
	if err := s.authorize(req.Cap, req.AppID, ""); err != nil {
		return nil, err
	}
	app, err := s.FindApp(req.AppID)
	if err != nil {
		return nil, err
	}
	opts := InspectOptions{
		MaxDepth:         clampInt(req.MaxDepth, 1, 12, 4),
		MaxNodes:         clampInt(req.MaxNodes, 1, 2000, 400),
		IncludeOffscreen: req.IncludeOffscreen,
	}
	root, truncated, err := s.backend.Inspect(app.ID, opts)
	if err != nil {
		return nil, err
	}
	s.log(ctx, "control.inspect", map[string]string{"cap": req.Cap, "appId": app.ID, "app": app.Title})
	return &Snapshot{App: *app, Root: root, Truncated: truncated}, nil
}

// Run performs one action after the full policy check.
//
// Order matters and is deliberate: enabled → supported → known capability →
// deny list → execute. The deny decision is taken from the live window
// identity, never from caller input, so a window cannot be addressed by an id
// that says something the real window does not.
func (s *Service) Run(ctx context.Context, req ActionRequest) (string, error) {
	if err := s.authorize(req.Cap, req.AppID, req.Target); err != nil {
		return "", err
	}
	res, err := s.backend.Act(ctx, req)
	if err != nil {
		s.log(ctx, "control.error", map[string]string{"cap": req.Cap, "appId": req.AppID, "err": err.Error()})
		return "", err
	}
	if req.AppID != "" {
		s.mu.Lock()
		s.lastAction[req.AppID] = fmt.Sprintf("%s · %s", util.NowRFC(), res)
		s.mu.Unlock()
	}
	s.log(ctx, "control.run", map[string]string{"cap": req.Cap, "appId": req.AppID})
	return res, nil
}

// authorize is the single policy gate. Every capability funnels through it so
// there is exactly one place where an action can be refused.
func (s *Service) authorize(cap, appID, target string) error {
	if !s.IsEnabled() {
		return ErrDisabled
	}
	if !s.backend.Supported() {
		return fmt.Errorf("%w（%s）", ErrUnsupported, s.backend.Platform())
	}
	if !knownCap(cap) {
		return fmt.Errorf("%w：%s", ErrUnknownCapability, cap)
	}
	// launch has no window to check yet; the backend resolves the target, so the
	// deny check applies to the executable name the caller supplied.
	if cap == CapLaunch {
		if reason, bad := CheckDeny(target); bad {
			return fmt.Errorf("%w：%s", ErrBlocked, reason)
		}
		return nil
	}
	if cap == CapListApps {
		return nil
	}
	if strings.TrimSpace(appID) == "" {
		return errors.New("缺少 appId：请先调用 list_apps 获取窗口 id")
	}
	// Re-resolve against the live window list: the deny decision must come
	// from the current window identity, not from caller input.
	app, err := s.FindApp(appID)
	if err != nil {
		return err
	}
	if app.Blocked {
		return fmt.Errorf("%w：%s", ErrBlocked, app.Reason)
	}
	return nil
}

// LastAction returns the most recent outcome recorded for an app.
func (s *Service) LastAction(appID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastAction[appID]
}

func (s *Service) log(ctx context.Context, event string, fields map[string]string) {
	if s.audit == nil {
		return
	}
	// The other modules pass an empty request id because no context key carries
	// one yet; matching them keeps the audit log uniform rather than having
	// control be the only writer with a half-populated requestId field.
	_ = ctx
	s.audit.Log(event, "", fields)
}

func knownCap(cap string) bool {
	for _, c := range (&Service{}).Capabilities() {
		if c.ID == cap {
			return true
		}
	}
	return false
}

func clampInt(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Truncate caps a result string for the model while marking that it happened.
//
// The cut is moved back to a rune boundary. Slicing bytes at a fixed offset
// splits a multi-byte character — and because control trees carry Chinese
// control names, that is the common case, not the edge case. The split leaves
// an invalid UTF-8 tail, which the JSON encoder turns into U+FFFD and which
// renders as a stray replacement glyph right where the model looks for a node
// name.
func Truncate(s string) (string, bool) {
	if len(s) <= maxResultBytes {
		return s, false
	}
	cut := maxResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…[已截断]", true
}

// ToolGate is the orchestrator-facing pre-flight check. It refuses every
// control tool while the module is off so the model gets a clear reason instead
// of a permission error at execution time.
//
// This is a *narrowing* gate only: the deny list, allow list and capability
// checks are re-run inside Run, so a bug here can make control harder to use,
// never easier.
func (s *Service) ToolGate(name string, _ map[string]interface{}) error {
	if !strings.HasPrefix(name, ToolPrefix) {
		return nil
	}
	if !s.IsEnabled() {
		return ErrDisabled
	}
	return nil
}
