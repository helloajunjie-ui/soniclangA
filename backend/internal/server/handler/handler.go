// Package handler implements the HTTP API: REST CRUD for every module, the SSE
// chat stream and the OpenAI-compatible surface (/v1/chat/completions,
// /v1/models). Routes follow DESIGN §15 (/api/v1/... prefix).
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agenthub/agent-proxy/internal/brief"
	"github.com/agenthub/agent-proxy/internal/config"
	"github.com/agenthub/agent-proxy/internal/control"
	"github.com/agenthub/agent-proxy/internal/conversation"
	"github.com/agenthub/agent-proxy/internal/job"
	"github.com/agenthub/agent-proxy/internal/knowledge"
	"github.com/agenthub/agent-proxy/internal/knowledge/ingest"
	"github.com/agenthub/agent-proxy/internal/mcp"
	"github.com/agenthub/agent-proxy/internal/memory"
	"github.com/agenthub/agent-proxy/internal/memory/consolidate"
	"github.com/agenthub/agent-proxy/internal/memory/extract"
	"github.com/agenthub/agent-proxy/internal/methodology"
	"github.com/agenthub/agent-proxy/internal/module"
	"github.com/agenthub/agent-proxy/internal/orchestrator"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/rea"
	"github.com/agenthub/agent-proxy/internal/role"
	"github.com/agenthub/agent-proxy/internal/skill"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
	"github.com/agenthub/agent-proxy/internal/tool/sandbox"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Handler holds every service and the orchestrator.
type Handler struct {
	Cfg       *config.Config
	Providers *provider.Service
	Roles     *role.Service
	Convs     *conversation.Service
	Skills    *skill.Service
	Knowledge *knowledge.Service
	Memory    *memory.Service
	Method    *methodology.Service
	Control   *control.Service
	REA       *rea.Service
	Tools     *registry.Registry
	Jobs      *job.Service
	Ingest    *ingest.Worker
	Orch      *orchestrator.Orchestrator
	// M3b: L1 distillation and the L1→L2→L3 consolidation pipeline.
	Extractor   *extract.Extractor
	Consolidate *consolidate.Worker
	// Modules is the switch registry (DESIGN §5); nil disables the routes.
	Modules *module.Registry
	// Briefs is the notes store used while a task is being prepared
	// (DESIGN §9.3); nil disables the routes.
	Briefs *brief.Service
	// Scripts and MCP are the self-built tool surfaces (DESIGN §8
	// /api/v1/scripts, /api/v1/mcp/servers); nil disables the routes.
	Scripts *sandbox.Service
	MCP     *mcp.Manager

	static http.Handler
}

// New constructs a Handler and a static file server for the embedded dist.
func New(cfg *config.Config, prov *provider.Service, roles *role.Service, convs *conversation.Service,
	sk *skill.Service, know *knowledge.Service, mem *memory.Service, meth *methodology.Service,
	ctrl *control.Service, reaSvc *rea.Service, tools *registry.Registry, jobs *job.Service,
	ing *ingest.Worker, orch *orchestrator.Orchestrator, ex *extract.Extractor,
	cons *consolidate.Worker, static http.Handler) *Handler {
	return &Handler{
		Cfg: cfg, Providers: prov, Roles: roles, Convs: convs, Skills: sk,
		Knowledge: know, Memory: mem, Method: meth, Control: ctrl, REA: reaSvc,
		Tools: tools, Jobs: jobs, Ingest: ing, Orch: orch,
		Extractor: ex, Consolidate: cons, static: static,
	}
}

// ---- middleware + dispatch ----

// ServeHTTP applies CORS + auth and dispatches to the right handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS previously answered "*" for every request and the API accepted any
	// Origin. Binding to loopback does not make that safe: the browser of the
	// user sitting at the machine is also "local", so any page they visited
	// could read this API's responses (conversation history, memories, window
	// titles) and drive it with side-effecting writes — including flipping the
	// desktop-control module on and then operating the desktop. The built-in UI
	// is served from this same origin and the dev server proxies /api, so no
	// wildcard is needed at all.
	originOK := originAllowed(r)
	if originOK {
		if o := r.Header.Get("Origin"); o != "" {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Add("Vary", "Origin")
		}
	}
	if r.Method == http.MethodOptions {
		if !originOK {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if isAPIPath(r.URL.Path) && !originOK {
		// Refuse before dispatch: a forbidden origin must not reach a handler,
		// whose side effect would happen regardless of what the browser then
		// allows the page to read.
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/health" {
		h.json(w, 200, map[string]interface{}{"status": "ok", "version": "M4"})
		return
	}
	if h.Cfg.Token != "" && !h.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/api/health" || p == "/api/v1/health":
		h.json(w, 200, map[string]interface{}{"status": "ok", "version": "M4"})
	case strings.HasPrefix(p, "/api/v1/debug"):
		h.debug(w, r, p)
	case strings.HasPrefix(p, "/api/v1/providers"):
		h.providers(w, r, p)
	case strings.HasPrefix(p, "/api/v1/roles"):
		h.roles(w, r, p)
	case strings.HasPrefix(p, "/api/v1/conversations"):
		h.conversations(w, r, p)
	case strings.HasPrefix(p, "/api/v1/skills"):
		h.skills(w, r, p)
	case strings.HasPrefix(p, "/api/v1/knowledge"):
		h.knowledge(w, r, p)
	case strings.HasPrefix(p, "/api/v1/memories"):
		h.memories(w, r, p)
	case strings.HasPrefix(p, "/api/v1/methodologies"):
		h.methodologies(w, r, p)
	case strings.HasPrefix(p, "/api/v1/control"):
		h.control(w, r, p)
	case strings.HasPrefix(p, "/api/v1/env"):
		h.env(w, r, p)
	case strings.HasPrefix(p, "/api/v1/briefs"):
		h.briefs(w, r, p)
	case p == "/api/v1/tools" || p == "/api/v1/tools/":
		h.json(w, 200, h.Tools.List())
	case strings.HasPrefix(p, "/api/v1/scripts"):
		h.scripts(w, r, p)
	case strings.HasPrefix(p, "/api/v1/mcp"):
		h.mcpServers(w, r, p)
	case strings.HasPrefix(p, "/api/v1/modules"):
		h.modules(w, r, p)
	case strings.HasPrefix(p, "/api/v1/jobs"):
		h.jobs(w, r, p)
	case strings.HasPrefix(p, "/api/v1/generations"):
		h.generations(w, r, p)
	case p == "/api/v1/settings":
		h.settings(w, r)
	case p == "/v1/chat/completions":
		h.openAIChat(w, r)
	case p == "/v1/models":
		h.models(w, r)
	default:
		if h.static != nil {
			// The SPA is rebuilt frequently in development and assets are
			// content-hashed. Never let a browser serve a stale index.html or
			// a stale chunk from cache — a stale index pins the whole app to
			// an old build, which is exactly the "streaming never shows up,
			// reload fixes it" failure. Force a revalidation every load.
			w.Header().Set("Cache-Control", "no-store")
			h.static.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func (h *Handler) authed(r *http.Request) bool {
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		return strings.TrimPrefix(ah, "Bearer ") == h.Cfg.Token
	}
	return r.URL.Query().Get("token") == h.Cfg.Token
}

// isAPIPath reports whether a path is served by this API (as opposed to the
// embedded UI or an OpenAI-compatible endpoint, which callers reach from their
// own clients and which are covered by the same rule below).
func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/v1" || strings.HasPrefix(p, "/v1/")
}

// originAllowed decides whether a request may touch the API.
//
// A request with no Origin header is allowed: that is curl, a native client, or
// a same-origin GET, none of which a web page can forge. The header only
// appears when a browser initiated the request from another origin, which is
// exactly the case that must be checked — a page the user happens to have open
// must not be able to read or drive their local agent.
//
// An origin is accepted when it is the same origin as the request Host (which
// also covers a LAN address, since both sides derive from the same URL) or a
// loopback name (the Vite dev server runs on its own port and proxies to the
// backend). Anything else is refused.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	// "null" is what a sandboxed iframe or a data:/file: document reports; it
	// carries no usable identity, so it cannot be trusted.
	if o == "null" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	return isLoopbackHost(u.Hostname())
}

