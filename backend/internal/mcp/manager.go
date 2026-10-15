// Package mcp — Manager: server lifecycle + tool proxying (DESIGN §5 mcp:
// Manager{ Add/Remove/Start/Stop/Test }).
//
// Persistence shape follows DESIGN §7 mcp_servers/{id}.json. Secret-looking env
// values are stored in the key vault and the document keeps only a reference,
// so a stolen data directory never hands over an API token in plaintext
// (DESIGN §10 MCP env 加密).
package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/tool"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
	"github.com/agenthub/agent-proxy/internal/tool/schema"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Connection status values.
const (
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusError    = "error"
)

// Server is the persisted MCP server record (DESIGN §7 mcp_servers/{id}.json).
type Server struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Env holds plain (non-secret) KEY=VALUE entries.
	Env map[string]string `json:"env,omitempty"`
	// SecretEnv holds the *masked* view of encrypted entries, for the UI. The
	// plaintext lives only in the vault under vaultID.
	SecretEnv map[string]string `json:"secretEnv,omitempty"`
	// EnvRefs maps an env key to its vault entry id.
	EnvRefs   map[string]string `json:"envRefs,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Enabled   bool              `json:"enabled"`
	Status    string            `json:"status"`
	LastError string            `json:"lastError,omitempty"`
	// ToolNames lists the tools proxied from this server (populated at start).
	ToolNames []string `json:"toolNames,omitempty"`
	// Protocol / ServerVersion come from the initialize handshake.
	Protocol      string `json:"protocol,omitempty"`
	ServerVersion string `json:"serverVersion,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// Info is the runtime view of a server (server + live client state).
type Info struct {
	Server
	Running bool `json:"running"`
	// ToolCount is len(ToolNames).
	ToolCount int `json:"toolCount"`
}

// secretEnvKeys are env names whose value is treated as a secret by default.
// The pattern covers the overwhelming majority of real MCP servers (tokens,
// API keys) without the user having to declare each one.
var secretEnvKeys = []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH", "SESSION"}

// IsSecretEnvKey reports whether an env variable name looks secret-bearing.
func IsSecretEnvKey(k string) bool {
	up := strings.ToUpper(k)
	for _, s := range secretEnvKeys {
		if strings.Contains(up, s) {
			return true
		}
	}
	return false
}

func vaultID(serverID, key string) string {
	return "mcp_" + serverID + "_" + strings.ToLower(strings.ReplaceAll(key, "-", "_"))
}

// Manager owns every MCP server: persistence, client lifecycles and the
// registration of proxied tools into the shared tool registry.
type Manager struct {
	store storage.Store
	vault *keyvault.Vault
	reg   *registry.Registry
	aud   *audit.Audit

	// lifetime is the parent context of every spawned server process. It is
	// deliberately NOT any request context: an MCP server must survive the HTTP
	// call that started it. Shutdown releases it via StopAll.
	lifetime    context.Context
	lifetimeEnd context.CancelFunc

	mu      sync.RWMutex
	servers map[string]*Server
	clients map[string]*Client
	// tools maps "serverID/toolName" → the prefixed registry tool name, so a
	// call can be routed back to the right server even when two servers expose
	// the same tool name.
	tools map[string]string
}

// NewManager constructs the manager and restores persisted servers. Servers are
// not auto-started here: starting arbitrary commands on boot without being asked
// would be a surprise, so the operator starts them (or flips Enabled, which the
// caller may act on explicitly).
func NewManager(store storage.Store, vault *keyvault.Vault, reg *registry.Registry, aud *audit.Audit) *Manager {
	lifetime, lifetimeEnd := context.WithCancel(context.Background())
	m := &Manager{
		store: store, vault: vault, reg: reg, aud: aud,
		lifetime: lifetime, lifetimeEnd: lifetimeEnd,
		servers: map[string]*Server{},
		clients: map[string]*Client{},
		tools:   map[string]string{},
	}
	m.load()
	return m
}

