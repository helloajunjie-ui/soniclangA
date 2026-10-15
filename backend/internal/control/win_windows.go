//go:build windows

// Windows backend for the software-control capability (DESIGN §9.1).
//
// Only the system DLLs are used — no third-party GUI/automation library — so the
// binary keeps its zero-dependency build. Two Win32 techniques are combined:
//
//   - Window enumeration (EnumWindows / EnumChildWindows) to discover apps and
//     read the control tree. This is the "control tree" path of DESIGN §14 C1:
//     it works for the vast majority of ordinary Win32 and common-toolkit apps.
//   - Input synthesis (SendInput) to click and type. UIAutomation's ValuePattern
//     would be the more precise way to set a text field, but binding its COM
//     interfaces by hand costs more than it buys for v1; SendInput targets the
//     focused control, which is what a human does anyway.
//
// Safety lives in the parent package, not here. This file only knows how to
// enumerate windows and move the mouse; it never decides whether an action is
// allowed.
package control

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procEnumWindows         = user32.NewProc("EnumWindows")
	procEnumChildWindows    = user32.NewProc("EnumChildWindows")
	procGetWindowTextLen    = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetClassNameW       = user32.NewProc("GetClassNameW")
	procGetWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procIsWindowEnabled     = user32.NewProc("IsWindowEnabled")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procGetWindowRect       = user32.NewProc("GetWindowRect")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procSendInput           = user32.NewProc("SendInput")
	procShellExecuteW       = shell32.NewProc("ShellExecuteW")
	procVkKeyScanW          = user32.NewProc("VkKeyScanW")
)

const (
	// swRestore restores a minimised window before activating it; without it
	// SetForegroundWindow on a minimised window appears to do nothing.
	swRestore = 9

	wmClose = 0x0010

	// Virtual key codes for the keys SendInput cannot express as a character.
	vkReturn  = 0x0D
	vkTab     = 0x09
	vkEscape  = 0x1B
	vkBack    = 0x08
	vkDelete  = 0x2E
	vkHome    = 0x24
	vkEnd     = 0x23
	vkLeft    = 0x25
	vkUp      = 0x26
	vkRight   = 0x27
	vkDown    = 0x28
	vkControl = 0x11
	vkShift   = 0x10
	vkMenu    = 0x12
	vkLWin    = 0x5B
	vkRWin    = 0x5C

	// MOUSEEVENTF_* flags.
	mouseEventFAbsolute = 0x8000
	mouseEventFMove     = 0x0001
	mouseEventFLeftDown = 0x0002
	mouseEventFLeftUp   = 0x0004
	mouseEventFWheel    = 0x0800
	// mouseEventFVirtualDesk makes absolute coordinates span every monitor.
	// Without it SendInput reads them as primary-monitor relative, so a click
	// computed on a secondary display lands on the wrong screen.
	mouseEventFVirtualDesk = 0x4000
	inputMouse             = 0
	inputKeyboard          = 1
	keyEventFKeyUp         = 0x0002
	keyEventFUnicode       = 0x0004
	keyEventFExtendedKey   = 0x0001
	// GetSystemMetrics indices. The primary-monitor pair is only correct on a
	// single-display setup; the virtual pair is what hit testing and click
	// normalisation must use once a second monitor exists.
	smCXScreen        = 0
	smCYScreen        = 1
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
	swShowMinimized   = 2
)

// namedKeys maps the friendly key names accepted in a hotkey request onto
// virtual key codes. Named keys are resolved here rather than sending raw scan
// codes so a model cannot accidentally emit an undefined key code.
var namedKeys = map[string]uint16{
	"enter": vkReturn, "return": vkReturn, "tab": vkTab, "esc": vkEscape,
	"escape": vkEscape, "backspace": vkBack, "back": vkBack,
	"delete": vkDelete, "del": vkDelete, "home": vkHome, "end": vkEnd,
	"left": vkLeft, "up": vkUp, "right": vkRight, "down": vkDown,
	"ctrl": vkControl, "control": vkControl, "shift": vkShift,
	"alt": vkMenu, "win": vkLWin, "space": 0x20,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74,
	"f6": 0x75, "f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79,
	"f11": 0x7A, "f12": 0x7B,
}

type winBackend struct{}

// newBackend returns the Windows control backend.
func newBackend() Backend { return winBackend{} }

