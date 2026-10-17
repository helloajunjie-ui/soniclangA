package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// CatalogEntry is one known-good MCP server that can be installed on demand.
//
// Adding a server by hand means knowing the exact command, the package name and
// which environment variables it needs — and getting any of those wrong shows up
// as "server exited" with no hint why. The catalog is the short list of servers
// that are worth having and are startable without a build step: each entry is a
// git/npm/pypi source that `npx`/`uvx` fetches on first run, so "install" is just
// recording the launch command.
type CatalogEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Source is the upstream repository, shown so the user can check what they
	// are about to run before installing it.
	Source  string   `json:"source"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// EnvDefaults are non-secret settings the server needs (paths, tokens that
	// are safe). SecretEnv names variables the user must fill in; they are stored
	// in the vault, never in the server record.
	EnvDefaults map[string]string `json:"envDefaults,omitempty"`
	SecretEnv   []string          `json:"secretEnv,omitempty"`
	// Requires lists host tools the entry needs (node, uv, docker) so the UI can
	// say why an install would not run.
	Requires string `json:"requires,omitempty"`
	// Verified records how the entry was checked against the real registry
	// (package + version, or the upstream repo) and when. A catalog entry that
	// nobody re-checked is how a package name goes stale and installs start
	// failing with "process exited".
	Verified string `json:"verified"`
}

// catalog is deliberately small and hand-checked: a list of everything on the
// internet would be an unverified attack surface, and these are the ones the
// tool-oriented workflows actually reach for.
var catalog = []CatalogEntry{
	{
		ID:       "filesystem",
		Verified: "npm @modelcontextprotocol/server-filesystem 1.0.0 · 2026-10-17", Name: "文件系统",
		Description: "按目录读、写、搜索本机文件（只能访问你授权的目录）",
		Source:      "https://github.com/modelcontextprotocol/servers/tree/main/src/filesystem",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "${DIR}"},
		EnvDefaults: map[string]string{"DIR": "."},
		Requires:    "node",
	},
	{
		ID:       "git",
		Verified: "PyPI mcp-server-git 2026.10.10 · 2026-10-17", Name: "Git",
		Description: "查看仓库状态、提交历史、diff，读取文件内容",
		Source:      "https://github.com/modelcontextprotocol/servers/tree/main/src/git",
		Command:     "uvx", Args: []string{"mcp-server-git", "--repository", "${DIR}"},
		EnvDefaults: map[string]string{"DIR": "."},
		Requires:    "uv",
	},
	{
		ID:       "fetch",
		Verified: "PyPI mcp-server-fetch 2026.10.10 · 2026-10-17", Name: "网页抓取",
		Description: "把网页转成 Markdown 给模型读（比原始 HTML 省 token）",
		Source:      "https://github.com/modelcontextprotocol/servers/tree/main/src/fetch",
		Command:     "uvx", Args: []string{"mcp-server-fetch"},
		Requires: "uv",
	},
	{
		ID:       "memory",
		Verified: "npm @modelcontextprotocol/server-memory 1.0.0 · 2026-10-17", Name: "知识图谱记忆",
		Description: "以实体/关系形式维护跨会话的长期记忆",
		Source:      "https://github.com/modelcontextprotocol/servers/tree/main/src/memory",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-memory"},
		Requires: "node",
	},
	{
		ID:       "sequential-thinking",
		Verified: "npm @modelcontextprotocol/server-sequential-thinking 1.0.0 · 2026-10-17", Name: "结构化思考",
		Description: "把复杂问题拆成可回退的思考步骤",
		Source:      "https://github.com/modelcontextprotocol/servers/tree/main/src/sequentialthinking",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-sequential-thinking"},
		Requires: "node",
	},
	{
		ID:       "time",
		Verified: "PyPI mcp-server-time 2026.10.10 · 2026-10-17", Name: "时间与时区",
		Description: "时区换算、当前时间、时间差计算",
		Source:      "https://github.com/modelcontextprotocol/servers/tree/main/src/time",
		Command:     "uvx", Args: []string{"mcp-server-time"},
		Requires: "uv",
	},
	{
		ID:       "sqlite",
		Verified: "PyPI mcp-server-sqlite 2025.4.25 · 2026-10-17", Name: "SQLite",
		Description: "对指定数据库文件跑只读查询并返回结果",
		Source:      "https://github.com/modelcontextprotocol/servers/tree/main/src/sqlite",
		Command:     "uvx", Args: []string{"mcp-server-sqlite", "--db-path", "${DB}"},
		EnvDefaults: map[string]string{"DB": "data/agenthub.db"},
		Requires:    "uv",
	},
	{
		// The npm package @modelcontextprotocol/server-github is deprecated ("no
		// longer supported"): installing it yields a server that starts and then
		// fails. The official replacement is the Go server published as a container
		// image. `-e VAR` (no value) forwards the variable from this process's
		// environment, so the token never appears in the command line.
		ID: "github", Name: "GitHub",
		Verified:    "github/github-mcp-server（官方；npm 旧包已废弃）· 2026-10-17",
		Description: "读写仓库、issue、PR（需 Docker 与个人访问令牌）",
		Source:      "https://github.com/github/github-mcp-server",
		Command:     "docker",
		Args:        []string{"run", "-i", "--rm", "-e", "GITHUB_PERSONAL_ACCESS_TOKEN", "ghcr.io/github/github-mcp-server"},
		SecretEnv:   []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
		Requires:    "docker",
	},
	{
		ID:       "playwright",
		Verified: "npm @playwright/mcp 0.0.83 · 2026-10-17", Name: "浏览器自动化",
		Description: "真实浏览器里点按、填表、截图、取无障碍树",
		Source:      "https://github.com/microsoft/playwright-mcp",
		Command:     "npx", Args: []string{"-y", "@playwright/mcp@latest"},
		Requires: "node",
	},
	{
		ID:       "context7",
		Verified: "npm @upstash/context7-mcp 4.4.2 · 2026-10-17", Name: "库文档检索",
		Description: "按库名取最新官方文档片段，减少凭记忆写 API",
		Source:      "https://github.com/upstash/context7",
		Command:     "npx", Args: []string{"-y", "@upstash/context7-mcp"},
		Requires: "node",
	},
}

// Catalog lists the installable servers, sorted by name so the UI is stable.
func (m *Manager) Catalog() []CatalogEntry {
	out := make([]CatalogEntry, len(catalog))
	copy(out, catalog)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CatalogEntry returns one entry by id.
func CatalogEntryByID(id string) (CatalogEntry, bool) {
	for _, e := range catalog {
		if e.ID == id {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

// InstallFromCatalog records a catalog entry as a server and starts it.
//
// env carries the values the entry asks for: EnvDefaults keys (paths) and
// SecretEnv keys (tokens, sealed through the usual vault path). Missing required
// values fail before anything is written, so a half-configured server never
// appears in the list.
func (m *Manager) InstallFromCatalog(ctx context.Context, id string, env map[string]string) (*Info, error) {
	entry, ok := CatalogEntryByID(id)
	if !ok {
		return nil, fmt.Errorf("未知的 MCP 目录项：%s", id)
	}
	merged := map[string]string{}
	for k, v := range entry.EnvDefaults {
		merged[k] = v
	}
	for k, v := range env {
		merged[k] = v
	}
	// ${VAR} placeholders in args make the launch line configurable without
	// turning the catalog into a template language.
	args := make([]string, 0, len(entry.Args))
	for _, a := range entry.Args {
		a = strings.ReplaceAll(a, "${DIR}", merged["DIR"])
		a = strings.ReplaceAll(a, "${DB}", merged["DB"])
		if strings.Contains(a, "${") {
			return nil, fmt.Errorf("参数 %q 里的占位符没有取值", a)
		}
		args = append(args, a)
	}
	for _, key := range entry.SecretEnv {
		if strings.TrimSpace(merged[key]) == "" {
			return nil, fmt.Errorf("该服务器需要 %s", key)
		}
	}
	// Plain env must not receive the secrets: splitSecrets moves them into the
	// vault, and passing them twice would leave a plaintext copy behind.
	plain := map[string]string{}
	for k, v := range merged {
		isSecret := false
		for _, s := range entry.SecretEnv {
			if k == s {
				isSecret = true
				break
			}
		}
		if !isSecret && v != "" {
			plain[k] = v
		}
	}
	srv := &Server{
		Name:    entry.Name,
		Command: entry.Command,
		Args:    args,
		Env:     plain,
		Enabled: true,
	}
	if err := m.Create(ctx, srv); err != nil {
		return nil, err
	}
	return m.Start(ctx, srv.ID)
}
