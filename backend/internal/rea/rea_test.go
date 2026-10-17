package rea

import (
	"strings"
	"testing"
)

// A profile is the "port" for a GUI-only app: what to do, in what order, with
// which parts parameterised. These cases cover the parts that would otherwise
// fail silently at replay time.

func newSvc(t *testing.T) *Service {
	t.Helper()
	return New(t.TempDir())
}

func sample() *Profile {
	return &Profile{
		App:        "comfy",
		Name:       "ComfyUI Desktop",
		MatchClass: "ComfyUI",
		Probe:      []string{"端口 8188 有 HTTP 响应"},
		Actions: map[string]Action{
			"txt2img": {
				Description: "把提示词贴进正向节点并排队",
				Params:      []string{"prompt"},
				Steps: []Step{
					{Op: "focus", Target: "ComfyUI"},
					{Op: "click", Target: "node:CLIPTextEncode"},
					{Op: "hotkey", Keys: "ctrl+a"},
					{Op: "type", Text: "${prompt}"},
				},
			},
		},
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	s := newSvc(t)
	if err := s.Save(sample()); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A fresh service (== next boot) reads it back from disk.
	s2 := New(s.dir)
	p, err := s2.Get("comfy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.Name != "ComfyUI Desktop" || len(p.Actions["txt2img"].Steps) != 4 {
		t.Errorf("profile did not survive the round trip: %+v", p)
	}
	if len(s2.List()) != 1 {
		t.Errorf("List returned %d profiles", len(s2.List()))
	}
}

func TestSaveRejectsUnsafeAppID(t *testing.T) {
	s := newSvc(t)
	for _, bad := range []string{"", "../escape", "a/b", strings.Repeat("x", 49)} {
		p := sample()
		p.App = bad
		if err := s.Save(p); err == nil {
			t.Errorf("Save accepted app id %q (it becomes a file name)", bad)
		}
	}
}

func TestSaveRejectsBadPlans(t *testing.T) {
	s := newSvc(t)
	p := sample()
	p.Actions["broken"] = Action{Steps: []Step{{Op: "screenshot"}}}
	if err := s.Save(p); err == nil || !strings.Contains(err.Error(), "screenshot") {
		t.Errorf("unknown op was accepted: %v", err)
	}

	p = sample()
	p.Actions["empty"] = Action{}
	if err := s.Save(p); err == nil {
		t.Error("an action with no steps was accepted")
	}

	p = sample()
	p.Actions = map[string]Action{}
	if err := s.Save(p); err == nil {
		t.Error("a profile with no actions was accepted")
	}
}

func TestResolveSubstitutesParameters(t *testing.T) {
	s := newSvc(t)
	if err := s.Save(sample()); err != nil {
		t.Fatalf("save: %v", err)
	}
	steps, err := s.Resolve("comfy", "txt2img", map[string]string{"prompt": "夕阳下的海浪"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := steps[3].Text; got != "夕阳下的海浪" {
		t.Errorf("placeholder not substituted: %q", got)
	}
	if len(steps) != 4 || steps[0].Op != "focus" {
		t.Errorf("step order lost: %+v", steps)
	}
}

// A missing parameter must stop the replay: typing an empty prompt into a text
// box "succeeds" and produces the wrong picture.
func TestResolveFailsOnMissingParameter(t *testing.T) {
	s := newSvc(t)
	if err := s.Save(sample()); err != nil {
		t.Fatalf("save")
	}
	if _, err := s.Resolve("comfy", "txt2img", nil); err == nil {
		t.Fatal("missing parameter produced no error")
	}
}

func TestResolveUnknownAppOrActionListsWhatExists(t *testing.T) {
	s := newSvc(t)
	if err := s.Save(sample()); err != nil {
		t.Fatalf("save")
	}
	if _, err := s.Resolve("nope", "txt2img", nil); err == nil || !strings.Contains(err.Error(), "rea_save") {
		t.Errorf("unknown app error should say how to create one: %v", err)
	}
	if _, err := s.Resolve("comfy", "img2img", nil); err == nil || !strings.Contains(err.Error(), "txt2img") {
		t.Errorf("unknown action error should list the available ones: %v", err)
	}
}