func (winBackend) Platform() string { return "windows" }

func (winBackend) Supported() bool {
	// Find resolves the proc once and caches it; on a system without the DLL
	// (a stripped container image, say) it reports the error instead of
	// panicking inside Call later.
	return procEnumWindows.Find() == nil && procSendInput.Find() == nil
}

// ---- window enumeration ----

type winRect struct {
	Left, Top, Right, Bottom int32
}

// enumWindows walks every top-level window. The callback runs on the calling
// thread inside the Win32 call, so it must not block; everything it does is
// cheap metadata reads.
func enumWindows(fn func(hwnd uintptr) bool) {
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if !fn(hwnd) {
			return 0 // stop
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
}

func enumChildWindows(parent uintptr, fn func(hwnd uintptr) bool) {
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if !fn(hwnd) {
			return 0
		}
		return 1
	})
	procEnumChildWindows.Call(parent, cb, 0)
}

// windowText reads a window's caption. A zero length means "no caption" (a
// container or a control that carries its text elsewhere), not an error, so the
// caller gets an empty string rather than failing the whole enumeration.
func windowText(hwnd uintptr) string {
	l, _, _ := procGetWindowTextLen.Call(hwnd)
	if l == 0 {
		return ""
	}
	buf := make([]uint16, l+1)
	if n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func windowClass(hwnd uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func windowPID(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func isVisible(hwnd uintptr) bool {
	r, _, _ := procIsWindowVisible.Call(hwnd)
	return r != 0
}

func isEnabled(hwnd uintptr) bool {
	r, _, _ := procIsWindowEnabled.Call(hwnd)
	return r != 0
}

func windowRect(hwnd uintptr) (winRect, bool) {
	var r winRect
	ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r, ret != 0
}

// processName resolves a pid to its executable name, without the directory.
// It can fail legitimately (the process may have exited between enumeration and
// the query, or it may be protected and OpenProcess refuses), so callers treat
// an empty result as "unknown" rather than an error.
//
// QueryFullProcessImageNameW(hProcess, dwFlags, lpExeName, lpdwSize) is easy to
// get wrong in a way that fails silently: dwFlags is a plain value (0 = Win32
// path) and lpdwSize is an in/out *pointer* to a DWORD. An earlier version
// passed a pointer where the flags belong and the length by value where the
// pointer belongs; the call returned zero every time, which reads as "this
// window has no process". Every deny-list rule that matches on a process name —
// cmd.exe, powershell, keepass, taskmgr, lsass — was inert as a result, and the
// console's process column was blank.
func processName(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	ret, _, _ := procGetProcessImageName.Call(
		uintptr(h),
		0, // dwFlags: Win32 path format
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0 || size == 0 {
		return ""
	}
	// size is in characters and excludes the terminating NUL, but a
	// misbehaving driver can report more than we offered.
	if int(size) > len(buf) {
		size = uint32(len(buf))
	}
	return filepath.Base(syscall.UTF16ToString(buf[:size]))
}

var (
	procGetProcessImageName = kernel32.NewProc("QueryFullProcessImageNameW")
)

// hwndRef encodes a window handle as the string used in App.ID.
//
// The handle is rendered as a decimal number rather than a hex pointer so the
// id stays a plain opaque token that JSON round-trips without precision loss on
// the JS side.
func hwndRef(hwnd uintptr) string { return "h:" + strconv.FormatUint(uint64(hwnd), 10) }

// parseHwnd is the inverse of hwndRef. It rejects anything that is not a handle
// we minted, so a caller cannot inject an arbitrary number and reach a window
// the enumeration never offered.
func parseHwnd(id string) (uintptr, error) {
	s := strings.TrimPrefix(id, "h:")
	if s == "" || s == id {
		return 0, fmt.Errorf("无效的窗口 id：%s", id)
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("无效的窗口 id：%s", id)
	}
	return uintptr(n), nil
}

func (winBackend) ListApps() ([]App, error) {
	var apps []App
	enumWindows(func(hwnd uintptr) bool {
		// Unowned message-only windows carry no title and no owning process;
		// they are system internals, not user applications.
		if !isVisible(hwnd) {
			return true
		}
		title := windowText(hwnd)
		pid := windowPID(hwnd)
		if strings.TrimSpace(title) == "" && pid == 0 {
			return true
		}
		proc := processName(pid)
		apps = append(apps, App{
			ID:      hwndRef(hwnd),
			Title:   title,
			Process: proc,
			Class:   windowClass(hwnd),
		})
		return true
	})
	return apps, nil
}

// Inspect walks the control tree of a window.
//
// The walk is bounded on both depth and node count. A browser window can have
// tens of thousands of child windows; an unbounded walk would hang the request
// and push megabytes of JSON into the model's context. When a bound is hit the
// walk returns truncated=true so the model knows the tree is partial.
func (winBackend) Inspect(appID string, opts InspectOptions) (*Node, bool, error) {
	hwnd, err := parseHwnd(appID)
	if err != nil {
		return nil, false, err
	}
	visited := 0
	truncated := false

	var walk func(h uintptr, depth int) *Node
	walk = func(h uintptr, depth int) *Node {
		if visited >= opts.MaxNodes {
			truncated = true
			return nil
		}
		visited++
		r, hasRect := windowRect(h)
		n := &Node{
			ID:      hwndRef(h),
			Name:    windowText(h),
			Class:   windowClass(h),
			Type:    controlTypeOf(windowClass(h)),
			Enabled: isEnabled(h),
			Depth:   depth,
		}
		if hasRect && r.Right-r.Left > 0 && r.Bottom-r.Top > 0 {
			// Zero-width windows are a common hidden-layout trick (a placeholder
			// used only to size a container); they get no geometry so the model
			// does not try to click a 0x0 target.
			n.Value = fmt.Sprintf("(%d,%d %dx%d)", r.Left, r.Top, r.Right-r.Left, r.Bottom-r.Top)
			// OnScreen matters more than Enabled for a click target: a control
			// scrolled out of view is enabled but cannot be hit. Recording it
			// explicitly lets the model pick a reachable node instead of
			// firing a click that silently lands on the user's desktop.
			n.OnScreen = screenVisible(r, opts.IncludeOffscreen)
		}
		if depth >= opts.MaxDepth {
			// Anything deeper is hidden, so the tree is partial. This is
			// reported rather than silently dropped: a model that believes it
			// saw the whole tree will conclude "there is no save button" when
			// one exists further down.
			truncated = true
			return n
		}
		enumChildWindows(h, func(child uintptr) bool {
			if visited >= opts.MaxNodes {
				// Stop enumerating, but do not touch `truncated` here: the flag
				// is owned by the walk, and a previous depth cut may already
				// have set it. The node-budget case is recorded once after the
				// walk returns.
				return false
			}
			if cn := walk(child, depth+1); cn != nil {
				n.Children = append(n.Children, cn)
			}
			return true
		})
		return n
	}

	root := walk(hwnd, 0)
	if visited >= opts.MaxNodes {
		truncated = true
	}
	if root == nil {
		return nil, false, fmt.Errorf("控件树为空：窗口可能已关闭")
	}
	// A near-empty tree on a Chromium/Electron app is a technique limitation,
	// not an empty window. Saying so saves the model from re-inspecting the
	// same window forever and from telling the user "no such button exists"
	// when the button is there but invisible to EnumChildWindows.
	if len(root.Children) == 0 && !truncated {
		if strings.Contains(strings.ToLower(windowClass(hwnd)), "chrome_widget") ||
			strings.Contains(strings.ToLower(windowClass(hwnd)), "cef") {
			root.Children = append(root.Children, &Node{
				ID:   "",
				Name: "（该应用为 Chromium/Electron 架构，界面渲染在单个窗口内，Win32 控件树无法枚举内部元素；需要 UI Automation 才能定位控件）",
				Type: "notice",
			})
		}
	}
	return root, truncated, nil
}

// controlTypeOf maps a window class onto the accessibility-ish role the model
// reasons about. The mapping is intentionally small: it covers the classes that
// matter for locating an action target, and anything unknown keeps its raw class
// so nothing is silently mislabelled.
func controlTypeOf(class string) string {
	switch strings.ToLower(class) {
	case "button", "tbutton":
		return "button"
	case "edit", "teedit", "richedit", "richedit20w", "tmemo", "textarea":
		return "input"
	case "static":
		return "label"
	case "combobox", "comboex32":
		return "select"
	case "systreeview32", "sylistview32", "sylistbox32", "listbox":
		return "list"
	case "mdiclient", "mdiarea":
		return "panel"
	case "dialog", "#32770":
		return "dialog"
	default:
		return ""
	}
}

// virtualScreen returns the bounding box of every monitor in virtual-desktop
// coordinates.
//
// The primary-monitor metrics are wrong the moment a display is attached to the
// left or above the primary one: those coordinates are negative, so a hit test
// against "0..primaryWidth" marks a perfectly visible window as off-screen and
// refuses the click. Falls back to the primary metrics if the virtual ones are
// unreadable (some remote-desktop configurations report 0).
func virtualScreen() (x0, y0, w, h int32) {
	vx, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	vy, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	vw, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	vh, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	if int32(vw) <= 0 || int32(vh) <= 0 {
		pw, _, _ := procGetSystemMetrics.Call(smCXScreen)
		ph, _, _ := procGetSystemMetrics.Call(smCYScreen)
		return 0, 0, int32(pw), int32(ph)
	}
	return int32(vx), int32(vy), int32(vw), int32(vh)
}

func screenVisible(r winRect, includeOffscreen bool) bool {
	if includeOffscreen {
		return true
	}
	x0, y0, w, h := virtualScreen()
	return r.Right > x0 && r.Bottom > y0 && r.Left < x0+w && r.Top < y0+h &&
		r.Right > r.Left && r.Bottom > r.Top
}

// ---- actions ----

func (winBackend) Act(ctx context.Context, req ActionRequest) (string, error) {
	switch req.Cap {
	case CapListApps:
		apps, err := winBackend{}.ListApps()
		if err != nil {
			return "", err
		}
		return describeApps(apps), nil
	case CapLaunch:
		return launch(req.Target)
	case CapInspect:
		// Honour the caller's bounds. The tree is also available as structured
		// data through Service.Inspect; this branch is what a chat tool call
		// reaches, so ignoring maxDepth/maxNodes here would silently cap every
		// tool-driven inspection at the defaults.
		root, truncated, err := winBackend{}.Inspect(req.AppID, InspectOptions{
			MaxDepth:         clampInt(req.MaxDepth, 1, 12, 4),
			MaxNodes:         clampInt(req.MaxNodes, 1, 2000, 400),
			IncludeOffscreen: req.IncludeOffscreen,
		})
		if err != nil {
			return "", err
		}
		return describeTree(root, truncated), nil
	case CapFocus:
		return focusWindow(req.AppID)
	case CapClose:
		return closeWindow(req.AppID)
	case CapClick:
		return clickNode(ctx, req)
	case CapTypeText:
		return typeText(ctx, req)
	case CapHotkey:
		return hotkey(ctx, req)
	default:
		return "", fmt.Errorf("%w：%s", ErrUnknownCapability, req.Cap)
	}
}

func describeApps(apps []App) string {
	if len(apps) == 0 {
		return "没有找到可控的应用窗口"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 个窗口：\n", len(apps))
	for _, a := range apps {
		proc := a.Process
		if proc != "" {
			proc = " [" + proc + "]"
		}
		fmt.Fprintf(&b, "- id=%s%s 标题=%q\n", a.ID, proc, a.Title)
	}
	return b.String()
}

// describeTree renders a control tree as indented text rather than JSON: it is
// far cheaper in tokens than JSON with repeated field names, and the model only
// needs the shape to pick a node id.
func describeTree(root *Node, truncated bool) string {
	var b strings.Builder
	b.WriteString("控件树（id 用于后续动作）：\n")
	var walk func(n *Node)
	walk = func(n *Node) {
		if n == nil {
			return
		}
		kind := n.Type
		if kind == "" {
			kind = "window"
		}
		state := ""
		if !n.Enabled {
			state = " [禁用]"
		}
		line := fmt.Sprintf("%s- id=%s type=%s name=%q", strings.Repeat("  ", n.Depth), n.ID, kind, n.Name)
		if n.Value != "" {
			line += " " + n.Value
		}
		b.WriteString(line + state + "\n")
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	if truncated {
		b.WriteString("…[控件树已截断，仅显示部分节点]\n")
	}
	return b.String()
}

func focusWindow(appID string) (string, error) {
	hwnd, err := parseHwnd(appID)
	if err != nil {
		return "", err
	}
	// A minimised window cannot take focus, so restore it first. Without this
	// the call silently appears to succeed while the user sees nothing.
	if isIconic(hwnd) {
		procShowWindow.Call(hwnd, swRestore)
		time.Sleep(80 * time.Millisecond)
	}
	procSetForegroundWindow.Call(hwnd)
	return "已请求聚焦窗口 " + appID, nil
}

func closeWindow(appID string) (string, error) {
	hwnd, err := parseHwnd(appID)
	if err != nil {
		return "", err
	}
	// WM_CLOSE rather than DestroyWindow: it gives the app its normal "save your
	// work?" path, and a force-kill from an agent is exactly the kind of
	// irreversible action this capability must not take.
	ok, _, _ := procPostMessageW.Call(hwnd, wmClose, 0, 0)
	if ok == 0 {
		return "", fmt.Errorf("窗口拒绝关闭（可能无消息循环或已退出）")
	}
	return "已向窗口 " + appID + " 发送关闭请求", nil
}

// isIconic reports whether the window is minimised.
//
// GetWindowPlacement requires the caller to pre-set the structure's `length`
// field. A freshly allocated zero buffer leaves it 0, the call fails, and the
// function reports "not minimised" for every window — so the restore-before-
// focus path never ran and focusing a minimised window did nothing visible.
// The showCmd field also sits at offset 8 (after length and flags), not at
// offset 4, where the previous code was reading the flags word instead.
func isIconic(hwnd uintptr) bool {
	// WINDOWPLACEMENT = length + flags + showCmd (3 x UINT) + POINT + POINT +
	// RECT = 44 bytes on both 386 and amd64.
	const placementSize = 44
	placement := make([]byte, placementSize)
	*(*uint32)(unsafe.Pointer(&placement[0])) = placementSize
	ret, _, _ := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&placement[0])))
	if ret == 0 {
		return false
	}
	showCmd := *(*uint32)(unsafe.Pointer(&placement[8]))
	return showCmd == swShowMinimized
}

var procGetWindowPlacement = user32.NewProc("GetWindowPlacement")

func clickNode(ctx context.Context, req ActionRequest) (string, error) {
	hwnd, err := parseHwnd(req.AppID)
	if err != nil {
		return "", err
	}
	r, ok := windowRect(hwnd)
	if !ok {
		return "", fmt.Errorf("无法读取窗口位置（窗口可能已关闭）")
	}
	// Centre of the window: the default target when the caller did not pick a
	// specific control. A precise per-node hit test would need UIAutomation's
	// BoundingRectangle; the centre is deterministic and explainable.
	cx := (r.Left + r.Right) / 2
	cy := (r.Top + r.Bottom) / 2
	if node := req.NodeID; node != "" {
		nh, err := parseHwnd(node)
		if err != nil {
			return "", err
		}
		nr, ok := windowRect(nh)
		if !ok {
			return "", fmt.Errorf("控件不存在：%s", node)
		}
		cx = (nr.Left + nr.Right) / 2
		cy = (nr.Top + nr.Bottom) / 2
	}
	if !screenVisible(winRect{Left: cx, Top: cy, Right: cx + 1, Bottom: cy + 1}, false) {
		return "", fmt.Errorf("目标位置 (%d,%d) 不在屏幕上，无法点击", cx, cy)
	}
	// Focus first: SendInput delivers to whatever holds focus, so clicking a
	// background window would otherwise type into the user's editor instead.
	if _, err := focusWindow(req.AppID); err != nil {
		return "", err
	}
	time.Sleep(60 * time.Millisecond)
	if err := sendMouseClick(ctx, cx, cy); err != nil {
		return "", err
	}
	return fmt.Sprintf("已点击窗口 %s 于 (%d,%d)", req.AppID, cx, cy), nil
}

func typeText(ctx context.Context, req ActionRequest) (string, error) {
	if req.Text == "" {
		return "", fmt.Errorf("缺少 text 参数")
	}
	if _, err := parseHwnd(req.AppID); err != nil {
		return "", err
	}
	if _, err := focusWindow(req.AppID); err != nil {
		return "", err
	}
	time.Sleep(60 * time.Millisecond)
	// Unicode injection rather than a keystroke stream: it handles CJK and
	// emoji without a dead-key dance, which is what a Chinese-language user
	// typing into a form actually needs.
	if err := sendUnicodeText(ctx, req.Text); err != nil {
		return "", err
	}
	runes := []rune(req.Text)
	return fmt.Sprintf("已向窗口 %s 输入 %d 个字符", req.AppID, len(runes)), nil
}

func hotkey(ctx context.Context, req ActionRequest) (string, error) {
	if len(req.Keys) == 0 {
		return "", fmt.Errorf("缺少 keys 参数")
	}
	if _, err := parseHwnd(req.AppID); err != nil {
		return "", err
	}
	codes := make([]uint16, 0, len(req.Keys))
	for _, k := range req.Keys {
		nk := strings.ToLower(strings.TrimSpace(k))
		if code, ok := namedKeys[nk]; ok {
			codes = append(codes, code)
			continue
		}
		// A single printable character is allowed so "ctrl+s" style shortcuts
		// can also be written "ctrl+S". VkKeyScanW answers the right question —
		// which key produces this character — and returns the virtual key in the
		// low byte. The previous code used MapVirtualKeyW's default map type,
		// which converts a *virtual key to a scan code*, and then fed that scan
		// code back in as a virtual key: "ctrl+s" sent whichever key owns scan
		// code 0x1F rather than "S".
		rs := []rune(nk)
		if len(rs) == 1 {
			probe, _, _ := procVkKeyScanW.Call(uintptr(rs[0]))
			if int16(probe) != -1 {
				codes = append(codes, uint16(probe&0xff))
				continue
			}
		}
		return "", fmt.Errorf("未知按键：%s（可用：enter/tab/esc/ctrl/alt/shift/win/f1-f12/方向键/单字符）", k)
	}
	if _, err := focusWindow(req.AppID); err != nil {
		return "", err
	}
	time.Sleep(60 * time.Millisecond)
	if err := sendHotkey(ctx, codes); err != nil {
		return "", err
	}
	return "已向窗口 " + req.AppID + " 发送快捷键 " + strings.Join(req.Keys, "+"), nil
}

func launch(target string) (string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", fmt.Errorf("缺少 target 参数")
	}
	// "open" verb with no directory lets ShellExecute resolve a bare executable
	// name, a file path or a URI the same way Explorer does.
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return "", err
	}
	p, err := syscall.UTF16PtrFromString(t)
	if err != nil {
		return "", err
	}
	r, _, _ := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(p)), 0, 0, 1)
	// ShellExecuteW returns >32 on success; anything else is a failure code.
	if int32(r) <= 32 {
		return "", fmt.Errorf("启动失败（ShellExecute=%d）：%s", int32(r), t)
	}
	return "已请求启动：" + t, nil
}