func (m *Manager) load() {
	ids, err := m.store.IDs()
	if err != nil {
		return
	}
	for _, id := range ids {
		var s Server
		if err := m.store.Load(id, &s); err != nil {
			continue
		}
		if s.ID == "" {
			s.ID = id
		}
		if s.Status == "" {
			s.Status = StatusStopped
		}
		m.servers[id] = &s
	}
}

// ---- CRUD ----

// List returns all servers with live runtime state.
func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Info, 0, len(m.servers))
	for _, s := range m.servers {
		cp := *s
		cp.EnvRefs = copyMap(s.EnvRefs)
		cp.Env = copyMap(s.Env)
		cp.SecretEnv = copyMap(s.SecretEnv)
		cp.Args = append([]string(nil), s.Args...)
		cp.ToolNames = append([]string(nil), s.ToolNames...)
		out = append(out, Info{Server: cp, Running: m.clients[s.ID] != nil, ToolCount: len(s.ToolNames)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns one server.
func (m *Manager) Get(id string) (*Server, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.servers[id]
	if !ok {
		return nil, fmt.Errorf("mcp server %s not found", id)
	}
	cp := *s
	cp.Env = copyMap(s.Env)
	cp.EnvRefs = copyMap(s.EnvRefs)
	cp.SecretEnv = copyMap(s.SecretEnv)
	cp.Args = append([]string(nil), s.Args...)
	cp.ToolNames = append([]string(nil), s.ToolNames...)
	return &cp, nil
}

// Create persists a new server (stopped). Secret-looking env values are moved
// into the vault immediately, so a plaintext token never reaches the JSON file
// even for one second of its life on disk.
func (m *Manager) Create(ctx context.Context, s *Server) error {
	if err := normalise(s); err != nil {
		return err
	}
	m.mu.Lock()
	if s.ID == "" {
		s.ID = util.NewID()
	}
	s.Status = StatusStopped
	now := util.NowUnix()
	s.CreatedAt, s.UpdatedAt = now, now
	m.mu.Unlock()

	if err := m.splitSecrets(s); err != nil {
		return err
	}
	if err := m.store.Save(s.ID, s); err != nil {
		return err
	}
	m.mu.Lock()
	cp := *s
	m.servers[s.ID] = &cp
	m.mu.Unlock()
	m.log("mcp_server_created", s.ID, map[string]string{"name": s.Name, "command": s.Command})
	return nil
}

// Update replaces a server definition. Changing the command, args or env
// invalidates the running client: the old process speaks for the old config, so
// it is stopped and its tools unregistered before the new record is saved.
func (m *Manager) Update(ctx context.Context, id string, s *Server) error {
	if err := normalise(s); err != nil {
		return err
	}
	m.mu.Lock()
	old, ok := m.servers[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mcp server %s not found", id)
	}
	transportChanged := s.Command != old.Command || !equalStrings(s.Args, old.Args) || s.Dir != old.Dir
	s.ID = id
	s.CreatedAt = old.CreatedAt
	s.UpdatedAt = util.NowUnix()
	cur := *old // snapshot of the live state, read while holding the lock
	m.mu.Unlock()

	// Stop first: the old client holds the old command/env.
	if transportChanged {
		_ = m.Stop(ctx, id)
	}
	if err := m.splitSecrets(s); err != nil {
		return err
	}
	if err := m.store.Save(id, s); err != nil {
		return err
	}
	m.mu.Lock()
	if transportChanged {
		s.Status = StatusStopped
		s.ToolNames = nil
		s.Protocol, s.ServerVersion = "", ""
		s.LastError = ""
	} else {
		// Runtime state is owned by the manager, never by the request body. A
		// client that GETs a server, edits the name and PUTs it back sends the
		// status it just read — but a client that builds a fresh object sends an
		// empty one. Trusting either would silently drop a live connection.
		s.Status = cur.Status
		s.ToolNames = append([]string(nil), cur.ToolNames...)
		s.Protocol, s.ServerVersion = cur.Protocol, cur.ServerVersion
		s.LastError = cur.LastError
	}
	cp := *s
	m.servers[id] = &cp
	m.mu.Unlock()
	m.log("mcp_server_updated", id, map[string]string{"name": s.Name})
	return nil
}

// Delete stops the server, unregisters its tools and removes the record plus
// every vault entry it owned (a deleted server must not leave usable tokens
// behind on disk).
func (m *Manager) Delete(ctx context.Context, id string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	// Collect the vault ids before the record disappears, otherwise the tokens
	// outlive the server that used them.
	var vids []string
	for _, vid := range s.EnvRefs {
		vids = append(vids, vid)
	}
	_ = m.Stop(ctx, id)
	m.mu.Lock()
	delete(m.servers, id)
	for k, v := range m.tools {
		if strings.HasPrefix(v, id+"/") {
			delete(m.tools, k)
		}
	}
	m.mu.Unlock()
	_ = m.store.Remove(id)
	if m.vault != nil {
		for _, vid := range vids {
			_ = m.vault.Delete(vid)
		}
	}
	m.log("mcp_server_deleted", id, map[string]string{"secretsRemoved": fmt.Sprint(len(vids))})
	return nil
}

// ---- lifecycle ----

// Start spawns the server, completes the handshake, pulls its tool list and
// registers each tool as a callable proxy. On failure the server is recorded in
// error state with the reason, and nothing is registered — a half-connected MCP
// server must never look usable.
func (m *Manager) Start(ctx context.Context, id string) (*Info, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if _, live := m.clients[id]; live {
		m.mu.Unlock()
		return m.Info(id), nil // already running: idempotent
	}
	s.Status = StatusStarting
	m.servers[id] = s
	m.mu.Unlock()

	env, err := m.resolveEnv(s)
	if err != nil {
		m.fail(id, "解析 env 失败: "+err.Error())
		return nil, err
	}
	// The child must outlive this request. Start derives the process lifetime
	// from the passed ctx, and an HTTP handler's ctx is cancelled the instant
	// the response is written — which would kill every MCP server a few
	// milliseconds after it started. The manager owns the lifetime; Stop/StopAll
	// release it.
	cl, err := Start(m.lifetime, Config{Command: s.Command, Args: s.Args, Env: env, Dir: s.Dir})
	if err != nil {
		m.fail(id, err.Error())
		return nil, err
	}
	tools, err := cl.ListTools(ctx)
	if err != nil {
		_ = cl.Close()
		m.fail(id, "拉取工具列表失败: "+err.Error())
		return nil, err
	}

	proto, ver := cl.ServerInfo()
	m.mu.Lock()
	cur := m.servers[id]
	cur.Status = StatusRunning
	cur.LastError = ""
	cur.Protocol = proto
	cur.ServerVersion = ver
	cur.ToolNames = nil
	for _, t := range tools {
		cur.ToolNames = append(cur.ToolNames, t.Name)
		m.tools[id+"/"+t.Name] = proxiedName(id, t.Name)
	}
	cur.UpdatedAt = util.NowUnix()
	_ = m.store.Save(id, cur)
	m.clients[id] = cl
	m.mu.Unlock()

	// Register the proxies. Names are prefixed with the server id so two servers
	// exposing "search" do not collide, and so the model can be told where a
	// tool came from.
	for _, t := range tools {
		m.reg.RegisterDynamic(registry.SourceMCP, id, newProxyTool(id, t, m))
	}
	m.log("mcp_server_started", id, map[string]string{"tools": fmt.Sprint(len(tools))})
	return m.Info(id), nil
}

// Stop terminates the server and unregisters its tools.
func (m *Manager) Stop(ctx context.Context, id string) error {
	m.mu.Lock()
	cl, live := m.clients[id]
	delete(m.clients, id)
	for k, v := range m.tools {
		if strings.HasPrefix(v, id+"/") {
			delete(m.tools, k)
		}
	}
	s, ok := m.servers[id]
	if ok {
		s.Status = StatusStopped
		s.ToolNames = nil
		s.UpdatedAt = util.NowUnix()
		_ = m.store.Save(id, s)
	}
	m.mu.Unlock()
	m.reg.UnregisterOwner(id)
	if live {
		return cl.Close()
	}
	return nil
}

// Test performs a one-shot connection check without registering any tools: it
// starts, lists, reports the tool names, then stops. This is what the UI's
// "测试连接" button calls before the operator commits to keeping a server.
func (m *Manager) Test(ctx context.Context, s *Server) (map[string]interface{}, error) {
	probe := *s
	if probe.ID == "" {
		probe.ID = "test-" + util.NewID()[:8]
	}
	env := map[string]string{}
	for k, v := range s.Env {
		env[k] = v
	}
	for k := range s.SecretEnv {
		if m.vault == nil {
			continue
		}
		if v, err := m.vault.Get(vaultID(probe.ID, k)); err == nil {
			env[k] = v
		}
	}
	// Test binds to the request lifetime on purpose: it must clean up after
	// itself when the request ends, whatever happens.
	cl, err := Start(ctx, Config{Command: probe.Command, Args: probe.Args, Env: env, Dir: probe.Dir})
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}, nil
	}
	defer cl.Close()
	tools, err := cl.ListTools(ctx)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error(), "stderr": cl.Stderr()}, nil
	}
	proto, ver := cl.ServerInfo()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return map[string]interface{}{
		"ok": true, "protocol": proto, "serverVersion": ver,
		"toolCount": len(tools), "tools": names,
	}, nil
}

