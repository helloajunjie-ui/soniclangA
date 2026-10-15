package control

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agenthub/agent-proxy/internal/tool"
)

// Tool prefix for every software-control tool. The prefix is what keeps the
// control surface recognisable in the tool list and lets the orchestrator
// classify the whole family in one place instead of enumerating every name.
const ToolPrefix = "control_"

// ToolName returns the registry name of the tool backing a capability.
func ToolName(cap string) string { return ToolPrefix + cap }

// RegisterTools mounts the control capabilities into the tool registry.
//
// The tools are registered eagerly but refuse at execution time when the module
// is off, so the model sees them (and can be told why they refuse) instead of
// hallucinating a capability that does not exist. The permission gate in the
// orchestrator treats every control_* tool as non-read-only, so under the
// strictest tier it is blocked before Execute is ever reached.
func (s *Service) RegisterTools(reg interface{ Register(tool.Tool) }) {
	for _, c := range s.Capabilities() {
		reg.Register(s.toolFor(c))
	}
}

func (s *Service) toolFor(c Capability) tool.Tool {
	return controlTool{svc: s, cap: c}
}

type controlTool struct {
	svc *Service
	cap Capability
}

func (t controlTool) Name() string { return ToolName(t.cap.ID) }

func (t controlTool) Description() string {
	d := t.cap.Description
	switch t.cap.ID {
	case CapListApps:
		d += "。注意：浏览器、Electron 等 Chromium 系应用只有一个空壳窗口，看不到内部控件"
	case CapInspect:
		// The model must know the tree can be shallow. Without this it sees a
		// two-node tree for a browser, concludes "there is no save button" and
		// either retries forever or claims it cannot help the user — when the
		// real cause is the technique, not the app.
		d += "。传统 Win32 应用（记事本、资源管理器、Office 等）可完整枚举；" +
			"Chromium/Electron 系应用只返回窗口本身，此时请勿反复重试"
	}
	return d
}

func (t controlTool) Schema() map[string]interface{} {
	props := map[string]interface{}{}
	var required []string

	switch t.cap.ID {
	case CapListApps:
		// no parameters
	case CapLaunch:
		props["target"] = map[string]interface{}{
			"type":        "string",
			"description": "要启动的应用：可执行文件名（如 notepad.exe）、文件路径或 URI",
		}
		required = []string{"target"}
	case CapInspect:
		props["appId"] = map[string]interface{}{
			"type":        "string",
			"description": "目标窗口 id（先用 control_list_apps 获取）",
		}
		props["maxDepth"] = map[string]interface{}{
			"type": "integer", "description": "控件树深度，默认 4，最大 12",
		}
		props["maxNodes"] = map[string]interface{}{
			"type": "integer", "description": "控件树节点上限，默认 400，最大 2000",
		}
		required = []string{"appId"}
	case CapTypeText:
		props["appId"] = map[string]interface{}{"type": "string", "description": "目标窗口 id"}
		props["text"] = map[string]interface{}{
			"type": "string", "description": "要输入的文本，支持中文",
		}
		required = []string{"appId", "text"}
	case CapHotkey:
		props["appId"] = map[string]interface{}{"type": "string", "description": "目标窗口 id"}
		props["keys"] = map[string]interface{}{
			"type":        "array",
			"items":       map[string]interface{}{"type": "string"},
			"description": "按键序列，如 [\"ctrl\",\"s\"]",
		}
		required = []string{"appId", "keys"}
	default:
		props["appId"] = map[string]interface{}{
			"type": "string", "description": "目标窗口 id（先用 control_list_apps 获取）",
		}
		props["nodeId"] = map[string]interface{}{
			"type": "string", "description": "控件 id（可选，来自 control_inspect）；留空则作用于窗口本身",
		}
		required = []string{"appId"}
	}

	out := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func (t controlTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	req := ActionRequest{
		Cap:   t.cap.ID,
		AppID: str(args, "appId"),
		Text:  str(args, "text"),
		// An empty appId is a common mistake after a control_list_apps call
		// returned windows whose title was empty; the authorise step explains
		// the requirement, so no special-casing is needed here.
		NodeID: str(args, "nodeId"),
		Target: str(args, "target"),
	}
	req.Keys = strSlice(args, "keys")
	req.MaxDepth = intOf(args, "maxDepth")
	req.MaxNodes = intOf(args, "maxNodes")
	if b, ok := args["includeOffscreen"].(bool); ok {
		req.IncludeOffscreen = b
	}

	if t.cap.ID == CapListApps {
		apps, err := t.svc.apps()
		if err != nil {
			return "", err
		}
		return renderApps(apps), nil
	}
	if t.cap.ID == CapInspect {
		snap, err := t.svc.Inspect(ctx, req)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(snap)
		if err != nil {
			return "", err
		}
		out, _ := Truncate(string(b))
		return out, nil
	}
	res, err := t.svc.Run(ctx, req)
	if err != nil {
		return "", err
	}
	out, _ := Truncate(res)
	return out, nil
}

// apps is the tool-facing wrapper around Apps that keeps the module gate in
// front: without it a disabled module would still enumerate windows.
func (s *Service) apps() ([]App, error) {
	if !s.IsEnabled() {
		return nil, ErrDisabled
	}
	return s.Apps()
}

func renderApps(apps []App) string {
	if len(apps) == 0 {
		return "当前没有可控的应用窗口"
	}
	out := fmt.Sprintf("共 %d 个窗口：\n", len(apps))
	for _, a := range apps {
		line := fmt.Sprintf("- id=%s", a.ID)
		if a.Process != "" {
			line += " 进程=" + a.Process
		}
		if a.Title != "" {
			line += " 标题=" + a.Title
		}
		if a.Blocked {
			line += " 【禁止控制：" + a.Reason + "】"
		}
		out += line + "\n"
	}
	return out
}

func str(m map[string]interface{}, k string) string {
	s, _ := m[k].(string)
	return s
}

func intOf(m map[string]interface{}, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func strSlice(m map[string]interface{}, k string) []string {
	raw, ok := m[k].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
