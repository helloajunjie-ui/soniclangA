package rea

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/agenthub/agent-proxy/internal/tool"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
	"github.com/agenthub/agent-proxy/internal/workspace"
)

// The tools below are how the model is supposed to approach a GUI-only app:
// look for an existing adapter, otherwise build one once, then reuse it. None of
// them touches the screen — execution stays with control_* / http_fetch, which
// already have the module switch, the allow list and the audit trail.

// RegisterTools adds the REA tool family to the registry.
func (s *Service) RegisterTools(reg *registry.Registry) {
	reg.Register(listTool{svc: s})
	reg.Register(saveTool{svc: s})
	reg.Register(planTool{svc: s})
	reg.Register(markTool{svc: s})
}

// ---- rea_list ----

type listTool struct{ svc *Service }

func (listTool) Name() string { return "rea_list" }
func (listTool) Description() string {
	return "列出已录制的软件外挂接口（app、可用 action、参数）。任务涉及某个桌面软件时先查这里，有档案就复用，别每次重新摸控件树。"
}
func (listTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"app": map[string]interface{}{"type": "string", "description": "只看某个 app（可选）"}},
	}
}
func (t listTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	app, _ := args["app"].(string)
	if app != "" {
		p, err := t.svc.Get(app)
		if err != nil {
			return "", err
		}
		return renderProfile(p), nil
	}
	list := t.svc.List()
	if len(list) == 0 {
		return "还没有任何 REA 档案。软件没有 API/CLI 时：先探端口与可脚本入口，试通一次后用 rea_save 固化。", nil
	}
	var b strings.Builder
	for _, p := range list {
		b.WriteString(renderProfile(&p))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func renderProfile(p *Profile) string {
	names := make([]string, 0, len(p.Actions))
	for n := range p.Actions {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s（%s）\n", p.App, p.Name)
	if p.MatchClass != "" || p.MatchTitle != "" {
		fmt.Fprintf(&b, "窗口匹配：class=%s title~%s\n", p.MatchClass, p.MatchTitle)
	}
	if len(p.Probe) > 0 {
		fmt.Fprintf(&b, "侦察结论：%s\n", strings.Join(p.Probe, "；"))
	}
	for _, n := range names {
		a := p.Actions[n]
		fmt.Fprintf(&b, "- %s(%s)：%s（%d 步）\n", n, strings.Join(a.Params, ", "), a.Description, len(a.Steps))
	}
	return b.String()
}

// ---- rea_save ----

type saveTool struct{ svc *Service }

func (saveTool) Name() string { return "rea_save" }
func (saveTool) Description() string {
	return "把某软件的操作步骤固化成外挂接口。探到本机端口就优先记 http 步骤（比模拟点击稳得多），只有确实没有接口时才记 click/type/hotkey。参数用 ${name} 占位。"
}
func (saveTool) Schema() map[string]interface{} {
	step := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"op":     map[string]interface{}{"type": "string", "enum": []string{"focus", "click", "type", "hotkey", "launch", "http"}},
			"target": map[string]interface{}{"type": "string", "description": "focus/launch 的目标；click 的控件 id 或 x,y"},
			"text":   map[string]interface{}{"type": "string", "description": "type 的文本，可含 ${var}"},
			"keys":   map[string]interface{}{"type": "string", "description": "hotkey，如 ctrl+s"},
			"url":    map[string]interface{}{"type": "string", "description": "http 的地址，可含 ${var}"},
			"note":   map[string]interface{}{"type": "string", "description": "给自己下次的提示"},
		},
		"required": []string{"op"},
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"app":        map[string]interface{}{"type": "string", "description": "稳定 id，如 comfy（字母数字-下划线）"},
			"name":       map[string]interface{}{"type": "string", "description": "显示名"},
			"matchClass": map[string]interface{}{"type": "string", "description": "窗口类名或进程名"},
			"matchTitle": map[string]interface{}{"type": "string", "description": "窗口标题片段"},
			"appVersion": map[string]interface{}{
				"type": "string", "description": "录制时的软件版本；软件升级后界面变了就重录并更新它",
			},
			"probe":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "侦察结论，如「端口 8188 有 HTTP 响应」"},
			"action": map[string]interface{}{"type": "string", "description": "action 名，如 txt2img"},
			"description": map[string]interface{}{
				"type": "string", "description": "这个 action 干什么",
			},
			"params": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			"steps":  map[string]interface{}{"type": "array", "items": step},
		},
		"required": []string{"app", "action", "steps"},
	}
}
func (t saveTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	app, _ := args["app"].(string)
	action, _ := args["action"].(string)
	p, err := t.svc.Get(app)
	if err != nil {
		p = &Profile{App: app, Actions: map[string]Action{}}
	}
	if p.Actions == nil {
		p.Actions = map[string]Action{}
	}
	if v, ok := args["name"].(string); ok && v != "" {
		p.Name = v
	}
	if v, ok := args["matchClass"].(string); ok && v != "" {
		p.MatchClass = v
	}
	if v, ok := args["matchTitle"].(string); ok && v != "" {
		p.MatchTitle = v
	}
	if v, ok := args["appVersion"].(string); ok && v != "" {
		p.AppVersion = v
	}
	if v, ok := args["probe"].([]interface{}); ok {
		for _, it := range v {
			if s, ok := it.(string); ok && s != "" {
				p.Probe = append(p.Probe, s)
			}
		}
	}
	var steps []Step
	raw, _ := args["steps"].([]interface{})
	for _, it := range raw {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		st := Step{}
		st.Op, _ = m["op"].(string)
		st.Target, _ = m["target"].(string)
		st.Text, _ = m["text"].(string)
		st.Keys, _ = m["keys"].(string)
		st.URL, _ = m["url"].(string)
		st.Note, _ = m["note"].(string)
		steps = append(steps, st)
	}
	var params []string
	if v, ok := args["params"].([]interface{}); ok {
		for _, it := range v {
			if s, ok := it.(string); ok {
				params = append(params, s)
			}
		}
	}
	desc, _ := args["description"].(string)
	p.Actions[action] = Action{Description: desc, Params: params, Steps: steps}
	if err := t.svc.Save(p); err != nil {
		return "", err
	}
	return fmt.Sprintf("已保存 %s/%s（%d 步，rev %d，适配版本 %s）。之后用 rea_plan 取步骤执行。",
		app, action, len(steps), p.Rev, orDash(p.AppVersion)), nil
}

