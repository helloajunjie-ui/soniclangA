package llmjson

import "testing"

// TestTrailingProseWithBrace is the regression that silently broke automatic
// memory extraction: slicing to the LAST "}" in the reply pulls trailing prose
// into the JSON whenever that prose contains a brace.
func TestTrailingProseWithBrace(t *testing.T) {
	raw := `{"memories":[{"category":"fact","content":"用户叫李工"}]}

希望对你有帮助 😊 如有其他问题 {例如格式} 欢迎继续提问。`
	var out struct {
		Memories []struct {
			Category string `json:"category"`
			Content  string `json:"content"`
		} `json:"memories"`
	}
	if err := UnmarshalInto(raw, &out); err != nil {
		t.Fatalf("trailing prose broke the parse: %v", err)
	}
	if len(out.Memories) != 1 || out.Memories[0].Content != "用户叫李工" {
		t.Fatalf("wrong payload: %+v", out.Memories)
	}
}

func TestCodeFence(t *testing.T) {
	raw := "好的，结果如下：\n```json\n{\"persona\":\"# 画像\\n- 后端架构师\"}\n```\n需要补充吗？"
	var out struct {
		Persona string `json:"persona"`
	}
	if err := UnmarshalInto(raw, &out); err != nil {
		t.Fatalf("code fence broke the parse: %v", err)
	}
	if out.Persona == "" || out.Persona[:2] != "# " {
		t.Fatalf("wrong payload: %q", out.Persona)
	}
}

// TestBracesInsideStrings makes sure the scanner does not treat a brace inside
// a JSON string value as structure.
func TestBracesInsideStrings(t *testing.T) {
	raw := `{"content":"配置形如 {\"a\":1} 的 JSON"}`
	var out struct {
		Content string `json:"content"`
	}
	if err := UnmarshalInto(raw, &out); err != nil {
		t.Fatalf("brace inside string broke the parse: %v", err)
	}
	if out.Content != `配置形如 {"a":1} 的 JSON` {
		t.Fatalf("wrong payload: %q", out.Content)
	}
}

func TestNoJSON(t *testing.T) {
	for _, raw := range []string{"", "抱歉，我无法完成。", "no object here"} {
		var out map[string]interface{}
		if err := UnmarshalInto(raw, &out); err == nil {
			t.Errorf("expected an error for %q", raw)
		}
	}
}

func TestUnbalanced(t *testing.T) {
	var out map[string]interface{}
	if err := UnmarshalInto(`{"a": {"b": 1}`, &out); err == nil {
		t.Error("expected an error for an unbalanced object")
	}
}
