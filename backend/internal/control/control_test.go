package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agenthub/agent-proxy/internal/tool/registry"
)

// fakeBackend stands in for the Win32 layer so the policy surface can be tested
// without a desktop. It is intentionally permissive: if a policy test passes
// here it proves the *policy* refused, not that the platform happened to fail.
type fakeBackend struct {
	apps      []App
	inspect   *Node
	acts      []ActionRequest
	actErr    error
	supported bool
	plat      string
}

func (f *fakeBackend) Platform() string {
	if f.plat == "" {
		return "fake"
	}
	return f.plat
}
func (f *fakeBackend) Supported() bool { return f.supported }

func (f *fakeBackend) ListApps() ([]App, error) {
	out := make([]App, len(f.apps))
	copy(out, f.apps)
	return out, nil
}

func (f *fakeBackend) Inspect(string, InspectOptions) (*Node, bool, error) {
	if f.inspect == nil {
		return nil, false, errors.New("no tree")
	}
	return f.inspect, false, nil
}

func (f *fakeBackend) Act(_ context.Context, req ActionRequest) (string, error) {
	f.acts = append(f.acts, req)
	if f.actErr != nil {
		return "", f.actErr
	}
	return "ok:" + req.Cap, nil
}

func newFake() *fakeBackend {
	return &fakeBackend{
		supported: true,
		apps: []App{
			{ID: "h:1", Title: "记事本", Process: "C:\\Windows\\notepad.exe", Class: "Notepad"},
			{ID: "h:2", Title: "Windows PowerShell", Process: "powershell.exe", Class: "ConsoleWindowClass"},
			{ID: "h:3", Title: "用户文件 - 密码管理器", Process: "keepass.exe", Class: "MainWindow"},
		},
		inspect: &Node{ID: "h:1", Name: "记事本", Type: "window", Depth: 0},
	}
}

func svcFor(b *fakeBackend) *Service {
	s := NewWithBackend(b, nil)
	s.SetEnabled(true)
	return s
}

// ---- deny list: the fail-closed invariant ----

// TestDenyListBlocksEachDeniedClass walks the deny list itself rather than a
// hand-picked sample. A pattern added later that matches nothing is a silent
// hole, and a sample-based test would not notice it.
func TestDenyListBlocksEachDeniedClass(t *testing.T) {
	for _, d := range denyList {
		if d.Match == "" {
			t.Fatalf("deny entry with empty match: %+v", d)
		}
		if _, blocked := CheckDeny(d.Match); !blocked {
			t.Errorf("deny entry %q does not block its own pattern", d.Match)
		}
		if d.Reason == "" {
			t.Errorf("deny entry %q has no reason; the refusal would be unexplained to the model", d.Match)
		}
	}
}

// TestDenyListResistsCosmeticSpelling covers the bypasses a prompt-injected model
// would actually try: different case, extra whitespace, a full path, an appended
// extension, or a disguised double extension.
func TestDenyListResistsCosmeticSpelling(t *testing.T) {
	cases := []string{
		"CMD.EXE", "cmd.exe", "  cmd.exe  ", `C:\Windows\System32\cmd.exe`,
		`C:\Windows\System32\CMD.EXE`, "powershell", "PowerShell.EXE",
		"C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
		"pwsh", "wt", "conhost", "taskmgr", "TASKMGR.EXE",
		"keepass", "KeepassXC.exe", "1Password.exe", "bitwarden.exe",
		"lsass", "LSASS.EXE", "defender", "MsMpEng.exe",
		"bash", "bash.EXE", "zsh", "wsl", "alacritty",
	}
	for _, c := range cases {
		if _, blocked := CheckDeny(c); !blocked {
			t.Errorf("deny list missed %q — a cosmetic spelling must not bypass it", c)
		}
	}
}

func TestDenyListAllowsOrdinaryApps(t *testing.T) {
	ok := []string{"notepad.exe", "chrome", "Code", "微信.exe", "explorer", "Calculator"}
	for _, c := range ok {
		if reason, blocked := CheckDeny(c); blocked {
			t.Errorf("deny list blocked an ordinary app %q (%s)", c, reason)
		}
	}
}

