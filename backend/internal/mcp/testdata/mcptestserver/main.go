// Command mcptestserver is a minimal stdio MCP server used by the mcp package
// tests. It speaks newline-delimited JSON-RPC 2.0 and implements just enough of
// the protocol (initialize / tools/list / tools/call) to exercise the client.
//
// It lives under testdata/ so the go tool ignores it — it is a fixture, never
// part of the shipped binary. Tests build it with `go build` into a temp dir.
//
// Behaviour switches (env):
//
//	MCPTEST_NOTIFY=1     emit a notification before each response, to prove
//	                     notifications are not mistaken for replies
//	MCPTEST_BANNER=1     print a non-JSON banner line on stdout before anything
//	MCPTEST_SLOW_MS=500  delay before answering tools/call
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

type req struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func reply(out *bufio.Writer, id int64, result interface{}) {
	b, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
	fmt.Fprintf(out, "%s\n", b)
	out.Flush()
}

func fail(out *bufio.Writer, id int64, code int, msg string) {
	b, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]interface{}{"code": code, "message": msg},
	})
	fmt.Fprintf(out, "%s\n", b)
	out.Flush()
}

func notify(out *bufio.Writer, method string) {
	b, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": map[string]interface{}{}})
	fmt.Fprintf(out, "%s\n", b)
	out.Flush()
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 8<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	if os.Getenv("MCPTEST_BANNER") == "1" {
		fmt.Fprintln(out, "mcptestserver starting up (not JSON)")
		out.Flush()
	}

	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		var r req
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.ID == nil {
			continue // notification from the client
		}
		if os.Getenv("MCPTEST_NOTIFY") == "1" {
			// A real server emits progress/log notifications mid-call. If the
			// client mistakes one for its reply, the tool silently returns the
			// wrong payload.
			notify(out, "notifications/message")
		}

		switch r.Method {
		case "initialize":
			reply(out, *r.ID, map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]string{"name": "mcptestserver", "version": "1.0"},
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			})
		case "tools/list":
			reply(out, *r.ID, map[string]interface{}{"tools": []interface{}{
				map[string]interface{}{
					"name":        "echo",
					"description": "回显传入文本",
					"inputSchema": map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{"text": map[string]interface{}{"type": "string"}},
						"required":   []string{"text"},
					},
				},
				map[string]interface{}{
					"name":        "boom",
					"description": "总是返回错误的工具",
					"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
				},
				map[string]interface{}{
					// No description on purpose: the client must synthesise one,
					// because an empty description makes the tool dead weight.
					"name":        "nodesc",
					"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
				},
			}})
		case "tools/call":
			// MCPTEST_HANG=1 models the server that completes the handshake and
			// then goes quiet: no reply, no exit. Before the client learned to
			// honour its context, such a server parked the reader forever while
			// holding callMu, and every later call blocked behind it.
			if os.Getenv("MCPTEST_HANG") == "1" {
				select {} // sleep forever, keeping stdin open
			}
			var ms int
			if v := os.Getenv("MCPTEST_SLOW_MS"); v != "" {
				ms, _ = strconv.Atoi(v)
			}
			if ms > 0 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
			var p struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"arguments"`
			}
			_ = json.Unmarshal(r.Params, &p)
			if p.Name == "boom" {
				reply(out, *r.ID, map[string]interface{}{
					"content": []interface{}{map[string]string{"type": "text", "text": "deliberate failure"}},
					"isError": true,
				})
				break
			}
			if p.Name == "nodesc" {
				reply(out, *r.ID, map[string]interface{}{
					"content": []interface{}{
						map[string]string{"type": "text", "text": "ok"},
						map[string]interface{}{"type": "image", "data": "base64blob", "mimeType": "image/png"},
					},
				})
				break
			}
			text, _ := p.Args["text"].(string)
			reply(out, *r.ID, map[string]interface{}{
				"content": []interface{}{map[string]string{"type": "text", "text": "echo: " + text}},
			})
		default:
			fail(out, *r.ID, -32601, "method not found: "+r.Method)
		}
	}
}
