package brief

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/agenthub/agent-proxy/internal/tool"
)

// Tool names. They are separate tools rather than one with an action parameter
// because the model picks tools by name, and "写下用户的想法" and "回看已经写下的"
// are different intents with different argument shapes.
const (
	ToolCreate = "brief_create"
	ToolAppend = "brief_append"
	ToolRead   = "brief_read"
	ToolList   = "brief_list"
)

// RegisterTools mounts the brief store.
func (s *Service) RegisterTools(reg interface{ Register(tool.Tool) }) {
	reg.Register(briefTool{svc: s, name: ToolCreate})
	reg.Register(briefTool{svc: s, name: ToolAppend})
	reg.Register(briefTool{svc: s, name: ToolRead})
	reg.Register(briefTool{svc: s, name: ToolList})
}

type briefTool struct {
	svc  *Service
	name string
}

func (t briefTool) Name() string { return t.name }

func (t briefTool) Description() string {
	switch t.name {
	case ToolCreate:
		return "开一份简报：当用户想做的东西一时半会开不了工（要先装软件、或要先花时间准备）时，" +
			"先把想做的记下来，边等边聊边补。之后用 brief_append 往里加内容。"
	case ToolAppend:
		return "往简报里补一段：用户又说了新的想法、改了主意、补了细节，就追加进去，不要重写整份。"
	case ToolRead:
		return "读某份简报的全文，开工前用它把用户的想法捞回来。"
	case ToolList:
		return "列出已有的简报（标题、时间、文件路径），看之前有没有记过。"
	}
	return ""
}

func (t briefTool) Schema() map[string]interface{} {
	switch t.name {
	case ToolCreate:
		return map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"title": map[string]interface{}{"type": "string", "description": "简报标题，例如「2.5D 游戏」"},
			},
			"required": []string{"title"},
		}
	case ToolAppend:
		return map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":   map[string]interface{}{"type": "string", "description": "简报 id（brief_create 返回的）"},
				"text": map[string]interface{}{"type": "string", "description": "要补进去的内容，Markdown"},
			},
			"required": []string{"id", "text"},
		}
	case ToolRead:
		return map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string", "description": "简报 id"}},
			"required":   []string{"id"},
		}
	}
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

func (t briefTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	get := func(k string) string {
		v, _ := args[k].(string)
		return strings.TrimSpace(v)
	}
	switch t.name {
	case ToolCreate:
		title := get("title")
		if title == "" {
			return "", errors.New("缺少 title")
		}
		b, err := t.svc.Create(title)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已开好简报：id=%s\n文件：%s\n（用 brief_append 往里补内容）", b.ID, b.Path), nil
	case ToolAppend:
		id, text := get("id"), get("text")
		if id == "" || text == "" {
			return "", errors.New("缺少 id 或 text")
		}
		b, err := t.svc.Append(id, text)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已补进简报 %s（现共 %d 字节）：\n%s", b.ID, b.Bytes, b.Path), nil
	case ToolRead:
		id := get("id")
		if id == "" {
			return "", errors.New("缺少 id")
		}
		b, err := t.svc.Get(id)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("简报 %s（%s）：\n\n%s", b.ID, b.Path, b.Content), nil
	case ToolList:
		list, err := t.svc.List()
		if err != nil {
			return "", err
		}
		if len(list) == 0 {
			return "还没有任何简报", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "共 %d 份简报：\n", len(list))
		for _, it := range list {
			fmt.Fprintf(&b, "- id=%s  %s  %dB  %s\n", it.ID, it.Title, it.Bytes, it.Path)
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("unknown brief tool %s", t.name)
}
