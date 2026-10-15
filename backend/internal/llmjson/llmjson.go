// Package llmjson extracts JSON objects from LLM responses.
//
// Models wrap structured output in prose, prepend "Sure!" or append commentary
// after the closing brace. Slicing between the first "{" and the LAST "}" in the
// reply looks reasonable and is wrong: trailing prose can contain another brace,
// which silently pulls garbage into the JSON and fails the parse. The scanner
// here walks the string and stops at the brace that actually closes the object,
// so trailing text is ignored no matter what it contains.
package llmjson

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Object returns the first balanced JSON object found in s, tolerating a
// Markdown code fence, a language tag, leading prose and trailing commentary.
func Object(s string) (string, error) {
	t := stripFence(strings.TrimSpace(s))
	if t == "" {
		return "", fmt.Errorf("empty response")
	}
	start := strings.Index(t, "{")
	if start < 0 {
		return "", fmt.Errorf("no JSON object in response")
	}
	depth := 0
	inStr, esc := false, false
	for i := start; i < len(t); i++ {
		c := t[i]
		switch {
		case esc:
			esc = false
		case inStr:
			if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return t[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced JSON in response")
}

// UnmarshalInto parses the first JSON object in raw into v.
func UnmarshalInto(raw string, v interface{}) error {
	obj, err := Object(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(obj), v)
}

// stripFence removes a Markdown code fence wrapper if present.
func stripFence(s string) string {
	i := strings.Index(s, "```")
	if i < 0 {
		return s
	}
	rest := s[i+3:]
	// drop the rest of the opening fence line (which may carry a language tag)
	if j := strings.Index(rest, "\n"); j >= 0 {
		rest = rest[j+1:]
	}
	if k := strings.Index(rest, "```"); k >= 0 {
		return strings.TrimSpace(rest[:k])
	}
	return strings.TrimSpace(rest)
}
