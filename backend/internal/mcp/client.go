// Package mcp implements the MCP client (DESIGN §5 mcp, §11 技术选型): a
// zero-dependency stdio JSON-RPC client with newline-delimited framing, plus a
// Manager that owns server lifecycles and proxies each server's tools into the
// tool registry.
//
// v1 speaks stdio only (DESIGN §14 A7: SSE / StreamableHTTP are v2). The
// transport is deliberately isolated behind Client so a v2 transport can be
// added without touching the Manager.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the MCP revision this client negotiates.
const ProtocolVersion = "2024-11-05"

// clientName / clientVersion identify us in the initialize handshake.
const (
	clientName    = "agenthub"
	clientVersion = "M6"
)

// CallTimeoutSeconds bounds a single tools/call round trip.
const CallTimeoutSeconds = 120

// StartTimeoutSeconds bounds process spawn + initialize handshake.
const StartTimeoutSeconds = 20

// ErrNotRunning is returned when a request is issued against a stopped server.
var ErrNotRunning = errors.New("mcp server is not running")

// rpcRequest is one JSON-RPC 2.0 request. MCP's stdio transport uses
// newline-delimited JSON, not Content-Length framing (unlike LSP).
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// rpcResponse is one JSON-RPC 2.0 response. A server may interleave
// notifications (no id), which we read and discard rather than mistaking for a
// reply to the call in flight.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Tool is one tool advertised by a server via tools/list.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
}

// contentBlock is one element of a tools/call result. MCP returns structured
// content; v1 renders text blocks and reports other kinds as a summary so an
// image-only response is still visible in the chat rather than looking empty.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// callResult is the tools/call payload.
type callResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// Client is a connected stdio MCP server.
//
// Concurrency: exactly one goroutine may read stdout at a time. A second reader
// on the same pipe would steal bytes from the reader waiting for its reply, so
// the persistent *bufio.Reader is owned by callMu and readLoop only reaps the
// process. Calls are therefore serialised, which matches the orchestrator loop
// anyway (tool calls in one turn run in sequence).
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	rd *bufio.Reader

	writeMu sync.Mutex
	callMu  sync.Mutex

	nextID int64
	closed atomic.Bool
	// procDone is closed when the child process exits, so a call waiting on a
	// reply from a crashed server fails fast instead of blocking for the full
	// timeout.
	procDone chan struct{}
	readErr  atomic.Value // error
	// procCancel releases the process-lifetime context in Close.
	procCancel context.CancelFunc

	// stderr captures the child's diagnostics (capped). A chatty server would
	// otherwise fill the pipe and deadlock, which looks identical to "the tool
	// hangs"; the tail is surfaced in the UI's connection panel.
	stderr *limited

	// initInfo is the initialize handshake result, captured at Start so the UI
	// can show what was negotiated without re-handshaking.
	initMu    sync.RWMutex
	initProto string
	initInfo  string
}

// Config is what Start needs to spawn a server.
type Config struct {
	Command string
	Args    []string
	// Env holds KEY=VALUE entries appended to the parent environment. Secret
	// values must already be decrypted by the caller (DESIGN §10: MCP env keys
	// are stored encrypted and never returned in plaintext).
	Env map[string]string
	// Dir is the working directory; empty means inherit.
	Dir string
}

// Start spawns the server and performs the initialize handshake. On any failure
// the process is killed, so a half-started server never lingers.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("command 不能为空")
	}
	// The process gets its own lifetime context, NOT the handshake one. Tying
	// exec to the handshake deadline would kill the server the instant Start
	// returns — every call after the first would hit "pipe is being closed".
	// Shutdown is driven by Close() (or by cancelling ctx).
	procCtx, procCancel := context.WithCancel(ctx)
	// Every failure path below must release procCancel, or the process-lifetime
	// context leaks (vet checks exactly this).
	started := false
	defer func() {
		if !started {
			procCancel()
		}
	}()
	hctx, hCancel := context.WithTimeout(ctx, StartTimeoutSeconds*time.Second)
	defer hCancel()

	cmd := exec.CommandContext(procCtx, cfg.Command, cfg.Args...)
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	if len(cfg.Env) > 0 {
		env := os.Environ()
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic child environment
		for _, k := range keys {
			env = append(env, k+"="+cfg.Env[k])
		}
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stderr is drained into a ring-ish cap: a chatty server that fills the
	// stderr pipe blocks forever, which looks exactly like "the tool hangs".
	stderr := &limited{Cap: 32 << 10}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 服务器失败: %w", err)
	}
	// From here the Client owns procCancel (Close releases it), so the deferred
	// cleanup above must stand down.
	started = true

	cl := &Client{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		rd:         bufio.NewReaderSize(stdout, 64<<10),
		nextID:     1,
		procDone:   make(chan struct{}),
		procCancel: procCancel,
	}
	cl.stderr = stderr
	go cl.waitProc()

	// initialize → notifications/initialized. A server that cannot complete the
	// handshake is not usable, so fail here rather than surfacing a confusing
	// error on the first real tool call.
	initRes, err := cl.call(hctx, "initialize", map[string]interface{}{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": clientName, "version": clientVersion},
	})
	if err != nil {
		cl.Close()
		return nil, fmt.Errorf("initialize 握手失败: %w", err)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if len(initRes) > 0 {
		_ = json.Unmarshal(initRes, &init)
	}
	cl.initMu.Lock()
	cl.initProto = init.ProtocolVersion
	if init.ProtocolVersion == "" {
		cl.initProto = ProtocolVersion
	}
	cl.initInfo = strings.TrimSpace(init.ServerInfo.Name + " " + init.ServerInfo.Version)
	cl.initMu.Unlock()

	if err := cl.notify(hctx, "notifications/initialized", map[string]interface{}{}); err != nil {
		cl.Close()
		return nil, fmt.Errorf("initialized 通知失败: %w", err)
	}
	return cl, nil
}

