// Package handler — routes for the two self-built tool surfaces: script tools
// (/api/v1/scripts) and MCP servers (/api/v1/mcp/servers). Both follow DESIGN §8.
package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/agenthub/agent-proxy/internal/mcp"
	"github.com/agenthub/agent-proxy/internal/tool/sandbox"
)

// errMissingTool is returned when a /call request omits the tool name.
var errMissingTool = errors.New("缺少 tool 字段")

// ---- scripts ----

func (h *Handler) scripts(w http.ResponseWriter, r *http.Request, p string) {
	if h.Scripts == nil {
		http.Error(w, "script tools not available", http.StatusNotFound)
		return
	}
	switch {
	case p == "/api/v1/scripts" || p == "/api/v1/scripts/":
		switch r.Method {
		case http.MethodGet:
			h.json(w, 200, h.Scripts.List())
		case http.MethodPost:
			var body struct {
				Script *sandbox.ScriptToolEntity `json:"script"`
			}
			if err := h.decode(r, &body); err != nil || body.Script == nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Scripts.Create(body.Script); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// A created script is deliberately disabled (DESIGN §10); say so in the
			// response so the UI does not have to hard-code the rule.
			h.json(w, 201, map[string]interface{}{
				"script": body.Script,
				"notice": "已保存。新脚本默认不启用——先执行准入校验，通过后再启用。",
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case p == "/api/v1/scripts/runtimes":
		h.json(w, 200, sandbox.RuntimeBinaries())
	case strings.HasSuffix(p, "/admit"):
		h.scriptAction(w, r, strings.TrimSuffix(p, "/admit"), func(id string) (interface{}, error) {
			res, err := h.Scripts.Admit(r.Context(), id)
			if err != nil {
				return nil, err
			}
			return res, nil
		})
	case strings.HasSuffix(p, "/toggle"):
		h.scriptAction(w, r, strings.TrimSuffix(p, "/toggle"), func(id string) (interface{}, error) {
			e, err := h.Scripts.Get(id)
			if err != nil {
				return nil, err
			}
			return h.Scripts.SetEnabled(id, !e.Enabled)
		})
	case strings.HasSuffix(p, "/run"):
		h.scriptAction(w, r, strings.TrimSuffix(p, "/run"), func(id string) (interface{}, error) {
			var body struct {
				Input string `json:"input"`
			}
			_, _ = h.decodeOptional(r, &body)
			return h.Scripts.Run(r.Context(), id, body.Input)
		})
	default:
		id := pathID(p, "/api/v1/scripts/")
		if id == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			e, err := h.Scripts.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, e)
		case http.MethodPut, http.MethodPatch:
			var body struct {
				Script *sandbox.ScriptToolEntity `json:"script"`
			}
			if err := h.decode(r, &body); err != nil || body.Script == nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Scripts.Update(id, body.Script); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			e, _ := h.Scripts.Get(id)
			h.json(w, 200, e)
		case http.MethodDelete:
			if err := h.Scripts.Delete(id); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// scriptAction runs fn for POST /scripts/{id}/{action} and maps errors to 400
// (they are all user-fixable: bad code, missing interpreter, not admitted yet).
func (h *Handler) scriptAction(w http.ResponseWriter, r *http.Request, path string, fn func(id string) (interface{}, error)) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := pathID(path, "/api/v1/scripts/")
	if id == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	out, err := fn(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.json(w, 200, out)
}

// ---- mcp ----

func (h *Handler) mcpServers(w http.ResponseWriter, r *http.Request, p string) {
	if h.MCP == nil {
		http.Error(w, "mcp not available", http.StatusNotFound)
		return
	}
	switch {
	case p == "/api/v1/mcp/catalog" || p == "/api/v1/mcp/catalog/":
		// The known-good list. Each entry is a git/npm/pypi source that npx/uvx
		// fetches on first run, so "install" is just recording the launch line.
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.json(w, 200, h.MCP.Catalog())
	case strings.HasPrefix(p, "/api/v1/mcp/catalog/"):
		// POST /api/v1/mcp/catalog/{id}/install {"env": {...}}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rest := strings.Trim(strings.TrimPrefix(p, "/api/v1/mcp/catalog/"), "/")
		id := strings.TrimSuffix(rest, "/install")
		if id == "" || id == rest && !strings.HasSuffix(rest, "/install") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body struct {
			Env map[string]string `json:"env"`
		}
		_, _ = h.decodeOptional(r, &body)
		info, err := h.MCP.InstallFromCatalog(r.Context(), id, body.Env)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.json(w, 201, info)
	case p == "/api/v1/mcp/servers" || p == "/api/v1/mcp/servers/":
		switch r.Method {
		case http.MethodGet:
			h.json(w, 200, h.MCP.List())
		case http.MethodPost:
			var s mcp.Server
			if err := h.decode(r, &s); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.MCP.Create(r.Context(), &s); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.json(w, 201, s)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case strings.HasSuffix(p, "/test"):
		// Test accepts an unsaved definition so an operator can validate a command
		// line before committing it; it never registers tools.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var s mcp.Server
		if err := h.decode(r, &s); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if s.Command == "" {
			id := strings.TrimSuffix(pathID(p, "/api/v1/mcp/servers/"), "/test")
			existing, err := h.MCP.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			s = *existing
		}
		res, err := h.MCP.Test(r.Context(), &s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.json(w, 200, res)
	default:
		// /api/v1/mcp/servers/{id}[/{action}]
		rest := strings.Trim(strings.TrimPrefix(p, "/api/v1/mcp/servers/"), "/")
		id, action := rest, ""
		if i := strings.Index(rest, "/"); i >= 0 {
			id, action = rest[:i], rest[i+1:]
		}
		switch action {
		case "start":
			h.mcpAction(w, r, id, func() (interface{}, error) { return h.MCP.Start(r.Context(), id) })
			return
		case "stop":
			h.mcpAction(w, r, id, func() (interface{}, error) {
				return map[string]interface{}{"ok": true}, h.MCP.Stop(r.Context(), id)
			})
			return
		case "call":
			h.mcpAction(w, r, id, func() (interface{}, error) {
				var body struct {
					Tool string                 `json:"tool"`
					Args map[string]interface{} `json:"args"`
				}
				if err := h.decode(r, &body); err != nil {
					return nil, err
				}
				if strings.TrimSpace(body.Tool) == "" {
					return nil, errMissingTool
				}
				return h.MCP.CallTool(r.Context(), id, body.Tool, body.Args)
			})
			return
		case "":
			// fall through to CRUD below
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if id == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s, err := h.MCP.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, s)
		case http.MethodPut, http.MethodPatch:
			var s mcp.Server
			if err := h.decode(r, &s); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.MCP.Update(r.Context(), id, &s); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.json(w, 200, s)
		case http.MethodDelete:
			if err := h.MCP.Delete(r.Context(), id); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func (h *Handler) mcpAction(w http.ResponseWriter, r *http.Request, id string, fn func() (interface{}, error)) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if id == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	out, err := fn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.json(w, 200, out)
}
