package schema

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	ok := []string{"a", "_x", "get_time", "mcp__srv__search", "tool-1", strings.Repeat("a", 64)}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Fatalf("%q should be valid: %v", n, err)
		}
	}
	// These are the shapes a provider rejects with an opaque 400, or that
	// collide with the registry's own namespace separators.
	bad := []string{"", "1abc", "has space", "dot.name", "中文", strings.Repeat("a", 65), "a/b"}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Fatalf("%q should be rejected", n)
		}
	}
}

func TestSanitizeRequiredMustExist(t *testing.T) {
	// required naming an undeclared property tells the model an argument is
	// mandatory for a field it can never fill.
	_, err := Sanitize(map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}},
		"required":   []interface{}{"b"},
	})
	if err == nil {
		t.Fatal("required naming a missing property must fail")
	}
	good, err := Sanitize(map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}},
		"required":   []interface{}{"a"},
	})
	if err != nil || good == nil {
		t.Fatalf("valid schema rejected: %v", err)
	}
}

func TestStripControlKeepsNewlines(t *testing.T) {
	in := "line1\nline2\ttab\x00null\x07bell\x7fdel"
	got := StripControl(in)
	for _, bad := range []string{"\x00", "\x07", "\x7f"} {
		if strings.Contains(got, bad) {
			t.Fatalf("control char %q survived", bad)
		}
	}
	if !strings.Contains(got, "\n") || !strings.Contains(got, "\t") {
		t.Fatal("newlines/tabs must survive — tool output is legitimately multi-line")
	}
}

func TestClampOnRuneBoundary(t *testing.T) {
	// Build a string exceeding the cap where the cut would land mid-rune.
	big := strings.Repeat("中", MaxOutputBytes) // 3 bytes per rune
	got := Clamp(big)
	if len(got) <= MaxOutputBytes {
		t.Fatal("expected truncation")
	}
	if !strings.Contains(got, "已截断") {
		t.Fatal("truncation must be announced, not silent")
	}
	// The kept prefix must be valid UTF-8 — a half rune would decode to U+FFFD.
	head := got[:strings.Index(got, "...")]
	for _, r := range head {
		if r == '�' {
			t.Fatal("cut landed mid-rune, producing a replacement char")
		}
	}
}

func TestClampShortStringUntouched(t *testing.T) {
	s := "hello"
	if Clamp(s) != s {
		t.Fatal("short output must pass through unchanged")
	}
}

func TestAdmitStopsAtContractGate(t *testing.T) {
	smokeRan := false
	res := Admit(context.Background(), Candidate{
		Name:        "bad name",
		Description: "a tool that does something useful here",
		Schema:      map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Smoke:       func(context.Context) error { smokeRan = true; return nil },
	})
	if res.OK || res.Gate != "contract" {
		t.Fatalf("expected contract failure, got %+v", res)
	}
	if smokeRan {
		t.Fatal("smoke must not run when the contract gate already failed")
	}
}

func TestAdmitStopsAtConsistencyGate(t *testing.T) {
	smokeRan := false
	res := Admit(context.Background(), Candidate{
		Name:   "good_name",
		Schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Smoke:  func(context.Context) error { smokeRan = true; return nil },
	})
	if res.OK || res.Gate != "consistency" {
		t.Fatalf("expected consistency failure, got %+v", res)
	}
	if smokeRan {
		t.Fatal("smoke must not run when consistency already failed")
	}
}

func TestAdmitRunsSmokeAndReportsFailure(t *testing.T) {
	res := Admit(context.Background(), Candidate{
		Name:        "echo_tool",
		Description: "回显输入内容，用于测试",
		Schema:      map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Smoke:       func(context.Context) error { return errors.New("interpreter missing") },
	})
	if res.OK || res.Gate != "smoke" {
		t.Fatalf("expected smoke failure, got %+v", res)
	}
	if !strings.Contains(res.Message, "interpreter missing") {
		t.Fatalf("smoke reason must reach the user: %q", res.Message)
	}
}

func TestAdmitPasses(t *testing.T) {
	calls := 0
	res := Admit(context.Background(), Candidate{
		Name:        "echo_tool",
		Description: "回显输入内容，用于测试",
		Schema:      map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Smoke:       func(context.Context) error { calls++; return nil },
	})
	if !res.OK {
		t.Fatalf("expected pass, got %+v", res)
	}
	// Exactly once: a double smoke run doubles wall time on a slow script.
	if calls != 1 {
		t.Fatalf("smoke ran %d times, want 1", calls)
	}
}