// TestDenyListDoesNotOverBlock pins the false-positive side of the trade-off.
//
// Every pattern must match a whole token, never a bare substring, so a rule for
// a generic word cannot fire on every name that happens to contain it. That
// matters more now than when the list was written: the process name it matches
// against was silently empty for a long time, so these rules were dormant, and
// a rule that over-blocks as soon as it wakes up is worse than one that never
// ran. The real system class "Shell_TrayWnd" is the original regression case —
// a substring rule flagged the entire Windows taskbar as a terminal — and
// "ImController.exe"/"LenovoVantageServices.exe" are the same mistake for the
// "control" and "services" entries, on hardware that actually ships them.
//
// A deny list that cries wolf gets switched off by the user, which disables the
// protection it was built to provide.
func TestDenyListDoesNotOverBlock(t *testing.T) {
	benign := []string{
		"Shell_TrayWnd", // Windows taskbar
		"Progman",       // desktop
		"WorkerW",
		"Chrome_WidgetWin_1",
		"ShellExperienceHost",
		"Windows.UI.Core.CoreWindow",
		"msctfms",
		"CrashHandler",
		"shareshot",
		"ImController.exe",          // Lenovo system component: contains "control"
		"LenovoVantageServices.exe", // contains "services"
		"DeviceController.exe",
	}
	for _, b := range benign {
		if reason, blocked := CheckDeny(b); blocked {
			t.Errorf("deny list false-positived on %q (%s)", b, reason)
		}
	}
}

// TestShortDenyPatternsStillMatch proves the boundary rule did not weaken the
// protection: a real shell must still be blocked, however it is spelled.
func TestShortDenyPatternsStillMatch(t *testing.T) {
	for _, c := range []string{"sh", "sh.exe", "bash", "bash.exe", "zsh", "pwsh"} {
		if _, blocked := CheckDeny(c); !blocked {
			t.Errorf("shell %q is no longer blocked after the boundary rule", c)
		}
	}
}

// A family whose executable fuses the brand to a suffix is listed by that
// executable name, since the whole-token rule will not reach it from the brand
// word alone. Losing those entries would quietly shrink the list.
func TestFusedSuffixFamiliesAreListedByTheirExecutable(t *testing.T) {
	for _, c := range []string{"hasplms", "hasplms.exe", "iterm2", "iTerm2.app"} {
		if _, blocked := CheckDeny(c); !blocked {
			t.Errorf("%q is not blocked; the family needs its executable name in the list", c)
		}
	}
}

