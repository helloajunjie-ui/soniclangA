//go:build !windows

// Non-Windows stub for the software-control capability (DESIGN §9.1).
//
// The control tree path in this milestone is Win32-specific: DESIGN §14 C1
// explicitly picks 控件树/UIAutomation because Windows coverage is by far the
// widest, with VLM Computer Use as the v2 fallback. Rather than pretend to work
// — returning an empty app list or a no-op success would make the agent claim
// it clicked something it never touched — this stub reports the platform as
// unsupported, and the policy layer turns that into an explicit error.
package control

import (
	"context"
	"errors"
)

// stubBackend is the placeholder implementation for platforms without a control
// backend yet.
type stubBackend struct{ platform string }

func newBackend() Backend { return stubBackend{platform: "unsupported"} }

func (s stubBackend) Platform() string { return s.platform }

func (s stubBackend) Supported() bool { return false }

func (s stubBackend) ListApps() ([]App, error) {
	return nil, errors.New("软件控制目前仅在 Windows 上实现（GUI 自动化引擎依赖 UI Automation）")
}

func (s stubBackend) Inspect(string, InspectOptions) (*Node, bool, error) {
	return nil, false, errors.New("控件树探查目前仅在 Windows 上实现")
}

func (s stubBackend) Act(context.Context, ActionRequest) (string, error) {
	return "", errors.New("桌面操作目前仅在 Windows 上实现")
}
