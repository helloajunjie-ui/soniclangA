// Package registry is the tool discovery & registration center. It also hosts
// the world-book lazy-loading seam (DESIGN §5 tool/registry): in M2 all enabled
// tools are registered eagerly; as the catalog grows, only top-level index
// entries are returned by Schemas() and full schemas are expanded on demand.
//
// Besides the compiled-in built-ins the registry is the single mount point for
// dynamic tools: script tools (tool/sandbox) and MCP-proxied tools (mcp) add
// and remove entries at runtime. Every entry therefore carries provenance
// (Source) and a "removable" flag, so a disabled script tool can be unregistered
// without the UI ever being able to evict a built-in.
package registry

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/tool"
	"github.com/agenthub/agent-proxy/internal/tool/schema"
)

// Provenance of a registry entry.
const (
	SourceBuiltin = "builtin" // compiled-in, not removable
	SourceScript  = "script"  // user script tool, removable
	SourceMCP     = "mcp"     // proxied from an MCP server, removable
)

// Entry is a registered tool plus its provenance.
type Entry struct {
	Tool tool.Tool
	// Source is one of SourceBuiltin / SourceScript / SourceMCP.
	Source string
	// Owner is the script id or MCP server id a dynamic tool belongs to.
	Owner string
}

// Registry holds all registered tools.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// New constructs an empty registry.
func New() *Registry {
	return &Registry{entries: map[string]Entry{}}
}

// Register adds a compiled-in tool. Built-ins are never removable.
func (r *Registry) Register(t tool.Tool) {
	r.add(t, SourceBuiltin, "")
}

// RegisterDynamic adds (or replaces) a runtime tool owned by a script or an MCP
// server. Replacing matters: re-enabling a script must not leave the previous
// closure registered under the same name.
func (r *Registry) RegisterDynamic(source, owner string, t tool.Tool) {
	r.add(t, source, owner)
}

func (r *Registry) add(t tool.Tool, source, owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[t.Name()] = Entry{Tool: t, Source: source, Owner: owner}
}

// Unregister removes a tool by name. Built-ins are refused: they are part of the
// binary, not user data, so removing one would only come back at the next boot
// and confuse the UI in the meantime.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[name]
	if !ok {
		return errUnknown(name)
	}
	if e.Source == SourceBuiltin {
		return errBuiltin(name)
	}
	delete(r.entries, name)
	return nil
}

// UnregisterOwner removes every dynamic tool belonging to owner (a script id or
// an MCP server id) and returns how many were removed. Used when a script is
// deleted or an MCP server stops, so no orphaned tool keeps answering calls.
func (r *Registry) UnregisterOwner(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for name, e := range r.entries {
		if e.Owner == owner && e.Source != SourceBuiltin {
			delete(r.entries, name)
			n++
		}
	}
	return n
}

// Get returns a registered tool by name.
func (r *Registry) Get(name string) (tool.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	if !ok {
		return nil, false
	}
	return e.Tool, true
}

// Owner returns the script id / MCP server id a dynamic tool belongs to.
func (r *Registry) Owner(name string) (source, owner string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, found := r.entries[name]
	if !found {
		return "", "", false
	}
	return e.Source, e.Owner, true
}

// List returns the public info of all tools, built-ins first then dynamic ones,
// each group sorted by name so the UI order is stable.
func (r *Registry) List() []tool.ToolInfo {
	r.mu.RLock()
	out := make([]tool.ToolInfo, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, tool.ToolInfo{
			Name:        e.Tool.Name(),
			Description: e.Tool.Description(),
			Schema:      e.Tool.Schema(),
			Enabled:     true,
			Source:      e.Source,
			Owner:       e.Owner,
		})
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Source == SourceBuiltin) != (out[j].Source == SourceBuiltin) {
			return out[i].Source == SourceBuiltin
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Schemas returns the LLM-facing tool specifications (world-book ready).
func (r *Registry) Schemas() []provider.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.ToolSpec, 0, len(r.entries))
	for _, e := range r.entries {
		t := e.Tool
		params := t.Schema()
		if params == nil {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, provider.ToolSpec{Name: t.Name(), Description: t.Description(), Parameters: params})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Execute runs a tool by name and returns its string result.
//
// Every result passes the output sanitiser (DESIGN §10 工具结果净化): control
// characters are stripped and the text is capped. Without this a single tool —
// a script printing a log file, an MCP server returning a megabyte blob — could
// inject control bytes into the SSE frame or blow up the caller's context
// window. Doing it here rather than per-tool means a new tool cannot forget.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", errUnknown(name)
	}
	res, err := t.Execute(ctx, args)
	if err != nil {
		// The error text reaches the model too, so it gets the same treatment —
		// a stack trace full of escapes is just as capable of breaking a frame.
		// The error's own message is authoritative; some tools put the detail in
		// the returned string instead, so keep that too rather than dropping it.
		msg := err.Error()
		if extra := strings.TrimSpace(res); extra != "" && !strings.Contains(msg, extra) {
			msg = msg + ": " + extra
		}
		return "", &toolError{msg: schema.SanitizeResult(msg)}
	}
	return schema.SanitizeResult(res), nil
}

// toolError keeps a sanitised message while staying a normal error.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

func errUnknown(name string) error {
	return &regError{name: name}
}

type regError struct{ name string }

func (e *regError) Error() string { return "tool not found: " + e.name }

func errBuiltin(name string) error {
	return &builtinError{name: name}
}

type builtinError struct{ name string }

func (e *builtinError) Error() string {
	return "built-in tool cannot be removed: " + e.name + " (use the module switch instead)"
}
