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
	"os/exec"
	"regexp"
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
func (httpTool) Execute(ctx context.Context, args map[string]interface{}) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = ""
			err = fmt.Errorf("HTTP 请求内部错误: %v", r)
		}
	}()
	raw, _ := args["url"].(string)
	if err := guardSSRF(raw); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
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

// ---- web_search (Bing CN, no API key, with timeout) ----

type webSearchTool struct{}

func (webSearchTool) Name() string { return "web_search" }
func (webSearchTool) Description() string {
	return "网络搜索（Bing+百度多源聚合，自动切换），返回标题+链接+摘要；时效性内容用它最合适"
}
func (webSearchTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query":       map[string]interface{}{"type": "string", "description": "搜索关键词"},
			"max_results": map[string]interface{}{"type": "integer", "description": "返回条数，默认 5"},
		},
		"required": []string{"query"},
	}
}
func (webSearchTool) Execute(ctx context.Context, args map[string]interface{}) (result string, err error) {
	// Panic guard: a malformed response must never crash the whole server.
	defer func() {
		if r := recover(); r != nil {
			result = ""
			err = fmt.Errorf("搜索内部错误: %v", r)
		}
	}()

	q, _ := args["query"].(string)
	if q == "" {
		return "", errors.New("query 不能为空")
	}
	maxN := 5
	if v, ok := args["max_results"].(float64); ok && v > 0 {
		maxN = int(v)
	}

	// Multi-source search: try Bing CN first, fall back to Baidu if results look bad.
	sources := []struct {
		name  string
		url   string
		parse func(html string, maxN int) []searchResult
	}{
		{"Bing", "https://cn.bing.com/search?q=" + url.QueryEscape(q) + "&count=" + strconv.Itoa(maxN*2), parseBingResults},
		{"百度", "https://www.baidu.com/s?wd=" + url.QueryEscape(q) + "&rn=" + strconv.Itoa(maxN*2), parseBaiduResults},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for _, src := range sources {
		results := trySearchSource(ctx, client, src.url, src.parse, maxN)
		if len(results) == 0 {
			continue
		}
		// Quality check: if the first result title doesn't contain any meaningful
		// substring of the query, this source likely split the keyword badly.
		if !resultsLookRelevant(results, q) {
			continue
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "[%s] 搜索结果：\n\n", src.name)
		for i, r := range results {
			fmt.Fprintf(&sb, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet)
		}
		return sb.String(), nil
	}
	return "未找到相关结果（已尝试多个搜索引擎）", nil
}

func trySearchSource(ctx context.Context, client *http.Client, searchURL string, parse func(html string, maxN int) []searchResult, maxN int) []searchResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	return parse(string(b), maxN)
}

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

// resultsLookRelevant checks if the search results seem to actually match the query.
// If the top result's title doesn't contain any 2+ char substring of the query,
// the search engine probably split the keyword (e.g. "柳岩" → "柳" dictionary entry).
func resultsLookRelevant(results []searchResult, query string) bool {
	if len(results) == 0 {
		return false
	}
	// Extract meaningful 2+ char substrings from the query (excluding quotes, spaces).
	q := strings.Trim(query, `"' `)
	if len(q) < 2 {
		return true // too short to judge, accept it
	}
	// Check if any result title contains at least one 2-char substring of the query.
	for i := 0; i+2 <= len(q); i++ {
		sub := q[i : i+2]
		for _, r := range results[:3] {
			if strings.Contains(r.Title, sub) {
				return true
			}
		}
	}
	return false
}

func parseBingResults(html string, maxN int) []searchResult {
	// Bing wraps organic results in <li class="b_algo"> blocks.
	blockRe := regexp.MustCompile(`(?is)<li class="b_algo"[^>]*>(.*?)</li>`)
	linkRe := regexp.MustCompile(`<h2[^>]*>\s*<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>\s*</h2>`)
	snipRe := regexp.MustCompile(`<p[^>]*>(.*?)</p>`)
	var results []searchResult
	blocks := blockRe.FindAllStringSubmatch(html, -1)
	for _, b := range blocks {
		if len(results) >= maxN {
			break
		}
		lm := linkRe.FindStringSubmatch(b[1])
		if lm == nil {
			continue
		}
		title := stripTags(lm[2])
		url := lm[1]
		snippet := ""
		if sm := snipRe.FindStringSubmatch(b[1]); sm != nil {
			snippet = stripTags(sm[1])
		}
		if title == "" {
			continue
		}
		results = append(results, searchResult{Title: title, URL: url, Snippet: snippet})
	}
	return results
}