func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ---- helpers ----

func (h *Handler) json(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) decode(r *http.Request, v interface{}) error {
	b, _ := io.ReadAll(r.Body)
	if len(b) == 0 {
		return fmt.Errorf("empty body")
	}
	return json.Unmarshal(b, v)
}

// decodeOptional unmarshals the body only when one was sent. Endpoints whose
// parameters are all optional use it so that a bare POST (curl, a UI switch)
// is not rejected as "bad body".
func (h *Handler) decodeOptional(r *http.Request, v interface{}) (bool, error) {
	b, _ := io.ReadAll(r.Body)
	if len(bytes.TrimSpace(b)) == 0 {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}

func pathID(p, prefix string) string {
	rest := strings.Trim(strings.TrimPrefix(p, prefix), "/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func queryInt(q map[string][]string, key string, def int) int {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		if n, err := strconv.Atoi(vs[0]); err == nil {
			return n
		}
	}
	return def
}

// ---- providers ----

func (h *Handler) providers(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/api/v1/providers" || p == "/api/v1/providers/":
		switch r.Method {
		case http.MethodGet:
			h.json(w, 200, h.Providers.List())
		case http.MethodPost:
			var body struct {
				Provider *provider.Provider `json:"provider"`
				Secrets  []string           `json:"secrets"`
			}
			if err := h.decode(r, &body); err != nil || body.Provider == nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Providers.Create(body.Provider, body.Secrets); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 201, body.Provider)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case p == "/api/v1/providers/types":
		h.json(w, 200, provider.TemplateTypes())
	default:
		id := pathID(p, "/api/v1/providers/")
		switch {
		case strings.HasSuffix(p, "/test"):
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := h.Providers.Test(id); err != nil {
				h.json(w, 200, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true})
		case strings.HasSuffix(p, "/models"):
			// The model catalogue is discovered from the vendor, so this is a
			// read-through refresh rather than an edit: it is the only way to
			// pick up models added upstream after the first probe.
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			models, err := h.Providers.RefreshModels(id)
			if err != nil {
				h.json(w, 200, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true, "models": models})
		case strings.HasSuffix(p, "/model-prefs/all"):
			// Bulk apply. One request instead of one per model: the catalogue
			// runs to hundreds of entries on an aggregator.
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var all struct {
				Disabled *bool `json:"disabled"`
				Favorite *bool `json:"favorite"`
			}
			if err := h.decode(r, &all); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Providers.SetAllModelPrefs(id, all.Disabled, all.Favorite); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			pv, err := h.Providers.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, pv)
		case strings.HasSuffix(p, "/model-prefs"):
			// Per-model curation. Both halves are pointers so that "leave this
			// half alone" is expressible: with bare bools, toggling a favourite
			// would carry disabled=false with it and silently re-enable a model
			// the user had switched off.
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				ModelID  string `json:"modelId"`
				Disabled *bool  `json:"disabled"`
				Favorite *bool  `json:"favorite"`
			}
			if err := h.decode(r, &body); err != nil || body.ModelID == "" {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Providers.SetModelPref(id, body.ModelID, body.Disabled, body.Favorite); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			pv, err := h.Providers.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, pv)
		case strings.HasSuffix(p, "/toggle"):
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := h.decode(r, &body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Providers.SetEnabled(id, body.Enabled); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			pv, err := h.Providers.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, pv)
		case strings.HasSuffix(p, "/keys"):
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Secret string `json:"secret"`
				Label  string `json:"label"`
			}
			if err := h.decode(r, &body); err != nil || body.Secret == "" {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Providers.AddKey(id, body.Secret, body.Label); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 201, map[string]interface{}{"ok": true})
		case strings.Contains(p, "/keys/"):
			if r.Method != http.MethodDelete {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			keyID := p[strings.Index(p, "/keys/")+len("/keys/"):]
			if err := h.Providers.RemoveKey(id, keyID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true})
		default:
			switch r.Method {
			case http.MethodGet:
				pv, err := h.Providers.Get(id)
				if err != nil {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				h.json(w, 200, pv)
			case http.MethodPut:
				var pv provider.Provider
				if err := h.decode(r, &pv); err != nil {
					http.Error(w, "bad body", http.StatusBadRequest)
					return
				}
				pv.ID = id
				if err := h.Providers.Update(id, &pv); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				h.json(w, 200, pv)
			case http.MethodDelete:
				if err := h.Providers.Delete(id); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				h.json(w, 200, map[string]interface{}{"ok": true})
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		}
	}
}

