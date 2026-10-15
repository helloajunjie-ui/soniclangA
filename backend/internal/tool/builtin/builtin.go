// Package builtin provides the core built-in tools (time, calculator, http
// fetch with SSRF guard, and memory/conversation search stubs that are wired to
// the memory service in M3b). Tool results are returned as strings (DESIGN §5
// tool/builtin).
package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agenthub/agent-proxy/internal/tool"
)

// ---- get_time ----

type timeTool struct{}

func (timeTool) Name() string { return "get_time" }
func (timeTool) Description() string {
	return "返回当前日期时间；可指定 IANA 时区，默认本地时区"
}
func (timeTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"timezone": map[string]interface{}{"type": "string", "description": "IANA 时区，如 Asia/Shanghai"}},
	}
}
func (timeTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	loc := time.Local
	if tz, _ := args["timezone"].(string); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return time.Now().In(loc).Format(time.RFC3339), nil
}

// ---- calculator ----

type calcTool struct{}

func (calcTool) Name() string { return "calculator" }
func (calcTool) Description() string {
	return "计算四则运算表达式，仅支持 + - * / ( ) 与数字、小数点"
}
func (calcTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"expression": map[string]interface{}{"type": "string", "description": "算术表达式，如 (1+2)*3"}},
		"required":   []string{"expression"},
	}
}
func (calcTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	expr, _ := args["expression"].(string)
	v, err := evalExpr(expr)
	if err != nil {
		return "", err
	}
	return strconv.FormatFloat(v, 'f', -1, 64), nil
}

func evalExpr(expr string) (float64, error) {
	if strings.TrimSpace(expr) == "" {
		return 0, errors.New("empty expression")
	}
	for _, r := range expr {
		if !(r == '+' || r == '-' || r == '*' || r == '/' || r == '(' || r == ')' || r == '.' || r == ' ' || (r >= '0' && r <= '9')) {
			return 0, fmt.Errorf("illegal character %q in expression", r)
		}
	}
	prio := map[rune]int{'+': 1, '-': 1, '*': 2, '/': 2}
	tokens := tokenize(expr)
	var output []string
	var ops []rune
	for _, tok := range tokens {
		switch {
		case tok == "(":
			ops = append(ops, '(')
		case tok == ")":
			for len(ops) > 0 && ops[len(ops)-1] != '(' {
				output = append(output, string(ops[len(ops)-1]))
				ops = ops[:len(ops)-1]
			}
			if len(ops) == 0 {
				return 0, errors.New("mismatched parentheses")
			}
			ops = ops[:len(ops)-1]
		case isOp(tok):
			for len(ops) > 0 && ops[len(ops)-1] != '(' && prio[ops[len(ops)-1]] >= prio[rune(tok[0])] {
				output = append(output, string(ops[len(ops)-1]))
				ops = ops[:len(ops)-1]
			}
			ops = append(ops, rune(tok[0]))
		default:
			output = append(output, tok)
		}
	}
	for len(ops) > 0 {
		if ops[len(ops)-1] == '(' {
			return 0, errors.New("mismatched parentheses")
		}
		output = append(output, string(ops[len(ops)-1]))
		ops = ops[:len(ops)-1]
	}
	var st []float64
	for _, t := range output {
		if isOp(t) {
			if len(st) < 2 {
				return 0, errors.New("bad expression")
			}
			b := st[len(st)-1]
			a := st[len(st)-2]
			st = st[:len(st)-2]
			var r float64
			switch t[0] {
			case '+':
				r = a + b
			case '-':
				r = a - b
			case '*':
				r = a * b
			case '/':
				if b == 0 {
					return 0, errors.New("division by zero")
				}
				r = a / b
			}
			st = append(st, r)
		} else {
			f, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return 0, err
			}
			st = append(st, f)
		}
	}
	if len(st) != 1 {
		return 0, errors.New("bad expression")
	}
	return st[0], nil
}

func tokenize(expr string) []string {
	var toks []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			toks = append(toks, string(cur))
			cur = nil
		}
	}
	for _, r := range expr {
		if r == ' ' {
			flush()
			continue
		}
		if r == '+' || r == '-' || r == '*' || r == '/' || r == '(' || r == ')' {
			flush()
			toks = append(toks, string(r))
			continue
		}
		cur = append(cur, r)
	}
	flush()
	return toks
}

func isOp(t string) bool {
	return len(t) == 1 && (t[0] == '+' || t[0] == '-' || t[0] == '*' || t[0] == '/')
}

// ---- http_fetch (with SSRF guard) ----

type httpTool struct{}

func (httpTool) Name() string { return "http_fetch" }
func (httpTool) Description() string {
	return "GET 一个公网 URL 并返回前 8KB 文本（拒绝内网/云元数据地址）"
}
func (httpTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"url": map[string]interface{}{"type": "string", "description": "要抓取的 URL"}},
		"required":   []string{"url"},
	}
}
func (httpTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	raw, _ := args["url"].(string)
	if err := guardSSRF(raw); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return string(b), nil
}

func guardSSRF(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return errors.New("SSRF guard: local address blocked")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.String() == "169.254.169.254" {
			return errors.New("SSRF guard: private/metadata address blocked")
		}
	}
	return nil
}

// ---- memory_search / conversation_search (wired in M3b) ----

// MemorySearcher is injected by the memory service once available (M3b).
type MemorySearcher func(query string, topK int) string

type memorySearchTool struct {
	search MemorySearcher
}

func (t memorySearchTool) Name() string        { return "memory_search" }
func (t memorySearchTool) Description() string { return "检索长期记忆（L0-L3 双路召回）" }
func (t memorySearchTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "description": "检索查询"},
			"topK":  map[string]interface{}{"type": "integer", "description": "返回条数，默认 5"},
		},
		"required": []string{"query"},
	}
}
func (t memorySearchTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	q, _ := args["query"].(string)
	if t.search == nil {
		return "[memory_search] 记忆检索将在 M3b 接入；当前记忆模块尚未挂载检索器。", nil
	}
	return t.search(q, topKOf(args)), nil
}

type conversationSearchTool struct {
	search MemorySearcher
}

func (t conversationSearchTool) Name() string        { return "conversation_search" }
func (t conversationSearchTool) Description() string { return "检索历史会话原文（L0）" }
func (t conversationSearchTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "description": "检索查询"},
			"topK":  map[string]interface{}{"type": "integer", "description": "返回条数，默认 5"},
		},
		"required": []string{"query"},
	}
}
func (t conversationSearchTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	q, _ := args["query"].(string)
	if t.search == nil {
		return "[conversation_search] 会话检索将在 M3b 接入。", nil
	}
	return t.search(q, topKOf(args)), nil
}

func topKOf(args map[string]interface{}) int {
	if v, ok := args["topK"].(float64); ok && v > 0 {
		return int(v)
	}
	return 5
}

// NewBuiltins returns the default built-in tool set.
func NewBuiltins() []tool.Tool {
	return []tool.Tool{
		timeTool{},
		calcTool{},
		httpTool{},
		memorySearchTool{},
		conversationSearchTool{},
	}
}

// InjectMemorySearcher attaches a real retriever to the memory/conversation
// search stubs (called from main once the memory service exists).
func InjectMemorySearcher(reg interface {
	Register(tool.Tool)
}, search MemorySearcher) {
	reg.Register(memorySearchTool{search: search})
	reg.Register(conversationSearchTool{search: search})
}
