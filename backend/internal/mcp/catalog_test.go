package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
)

// The catalog exists so a user can add a known MCP server without knowing its
// package name, args and environment. These tests guard the parts that would
// otherwise fail silently at launch time.

func TestCatalogIsWellFormed(t *testing.T) {
	m := &Manager{}
	entries := m.Catalog()
	if len(entries) < 5 {
		t.Fatalf("catalog has only %d entries", len(entries))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.ID == "" || seen[e.ID] {
			t.Errorf("duplicate or empty id %q", e.ID)
		}
		seen[e.ID] = true
		if e.Name == "" || e.Description == "" {
			t.Errorf("%s: missing name/description", e.ID)
		}
		if e.Command == "" || len(e.Args) == 0 {
			t.Errorf("%s: no launch command", e.ID)
		}
		// Every entry must name its upstream, so the user can see what they run.
		if !strings.HasPrefix(e.Source, "https://github.com/") {
			t.Errorf("%s: source %q is not a repository URL", e.ID, e.Source)
		}
		// And say what was checked: an unmaintained entry is how a package
		// disappears and every install starts failing with "process exited".
		if e.Verified == "" {
			t.Errorf("%s: no verification note", e.ID)
		}
		// Placeholders must be resolvable: one that no EnvDefault covers would
		// make install fail every time.
		for _, a := range e.Args {
			if !strings.Contains(a, "${") {
				continue
			}
			for _, name := range []string{"DIR", "DB"} {
				if strings.Contains(a, "${"+name+"}") && e.EnvDefaults[name] == "" {
					t.Errorf("%s: placeholder ${%s} has no default", e.ID, name)
				}
			}
		}
	}
}

func newCatalogManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	vault, err := keyvault.New(dir)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	return NewManager(storage.NewJSON(filepath.Join(dir, "mcp")), vault, registry.New(), audit.New(dir))
}

func TestInstallRejectsUnknownEntry(t *testing.T) {
	m := newCatalogManager(t)
	if _, err := m.InstallFromCatalog(context.Background(), "no-such-server", nil); err == nil {
		t.Fatal("installing an unknown catalog id returned nil")
	}
	if len(m.List()) != 0 {
		t.Error("a rejected install still created a server")
	}
}

// A server that needs a token must be refused before anything is written: a
// half-configured entry in the list looks installed but can never start.
func TestInstallRequiresSecretBeforeCreatingAnything(t *testing.T) {
	m := newCatalogManager(t)
	_, err := m.InstallFromCatalog(context.Background(), "github", nil)
	if err == nil {
		t.Fatal("install without the token returned nil")
	}
	if !strings.Contains(err.Error(), "GITHUB_PERSONAL_ACCESS_TOKEN") {
		t.Errorf("error does not name the missing variable: %v", err)
	}
	if len(m.List()) != 0 {
		t.Error("a server was created despite the missing token")
	}
}

// Placeholders are substituted from the env, so a user-provided directory works
// without editing the command line by hand.
func TestInstallSubstitutesPlaceholders(t *testing.T) {
	m := newCatalogManager(t)
	// Point the entry at the mock server so the test also proves the install
	// actually starts something, instead of waiting out a real npx handshake.
	before := len(catalog)
	catalog = append(catalog, CatalogEntry{
		ID: "test-filesystem", Name: "测试文件系统", Description: "mock",
		Source: "https://github.com/modelcontextprotocol/servers", Command: serverBin,
		Args: []string{"--dir", "${DIR}"}, EnvDefaults: map[string]string{"DIR": "."},
	})
	defer func() { catalog = catalog[:before] }()

	info, err := m.InstallFromCatalog(context.Background(), "test-filesystem", map[string]string{"DIR": `E:\work\proj`})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info == nil || info.Status != "running" {
		t.Fatalf("install did not start the server: %+v", info)
	}

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("install did not create a server entry (%d entries)", len(list))
	}
	srv, err := m.Get(list[0].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if srv.Command != serverBin {
		t.Errorf("command = %q, want the catalog entry's command", srv.Command)
	}
	if !strings.Contains(strings.Join(srv.Args, " "), `E:\work\proj`) {
		t.Errorf("placeholder was not substituted: %v", srv.Args)
	}
	if srv.Env["DIR"] != `E:\work\proj` {
		t.Errorf("DIR env = %q", srv.Env["DIR"])
	}
}

// Discovery links must be real https URLs: the panel renders them as-is, and a
// typo would only show up as a dead link in the UI.
func TestSourcesAreWellFormed(t *testing.T) {
	m := &Manager{}
	list := m.Sources()
	if len(list) < 5 {
		t.Fatalf("only %d sources", len(list))
	}
	seen := map[string]bool{}
	for _, s := range list {
		if s.Name == "" || s.Description == "" {
			t.Errorf("source %q is missing a name/description", s.URL)
		}
		if !strings.HasPrefix(s.URL, "https://") {
			t.Errorf("%s: url %q is not https", s.Name, s.URL)
		}
		if seen[s.URL] {
			t.Errorf("duplicate url %q", s.URL)
		}
		seen[s.URL] = true
	}
}
