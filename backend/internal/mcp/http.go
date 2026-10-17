package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// session is one live MCP connection, whatever the transport.
//
// Until now a connection could only be a child process talking over stdin/stdout.
// Plenty of useful MCP servers are hosted (a URL, sometimes with a token), and
// those could not be added at all. The interface is deliberately the shape the
// manager already uses, so stdio stays the default and HTTP is an addition rather
// than a rewrite.
type session interface {
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error)
	ServerInfo() (proto, server string)
	Stderr() string
	Running() bool
	Close() error
}

var _ session = (*Client)(nil)

// httpSession speaks the Streamable HTTP transport: every JSON-RPC message is one
// POST to the server URL, and the reply comes back either as application/json or
// as a single event-stream message carrying the same JSON.
//
// There is no persistent connection to keep alive, which is why this transport is
// implemented first: it needs no reader goroutine, so a hung server cannot wedge a
// shared reader the way a stdio pipe could.
type httpSession struct {
	url     string
	token   string
	client  *http.Client
	nextID  atomic.Int64
	proto   string
	server  string
	closed  atomic.Bool
	session string // Mcp-Session-Id handed out by servers that keep state
}

// startHTTP performs the initialize handshake and returns a usable session.
func startHTTP(ctx context.Context, url, token string) (*httpSession, error) {
	if strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("MCP 服务器 URL 为空")
	}
	h := &httpSession{url: strings.TrimSpace(url), token: token, client: &http.Client{}}
	res, err := h.rpc(ctx, "initialize", map[string]interface{}{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]string{"name": "AgentHub", "version": "M4"},
	})
	if err != nil {
		return nil, err
	}
	var info struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if json.Unmarshal(res, &info) == nil {
		h.proto = info.ProtocolVersion
		h.server = strings.TrimSpace(info.ServerInfo.Name + " " + info.ServerInfo.Version)
	}
	if err := h.notify(ctx, "notifications/initialized", nil); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *httpSession) ServerInfo() (string, string) { return h.proto, h.server }
func (h *httpSession) Stderr() string               { return "" }
func (h *httpSession) Running() bool                { return !h.closed.Load() }

func (h *httpSession) Close() error {
	h.closed.Store(true)
	return nil
}

func (h *httpSession) ListTools(ctx context.Context) ([]Tool, error) {
	res, err := h.rpc(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("解析 tools/list 响应失败: %w", err)
	}
	return out.Tools, nil
}

func (h *httpSession) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	if args == nil {
		args = map[string]interface{}{}
	}
	res, err := h.rpc(ctx, "tools/call", map[string]interface{}{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	return renderToolResult(res), nil
}

// renderToolResult is shared by both transports so a server cannot behave
// differently depending on how it is reached.
func renderToolResult(res json.RawMessage) string {
	var cr callResult
	if err := json.Unmarshal(res, &cr); err != nil {
		return string(res)
	}
	var sb strings.Builder
	nonText := 0
	for _, b := range cr.Content {
		if b.Type == "text" || b.Text != "" {
			sb.WriteString(b.Text)
			if !strings.HasSuffix(b.Text, "\n") {
				sb.WriteString("\n")
			}
			continue
		}
		nonText++
	}
	if nonText > 0 {
		fmt.Fprintf(&sb, "[另有 %d 个非文本返回块]", nonText)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// rpc sends one request and waits for its reply.
func (h *httpSession) rpc(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id := h.nextID.Add(1)
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	raw, ctype, err := h.post(ctx, body)
	if err != nil {
		return nil, err
	}
	if strings.Contains(ctype, "text/event-stream") {
		if raw, err = firstEventData(raw); err != nil {
			return nil, err
		}
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%s 响应不是 JSON: %w", method, err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

func (h *httpSession) notify(ctx context.Context, method string, params interface{}) error {
	body, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	_, _, err = h.post(ctx, body)
	return err
}

func (h *httpSession) post(ctx context.Context, body []byte) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	if h.session != "" {
		req.Header.Set("Mcp-Session-Id", h.session)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("连接 MCP 服务器失败: %w", err)
	}
	defer resp.Body.Close()
	// Sessions are optional; remember the id when one is handed out.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.session = sid
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, "", fmt.Errorf("MCP 服务器拒绝访问（%d）：检查令牌", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("MCP 服务器返回 %d：%s", resp.StatusCode, truncate(string(out), 300))
	}
	return out, resp.Header.Get("Content-Type"), nil
}

// firstEventData pulls the payload out of a single-message event stream.
func firstEventData(raw []byte) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if line == "" && len(data) > 0 {
			break // end of the first event
		}
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("事件流里没有 data")
	}
	return []byte(strings.Join(data, "\n")), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