// Info returns the runtime view of one server.
func (m *Manager) Info(id string) *Info {
	s, err := m.Get(id)
	if err != nil {
		return nil
	}
	return &Info{Server: *s, Running: m.clients[id] != nil, ToolCount: len(s.ToolNames)}
}

// CallTool routes a proxied tool call back to its server. The orchestrator goes
// through the registry proxy; this direct entry exists for the UI's "try it"
// button and for tests.
func (m *Manager) CallTool(ctx context.Context, serverID, toolName string, args map[string]interface{}) (string, error) {
	m.mu.RLock()
	cl, ok := m.clients[serverID]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("mcp server %s 未运行", serverID)
	}
	return cl.CallTool(ctx, toolName, args)
}

// StopAll terminates every running server and releases the manager's lifetime
// context (called on process exit).
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.clients))
	for id := range m.clients {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(context.Background(), id)
	}
	if m.lifetimeEnd != nil {
		m.lifetimeEnd()
	}
}

// ---- helpers ----

func (m *Manager) fail(id, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.servers[id]; ok {
		s.Status = StatusError
		s.LastError = reason
		s.UpdatedAt = util.NowUnix()
		_ = m.store.Save(id, s)
	}
}

func (m *Manager) log(event, id string, fields map[string]string) {
	if m.aud == nil {
		return
	}
	if fields == nil {
		fields = map[string]string{}
	}
	fields["serverId"] = id
	m.aud.Log(event, "", fields)
}