func parseBaiduResults(html string, maxN int) []searchResult {
	// Baidu wraps organic results in <div class="result c-container ..."> blocks.
	// Title in <h3 class="t"><a href="...">title</a></h3>, snippet in <span class="content-right_8Zs40"> or similar.
	blockRe := regexp.MustCompile(`(?is)<div[^>]*class="[^"]*result[^"]*c-container[^"]*"[^>]*>(.*?)</div>\s*(?=<div[^>]*class="[^"]*result|<div id="page")`)
	linkRe := regexp.MustCompile(`<h3[^>]*>\s*<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>\s*</h3>`)
	snipRe := regexp.MustCompile(`<span[^>]*class="[^"]*content[^"]*"[^>]*>(.*?)</span>`)
	var results []searchResult
	blocks := blockRe.FindAllStringSubmatch(html, -1)
	for _, b := range blocks {
		if len(results) >= maxN {
			break
		}
		lm := linkRe.FindStringSubmatch(b[1])
		if lm == nil {
			continue
		}
		title := stripTags(lm[2])
		url := lm[1]
		snippet := ""
		if sm := snipRe.FindStringSubmatch(b[1]); sm != nil {
			snippet = stripTags(sm[1])
		}
		if title == "" {
			continue
		}
		results = append(results, searchResult{Title: title, URL: url, Snippet: snippet})
	}
	return results
}

func stripTags(s string) string {
	re := regexp.MustCompile(`<[^>]+>`)
	return strings.TrimSpace(re.ReplaceAllString(s, ""))
}

// ---- web_fetch (fetch URL and extract readable text) ----

type webFetchTool struct{}

func (webFetchTool) Name() string { return "web_fetch" }
func (webFetchTool) Description() string {
	return "抓取网页正文（自动去 HTML 标签，提取可读文本），适合获取文章/文档内容"
}
func (webFetchTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{"type": "string", "description": "要抓取的网页 URL"},
		},
		"required": []string{"url"},
	}
}
func (webFetchTool) Execute(ctx context.Context, args map[string]interface{}) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = ""
			err = fmt.Errorf("网页抓取内部错误: %v", r)
		}
	}()
	raw, _ := args["url"].(string)
	if err := guardSSRF(raw); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 AgentHub/1.0")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 131072))
	text := htmlToText(string(b))
	if len(text) > 8192 {
		text = text[:8192] + "\n…（内容已截断）"
	}
	return text, nil
}

func htmlToText(html string) string {
	// 去掉 script/style（Go RE2 不支持反向引用 \1，分两条写）
	html = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(html, "")
	html = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(html, "")
	// <br> 和 <p> 换成换行
	re := regexp.MustCompile(`(?i)<br\s*/?>|<p[^>]*>|</p>|<div[^>]*>|</div>|<li[^>]*>|</li>|<h[1-6][^>]*>|</h[1-6]>`)
	html = re.ReplaceAllString(html, "\n")
	// 去所有标签
	re = regexp.MustCompile(`<[^>]+>`)
	html = re.ReplaceAllString(html, "")
	// 解码常见实体
	html = strings.ReplaceAll(html, "&nbsp;", " ")
	html = strings.ReplaceAll(html, "&amp;", "&")
	html = strings.ReplaceAll(html, "&lt;", "<")
	html = strings.ReplaceAll(html, "&gt;", ">")
	html = strings.ReplaceAll(html, "&quot;", `"`)
	html = strings.ReplaceAll(html, "&#39;", "'")
	// 压缩空行
	re = regexp.MustCompile(`\n{3,}`)
	html = re.ReplaceAllString(html, "\n\n")
	return strings.TrimSpace(html)
}

// ---- shell_exec (run system commands) ----

type shellExecTool struct{}

func (shellExecTool) Name() string { return "shell_exec" }
func (shellExecTool) Description() string {
	return "执行系统命令（PowerShell/cmd），返回 stdout+stderr；用于安装软件、跑脚本、查环境"
}
func (shellExecTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{"type": "string", "description": "要执行的命令"},
			"timeout": map[string]interface{}{"type": "integer", "description": "超时秒数，默认 30"},
		},
		"required": []string{"command"},
	}
}

