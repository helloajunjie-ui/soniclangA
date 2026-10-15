package orchestrator

import (
	"testing"

	"github.com/agenthub/agent-proxy/internal/config"
	"github.com/agenthub/agent-proxy/internal/conversation"
	"github.com/agenthub/agent-proxy/internal/role"
)

// newRoleResolver builds an Orchestrator carrying only a config, which is all
// resolveRoleID touches.
func newRoleResolver(defaultRoleID string) *Orchestrator {
	return &Orchestrator{Config: &config.Config{
		Settings: config.Settings{DefaultRoleId: defaultRoleID},
	}}
}

// A conversation is created against a specific role. That role decides the
// provider, model, tools, skills and methodology for every turn. If the turn
// re-resolved the role from the global default instead, the conversation would
// silently answer with a different model and a different toolset — the user sees
// no error, just the wrong assistant.
func TestConversationRoleIDWinsOverGlobalDefault(t *testing.T) {
	o := newRoleResolver("global-default-role")
	got := o.resolveRoleID(
		ChatInput{ConversationID: "conv-1"},
		&conversation.Conversation{RoleId: "claude-specialist"},
	)
	if got != "claude-specialist" {
		t.Fatalf("conversation role must win, got %q (want %q)", got, "claude-specialist")
	}
}

// An explicit role in the request is an intentional override and still wins.
func TestExplicitRequestRoleOverridesConversation(t *testing.T) {
	o := newRoleResolver("global-default-role")
	got := o.resolveRoleID(
		ChatInput{ConversationID: "conv-1", RoleID: "per-request-role"},
		&conversation.Conversation{RoleId: "conv-role"},
	)
	if got != "per-request-role" {
		t.Fatalf("request role must win, got %q", got)
	}
}

// Without a conversation (ad-hoc chat) the global default still applies.
func TestGlobalDefaultAppliesWithoutConversation(t *testing.T) {
	o := newRoleResolver("global-default-role")
	if got := o.resolveRoleID(ChatInput{}, nil); got != "global-default-role" {
		t.Fatalf("global default expected, got %q", got)
	}
}

// A conversation with no role recorded must still fall back cleanly rather than
// returning an empty id, which would fail the Roles.Get lookup.
func TestRoleIDFallsBackToGeneral(t *testing.T) {
	o := newRoleResolver("")
	if got := o.resolveRoleID(ChatInput{ConversationID: "c"}, &conversation.Conversation{}); got != "general" {
		t.Fatalf("expected the built-in fallback, got %q", got)
	}
}

// The permission applied to a turn must come from the same role the turn
// actually resolved to — otherwise fixing the role id would still gate tools
// using the default role's (possibly stricter) level.
func TestPermissionUsesResolvedRole(t *testing.T) {
	convRole := &role.Role{ID: "conv-role", Permission: PermFull}
	// Sanity: the resolved role's own level is what gets gated on. Assert the
	// behaviour (an ordinary mutating tool runs) rather than gate nil-ness: with
	// no control gate wired the returned func is non-nil, because the control_*
	// family is gated in every tier.
	if g := permissionGate(convRole, nil); g != nil {
		if err := g("write_file", nil); err != nil {
			t.Fatalf("full role should not gate ordinary tools: %v", err)
		}
	}
	// A conversation-level override still wins over either role.
	g := permissionGate(convRole, &conversation.Conversation{Permission: PermAsk})
	if g == nil {
		t.Fatal("conversation ask should gate even when the role is full")
	}
	if err := g("write_file", nil); err == nil {
		t.Fatal("ask must block mutating tools")
	}
}