// splitSecrets moves secret-looking env values into the vault and leaves only a
// mask in the persisted document.
func (m *Manager) splitSecrets(s *Server) error {
	if s.Env == nil {
		s.Env = map[string]string{}
	}
	if s.SecretEnv == nil {
		s.SecretEnv = map[string]string{}
	}
	if s.EnvRefs == nil {
		s.EnvRefs = map[string]string{}
	}
	// A key previously stored as a secret but now typed in plain (or vice versa)
	// must be reconciled, otherwise a stale ref would resurrect an old token.
	for k := range s.SecretEnv {
		if plain, ok := s.Env[k]; ok {
			if m.vault != nil {
				vid := vaultID(s.ID, k)
				if err := m.vault.Set(vid, plain); err != nil {
					return err
				}
				s.EnvRefs[k] = vid
				s.SecretEnv[k] = util.MaskSecret(plain)
			} else {
				s.SecretEnv[k] = plain // no vault: keep as-is (single-user local)
			}
			delete(s.Env, k)
		}
	}
	// Plain env entries that look secret get vaulted on the way in.
	for k, v := range s.Env {
		if !IsSecretEnvKey(k) {
			continue
		}
		if m.vault != nil {
			vid := vaultID(s.ID, k)
			if err := m.vault.Set(vid, v); err != nil {
				return err
			}
			s.EnvRefs[k] = vid
			s.SecretEnv[k] = util.MaskSecret(v)
		} else {
			s.SecretEnv[k] = v
		}
		delete(s.Env, k)
	}
	return nil
}

