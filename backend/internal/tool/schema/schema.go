// Package schema implements the tool schema wash-gate (DESIGN §5 tool/schema):
// a bad schema must never poison an entire round request, so every tool schema
// is validated before it is offered to the model.
//
// It also hosts the admission triple-gate for self-built tools (script + MCP,
// DESIGN §2 risk table): contract → consistency → live smoke. Half-finished
// tools are rejected at write time rather than failing mysteriously mid-chat.
package schema

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// MaxOutputBytes caps a tool's returned text. A script that prints a whole file
// (or loops until the timeout) must not be able to blow up the context window
// of the conversation that called it (DESIGN §10 tool result hygiene).
const MaxOutputBytes = 16 << 10

// nameRe is the tool-name shape accepted by every mainstream provider: a letter
// or underscore followed by letters/digits/underscore/hyphen, capped at 64 chars.
// Providers reject other shapes with an opaque 400, so we reject them first with
// a message that says what to fix.
var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

// ValidateName checks a tool name against the provider-accepted shape.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("工具名 %q 不合法：须以字母或下划线开头，只含字母/数字/下划线/连字符，长度 ≤ 64", name)
	}
	return nil
}

// Sanitize validates a JSON-schema-like tool parameter spec. It returns the
// schema unchanged on success, or an error when the shape is unusable.
func Sanitize(s map[string]interface{}) (map[string]interface{}, error) {
	if s == nil {
		return nil, errors.New("nil schema")
	}
	t, _ := s["type"].(string)
	if t != "object" {
		return nil, errors.New("root type must be object")
	}
	props, ok := s["properties"].(map[string]interface{})
	if !ok {
		return nil, errors.New("properties must be an object")
	}
	// required, when present, must name properties that actually exist —
	// otherwise the model is told an argument is mandatory for a field it can
	// never fill, and it either hallucinates it or loops asking for it.
	if req, present := s["required"]; present {
		list, isList := req.([]interface{})
		if !isList {
			return nil, errors.New("required must be an array of property names")
		}
		for _, r := range list {
			name, _ := r.(string)
			if name == "" {
				return nil, errors.New("required entries must be non-empty strings")
			}
			if _, ok := props[name]; !ok {
				return nil, fmt.Errorf("required names %q which is not declared in properties", name)
			}
		}
	}
	return s, nil
}

// Clamp truncates a tool result to MaxOutputBytes on a rune boundary and notes
// how much was dropped, so the model is never silently handed a half-cut
// multibyte character.
func Clamp(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	cut := MaxOutputBytes
	// Back off to a valid UTF-8 boundary so the result never decodes to U+FFFD.
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	dropped := len(s) - cut
	return s[:cut] + fmt.Sprintf("\n...[输出过长，已截断 %d 字节]", dropped)
}

// StripControl removes control characters that can corrupt the SSE frame or the
// terminal (DESIGN §10). Tab, newline and carriage return survive because tool
// output is legitimately multi-line; everything else below 0x20 goes, as does
// DEL.
func StripControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SanitizeResult is the single exit every tool result passes through.
func SanitizeResult(s string) string { return Clamp(StripControl(s)) }

// AdmissionResult reports how a candidate tool fared against the triple gate.
type AdmissionResult struct {
	OK      bool     `json:"ok"`
	Gate    string   `json:"gate"`    // contract | consistency | smoke
	Message string   `json:"message"` // human-readable reason, shown in the UI
	Details []string `json:"details,omitempty"`
}

// Candidate is a tool awaiting admission.
type Candidate struct {
	Name        string
	Description string
	Schema      map[string]interface{}
	// Smoke runs the tool once. It is required: a tool that has never actually
	// run is exactly the "half-finished product" the gate exists to keep out.
	Smoke func(ctx context.Context) error
}

// Admit runs the three gates in order and stops at the first failure:
//
//  1. 契约 (contract):     name shape + schema shape. Cheap, catches typos.
//  2. 一致性 (consistency): description non-empty and substantive; a tool whose
//     description says nothing gives the model no basis to choose it, so it
//     becomes dead weight in every request.
//  3. 实弹冒烟 (smoke):     actually invoke the tool once. Only this catches the
//     runtime-level failures (missing interpreter, bad code, server not
//     reachable) that static checks cannot see.
func Admit(ctx context.Context, c Candidate) AdmissionResult {
	if err := ValidateName(c.Name); err != nil {
		return AdmissionResult{Gate: "contract", Message: err.Error()}
	}
	if _, err := Sanitize(c.Schema); err != nil {
		return AdmissionResult{Gate: "contract", Message: "参数 schema 不合法: " + err.Error()}
	}
	if strings.TrimSpace(c.Description) == "" {
		return AdmissionResult{Gate: "consistency", Message: "工具描述为空：模型无法据此判断何时调用该工具"}
	}
	if len([]rune(strings.TrimSpace(c.Description))) < 4 {
		return AdmissionResult{
			Gate:    "consistency",
			Message: "工具描述过短：至少 4 个字，说明这个工具做什么用",
		}
	}
	if c.Smoke == nil {
		return AdmissionResult{Gate: "smoke", Message: "未提供冒烟执行器，无法验证工具是否真的能跑"}
	}
	if err := c.Smoke(ctx); err != nil {
		return AdmissionResult{
			Gate:    "smoke",
			Message: "冒烟执行失败: " + err.Error(),
			Details: []string{"工具未通过准入，未注册为可调用工具；修复后可重试"},
		}
	}
	return AdmissionResult{OK: true, Gate: "smoke", Message: "三关校验通过"}
}
