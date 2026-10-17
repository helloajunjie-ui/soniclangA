package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeHTTPMCPServer answers the Streamable HTTP transport: one POST per JSON-RPC
// message, JSON replies. `sse` makes it answer in an event stream instead, which
// the same server software really does depending on the request.
func fakeHTTPMCPServer(t *testing.T, sse bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "post only", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result interface{}
		switch req.Method {
		case "initialize":
			result = map[string]interface{}{
				"protocolVersion": "2025-06-18",
				"serverInfo":      map[string]string{"name": "fake", "version": "1.2"},
			}
		case "tools/list":
			result = map[string]interface{}{"tools": []map[string]interface{}{{
				"name": "echo", "description": "回显",
				"inputSchema": map[string]interface{}{"type": "object"},
			}}}
		case "tools/call":
			var p struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "boom" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"炸了"}}`))
				return
			}
			result = map[string]interface{}{"content": []map[string]string{
				{"type": "text", "text": "echo:" + p.Arguments["text"].(string)},
			}}
		}
		payload, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message\ndata: " + string(payload) + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
}

func TestHTTPSessionJSON(t *testing.T) {
	srv := fakeHTTPMCPServer(t, false)
	defer srv.Close()
	ctx := context.Background()
	h, err := startHTTP(ctx, srv.URL, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close()
	proto, name := h.ServerInfo()
	if proto != "2025-06-18" || !strings.Contains(name, "fake") {
		t.Errorf("server info = %q %q", proto, name)
	}
	tools, err := h.ListTools(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	out, err := h.CallTool(ctx, "echo", map[string]interface{}{"text": "你好"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out != "echo:你好" {
		t.Errorf("result = %q", out)
	}
	if _, err := h.CallTool(ctx, "boom", nil); err == nil {
		t.Error("a JSON-RPC error came back as success")
	}
}

// The same server may answer in an event stream; the transport has to accept both
// without the caller noticing.
func TestHTTPSessionEventStream(t *testing.T) {
	srv := fakeHTTPMCPServer(t, true)
	defer srv.Close()
	h, err := startHTTP(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close()
	tools, err := h.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestHTTPRejectsEmptyURL(t *testing.T) {
	if _, err := startHTTP(context.Background(), "  ", ""); err == nil {
		t.Fatal("empty URL accepted")
	}
}