// resolveEnv builds the plaintext child environment, decrypting vault refs.
func (m *Manager) resolveEnv(s *Server) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range s.Env {
		out[k] = v
	}
	for k, vid := range s.EnvRefs {
		if m.vault == nil {
			continue
		}
		v, err := m.vault.Get(vid)
		if err != nil {
			return nil, fmt.Errorf("读取密钥 %s 失败: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// proxiedName builds the registry-visible name for an MCP tool. The double
// underscore is chosen because tool names must match [A-Za-z_][A-Za-z0-9_-]*
// and "." is not allowed, while remaining readable in a transcript.
func proxiedName(serverID, tool string) string {
	safe := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteRune('_')
			}
		}
		return b.String()
	}
	name := "mcp__" + safe(serverID) + "__" + safe(tool)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// proxyTool forwards a registry tool call to the owning server. It resolves the
// live client through the Manager on every call rather than capturing a client
// pointer: a restart swaps the client, and a captured pointer would keep routing
// calls into a closed pipe forever.
type proxyTool struct {
	name string
	desc string
	sch  map[string]interface{}
	srv  string
	tool string
	mgr  *Manager
}

func newProxyTool(serverID string, t Tool, mgr *Manager) tool.Tool {
	sch := t.InputSchema
	if sch == nil {
		sch = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	// Every entry gets a description: an empty one is not fatal, but a tool the
	// model cannot tell apart from its neighbours is dead weight in every request.
	desc := t.Description
	if strings.TrimSpace(desc) == "" {
		desc = "MCP 服务器 " + serverID + " 提供的工具 " + t.Name
	}
	return &proxyTool{
		name: proxiedName(serverID, t.Name),
		desc: desc,
		sch:  sch,
		srv:  serverID,
		tool: t.Name,
		mgr:  mgr,
	}
}

func (p *proxyTool) Name() string        { return p.name }
func (p *proxyTool) Description() string { return p.desc }
func (p *proxyTool) Schema() map[string]interface{} {
	return p.sch
}

func (p *proxyTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	out, err := p.mgr.CallTool(ctx, p.srv, p.tool, args)
	if err != nil {
		return "", fmt.Errorf("[mcp:%s] %w", p.srv, err)
	}
	return schema.SanitizeResult(out), nil
}

func normalise(s *Server) error {
	s.Name = strings.TrimSpace(s.Name)
	s.Command = strings.TrimSpace(s.Command)
	if s.Name == "" {
		s.Name = s.Command
	}
	if s.Command == "" {
		return fmt.Errorf("command 不能为空")
	}
	// A single-element Args containing spaces is almost always a UI slip where
	// the whole command line landed in one box ("npx -y foo"). Split it on
	// whitespace so the server is not launched with one bogus argument. A real
	// single argument that legitimately contains spaces (a path, a filter) is
	// preserved by the caller passing it as its own list entry via the API.
	if len(s.Args) == 1 && strings.ContainsAny(s.Args[0], " \t") {
		s.Args = strings.Fields(s.Args[0])
	}
	return nil
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
