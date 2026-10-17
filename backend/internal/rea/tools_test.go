package rea

import (
	"context"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/tool/registry"
)

// The model-facing contract: save an adapter once, then get a concrete call list
// back instead of rediscovering the window every turn.
func TestToolsSaveListPlan(t *testing.T) {
	svc := New(t.TempDir())
	ctx := context.Background()

	out, err := saveTool{svc: svc}.Execute(ctx, map[string]interface{}{
		"app": "comfy", "name": "ComfyUI Desktop", "matchClass": "ComfyUI",
		"probe":  []interface{}{"端口 8188 有 HTTP 响应"},
		"action": "txt2img", "description": "提交一次生成", "params": []interface{}{"prompt"},
		"steps": []interface{}{
			map[string]interface{}{"op": "focus", "target": "ComfyUI"},
			map[string]interface{}{"op": "click", "target": "node:CLIPTextEncode"},
			map[string]interface{}{"op": "type", "text": "${prompt}"},
			map[string]interface{}{"op": "http", "url": "http://127.0.0.1:8188/prompt"},
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(out, "comfy/txt2img") {
		t.Errorf("save output = %q", out)
	}

	list, err := listTool{svc: svc}.Execute(ctx, map[string]interface{}{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, "txt2img(prompt)") || !strings.Contains(list, "端口 8188") {
		t.Errorf("list output = %q", list)
	}

	plan, err := planTool{svc: svc}.Execute(ctx, map[string]interface{}{
		"app": "comfy", "action": "txt2img",
		"vars": map[string]interface{}{"prompt": "夕阳"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, want := range []string{"control_focus", "control_click", "control_type", "http_fetch", "夕阳"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan is missing %q:\n%s", want, plan)
		}
	}
}

// Saving twice must extend the profile, not replace the actions already recorded.
func TestSaveExtendsExistingProfile(t *testing.T) {
	svc := New(t.TempDir())
	ctx := context.Background()
	for _, action := range []string{"one", "two"} {
		if _, err := (saveTool{svc: svc}).Execute(ctx, map[string]interface{}{
			"app": "app", "action": action,
			"steps": []interface{}{map[string]interface{}{"op": "focus", "target": "w"}},
		}); err != nil {
			t.Fatalf("save %s: %v", action, err)
		}
	}
	p, err := svc.Get("app")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(p.Actions) != 2 {
		t.Errorf("actions = %v, want both kept", p.Actions)
	}
}

// A plan for a missing parameter must fail loudly rather than emit an empty type
// step that "succeeds" and writes nothing.
func TestPlanFailsWithoutParameter(t *testing.T) {
	svc := New(t.TempDir())
	ctx := context.Background()
	if _, err := (saveTool{svc: svc}).Execute(ctx, map[string]interface{}{
		"app": "app", "action": "a", "params": []interface{}{"prompt"},
		"steps": []interface{}{map[string]interface{}{"op": "type", "text": "${prompt}"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := (planTool{svc: svc}).Execute(ctx, map[string]interface{}{"app": "app", "action": "a"}); err == nil {
		t.Fatal("plan without the parameter returned nil")
	}
}

// Registering must not disturb the existing tool set.
func TestRegisterToolsAddsThree(t *testing.T) {
	reg := registry.New()
	New(t.TempDir()).RegisterTools(reg)
	names := map[string]bool{}
	for _, s := range reg.Schemas() {
		names[s.Name] = true
	}
	for _, want := range []string{"rea_list", "rea_save", "rea_plan"} {
		if !names[want] {
			t.Errorf("%s was not registered", want)
		}
	}
}