// ---- input synthesis ----

// input mirrors the Win32 INPUT structure.
//
// INPUT ends in a C union, so its size is *not* the sum of its variants — it is
// `type` + padding + max(MOUSEINPUT, KEYBDINPUT). Modelling the union as two
// sequential Go fields inflates the struct to 64 bytes on amd64 where the API
// expects 40, and SendInput rejects a cbSize that differs from sizeof(INPUT):
// every click, keystroke and shortcut failed with a parameter error while the
// code read as correct. Representing the union as an array of uintptr words
// lets the compiler place it at the ABI offset on both 386 and amd64 and makes
// unsafe.Sizeof(input{}) come out at exactly sizeof(INPUT) (40 / 28).
type input struct {
	Type  uint32
	union [unsafe.Sizeof(mouseInput{}) / unsafe.Sizeof(uintptr(0))]uintptr
}

type keybdInput struct {
	Vk      uint16
	Scan    uint16
	Flags   uint32
	Time    uint32
	ExtraIn uintptr
}

type mouseInput struct {
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraIn   uintptr
}

// kbInput builds a keyboard INPUT by writing a KEYBDINPUT into the union area.
func kbInput(ki keybdInput) input {
	in := input{Type: inputKeyboard}
	*(*keybdInput)(unsafe.Pointer(&in.union[0])) = ki
	return in
}

