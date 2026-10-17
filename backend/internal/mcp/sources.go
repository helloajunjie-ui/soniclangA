package mcp

// Source is a public MCP registry. These are discovery links, not installable
// entries: each one lists servers from many vendors, so the panel shows them as
// "更多 MCP" next to the hand-checked catalog above.
type Source struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// sources is the discovery list. Order is deliberate: official first, then the
// large aggregators, then community lists.
var sources = []Source{
	{"Model Context Protocol Servers", "https://github.com/modelcontextprotocol/servers", "官方 MCP 服务器集合"},
	{"Awesome MCP Servers", "https://github.com/punkpeye/awesome-mcp-servers", "精选的 MCP 服务器列表"},
	{"MCP World", "https://www.mcpworld.com/", "百度旗下 MCP 聚合平台"},
	{"BigModel MCP Market", "https://bigmodel.cn/marketplace/mcp", "智谱 MCP 市场，精选 MCP 极速接入"},
	{"ModelScope", "https://modelscope.cn/mcp", "魔搭社区 MCP 服务器"},
	{"Higress MCP Server", "https://mcp.higress.ai/", "Higress MCP 服务器目录"},
	{"mcp.so", "https://mcp.so/", "MCP 服务器发现平台"},
	{"Smithery", "https://smithery.ai/", "Smithery MCP 工具市场"},
	{"Glama", "https://glama.ai/mcp/servers", "Glama MCP 服务器目录"},
	{"PulseMCP", "https://www.pulsemcp.com/", "Pulse MCP 服务器索引"},
	{"Composio", "https://mcp.composio.dev/", "Composio MCP 开发工具"},
}

// Sources returns the discovery links for the UI.
func (m *Manager) Sources() []Source {
	out := make([]Source, len(sources))
	copy(out, sources)
	return out
}
