package orchestrator

import (
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/brief"
	"github.com/agenthub/agent-proxy/internal/control"
	"github.com/agenthub/agent-proxy/internal/conversation"
	"github.com/agenthub/agent-proxy/internal/envprobe"
	"github.com/agenthub/agent-proxy/internal/orchestrator/loop"
	"github.com/agenthub/agent-proxy/internal/role"
)

// The permission field was decorative: tools ran regardless. These tests pin the
// three levels so a mutating call can never silently succeed under a low level.
//
// They assert behaviour — "does this call run?" — rather than whether the gate
// is nil. Nil-ness used to be the shorthand for "nothing is gated", but that
// stopped being expressible once control_* tools had to be gated in every tier
// (see TestControlToolsAreGatedInEveryTier).

func allowed(t *testing.T, g loop.Gate, name string) {
	t.Helper()
	if g == nil {
		return
	}
	if err := g(name, nil); err != nil {
		t.Fatalf("%s should be allowed, got: %v", name, err)
	}
}

func blocked(t *testing.T, g loop.Gate, name string) {
	t.Helper()
	if g == nil {
		t.Fatalf("%s ran with no gate at all", name)
	}
	if err := g(name, nil); err == nil {
		t.Fatalf("%s should be blocked", name)
	}
}

func TestPermissionGateFull(t *testing.T) {
	g := permissionGate(&role.Role{Permission: PermFull}, nil)
	allowed(t, g, "calculator")
	allowed(t, g, "write_file")
}

func TestPermissionGateAsk(t *testing.T) {
	g := permissionGate(&role.Role{Permission: PermAsk}, nil)
	// Read-only tools stay usable; otherwise the product is unusable by default.
	allowed(t, g, "calculator")
	allowed(t, g, "memory_search")
	// Mutating tools are refused, and the refusal is explicit.
	blocked(t, g, "write_file")
}

func TestPermissionGateAutoAllowsReadOnly(t *testing.T) {
	// auto == full for now: both run everything; the distinction is kept so a
	// later "accept edits but ask for shell" level has a home.
	g := permissionGate(&role.Role{Permission: PermAuto}, nil)
	allowed(t, g, "write_file")
}

// The conversation's own permission wins over the role's.
func TestConversationPermissionOverridesRole(t *testing.T) {
	g := permissionGate(
		&role.Role{Permission: PermFull},
		&conversation.Conversation{Permission: PermAsk},
	)
	blocked(t, g, "write_file")

	g2 := permissionGate(
		&role.Role{Permission: PermAsk},
		&conversation.Conversation{Permission: PermFull},
	)
	allowed(t, g2, "write_file")
}

// An unset permission must default to the safest level, not to "allow".
func TestPermissionDefaultIsAsk(t *testing.T) {
	g := permissionGate(&role.Role{}, nil)
	blocked(t, g, "write_file")
	allowed(t, g, "calculator")
}

// ---- software control (§9.1) ----

func wiredControlGate(t *testing.T) loop.Gate {
	t.Helper()
	return func(name string, _ map[string]interface{}) error {
		if !strings.HasPrefix(name, control.ToolPrefix) {
			t.Fatalf("control gate received a foreign tool %q", name)
		}
		return nil
	}
}

// Software control answers to the module switch and the deny list, not to the
// permission tier. The tier says how far the agent is trusted with *its own*
// tools; consent to drive the desktop is the module switch itself — the user
// settled that when they asked for the job. A fresh install runs at "ask", so
// gating the family there would leave the product's first capability doing
// nothing out of the box, and asking again before every click would be a
// sign-off queue rather than a capability.
func TestControlToolsFollowTheModuleSwitchNotTheTier(t *testing.T) {
	readOnly := control.ToolName(control.CapListApps)
	click := control.ToolName(control.CapClick)

	for _, level := range []string{PermAsk, PermAuto, PermFull} {
		// No gate wired: refuse outright. The module ships off, so "cannot be
		// verified here" must not decay into "allowed".
		g := permissionGate(&role.Role{Permission: level}, nil)
		blocked(t, g, readOnly)
		blocked(t, g, click)

		// Gate wired and satisfied: every tier may act, "ask" included.
		g2 := permissionGateWith(&role.Role{Permission: level}, nil, wiredControlGate(t))
		allowed(t, g2, readOnly)
		allowed(t, g2, click)
	}
}

// A control gate that refuses must be what stops the call, and its reason must
// survive — the module switch's error message is what tells the user how to
// enable the capability.
func TestControlGateRefusalPropagates(t *testing.T) {
	g := permissionGateWith(&role.Role{Permission: PermFull}, nil,
		func(string, map[string]interface{}) error { return control.ErrDisabled })

	err := g(control.ToolName(control.CapListApps), nil)
	// Compare against the sentinel's own text rather than a copy of it: what
	// this pins is that the reason survives the gate, not the exact wording of
	// a sentence the user-facing copy is free to improve.
	if err == nil || !strings.Contains(err.Error(), control.ErrDisabled.Error()) {
		t.Fatalf("expected the module-switch reason, got %v", err)
	}
}

// The orchestrator's prefix constant and the control package's must agree: the
// gate keys off one and the tool names are built from the other.
func TestControlToolPrefixMatchesControlPackage(t *testing.T) {
	if controlToolPrefix != control.ToolPrefix {
		t.Fatalf("prefix drift: orchestrator=%q control=%q", controlToolPrefix, control.ToolPrefix)
	}
}

// "One sentence and we start" (DESIGN §9.3) turns on two tool families that
// must work at the tier a fresh install actually runs at. If env_probe were
// gated, the agent could not answer "can this machine do the job" without first
// asking permission to look — which is a form to fill in, not a capability. The
// brief tools are how the agent keeps taking notes during the wait, and a
// gated notebook is useless in exactly the situation it exists for.
func TestFlowToolsRunAtTheDefaultTier(t *testing.T) {
	g := permissionGate(&role.Role{}, nil) // unset permission == ask
	for _, name := range []string{
		envprobe.ToolName,
		brief.ToolCreate, brief.ToolAppend, brief.ToolRead, brief.ToolList,
		// Read-only network tools fetch information and never mutate state.
		"http_fetch", "web_search", "web_fetch", "get_time",
	} {
		allowed(t, g, name)
	}
	// The loosening is specific, not a general "ask now allows everything".
	blocked(t, g, "write_file")
}

// shell_exec stays out of the read-only set on purpose: it is the one builtin
// that can change the machine, so at the default (ask) tier it must be refused
// and the human has to raise the tier. There is deliberately no built-in
// blacklist of "dangerous" commands — which commands are acceptable is the
// user's decision, not the product's (DESIGN §17 原则九/十一).
func TestShellExecIsGatedByThePermissionTier(t *testing.T) {
	g := permissionGate(&role.Role{}, nil) // unset permission == ask
	blocked(t, g, "shell_exec")
	blocked(t, g, "write_file")

	// Raising the tier to full is the human saying "I authorize this".
	gFull := permissionGate(&role.Role{Permission: PermFull}, nil)
	allowed(t, gFull, "shell_exec")
}
