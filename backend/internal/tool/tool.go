// Package tool defines the Tool contract used by the orchestrator loop and the
// tool registry. Concrete tools live in tool/builtin (built-ins), tool/sandbox
// (script sandbox) and external MCP registrations (DESIGN §5 tool).
package tool

import "context"

// Tool is the uniform tool contract (name + JSON schema + execute).
type Tool interface {
	Name() string
	Description() string
	Schema() map[string]interface{}
	Execute(ctx context.Context, args map[string]interface{}) (string, error)
}

// ToolInfo is the public view of a registered tool.
type ToolInfo struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Schema      map[string]interface{} `json:"schema"`
	Enabled     bool                   `json:"enabled"`
	// Source is "builtin" | "script" | "mcp" and Owner is the script id or MCP
	// server id for dynamic tools (DESIGN §7 scripts/, mcp_servers/). Both are
	// empty for built-ins.
	Source string `json:"source,omitempty"`
	Owner  string `json:"owner,omitempty"`
}