// ServerInfo returns the protocol version and server identity negotiated during
// Start.
func (c *Client) ServerInfo() (protocol, server string) {
	c.initMu.RLock()
	defer c.initMu.RUnlock()
	return c.initProto, c.initInfo
}

// ListTools calls tools/list.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := c.call(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("tools/list 返回无法解析: %w", err)
	}
	return out.Tools, nil
}

// CallTool calls tools/call and flattens the content blocks to text. An
// isError result is returned as an error so the orchestrator records it as a
// failed tool call rather than feeding a failure string to the model as if it
// were a successful answer.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	if args == nil {
		args = map[string]interface{}{}
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := c.call(ctx, "tools/call", map[string]interface{}{
		"name": name, "arguments": args,
	})
	if err != nil {
		return "", err
	}
	var cr callResult
	if err := json.Unmarshal(res, &cr); err != nil {
		// Some servers answer with a bare string or an arbitrary object. Fall
		// back to the raw JSON instead of failing the whole call.
		return string(res), nil
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
		sb.WriteString(fmt.Sprintf("[%d 个非文本内容块未展开]\n", nonText))
	}
	if cr.IsError {
		return "", fmt.Errorf("MCP 工具返回错误: %s", strings.TrimSpace(sb.String()))
	}
	return strings.TrimSpace(sb.String()), nil
}

// call performs one request/response round trip.
func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, ErrNotRunning
	}
	c.callMu.Lock()
	defer c.callMu.Unlock()

	id := atomic.AddInt64(&c.nextID, 1) - 1
	raw, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')

	c.writeMu.Lock()
	_, werr := c.stdin.Write(raw)
	c.writeMu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("写入 %s 失败: %w", method, werr)
	}

	// Read until our id comes back, discarding notifications and replies to
	// other (already-timed-out) calls. The reader is the client's single
	// persistent one; a fresh bufio.Reader per call would swallow bytes that a
	// previous call had already buffered past the newline.
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.procDone:
			if e, ok := c.readErr.Load().(error); ok && e != nil {
				return nil, e
			}
			return nil, ErrNotRunning
		default:
		}

		line, err := readLine(c.rd, 8<<20)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, ErrNotRunning
			}
			return nil, fmt.Errorf("读取 %s 响应失败: %w", method, err)
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// A non-JSON line on stdout is a server that prints banners; skip it
			// rather than failing the call.
			continue
		}
		if resp.ID == nil || *resp.ID != id {
			continue // notification or stale reply
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *Client) notify(ctx context.Context, method string, params interface{}) error {
	raw, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(raw, '\n'))
	return err
}

// waitProc reaps the child process and then signals procDone. It deliberately
// does not touch stdout: that pipe belongs to the reader inside call(), and a
// second concurrent reader would steal bytes from a call awaiting its reply.
func (c *Client) waitProc() {
	err := c.cmd.Wait()
	if err != nil && c.readErr.Load() == nil {
		// Exit status alone is not diagnostic here (it is usually "killed" after
		// a deliberate Close), so keep the message but let ErrNotRunning win in
		// call().
		c.readErr.Store(fmt.Errorf("mcp 服务器进程退出: %w", err))
	}
	close(c.procDone)
}

// Close terminates the server. It is idempotent and safe to call concurrently.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	// Closing stdin first lets a well-behaved server exit on its own.
	_ = c.stdin.Close()
	// waitProc owns cmd.Wait, so Close must not call it again.
	select {
	case <-c.procDone:
	case <-time.After(3 * time.Second):
		// A server that ignores stdin close gets killed; waitProc still reaps it
		// so no zombie is left behind.
		if c.procCancel != nil {
			c.procCancel()
		}
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		select {
		case <-c.procDone:
		case <-time.After(2 * time.Second):
		}
	}
	if c.procCancel != nil {
		c.procCancel()
	}
	_ = c.stdout.Close()
	return nil
}

// Running reports whether the client is still connected.
func (c *Client) Running() bool { return !c.closed.Load() }

// Stderr returns the tail of the child's stderr output (diagnostics only).
func (c *Client) Stderr() string {
	if c.stderr == nil {
		return ""
	}
	return c.stderr.String()
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, CallTimeoutSeconds*time.Second)
}

// readLine reads one newline-delimited frame with a size cap.
func readLine(rd *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := rd.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > max {
			return nil, fmt.Errorf("响应超过 %d 字节上限", max)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// limited is a bounded writer used for draining stderr.
type limited struct {
	Cap  int
	buf  []byte
	full bool
}

func (l *limited) Write(p []byte) (int, error) {
	if !l.full {
		room := l.Cap - len(l.buf)
		if len(p) < room {
			l.buf = append(l.buf, p...)
		} else {
			l.buf = append(l.buf, p[:room]...)
			l.full = true
		}
	}
	return len(p), nil
}

func (l *limited) String() string { return string(l.buf) }