// blockedCommands is the fail-closed deny list for shell_exec.
//
// This is a guard rail, not a sandbox: it stops the irreversible, obviously
// system-level commands (formatting, system-wide recursive deletes, shutdown,
// boot/registry/ACL tampering, account creation) that no user asks an assistant
// for. Anything the model spells differently still runs — which is why the tool
// description says it acts on the user's machine and why the orchestrator's
// permission tier gates it. Refusing locally is cheaper than explaining a wiped
// disk.
var blockedCommands = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(?i)\bformat(\.com)?\s+[a-z]:`), "格式化磁盘"},
	{regexp.MustCompile(`(?i)\bdiskpart\b`), "分区操作"},
	{regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b`), "格式化文件系统"},
	{regexp.MustCompile(`(?i)\bdd\b[^\n]*\bof\s*=\s*/dev/`), "直接写块设备"},
	{regexp.MustCompile(`(?i)\brm\s+(-[a-z]*[rf][a-z]*\s+)+(/|/\*|~)`), "递归删除根目录"},

	{regexp.MustCompile(`(?i)\bdel\b[^\n]*/s[^\n]*/q`), "递归静默删除"},
	{regexp.MustCompile(`(?i)\b(shutdown|stop-computer|restart-computer)\b`), "关机或重启"},
	{regexp.MustCompile(`(?i)\bbcdedit\b`), "修改启动配置"},
	{regexp.MustCompile(`(?i)\bvssadmin\b[^\n]*\bdelete\b`), "删除卷影副本"},
	{regexp.MustCompile(`(?i)\bcipher\b[^\n]*/w`), "擦除磁盘空闲空间"},
	{regexp.MustCompile(`(?i)\breg\b[^\n]*\bdelete\b[^\n]*\bhklm\b`), "删除注册表系统项"},
	{regexp.MustCompile(`(?i)\b(net\s+user|net\s+localgroup)\b[^\n]*/add`), "创建账户或加组"},
}

// driveRoot matches a bare drive target such as `c:`, `c:\`, `"c:\"` — argument
// order in a recursive delete is not fixed, so this is checked separately from
// the regex list.
var driveRoot = regexp.MustCompile(`[a-z]:[\\/]?["'\s]|[a-z]:[\\/]?$`)

// blockedCommand reports whether the command matches the deny list.
func blockedCommand(cmd string) (string, bool) {
	for _, b := range blockedCommands {
		if b.re.MatchString(cmd) {
			return b.why, true
		}
	}
	low := strings.ToLower(cmd)
	if strings.Contains(low, "remove-item") && strings.Contains(low, "-recurse") && driveRoot.MatchString(low) {
		return "递归删除系统盘", true
	}
	return "", false
}

func (shellExecTool) Execute(ctx context.Context, args map[string]interface{}) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = ""
			err = fmt.Errorf("命令执行内部错误: %v", r)
		}
	}()
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return "", errors.New("command 不能为空")
	}
	// Fail closed before anything is spawned.
	if why, blocked := blockedCommand(cmdStr); blocked {
		return "", fmt.Errorf("拒绝执行：命令被判为「%s」，属于系统级破坏性操作；如果是你确实想做的，请用户自己执行", why)
	}
	timeout := 30
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeout = int(v)
	}
	if timeout > 300 {
		timeout = 300
	}
	// Enforce a hard wall-clock timeout with an independent context.
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	// Use PowerShell on Windows.
	cmd := exec.CommandContext(cmdCtx, "powershell", "-NoProfile", "-Command", cmdStr)
	out, err := cmd.CombinedOutput()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("命令超时（%d 秒），已终止。已输出:\n%s", timeout, string(out)), nil
	}
	if err != nil {
		return fmt.Sprintf("exit code: %v\n输出:\n%s", err, string(out)), nil
	}
	return string(out), nil
}

// NewBuiltins returns the default built-in tool set.
func NewBuiltins() []tool.Tool {
	return []tool.Tool{
		timeTool{},
		calcTool{},
		httpTool{},
		webSearchTool{},
		webFetchTool{},
		shellExecTool{},
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
