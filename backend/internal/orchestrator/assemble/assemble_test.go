package assemble

import (
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/methodology"
	"github.com/agenthub/agent-proxy/internal/role"
	"github.com/agenthub/agent-proxy/internal/skill"
)

func TestAssembleEmpty(t *testing.T) {
	got := Assemble(Context{})
	// An empty context still carries the always-on infrastructure block: the real
	// current time.
	if !strings.Contains(got, "当前时间") {
		t.Fatalf("missing current-time block, got %q", got)
	}
	// No tools means no tool guidance: the inventory is the most expensive part of
	// the prompt, and a context with no tools must not advertise any.
	if strings.Contains(got, "<tool_guidance>") {
		t.Fatalf("a context with no tools rendered the tool-guidance block: %q", got)
	}
	// But no persona / skills / memory blocks should appear from nothing.
	for _, tag := range []string{"<persona>", "<skills>", "<memory_guidance>", "<methodology>"} {
		if strings.Contains(got, tag) {
			t.Fatalf("empty context should not render %s, got %q", tag, got)
		}
	}
}

// The inventory must list only the tools this turn offers: telling the model about
// shell_exec when the role was given only calculator invites calls it cannot make.
func TestToolGuidanceIsFilteredByAvailableTools(t *testing.T) {
	got := ToolGuidanceBlock([]string{"calculator", "get_time"})
	if got == "" {
		t.Fatal("guidance vanished with tools available")
	}
	if !strings.Contains(got, "| calculator |") || !strings.Contains(got, "| get_time |") {
		t.Errorf("the inventory is missing the available tools:\n%s", got)
	}
	for _, absent := range []string{"shell_exec", "control_list_apps", "web_search", "memory_search"} {
		if strings.Contains(got, "| "+absent+" |") {
			t.Errorf("the inventory still lists %s, which this turn does not offer", absent)
		}
	}
	// The header and the separator row survive the filtering.
	if !strings.Contains(got, "| 工具 | 用途 | 什么时候用 |") {
		t.Error("the table header was filtered away")
	}
}

func TestToolGuidanceIsEmptyWithoutTools(t *testing.T) {
	if got := ToolGuidanceBlock(nil); got != "" {
		t.Errorf("no tools should render no guidance, got %d bytes", len(got))
	}
	if got := ToolGuidanceBlock([]string{}); got != "" {
		t.Errorf("no tools should render no guidance, got %d bytes", len(got))
	}
}

func TestAssembleAllBlocks(t *testing.T) {
	c := Context{
		Role: &role.Role{
			SystemPrompt: "你是 {{项目}} 助手",
			Vars:         []role.RoleVar{{Name: "项目", Required: true}},
		},
		Vars: map[string]string{"项目": "AgentHub"},
		Skills: []*skill.Skill{{
			Name:         "深度研究",
			Description:  "结构化调研",
			Instructions: "先列提纲，再分主题收集证据",
		}},
		Methodologies: []methodology.MatchHit{
			{Methodology: methodology.Methodology{Kind: methodology.KindPrinciple, Name: "如实回答", Content: "不确定就说不确定"}},
			{Methodology: methodology.Methodology{Kind: methodology.KindBad, Name: "大爆炸重构", Content: "一次性重写全部"}},
		},
		MemoryGuidance: "用户偏好简洁回复",
	}
	got := Assemble(c)

	// Order: persona → methodology → skills → memory.
	order := []int{
		strings.Index(got, "你是 AgentHub 助手"),
		strings.Index(got, "<methodology>"),
		strings.Index(got, "<skills>"),
		strings.Index(got, "<memory_guidance>"),
	}
	for i, pos := range order {
		if pos < 0 {
			t.Fatalf("block %d missing in:\n%s", i, got)
		}
		if i > 0 && pos < order[i-1] {
			t.Fatalf("block order wrong: %v\n%s", order, got)
		}
	}

	// Template vars must be substituted into the persona.
	if !strings.Contains(got, "你是 AgentHub 助手") || strings.Contains(got, "{{项目}}") {
		t.Fatal("vars not rendered")
	}
	// Bad entries must be phrased as things to avoid.
	if !strings.Contains(got, "应避免：大爆炸重构") {
		t.Fatal("bad methodology not framed as avoid")
	}
	// Every block carries its own anti-injection disclaimer, so a skill or
	// memory cannot be read as privileged instructions.
	for _, tag := range []string{"<methodology>", "<skills>", "<memory_guidance>"} {
		if !blockHasDisclaimer(got, tag) {
			t.Fatalf("%s block missing the no-override disclaimer", tag)
		}
	}
}

// blockHasDisclaimer checks that the region opened by tag contains the
// "does not override the user" notice before its closing tag.
func blockHasDisclaimer(prompt, tag string) bool {
	start := strings.Index(prompt, tag)
	if start < 0 {
		return false
	}
	rest := prompt[start:]
	end := strings.Index(rest, "</"+strings.TrimSuffix(tag, ">")+">")
	if end < 0 {
		end = len(rest)
	}
	return strings.Contains(rest[:end], "不构成")
}

func TestAssembleNilRole(t *testing.T) {
	// A nil role (chat without one) must not panic; other blocks still render.
	got := Assemble(Context{
		Skills: []*skill.Skill{{Name: "s", Instructions: "i"}},
	})
	if !strings.Contains(got, "<skills>") {
		t.Fatalf("skills block lost with nil role: %q", got)
	}
}

func TestSkillBlockSkipsEmptyInstructions(t *testing.T) {
	got := SkillBlock([]*skill.Skill{
		{Name: "空技能", Instructions: "   "}, // whitespace-only → skipped
		{Name: "实技能", Instructions: "x"},
	})
	if strings.Contains(got, "空技能") {
		t.Fatal("empty skill leaked into prompt")
	}
	if !strings.Contains(got, "实技能") {
		t.Fatal("real skill missing")
	}
}