func orDash(s string) string {
	if s == "" {
		return "未记录"
	}
	return s
}

// ---- rea_plan ----

type planTool struct{ svc *Service }

func (planTool) Name() string { return "rea_plan" }
func (planTool) Description() string {
	return "取出某个 action 的具体步骤并填入参数，返回按顺序要执行的调用清单（http → http_fetch，其余 → control_*）。执行完再判断结果，别盲跑。"
}
func (planTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"app":    map[string]interface{}{"type": "string"},
			"action": map[string]interface{}{"type": "string"},
			"vars":   map[string]interface{}{"type": "object", "description": "参数值，如 {\"prompt\": \"...\"}"},
		},
		"required": []string{"app", "action"},
	}
}
func (t planTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	app, _ := args["app"].(string)
	action, _ := args["action"].(string)
	vars := map[string]string{}
	if m, ok := args["vars"].(map[string]interface{}); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				vars[k] = s
			}
		}
	}
	// DIR defaults to the task workspace: recorded steps use ${DIR} instead of a
	// hard-coded path, so the same adapter works for the next task too.
	if _, ok := vars["DIR"]; !ok {
		if dir := workspace.FromContext(ctx); dir != "" {
			vars["DIR"] = dir
		}
	}
	steps, err := t.svc.Resolve(app, action, vars)
	if err != nil {
		return "", err
	}
	out := make([]map[string]interface{}, 0, len(steps))
	for i, st := range steps {
		call := map[string]interface{}{"step": i + 1, "tool": st.Op}
		switch st.Op {
		case "http":
			call["tool"], call["args"] = "http_fetch", map[string]string{"url": st.URL}
		case "focus", "click", "type", "hotkey", "launch":
			a := map[string]string{}
			if st.Target != "" {
				a["target"] = st.Target
			}
			if st.Text != "" {
				a["text"] = st.Text
			}
			if st.Keys != "" {
				a["keys"] = st.Keys
			}
			call["tool"], call["args"] = "control_"+st.Op, a
		}
		if st.Note != "" {
			call["note"] = st.Note
		}
		out = append(out, call)
	}
	status := "未验证（首次执行后请用 rea_mark 记录结果）"
	if a, err := t.svc.Get(app); err == nil {
		if act, ok := a.Actions[action]; ok {
			switch {
			case act.Stale:
				status = "已失效（上次回放失败，界面可能变了；重新录制这个 action 再保存）"
			case act.Verified:
				status = "已验证"
			}
			if act.LastNote != "" {
				status += "；上次观察：" + act.LastNote
			}
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return fmt.Sprintf("状态：%s\n%s", status, string(b)), nil
}

var _ tool.Tool = listTool{}

// ---- rea_mark ----

type markTool struct{ svc *Service }

func (markTool) Name() string { return "rea_mark" }
func (markTool) Description() string {
	return "记录一次回放的结果：成功则标为已验证，失败则标为失效并写清现象（如「窗口标题变了」）。下次 rea_plan 会据此提示重录，而不是让你照旧步骤硬试。"
}
func (markTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"app":    map[string]interface{}{"type": "string"},
			"action": map[string]interface{}{"type": "string"},
			"ok":     map[string]interface{}{"type": "boolean", "description": "这次回放成功了吗"},
			"note":   map[string]interface{}{"type": "string", "description": "失败现象或成功补充"},
		},
		"required": []string{"app", "action", "ok"},
	}
}
func (t markTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	app, _ := args["app"].(string)
	action, _ := args["action"].(string)
	ok, _ := args["ok"].(bool)
	note, _ := args["note"].(string)
	if err := t.svc.Mark(app, action, ok, note); err != nil {
		return "", err
	}
	if ok {
		return "已标记为验证通过：" + app + "/" + action, nil
	}
	return "已标记为失效：" + app + "/" + action + "。下次请重录该 action 并用 rea_save 更新。", nil
}