// ---- roles ----

func (h *Handler) roles(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/api/v1/roles" || p == "/api/v1/roles/":
		switch r.Method {
		case http.MethodGet:
			h.json(w, 200, h.Roles.List())
		case http.MethodPost:
			var rl role.Role
			if err := h.decode(r, &rl); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Roles.Create(&rl); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 201, rl)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case p == "/api/v1/roles/import":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		rl, err := h.Roles.Import(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.json(w, 201, rl)
	case p == "/api/v1/roles/export":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, err := h.Roles.ExportAll()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	default:
		id := pathID(p, "/api/v1/roles/")
		if strings.HasSuffix(p, "/duplicate") {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			_ = h.decode(r, &body)
			rl, err := h.Roles.Duplicate(id, body.Name)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 201, rl)
			return
		}
		if strings.HasSuffix(p, "/render") {
			// Preview the system prompt with the given variable values, so the
			// UI can show the user exactly what the model will receive.
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Vars map[string]string `json:"vars"`
			}
			_ = h.decode(r, &body)
			rl, err := h.Roles.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			prompt, missing, err := rl.ApplyVars(body.Vars)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.json(w, 200, map[string]interface{}{
				"prompt": prompt, "missing": missing,
				"policy": rl.Policy(), "unknown": rl.UnknownVars(),
			})
			return
		}
		if strings.HasSuffix(p, "/export") {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			data, err := h.Roles.Export(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		switch r.Method {
		case http.MethodGet:
			rl, err := h.Roles.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, rl)
		case http.MethodPut:
			var rl role.Role
			if err := h.decode(r, &rl); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Roles.Update(id, &rl); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 200, rl)
		case http.MethodDelete:
			if err := h.Roles.Delete(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// ---- conversations ----

func (h *Handler) conversations(w http.ResponseWriter, r *http.Request, p string) {
	if p == "/api/v1/conversations" || p == "/api/v1/conversations/" {
		switch r.Method {
		case http.MethodGet:
			list, _ := h.Convs.List(nil)
			h.json(w, 200, list)
		case http.MethodPost:
			var body struct {
				RoleID string            `json:"roleId"`
				Mode   string            `json:"mode"`
				Vars   map[string]string `json:"vars"`
			}
			_ = h.decode(r, &body)
			if body.RoleID == "" {
				body.RoleID = h.Cfg.GetSettings().DefaultRoleId
			}
			if body.RoleID == "" {
				body.RoleID = "general"
			}
			// If the role declares required template variables, honour its
			// missing-var policy before the conversation is created (M4c).
			if len(body.Vars) > 0 || body.RoleID != "general" {
				if rl, err := h.Roles.Get(body.RoleID); err == nil && len(rl.VarsOf()) > 0 {
					if _, missing, err := rl.ApplyVars(body.Vars); err != nil {
						h.json(w, 400, map[string]interface{}{
							"error": err.Error(), "missing": missing, "vars": rl.VarsOf(),
						})
						return
					} else if len(missing) > 0 && rl.Policy() == "ask" {
						h.json(w, 400, map[string]interface{}{
							"error": "缺少角色必填变量", "missing": missing, "vars": rl.VarsOf(),
						})
						return
					}
				}
			}
			c, err := h.Convs.Create(body.RoleID, conversation.Mode(body.Mode))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(body.Vars) > 0 {
				if err := h.Convs.SetVars(c.ID, body.Vars); err != nil {
					// The conversation exists; a vars persistence failure must
					// not read as a failed create. Report it as a warning.
					h.json(w, 201, map[string]interface{}{
						"conversation": c, "varsWarning": "变量保存失败：" + err.Error(),
					})
					return
				}
				c.Vars = body.Vars
			}
			h.json(w, 201, c)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	id := pathID(p, "/api/v1/conversations/")
	switch {
	case strings.HasSuffix(p, "/messages"):
		switch r.Method {
		case http.MethodGet:
			msgs, err := h.Convs.Messages(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, msgs)
		case http.MethodPost:
			// canonical chat entry point: streaming SSE (DESIGN §15)
			h.chatSSE(w, r, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case strings.HasSuffix(p, "/export"):
		data, err := h.Convs.Export(id, r.URL.Query().Get("format"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, map[string]interface{}{"content": string(data)})
	default:
		switch r.Method {
		case http.MethodGet:
			c, err := h.Convs.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, c)
		case http.MethodPut, http.MethodPatch:
			var patch conversation.MetaPatch
			if err := h.decode(r, &patch); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Convs.UpdateMeta(id, patch); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			c, err := h.Convs.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, c)
		case http.MethodDelete:
			if err := h.Convs.Delete(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// ---- skills ----

func (h *Handler) skills(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/api/v1/skills" || p == "/api/v1/skills/":
		switch r.Method {
		case http.MethodGet:
			h.json(w, 200, h.Skills.List())
		case http.MethodPost:
			var sk skill.Skill
			if err := h.decode(r, &sk); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Skills.Create(&sk); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 201, sk)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	// NOTE: /dirs, /scan, /import and /export are matched before the
	// /{id} fallback, otherwise they are swallowed as an id.
	case p == "/api/v1/skills/dirs":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.json(w, 200, map[string]interface{}{"dirs": skill.Dirs(h.Cfg.DataDir)})
	case p == "/api/v1/skills/scan":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Dirs []string `json:"dirs"`
		}
		_ = h.decode(r, &body)
		dirs := body.Dirs
		if len(dirs) == 0 {
			dirs = skill.Dirs(h.Cfg.DataDir)
		}
		res, err := h.Skills.Scan(dirs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.json(w, 200, res)
	case p == "/api/v1/skills/import":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got, err := h.Skills.Import(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.json(w, 201, got)
	case p == "/api/v1/skills/export":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, err := h.Skills.ExportAll()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	default:
		id := pathID(p, "/api/v1/skills/")
		if strings.HasSuffix(p, "/toggle") {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			sk, err := h.Skills.Toggle(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, sk)
			return
		}
		if strings.HasSuffix(p, "/export") {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			data, err := h.Skills.Export(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		switch r.Method {
		case http.MethodGet:
			sk, err := h.Skills.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, sk)
		case http.MethodPut:
			var sk skill.Skill
			if err := h.decode(r, &sk); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Skills.Update(id, &sk); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 200, sk)
		case http.MethodDelete:
			if err := h.Skills.Delete(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// debug exposes read-only introspection endpoints used to diagnose prompt
// assembly. It never mutates state.
func (h *Handler) debug(w http.ResponseWriter, r *http.Request, p string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch p {
	case "/api/v1/debug/system-prompt", "/api/v1/debug/system-prompt/":
		var body struct {
			ConvID string `json:"convId"`
			Query  string `json:"query"`
		}
		_ = h.decode(r, &body)
		if body.Query == "" {
			body.Query = r.URL.Query().Get("query")
		}
		if body.ConvID == "" {
			body.ConvID = r.URL.Query().Get("convId")
		}
		if body.ConvID == "" {
			http.Error(w, "convId is required", http.StatusBadRequest)
			return
		}
		msgs, err := h.Orch.BuildMessages(body.ConvID, body.Query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		var system string
		history := 0
		for _, m := range msgs {
			if m.Role == "system" {
				system += m.Content + "\n"
				continue
			}
			history++
		}
		h.json(w, 200, map[string]interface{}{
			"system":       system,
			"historyCount": history,
			"messageCount": len(msgs),
		})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// modules exposes the module registry and its per-module switches (DESIGN §5).
func (h *Handler) modules(w http.ResponseWriter, r *http.Request, p string) {
	if h.Modules == nil {
		http.Error(w, "module registry not available", http.StatusNotFound)
		return
	}
	if p == "/api/v1/modules" || p == "/api/v1/modules/" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.json(w, 200, h.Modules.List())
		return
	}
	rest := strings.Trim(strings.TrimPrefix(p, "/api/v1/modules/"), "/")
	if strings.HasSuffix(rest, "/toggle") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimSuffix(rest, "/toggle")
		// A body is optional: an empty POST means "flip the current state",
		// which is what a UI switch and a curl smoke test both want.
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if _, err := h.decodeOptional(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		enabled := !h.Modules.IsEnabled(id)
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		if err := h.Modules.SetEnabled(id, enabled); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if h.Orch != nil && h.Orch.OnModulesChanged != nil {
			h.Orch.OnModulesChanged()
		}
		h.json(w, 200, map[string]interface{}{"id": id, "enabled": enabled})
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// ---- methodologies (M4) ----

func (h *Handler) methodologies(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/api/v1/methodologies" || p == "/api/v1/methodologies/":
		switch r.Method {
		case http.MethodGet:
			h.json(w, 200, h.Method.List())
		case http.MethodPost:
			var m methodology.Methodology
			if err := h.decode(r, &m); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Method.Create(&m); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.json(w, 201, m)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case p == "/api/v1/methodologies/import":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got, err := h.Method.Import(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.json(w, 201, got)
	case p == "/api/v1/methodologies/export":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, err := h.Method.ExportAll()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case p == "/api/v1/methodologies/match":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Query  string   `json:"query"`
			Pinned []string `json:"pinned"`
		}
		if err := h.decode(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		h.json(w, 200, h.Method.Match(body.Query, body.Pinned))
	default:
		id := pathID(p, "/api/v1/methodologies/")
		if strings.HasSuffix(p, "/toggle") {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			m, err := h.Method.Toggle(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, m)
			return
		}
		if strings.HasSuffix(p, "/retire") {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				By string `json:"by"`
			}
			_ = h.decode(r, &body)
			m, err := h.Method.Retire(id, body.By)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, m)
			return
		}
		switch r.Method {
		case http.MethodGet:
			m, err := h.Method.Get(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, m)
		case http.MethodPut:
			var m methodology.Methodology
			if err := h.decode(r, &m); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Method.Update(id, &m); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.json(w, 200, m)
		case http.MethodDelete:
			if err := h.Method.Delete(id); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h.json(w, 200, map[string]interface{}{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// ---- knowledge (M3a RAG) ----

func (h *Handler) knowledge(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/api/v1/knowledge/collections" || p == "/api/v1/knowledge/collections/":
		switch r.Method {
		case http.MethodGet:
			h.json(w, 200, h.Knowledge.List())
		case http.MethodPost:
			var c knowledge.Collection
			if err := h.decode(r, &c); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := h.Knowledge.Create(&c); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.json(w, 201, c)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case p == "/api/v1/knowledge/search":
		h.knowledgeSearch(w, r)
	case strings.HasPrefix(p, "/api/v1/knowledge/collections/"):
		rest := strings.Trim(strings.TrimPrefix(p, "/api/v1/knowledge/collections/"), "/")
		parts := strings.SplitN(rest, "/", 2)
		collID := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
		}
		switch sub {
		case "":
			switch r.Method {
			case http.MethodGet:
				c, err := h.Knowledge.Get(collID)
				if err != nil {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				h.json(w, 200, c)
			case http.MethodDelete:
				if err := h.Knowledge.Delete(collID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				h.json(w, 200, map[string]interface{}{"ok": true})
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "documents":
			switch r.Method {
			case http.MethodGet:
				h.json(w, 200, h.Knowledge.ListDocs(collID))
			case http.MethodPost:
				h.uploadDocument(w, r, collID)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	case strings.HasPrefix(p, "/api/v1/knowledge/documents/"):
		// DELETE /api/v1/knowledge/documents/{id} (design §15)
		docID := strings.TrimPrefix(p, "/api/v1/knowledge/documents/")
		docID = strings.Trim(docID, "/")
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := h.deleteDocument(docID); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, map[string]interface{}{"ok": true})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// uploadDocument accepts multipart or JSON and returns {docId, jobId}.
func (h *Handler) uploadDocument(w http.ResponseWriter, r *http.Request, collID string) {
	var (
		name string
		data []byte
	)
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
			return
		}
		var f multipart.File
		if fh, ok := r.MultipartForm.File["file"]; ok && len(fh) > 0 {
			name = fh[0].Filename
			f, _ = fh[0].Open()
			defer f.Close()
			data, _ = io.ReadAll(f)
		} else {
			http.Error(w, "missing 'file' part", http.StatusBadRequest)
			return
		}
	} else {
		var body struct {
			Name    string `json:"name"`
			Content string `json:"content"`
			Type    string `json:"type"`
		}
		if err := h.decode(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		name, data = body.Name, []byte(body.Content)
	}
	if name == "" {
		name = "paste.txt"
	}
	if len(data) == 0 {
		http.Error(w, "empty content", http.StatusBadRequest)
		return
	}

	kind := knowledge.KindFromName(name)
	doc, err := h.Knowledge.CreateDoc(collID, name, kind, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// stage to disk so large uploads are not buffered into the job payload
	path := ""
	if h.Ingest != nil {
		path, _ = h.Ingest.Stage(doc.ID, data)
	}
	jobID := util.NewID()
	payload, _ := json.Marshal(ingest.Payload{
		JobID: jobID, CollectionID: collID, DocID: doc.ID,
		Raw: data, RawPath: path,
	})
	if _, err := h.Jobs.Submit(job.Job{ID: jobID, Type: ingest.TypeIngestDocument, Payload: payload}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.json(w, 202, map[string]interface{}{"docId": doc.ID, "jobId": jobID, "status": "pending"})
}

func (h *Handler) deleteDocument(docID string) error {
	for _, c := range h.Knowledge.List() {
		if _, err := h.Knowledge.GetDoc(c.ID, docID); err == nil {
			return h.Knowledge.DeleteDoc(c.ID, docID)
		}
	}
	return fmt.Errorf("document %s not found", docID)
}

func (h *Handler) knowledgeSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	topK := queryInt(r.URL.Query(), "topK", 5)
	var ids []string
	if v := r.URL.Query().Get("collection"); v != "" {
		ids = append(ids, v)
	}
	if r.Method == http.MethodPost {
		var body struct {
			CollectionIDs []string `json:"collectionIds"`
			Query         string   `json:"query"`
			TopK          int      `json:"topK"`
		}
		if err := h.decode(r, &body); err == nil {
			ids = body.CollectionIDs
			q = body.Query
			if body.TopK > 0 {
				topK = body.TopK
			}
		}
	}
	if len(ids) == 0 {
		for _, c := range h.Knowledge.List() {
			ids = append(ids, c.ID)
		}
	}
	h.json(w, 200, h.Knowledge.SearchCollections(ids, q, topK))
}

// ---- memories ----

// memories dispatches the memory subsystem: L1 CRUD, the L2/L3 Markdown
// layers, drilldown, consolidation and the contradiction queue.
//
// Route order matters: multi-segment sub-resources (persona, scenes/{name},
// conflicts/{id}/resolve, {id}/drilldown) are matched before the bare
// /{id} collection routes, otherwise "persona" would parse as a memory id.
func (h *Handler) memories(w http.ResponseWriter, r *http.Request, p string) {
	rest := strings.Trim(strings.TrimPrefix(p, "/api/v1/memories"), "/")
	if rest == "" {
		h.memoriesCRUD(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	switch parts[0] {
	case "search":
		q := r.URL.Query().Get("q")
		if q == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		topK := queryInt(r.URL.Query(), "topK", 5)
		scope := r.URL.Query().Get("scope")
		if scope == "" {
			scope = memory.ScopeGlobal
		}
		out, err := h.Memory.Recall(q, topK, scope)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.json(w, 200, out)
	case "guidance":
		h.json(w, 200, map[string]string{"guidance": h.Memory.Guidance(queryInt(r.URL.Query(), "maxChars", 1500))})
	case "persona":
		// /persona/drilldown must be handled before /persona, which only
		// accepts GET (read) and PUT (hand edit).
		if len(parts) >= 2 && parts[1] == "drilldown" {
			h.drilldown(w, r, memory.PersonaID)
			return
		}
		h.memoriesPersona(w, r)
	case "scenes":
		h.memoriesScenes(w, r, parts)
	case "consolidate":
		h.memoriesConsolidate(w, r)
	case "extract":
		h.memoriesExtract(w, r)
	case "conflicts":
		h.memoriesConflicts(w, r, parts)
	case "offload":
		h.memoriesOffload(w, r)
	default:
		h.memoriesItem(w, r, parts)
	}
}

// memoriesCRUD handles GET/POST on /api/v1/memories (DESIGN §15.2).
func (h *Handler) memoriesCRUD(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		f := memory.ListFilter{
			Level:    r.URL.Query().Get("level"),
			Scope:    r.URL.Query().Get("scope"),
			Category: r.URL.Query().Get("category"),
			Scene:    r.URL.Query().Get("scene"),
			Include:  r.URL.Query().Get("include") == "true",
		}
		h.json(w, 200, h.Memory.List(f))
	case http.MethodPost:
		var m memory.Memory
		if err := h.decode(r, &m); err != nil || strings.TrimSpace(m.Content) == "" {
			http.Error(w, "bad body: content required", http.StatusBadRequest)
			return
		}
		if m.Level == "" {
			m.Level = memory.LevelL1
		}
		if m.Scope == "" {
			m.Scope = memory.ScopeGlobal
		}
		if m.Source == "" {
			m.Source = "manual"
		}
		saved, err := h.Memory.Add(&m)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.json(w, 201, saved)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// memoriesItem handles PUT/DELETE/GET on /api/v1/memories/{id} plus drilldown
// and invalidate.
func (h *Handler) memoriesItem(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) >= 2 && parts[1] == "drilldown" {
		h.drilldown(w, r, parts[0])
		return
	}
	// Soft retire: the record survives, recall stops. The UI's 失效 button
	// posts here, so a missing route meant the button silently did nothing
	// (the client throws on 404 and nothing in the page surfaced it).
	if len(parts) == 2 && parts[1] == "invalidate" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := h.Memory.Invalidate(parts[0]); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, map[string]string{"status": "invalidated"})
		return
	}
	if len(parts) != 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		m, err := h.Memory.Get(parts[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, m)
	case http.MethodPut:
		cur, err := h.Memory.Get(parts[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		var in memory.Memory
		if err := h.decode(r, &in); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		in.ID = cur.ID
		if in.Content == "" {
			in.Content = cur.Content
		}
		if in.Level == "" {
			in.Level = cur.Level
		}
		if in.Scope == "" {
			in.Scope = cur.Scope
		}
		in.CreatedAt = cur.CreatedAt
		if err := h.Memory.Update(&in); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.json(w, 200, &in)
	case http.MethodDelete:
		if err := h.Memory.Delete(parts[0]); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// drilldown resolves an id (persona / scene:{name} / memory id) down to L0.
func (h *Handler) drilldown(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, err := h.Memory.Drilldown(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	h.json(w, 200, d)
}

// memoriesPersona handles the L3 profile document (human-editable Markdown).
func (h *Handler) memoriesPersona(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.json(w, 200, map[string]string{"content": h.Memory.Persona()})
	case http.MethodPut:
		var body struct {
			Content string `json:"content"`
		}
		if err := h.decode(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if err := h.Memory.SetPersona(body.Content); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.json(w, 200, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// memoriesScenes exposes the L2 layer: list all scenes or read/delete one.
func (h *Handler) memoriesScenes(w http.ResponseWriter, r *http.Request, parts []string) {
	name := ""
	if len(parts) >= 2 {
		name = parts[1]
	}
	if name == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.json(w, 200, h.Memory.Scenes())
		return
	}
	switch r.Method {
	case http.MethodGet:
		body, members, err := h.Memory.Scene(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, map[string]interface{}{"name": name, "content": body, "memberIds": members})
	case http.MethodDelete:
		if err := h.Memory.DeleteScene(name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.json(w, 200, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// memoriesConsolidate queues the L1→L2→L3 pipeline and returns a jobId.
func (h *Handler) memoriesConsolidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ProviderId string `json:"providerId"`
		Model      string `json:"model"`
		UpdateL3   *bool  `json:"updateL3"`
	}
	_ = h.decode(r, &body)
	pid, model, err := h.resolveChatTarget(body.ProviderId, body.Model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updateL3 := true
	if body.UpdateL3 != nil {
		updateL3 = *body.UpdateL3
	}
	jobID := util.NewID()
	payload, _ := json.Marshal(consolidate.Payload{
		JobID: jobID, ProviderID: pid, Model: model, UpdateL3: updateL3,
	})
	if _, err := h.Jobs.Submit(job.Job{ID: jobID, Type: consolidate.TypeConsolidate, Payload: payload}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.json(w, 202, map[string]interface{}{"jobId": jobID, "type": consolidate.TypeConsolidate})
}

// memoriesExtract runs L1 distillation on demand (bypasses only the cadence
// gate; the input/output limits still apply).
func (h *Handler) memoriesExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Extractor == nil {
		http.Error(w, "extraction not configured", http.StatusNotImplemented)
		return
	}
	var body struct {
		ConversationId string `json:"conversationId"`
		ProviderId     string `json:"providerId"`
		Model          string `json:"model"`
	}
	if err := h.decode(r, &body); err != nil || body.ConversationId == "" {
		http.Error(w, "bad body: conversationId required", http.StatusBadRequest)
		return
	}
	pid, model, err := h.resolveChatTarget(body.ProviderId, body.Model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msgs, err := h.Convs.Messages(body.ConversationId)
	if err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	inputs := make([]memory.ExtractInput, 0, len(msgs))
	for _, m := range msgs {
		inputs = append(inputs, memory.ExtractInput{ID: m.ID, Role: m.Role, Content: m.Content})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	res, err := h.Extractor.Extract(ctx, body.ConversationId, pid, model, inputs, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	h.json(w, 200, res)
}

// memoriesConflicts is the pending-contradiction queue. Consolidation only
// registers conflicts; closing one always requires an explicit decision here.
func (h *Handler) memoriesConflicts(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) >= 3 && parts[2] == "resolve" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Resolution  string `json:"resolution"`
			ResolvedBy  string `json:"resolvedBy"`
			SupersedeId string `json:"supersedeId"`
		}
		if err := h.decode(r, &body); err != nil || strings.TrimSpace(body.Resolution) == "" {
			http.Error(w, "bad body: resolution required", http.StatusBadRequest)
			return
		}
		if body.ResolvedBy == "" {
			body.ResolvedBy = "user"
		}
		if err := h.Memory.ResolveConflict(parts[1], body.Resolution, body.ResolvedBy, body.SupersedeId); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, map[string]string{"status": "resolved"})
		return
	}
	if len(parts) != 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	openOnly := r.URL.Query().Get("status") != "all"
	h.json(w, 200, h.Memory.Conflicts(openOnly))
}

// memoriesOffload exposes evicted short-term blocks (summary + node_id, or the
// original text when a node_id is given).
func (h *Handler) memoriesOffload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	convID := r.URL.Query().Get("conversationId")
	if convID == "" {
		http.Error(w, "conversationId required", http.StatusBadRequest)
		return
	}
	if node := r.URL.Query().Get("nodeId"); node != "" {
		text, err := h.Memory.OffloadGet(convID, node)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, map[string]interface{}{"nodeId": node, "content": text})
		return
	}
	h.json(w, 200, h.Memory.OffloadBlocks(convID))
}

// resolveChatTarget picks the provider/model for an internal LLM call: explicit
// request values win, otherwise the configured default provider is used.
func (h *Handler) resolveChatTarget(providerID, model string) (string, string, error) {
	if providerID != "" && model != "" {
		return providerID, model, nil
	}
	if providerID == "" {
		providerID = h.Cfg.GetSettings().DefaultProviderId
	}
	if providerID == "" {
		for _, p := range h.Providers.List() {
			if p.Enabled && len(p.Keys) > 0 {
				providerID = p.ID
				break
			}
		}
	}
	if providerID == "" {
		return "", "", fmt.Errorf("no provider configured")
	}
	p, err := h.Providers.Get(providerID)
	if err != nil {
		return "", "", err
	}
	if model == "" {
		for _, m := range p.Models {
			if m.Capabilities.Chat {
				model = m.ID
				break
			}
		}
	}
	if model == "" {
		return "", "", fmt.Errorf("no chat model on provider %s", providerID)
	}
	return providerID, model, nil
}

// ---- jobs ----

func (h *Handler) jobs(w http.ResponseWriter, r *http.Request, p string) {
	// Bare /api/v1/jobs lists every job: the UI polls for upload/consolidate
	// progress without knowing the id in advance.
	if p == "/api/v1/jobs" || p == "/api/v1/jobs/" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.json(w, 200, h.Jobs.List())
		return
	}
	id := pathID(p, "/api/v1/jobs/")
	if strings.HasSuffix(p, "/cancel") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := h.Jobs.Cancel(id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, map[string]interface{}{"ok": true})
		return
	}
	switch r.Method {
	case http.MethodGet:
		st, err := h.Jobs.Status(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h.json(w, 200, st)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- generations (image / video from a provider's image-capable models) ----

func (h *Handler) generations(w http.ResponseWriter, r *http.Request, p string) {
	if p == "/api/v1/generations/image" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ProviderID string `json:"providerId"`
			Model      string `json:"model"`
			Prompt     string `json:"prompt"`
			provider.ImageOpts
		}
		if err := h.decode(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if body.ProviderID == "" || body.Model == "" || body.Prompt == "" {
			http.Error(w, "providerId/model/prompt required", http.StatusBadRequest)
			return
		}
		rt, err := h.Providers.GetRuntime(body.ProviderID)
		if err != nil {
			h.json(w, 200, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		gen, ok := rt.(provider.ImageGenerator)
		if !ok {
			h.json(w, 200, map[string]interface{}{"ok": false, "error": "该服务商不支持图像生成"})
			return
		}
		res, err := gen.GenerateImage(r.Context(), body.Model, body.Prompt, body.ImageOpts)
		if err != nil {
			h.json(w, 200, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		// Auto-save generated pictures to the local data dir so the user's work
		// is never lost even if the upstream URL expires (TOS links do).
		if res != nil && (res.URL != "" || len(res.Images) > 0) {
			h.saveImagesLocal(res)
		}
		h.json(w, 200, map[string]interface{}{"ok": true, "result": res})
		return
	}
	if p == "/api/v1/generations/video" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ProviderID string `json:"providerId"`
			Model      string `json:"model"`
			Prompt     string `json:"prompt"`
		}
		if err := h.decode(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if body.ProviderID == "" || body.Model == "" || body.Prompt == "" {
			http.Error(w, "providerId/model/prompt required", http.StatusBadRequest)
			return
		}
		rt, err := h.Providers.GetRuntime(body.ProviderID)
		if err != nil {
			h.json(w, 200, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		gen, ok := rt.(provider.VideoGenerator)
		if !ok {
			h.json(w, 200, map[string]interface{}{"ok": false, "error": "该服务商不支持视频生成"})
			return
		}
		res, err := gen.GenerateVideo(r.Context(), body.Model, body.Prompt)
		if err != nil {
			h.json(w, 200, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		h.json(w, 200, map[string]interface{}{"ok": true, "result": res})
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// ---- settings ----

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.json(w, 200, h.Cfg.GetSettings())
	case http.MethodPut:
		var patch config.SettingsPatch
		if err := h.decode(r, &patch); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		// Overlay onto the current settings, not onto the defaults: a partial
		// PUT must leave every field it did not mention exactly as it was.
		// Decoding straight into Settings could not do that — the missing
		// booleans arrived as false and switched failover, auto-routing and
		// memory extraction off without a word.
		if err := h.Cfg.UpdateSettings(func(cur *config.Settings) {
			*cur = patch.Apply(*cur)
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.json(w, 200, h.Cfg.GetSettings())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// saveImagesLocal downloads every generated image into the data dir so the
// result survives the upstream TOS link's 24h expiry. Failures are silent:
// the picture still returned to the UI via the live URL, we just could not
// keep a copy. Only the first image is used for LocalPath to keep it simple.
func (h *Handler) saveImagesLocal(res *provider.ImageResult) {
	dir := filepath.Join(h.Cfg.DataDir, "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	dl := func(src string) (string, error) {
		req, err := http.NewRequest(http.MethodGet, src, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "AQBot-compat/1.0")
		cli := &http.Client{Timeout: 60 * time.Second}
		resp, err := cli.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("download %d", resp.StatusCode)
		}
		ext := ".png"
		if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "jpeg") || strings.Contains(ct, "jpg") {
			ext = ".jpg"
		}
		name := fmt.Sprintf("%d_%s%s", time.Now().UnixNano(), randHex(6), ext)
		full := filepath.Join(dir, name)
		f, err := os.Create(full)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			f.Close()
			os.Remove(full)
			return "", err
		}
		f.Close()
		return full, nil
	}
	candidates := res.Images
	if len(candidates) == 0 && res.URL != "" {
		candidates = []string{res.URL}
	}
	saved := make([]string, 0, len(candidates))
	for _, u := range candidates {
		if p, err := dl(u); err == nil {
			saved = append(saved, p)
		}
	}
	if len(saved) > 0 {
		res.LocalPath = saved[0]
		res.Images = saved
	}
}

// randHex returns n lowercase hex chars for a unique-ish local file suffix.
func randHex(n int) string {
	const hex = "0123456789abcdef"
	var b []byte
	for i := 0; i < n; i++ {
		b = append(b, hex[time.Now().UnixNano()%16])
	}
	return string(b)
}

// ---- chat SSE (native events, DESIGN §15.1) ----

func (h *Handler) chatSSE(w http.ResponseWriter, r *http.Request, convID string) {
	if _, err := h.Convs.Get(convID); err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	var body struct {
		Message   string `json:"message"`
		Content   string `json:"content"`
		RoleID    string `json:"roleId"`
		Model     string `json:"model"`
		Mode      string `json:"mode"`
		Reasoning string `json:"reasoning"`
	}
	_ = h.decode(r, &body)
	msg := body.Message
	if msg == "" {
		msg = body.Content
	}
	if msg == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	in := orchestrator.ChatInput{
		ConversationID: convID,
		RoleID:         body.RoleID,
		Message:        msg,
		Model:          body.Model,
		Mode:           body.Mode,
		Reasoning:      body.Reasoning,
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, err := h.Orch.Chat(r.Context(), in)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]interface{}{"message": err.Error()}))
		flusher.Flush()
		return
	}
	for ev := range ch {
		payload := map[string]interface{}{"type": ev.Type, "data": ev.Data, "requestId": ev.RequestID}
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(payload))
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		default:
		}
	}
}

// ---- OpenAI-compatible chat ----

func (h *Handler) openAIChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model       string                   `json:"model"`
		Messages    []map[string]interface{} `json:"messages"`
		Stream      bool                     `json:"stream"`
		Temperature *float64                 `json:"temperature"`
		MaxTokens   *int                     `json:"max_tokens"`
		RoleID      string                   `json:"role"`
	}
	if err := h.decode(r, &req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	content := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i]["role"] == "user" {
			if c, ok := req.Messages[i]["content"].(string); ok {
				content = c
				break
			}
		}
	}
	if content == "" {
		http.Error(w, "no user message", http.StatusBadRequest)
		return
	}
	model := req.Model
	if def := h.Cfg.GetSettings().DefaultProviderId; model != "" && !strings.Contains(model, "/") && def != "" {
		model = def + "/" + model
	}
	roleID := req.RoleID
	if roleID == "" {
		roleID = h.Cfg.GetSettings().DefaultRoleId
	}
	if roleID == "" {
		roleID = "general"
	}
	in := orchestrator.ChatInput{Message: content, Model: model, RoleID: roleID}
	// Request-level sampling overrides. Pointers distinguish "absent" from an
	// explicit 0, which is a valid temperature.
	if req.Temperature != nil {
		t := *req.Temperature
		in.Temperature = &t
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		m := *req.MaxTokens
		in.MaxTokens = &m
	}

	if !req.Stream {
		h.openAIOnce(w, r, in)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	cmplID := "chatcmpl-" + util.NewID()
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
		"id": cmplID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{"role": "assistant"}, "finish_reason": nil}},
	}))
	flusher.Flush()

	ch, err := h.Orch.Chat(r.Context(), in)
	if err != nil {
		h.writeOAError(w, flusher, cmplID, err.Error())
		return
	}
	for ev := range ch {
		if ev.Type == "delta" {
			if d, ok := ev.Data["delta"].(string); ok && d != "" {
				fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
					"id": cmplID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
					"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{"content": d}, "finish_reason": nil}},
				}))
				flusher.Flush()
			}
		} else if ev.Type == "error" {
			msg, _ := ev.Data["message"].(string)
			h.writeOAError(w, flusher, cmplID, msg)
			return
		}
		select {
		case <-r.Context().Done():
			return
		default:
		}
	}
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
		"id": cmplID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"}},
	}))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (h *Handler) writeOAError(w http.ResponseWriter, flusher http.Flusher, cmplID, msg string) {
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
		"id": cmplID, "object": "chat.completion.chunk",
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{"content": "错误：" + msg}, "finish_reason": "stop"}},
	}))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// openAIOnce serves a non-streaming OpenAI-compatible completion.
//
// It must stay attached to the request context: a detached context would keep
// the provider call (and its goroutine) running after the client gave up. Errors
// are surfaced as an HTTP error instead of an empty completion, otherwise a
// caller cannot tell a failed turn from a model that said nothing.
func (h *Handler) openAIOnce(w http.ResponseWriter, r *http.Request, in orchestrator.ChatInput) {
	ch, err := h.Orch.Chat(r.Context(), in)
	if err != nil {
		h.writeOAJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	var content string
	var usage provider.Usage
	finish := "stop"
	var turnErr string
	for ev := range ch {
		switch ev.Type {
		case "delta":
			if d, ok := ev.Data["delta"].(string); ok {
				content += d
			}
		case "done":
			// usage travels as a JSON-shaped map (it went through mustJSON), so a
			// direct type assertion on provider.Usage would never match.
			if u, ok := ev.Data["usage"].(map[string]interface{}); ok {
				usage.PromptTokens = intOf(u["promptTokens"])
				usage.CompletionTokens = intOf(u["completionTokens"])
				usage.TotalTokens = intOf(u["totalTokens"])
				usage.Cost = numOf(u["cost"])
			}
			if fr, ok := ev.Data["finishReason"].(string); ok && fr != "" {
				finish = fr
			}
		case "error":
			if m, ok := ev.Data["message"].(string); ok && turnErr == "" {
				turnErr = m
			}
		}
	}
	if turnErr != "" {
		h.writeOAJSONError(w, http.StatusBadGateway, turnErr)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	cmplID := "chatcmpl-" + util.NewID()
	h.json(w, 200, map[string]interface{}{
		"id": cmplID, "object": "chat.completion", "created": time.Now().Unix(),
		"choices": []map[string]interface{}{{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": content}, "finish_reason": finish}},
		"usage": map[string]interface{}{
			"prompt_tokens": usage.PromptTokens, "completion_tokens": usage.CompletionTokens,
			"total_tokens": usage.TotalTokens,
		},
	})
}

func intOf(v interface{}) int {
	f := numOf(v)
	if f <= 0 {
		return 0
	}
	return int(f)
}

// numOf coerces the numeric shapes a JSON round trip can produce.
func numOf(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

// writeOAJSONError emits an OpenAI-shaped error body with a real status code.
func (h *Handler) writeOAJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{"message": msg, "type": "agenthub_error"},
	})
}

// ---- models (OpenAI-compatible) ----

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	providers := h.Providers.List()
	data := make([]map[string]interface{}, 0, len(providers))
	for _, p := range providers {
		// Curation-aware: this endpoint feeds third-party clients' model lists,
		// and a 563-entry catalogue is not a model list anyone can use. A
		// provider that was never tested has no cached catalogue, so UsableModels
		// asks the vendor once instead of reporting nothing.
		models, err := h.Providers.UsableModels(p.ID)
		if err != nil {
			models = nil
		}
		for _, m := range models {
			data = append(data, map[string]interface{}{
				"id":       p.ID + "/" + m.ID,
				"object":   "model",
				"created":  p.CreatedAt / 1000,
				"owned_by": p.ID,
			})
		}
	}
	h.json(w, 200, map[string]interface{}{"object": "list", "data": data})
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