// TestDenyListCoversWindowTitles proves the check runs on titles, not just
// process names: a terminal renamed to "Editor" is still blocked by its class,
// while a password manager is caught by its title.
func TestDenyListCoversWindowTitles(t *testing.T) {
	if _, blocked := CheckDeny("ConsoleWindowClass"); !blocked {
		t.Error("terminal window class not blocked")
	}
	if _, blocked := CheckDeny("密码管理器"); !blocked {
		t.Error("password-manager title not blocked")
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		`C:\Windows\System32\cmd.exe`: "cmd",
		"CMD.EXE":                     "cmd",
		"  pwsh.exe  ":                "pwsh",
		"/usr/bin/bash":               "bash",
		"":                            "",
	}
	for in, want := range cases {
		if got := normalizeKey(in); got != want {
			t.Errorf("normalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- module gate ----

func TestDisabledByDefaultRefusesEverything(t *testing.T) {
	b := newFake()
	s := NewWithBackend(b, nil) // deliberately not enabled
	for _, cap := range []string{CapListApps, CapInspect, CapFocus, CapClick, CapTypeText, CapHotkey, CapClose, CapLaunch} {
		_, err := s.Run(context.Background(), ActionRequest{Cap: cap, AppID: "h:1", Target: "notepad.exe"})
		if !errors.Is(err, ErrDisabled) {
			t.Errorf("cap %s: got %v, want ErrDisabled", cap, err)
		}
	}
	// Apps() must refuse too, otherwise a disabled module would still leak the
	// user's window list.
	if _, err := s.Apps(); !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrUnsupported) {
		t.Errorf("Apps() on a disabled service returned %v, want a refusal", err)
	}
	if len(b.acts) != 0 {
		t.Errorf("backend was called %d times while disabled", len(b.acts))
	}
}

func TestEnableAllowsActions(t *testing.T) {
	b := newFake()
	s := svcFor(b)
	res, err := s.Run(context.Background(), ActionRequest{Cap: CapFocus, AppID: "h:1"})
	if err != nil {
		t.Fatalf("focus on an ordinary app failed: %v", err)
	}
	if !strings.Contains(res, "ok") {
		t.Errorf("unexpected result %q", res)
	}
}

// ---- deny list enforcement in the action path ----

func TestBlockedAppCannotBeActedOn(t *testing.T) {
	b := newFake()
	s := svcFor(b)
	for _, id := range []string{"h:2", "h:3"} {
		for _, cap := range []string{CapFocus, CapClick, CapTypeText, CapHotkey, CapClose} {
			_, err := s.Run(context.Background(), ActionRequest{Cap: cap, AppID: id, Text: "x"})
			if !errors.Is(err, ErrBlocked) {
				t.Errorf("cap %s on blocked window %s: got %v, want ErrBlocked", cap, id, err)
			}
		}
	}
	if len(b.acts) != 0 {
		t.Fatalf("backend executed %d actions against blocked windows", len(b.acts))
	}
}

// TestDenyListCannotBeBypassed is the core invariant of DESIGN §14 C4. With the
// registration step gone (DESIGN §9.3) the deny list is the only standing
// boundary left on the action path, so it is the one that must be proven
// un-bypassable: every app is addressable by default, and a blocked one stays
// blocked.
func TestDenyListCannotBeBypassed(t *testing.T) {
	b := newFake()
	s := svcFor(b)

	if _, err := s.Run(context.Background(), ActionRequest{Cap: CapFocus, AppID: "h:2"}); !errors.Is(err, ErrBlocked) {
		t.Errorf("a terminal window was not refused: got %v, want ErrBlocked", err)
	}
	if len(b.acts) != 0 {
		t.Errorf("the backend was reached %d times for a blocked window", len(b.acts))
	}
	// An ordinary app needs no ceremony at all: saying what you want is the
	// whole authorisation story.
	if _, err := s.Run(context.Background(), ActionRequest{Cap: CapFocus, AppID: "h:1"}); err != nil {
		t.Fatalf("an ordinary window was refused: %v", err)
	}
}

func TestLaunchDeniesBlockedTargets(t *testing.T) {
	b := newFake()
	s := svcFor(b)
	for _, target := range []string{"cmd.exe", "powershell", "C:\\Windows\\System32\\cmd.exe", "taskmgr"} {
		if _, err := s.Run(context.Background(), ActionRequest{Cap: CapLaunch, Target: target}); !errors.Is(err, ErrBlocked) {
			t.Errorf("launch %q: got %v, want ErrBlocked", target, err)
		}
	}
	if len(b.acts) != 0 {
		t.Errorf("launch reached the backend %d times", len(b.acts))
	}
}

// ---- action request validation ----

func TestUnknownCapabilityRejected(t *testing.T) {
	s := svcFor(newFake())
	if _, err := s.Run(context.Background(), ActionRequest{Cap: "delete_everything", AppID: "h:1"}); !errors.Is(err, ErrUnknownCapability) {
		t.Errorf("got %v, want ErrUnknownCapability", err)
	}
}

func TestMissingAppIDExplains(t *testing.T) {
	s := svcFor(newFake())
	_, err := s.Run(context.Background(), ActionRequest{Cap: CapFocus})
	if err == nil {
		t.Fatal("an action without an appId was accepted")
	}
	if !strings.Contains(err.Error(), "appId") {
		t.Errorf("error %q does not tell the caller what is missing", err)
	}
}

func TestUnknownWindowExplainsHowToRecover(t *testing.T) {
	s := svcFor(newFake())
	_, err := s.Run(context.Background(), ActionRequest{Cap: CapFocus, AppID: "h:12345"})
	if err == nil {
		t.Fatal("acting on a nonexistent window succeeded")
	}
	if !strings.Contains(err.Error(), "list_apps") {
		t.Errorf("error %q should point the caller at re-listing windows", err)
	}
}

// TestForgedAppIDRejected covers a caller inventing a handle: FindApp re-resolves
// against the live list, so an id that was never enumerated cannot be reached.
func TestForgedAppIDRejected(t *testing.T) {
	s := svcFor(newFake())
	for _, id := range []string{"h:0", "h:99999", "not-a-handle", "", "0x1234"} {
		_, err := s.Run(context.Background(), ActionRequest{Cap: CapFocus, AppID: id})
		if err == nil {
			t.Errorf("forged window id %q was accepted", id)
		}
	}
}

// ---- platform support ----

func TestUnsupportedPlatformRefusesClearly(t *testing.T) {
	b := newFake()
	b.supported = false
	s := svcFor(b)
	_, err := s.Run(context.Background(), ActionRequest{Cap: CapFocus, AppID: "h:1"})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
	if _, ok := s.Platform(); ok {
		t.Error("Platform reported an unsupported backend as supported")
	}
}

// ---- inspect ----

func TestInspectRefusesBlockedWindow(t *testing.T) {
	s := svcFor(newFake())
	if _, err := s.Inspect(context.Background(), ActionRequest{Cap: CapInspect, AppID: "h:2"}); !errors.Is(err, ErrBlocked) {
		t.Errorf("got %v, want ErrBlocked — inspecting a terminal leaks its content", err)
	}
}

func TestInspectBoundsAreClamped(t *testing.T) {
	// Out-of-range bounds must be clamped rather than passed through: an
	// unbounded walk over a browser window can hang the request.
	if got := clampInt(9999, 1, 2000, 400); got != 2000 {
		t.Errorf("clampInt did not cap the node budget: %d", got)
	}
	if got := clampInt(-5, 1, 12, 4); got != 1 {
		t.Errorf("clampInt did not floor the depth: %d", got)
	}
	if got := clampInt(0, 1, 12, 4); got != 4 {
		t.Errorf("clampInt did not apply the default: %d", got)
	}
}

// ---- capability metadata ----

// The table describes what each action does, and that description is what the
// model reads when deciding whether a tool fits. It is the only classification
// left: the actions back tool registration and /control/status directly, and
// nothing here decides whether the agent is allowed to act.
func TestCapabilityMetadataIsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range (&Service{}).Capabilities() {
		if c.ID == "" || c.Name == "" || c.Description == "" {
			t.Errorf("incomplete capability metadata: %+v", c)
		}
		if seen[c.ID] {
			t.Errorf("capability %q appears twice in the table", c.ID)
		}
		seen[c.ID] = true
	}
	// The constants are how the rest of the code names actions; a row without
	// one, or a constant without a row, means the two have drifted.
	for _, id := range []string{
		CapListApps, CapInspect, CapFocus, CapClick,
		CapTypeText, CapHotkey, CapClose, CapLaunch,
	} {
		if !seen[id] {
			t.Errorf("capability %q has a constant but no row in the table", id)
		}
		delete(seen, id)
	}
	for id := range seen {
		t.Errorf("capability %q is in the table but has no constant", id)
	}
}

func TestToolNamesArePrefixed(t *testing.T) {
	// The orchestrator's control gate keys off the prefix; an unprefixed name
	// would silently skip that gate.
	for _, c := range (&Service{}).Capabilities() {
		if !strings.HasPrefix(ToolName(c.ID), ToolPrefix) {
			t.Errorf("tool name %q lacks the %q prefix", ToolName(c.ID), ToolPrefix)
		}
	}
}

// ---- result truncation ----

func TestTruncateMarksItsOutput(t *testing.T) {
	short := strings.Repeat("a", 100)
	if out, cut := Truncate(short); cut || out != short {
		t.Errorf("short text was altered: cut=%v", cut)
	}
	long := strings.Repeat("b", maxResultBytes*2)
	out, cut := Truncate(long)
	if !cut {
		t.Error("oversized result was not marked as truncated")
	}
	if len(out) > maxResultBytes+64 {
		t.Errorf("truncated result is still %d bytes", len(out))
	}
	if !strings.Contains(out, "已截断") {
		t.Error("truncation is not visible to the reader")
	}
}

// ---- deny list immutability ----

func TestDenyListViewIsACopy(t *testing.T) {
	view := DenyListView()
	if len(view) == 0 {
		t.Fatal("deny list view is empty")
	}
	view[0].Match = "mutated"
	if denyList[0].Match == "mutated" {
		t.Error("DenyListView exposed the live deny list; a caller could clear the protection")
	}
	view = append(view, DeniedTarget{Match: "x"})
	if len(DenyListView()) != len(denyList) {
		t.Error("DenyListView returned a slice aliasing the deny list")
	}
}

// ---- tools ----

func TestToolsRefuseWhileDisabled(t *testing.T) {
	b := newFake()
	s := NewWithBackend(b, nil) // off
	reg := registry.New()
	s.RegisterTools(reg)

	names := registeredNames(t, reg)
	if len(names) != len(s.Capabilities()) {
		t.Fatalf("registered %d tools, want %d", len(names), len(s.Capabilities()))
	}
	for _, name := range names {
		if _, err := reg.Execute(context.Background(), name, map[string]interface{}{"appId": "h:1"}); err == nil {
			t.Errorf("%s executed while the module was disabled", name)
		}
	}
	if len(b.acts) != 0 {
		t.Errorf("a disabled module reached the backend %d times", len(b.acts))
	}
}

func TestListAppsToolMarksBlockedWindows(t *testing.T) {
	s := svcFor(newFake())
	reg := registry.New()
	s.RegisterTools(reg)
	out, err := reg.Execute(context.Background(), ToolName(CapListApps), map[string]interface{}{})
	if err != nil {
		t.Fatalf("list_apps failed: %v", err)
	}
	if !strings.Contains(out, "禁止控制") {
		t.Errorf("list_apps output does not flag blocked windows:\n%s", out)
	}
	if !strings.Contains(out, "h:1") {
		t.Errorf("list_apps output lost an ordinary window:\n%s", out)
	}
}

func TestToolSchemaDeclaresRequiredParams(t *testing.T) {
	s := svcFor(newFake())
	reg := registry.New()
	s.RegisterTools(reg)
	// type_text without text would be a no-op that reports success; the schema
	// must require it so the model is told at call time.
	tl, ok := reg.Get(ToolName(CapTypeText))
	if !ok {
		t.Fatal("type_text tool not registered")
	}
	sch := tl.Schema()
	req, ok := sch["required"].([]string)
	if !ok {
		t.Fatalf("type_text schema has no required list: %+v", sch)
	}
	if fmt.Sprint(req) != "[appId text]" {
		t.Errorf("required = %v, want [appId text]", req)
	}
}

func registeredNames(t *testing.T, reg *registry.Registry) []string {
	t.Helper()
	var out []string
	for _, ti := range reg.List() {
		out = append(out, ti.Name)
	}
	return out
}

// ---- tool gate (orchestrator pre-flight) ----

func TestToolGateRefusesWhileDisabled(t *testing.T) {
	s := NewWithBackend(newFake(), nil) // ships disabled
	if err := s.ToolGate(ToolName(CapListApps), nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled module let a control tool through: %v", err)
	}
	s.SetEnabled(true)
	if err := s.ToolGate(ToolName(CapListApps), nil); err != nil {
		t.Errorf("enabled module refused a control tool: %v", err)
	}
	// A non-control tool is none of this gate's business.
	if err := s.ToolGate("calculator", nil); err != nil {
		t.Errorf("the control gate rejected a foreign tool: %v", err)
	}
}

// ---- truncation ----

// Cutting a byte slice at a fixed offset splits a multi-byte character, and a
// control tree full of Chinese control names makes that the common case. The
// split leaves invalid UTF-8, which the JSON encoder turns into U+FFFD exactly
// where the model reads a node name.
func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	// The leading ASCII byte shifts every following 3-byte character so the
	// limit lands in the middle of one.
	long := "a" + strings.Repeat("中", maxResultBytes)
	out, cut := Truncate(long)
	if !cut {
		t.Fatal("an oversized result was not truncated")
	}
	if !utf8.ValidString(out) {
		t.Error("truncation produced invalid UTF-8")
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Error("truncation left a replacement character at the cut")
	}
	if !strings.Contains(out, "已截断") {
		t.Error("truncation is not visible to the reader")
	}
}