// msInput builds a mouse INPUT by writing a MOUSEINPUT into the union area.
func msInput(mi mouseInput) input {
	in := input{Type: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&in.union[0])) = mi
	return in
}

func sendInputs(ctx context.Context, ins []input) error {
	if len(ins) == 0 {
		return nil
	}
	// Check for cancellation before and after the blocking call: SendInput is
	// synchronous and can only be interrupted by the OS, so an aborted request
	// must not keep pushing keystrokes into the focused window.
	if err := ctx.Err(); err != nil {
		return err
	}
	n, _, err := procSendInput.Call(uintptr(len(ins)), uintptr(unsafe.Pointer(&ins[0])), unsafe.Sizeof(input{}))
	if err != nil {
		return fmt.Errorf("SendInput 失败：%w", err)
	}
	if int(n) != len(ins) {
		// A partial insert means the OS dropped events (typically UIPI: the
		// target window runs elevated). Silently continuing would look like a
		// successful action while the user sees nothing happen.
		return fmt.Errorf("SendInput 只成功注入 %d/%d 个事件（目标窗口可能以管理员权限运行）", n, len(ins))
	}
	return nil
}

// keyDown / keyUp are the two halves of a keystroke. They are separate because
// a shortcut needs them *decoupled*: pressing and releasing each key in turn is
// not a hotkey, it is a sequence of single keystrokes (see sendHotkey).
func keyDown(vk uint16) input { return kbInput(keybdInput{Vk: vk}) }
func keyUp(vk uint16) input {
	return kbInput(keybdInput{Vk: vk, Flags: keyEventFKeyUp})
}

