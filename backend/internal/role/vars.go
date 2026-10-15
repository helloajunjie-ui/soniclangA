package role

import (
	"fmt"
	"sort"
	"strings"
)

// Missing-var policies (DESIGN §7 Role.missingVarPolicy).
const (
	MissingAsk   = "ask"   // 缺失时拒绝本次生成，提示用户补齐
	MissingError = "error" // 缺失时直接报错
	MissingSkip  = "skip"  // 缺失时保留原占位符继续（默认）
)

// MissingVarError reports required role variables that were not supplied.
type MissingVarError struct {
	RoleID string
	Names  []string
}

func (e *MissingVarError) Error() string {
	return fmt.Sprintf("角色 %s 缺少必填变量：%s", e.RoleID, strings.Join(e.Names, "、"))
}

// MissingVars is the payload the handler returns for policy=ask, so the UI can
// render one input per variable without parsing the error string.
type MissingVars struct {
	RoleID string    `json:"roleId"`
	Names  []string  `json:"names"`
	Vars   []RoleVar `json:"vars"`
}

// VarsOf returns the variables a role declares, always as a non-nil slice.
func (r *Role) VarsOf() []RoleVar {
	if r == nil {
		return nil
	}
	return r.Vars
}

// Policy returns the missing-variable policy, defaulting to skip so roles
// written before this feature behave exactly as before.
func (r *Role) Policy() string {
	if r == nil {
		return MissingSkip
	}
	switch strings.ToLower(strings.TrimSpace(r.MissingVarPolicy)) {
	case MissingAsk, MissingError:
		return strings.ToLower(strings.TrimSpace(r.MissingVarPolicy))
	default:
		return MissingSkip
	}
}

// MissingVarsFor computes which required variables are absent. A variable with
// a default is never considered missing — the default is substituted instead.
func (r *Role) MissingVarsFor(values map[string]string) []string {
	var missing []string
	for _, v := range r.Vars {
		name := strings.TrimSpace(v.Name)
		if name == "" || !v.Required {
			continue
		}
		if strings.TrimSpace(values[name]) == "" && strings.TrimSpace(v.Default) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// RenderPrompt substitutes {{name}} placeholders in the system prompt.
//
// Substitution is textual and deliberately dumb: it only touches placeholders
// the role declares, so a prompt containing literal braces for JSON examples is
// left alone. Undeclared placeholders are reported by UnknownVars rather than
// being silently swallowed.
func (r *Role) RenderPrompt(values map[string]string) string {
	if r == nil || r.SystemPrompt == "" {
		return ""
	}
	return renderPlaceholders(r.SystemPrompt, r.resolve(values))
}

func (r *Role) resolve(values map[string]string) map[string]string {
	resolved := map[string]string{}
	for _, v := range r.Vars {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			continue
		}
		if got := strings.TrimSpace(values[name]); got != "" {
			resolved[name] = got
			continue
		}
		if v.Default != "" {
			resolved[name] = v.Default
		}
	}
	// Values passed for undeclared names are still honoured — a caller may know
	// about a placeholder the role definition has not been updated for yet.
	for k, v := range values {
		if _, ok := resolved[k]; !ok {
			resolved[k] = v
		}
	}
	return resolved
}

// ApplyVars renders the system prompt honouring the role's missing-variable
// policy. It returns the rendered prompt, the missing required variable names
// (policy=ask), or an error (policy=error).
func (r *Role) ApplyVars(values map[string]string) (string, []string, error) {
	missing := r.MissingVarsFor(values)
	switch r.Policy() {
	case MissingError:
		if len(missing) > 0 {
			return "", missing, &MissingVarError{RoleID: r.ID, Names: missing}
		}
	case MissingAsk:
		if len(missing) > 0 {
			return "", missing, nil
		}
	}
	return r.RenderPrompt(values), missing, nil
}

// UnknownVars lists {{placeholders}} used in the prompt that the role does not
// declare. It is a diagnostic aid for the role editor, not a hard failure.
func (r *Role) UnknownVars() []string {
	if r == nil {
		return nil
	}
	declared := map[string]bool{}
	for _, v := range r.Vars {
		declared[strings.TrimSpace(v.Name)] = true
	}
	var out []string
	for _, name := range placeholderNames(r.SystemPrompt) {
		if !declared[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// placeholderNames extracts every {{name}} occurrence from text.
func placeholderNames(text string) []string {
	var out []string
	for {
		start := strings.Index(text, "{{")
		if start < 0 {
			return out
		}
		rest := text[start+2:]
		end := strings.Index(rest, "}}")
		if end < 0 {
			return out
		}
		out = append(out, strings.TrimSpace(rest[:end]))
		text = rest[end+2:]
	}
}

func renderPlaceholders(text string, values map[string]string) string {
	var b strings.Builder
	for {
		start := strings.Index(text, "{{")
		if start < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:start])
		rest := text[start+2:]
		end := strings.Index(rest, "}}")
		if end < 0 {
			// Unterminated placeholder: emit the rest verbatim rather than
			// eating it — a truncated prompt is easier to spot than a silent gap.
			b.WriteString(text[start:])
			return b.String()
		}
		name := strings.TrimSpace(rest[:end])
		if v, ok := values[name]; ok {
			b.WriteString(v)
		} else {
			// Keep the placeholder so the gap is visible in the rendered prompt.
			b.WriteString("{{")
			b.WriteString(name)
			b.WriteString("}}")
		}
		text = rest[end+2:]
	}
}