// charPair is the press+release pair that types one character. Unlike a hotkey
// the two events belong to the same key and must stay adjacent.
func charPair(unit uint16) []input {
	return []input{
		kbInput(keybdInput{Scan: unit, Flags: keyEventFUnicode}),
		kbInput(keybdInput{Scan: unit, Flags: keyEventFUnicode | keyEventFKeyUp}),
	}
}

// textUnits encodes a string to UTF-16 code units rather than truncating each
// rune into a uint16: a rune outside the BMP (emoji, CJK extension B) needs a
// surrogate pair, and uint16(r) would send a lone surrogate that renders as
// garbage. Split out from the sender so the encoding is testable without
// touching the desktop.
func textUnits(s string) []uint16 { return utf16.Encode([]rune(s)) }

func sendUnicodeText(ctx context.Context, text string) error {
	units := textUnits(text)
	const batch = 64
	for i := 0; i < len(units); i += batch {
		end := i + batch
		if end > len(units) {
			end = len(units)
		}
		var ins []input
		for _, u := range units[i:end] {
			ins = append(ins, charPair(u)...)
		}
		if err := sendInputs(ctx, ins); err != nil {
			return err
		}
	}
	return nil
}

// hotkeyInputs builds the event sequence for a key combination.
//
// All keys go down *together*, then release in reverse. Emitting a
// press+release pair per key (the previous behaviour) sends ctrl down/up and
// then s down/up — the application sees two separate keystrokes and the
// shortcut never fires. It also doubled the event count because the release
// pass re-sent the same pairs.
//
// Kept separate from sendHotkey so the ordering can be asserted in a test; the
// alternative is a test that synthesises real keystrokes on the user's desktop.
func hotkeyInputs(codes []uint16) []input {
	combo := make([]input, 0, len(codes)*2)
	for _, c := range codes {
		combo = append(combo, keyDown(c))
	}
	for i := len(codes) - 1; i >= 0; i-- {
		combo = append(combo, keyUp(codes[i]))
	}
	return combo
}

func sendHotkey(ctx context.Context, codes []uint16) error {
	return sendInputs(ctx, hotkeyInputs(codes))
}

func sendMouseClick(ctx context.Context, x, y int32) error {
	x0, y0, w, h := virtualScreen()
	if w <= 0 || h <= 0 {
		return fmt.Errorf("无法读取屏幕尺寸")
	}
	// SendInput wants normalised absolute coordinates in 0..65535 across the
	// virtual desktop, which is what mouseEventFVirtualDesk declares. Values
	// outside that range are undefined, so they are clamped rather than passed
	// through.
	ax := clampInt64(int64(x-x0)*65535/int64(w), 0, 65535)
	ay := clampInt64(int64(y-y0)*65535/int64(h), 0, 65535)
	ins := []input{
		msInput(mouseInput{Dx: int32(ax), Dy: int32(ay),
			Flags: mouseEventFAbsolute | mouseEventFVirtualDesk | mouseEventFMove}),
		msInput(mouseInput{Flags: mouseEventFLeftDown}),
		msInput(mouseInput{Flags: mouseEventFLeftUp}),
	}
	return sendInputs(ctx, ins)
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---- misc ----
