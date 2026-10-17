package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
)

// serverBin is the compiled fixture, built once for the whole package.
var serverBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agenthub-mcp-test-*")
	if err != nil {
		panic(err)
	}
	serverBin = filepath.Join(dir, "mcptestserver.exe")
	if runtimeIsWindows() {
		serverBin = filepath.Join(dir, "mcptestserver.exe")
	} else {
		serverBin = filepath.Join(dir, "mcptestserver")
	}
	cmd := exec.Command("go", "build", "-o", serverBin, "./testdata/mcptestserver")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("building mcp test server: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func runtimeIsWindows() bool { return os.PathSeparator == '\\' }

func TestStartHandshakeAndListTools(t *testing.T) {
	cl, err := Start(context.Background(), Config{Command: serverBin})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	proto, info := cl.ServerInfo()
	if proto != ProtocolVersion {
		t.Fatalf("protocol = %q, want %q", proto, ProtocolVersion)
	}
	if !strings.Contains(info, "mcptestserver") {
		t.Fatalf("server identity not captured: %q", info)
	}

	tools, err := cl.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("want 3 tools, got %d", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Fatalf("first tool = %q", tools[0].Name)
	}
	// The declared schema must survive the round trip; a dropped inputSchema
	// leaves the model unable to fill the arguments.
	if tools[0].InputSchema == nil {
		t.Fatal("inputSchema lost in transport")
	}
}

func TestCallTool(t *testing.T) {
	cl, err := Start(context.Background(), Config{Command: serverBin})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	out, err := cl.CallTool(context.Background(), "echo", map[string]interface{}{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "echo: hi") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestCallToolIsErrorBecomesGoError(t *testing.T) {
	cl, err := Start(context.Background(), Config{Command: serverBin})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	// isError must surface as an error, otherwise the orchestrator records a
	// failure string as if it were a successful answer.
	if _, err := cl.CallTool(context.Background(), "boom", nil); err == nil {
		t.Fatal("isError result must be an error")
	}
}

func TestNonTextBlocksAreNoticed(t *testing.T) {
	cl, err := Start(context.Background(), Config{Command: serverBin})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	out, err := cl.CallTool(context.Background(), "nodesc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("text block lost: %q", out)
	}
	// An image block must be reported rather than silently dropped, or the
	// model believes it received the whole payload.
	if !strings.Contains(out, "非文本内容块") {
		t.Fatalf("non-text block silently dropped: %q", out)
	}
}

func TestNotificationsDoNotConfuseReplyMatching(t *testing.T) {
	// A real server emits notifications mid-call. If the client returns one
	// instead of the reply, every tool silently returns the wrong payload.
	cmd := serverBin
	cl, err := Start(context.Background(), Config{Command: cmd, Env: map[string]string{"MCPTEST_NOTIFY": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	out, err := cl.CallTool(context.Background(), "echo", map[string]interface{}{"text": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "echo: x") {
		t.Fatalf("notification was mistaken for the reply: %q", out)
	}
}

func TestBannerLinesAreSkipped(t *testing.T) {
	cl, err := Start(context.Background(), Config{Command: serverBin, Env: map[string]string{"MCPTEST_BANNER": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	// The banner is printed before the handshake; if it were not skipped the
	// initialize call would fail here.
	if _, _, err := func() (string, string, error) { p, i := cl.ServerInfo(); return p, i, nil }(); err != nil {
		t.Fatal(err)
	}
}

func TestStartFailsForMissingCommand(t *testing.T) {
	if _, err := Start(context.Background(), Config{Command: "definitely-not-a-real-binary-xyz"}); err == nil {
		t.Fatal("a missing command must produce an error, not a silent dead server")
	}
}

func TestStartFailsForEmptyCommand(t *testing.T) {
	if _, err := Start(context.Background(), Config{}); err == nil {
		t.Fatal("empty command must be rejected")
	}
}

func TestCallAfterCloseFails(t *testing.T) {
	cl, err := Start(context.Background(), Config{Command: serverBin})
	if err != nil {
		t.Fatal(err)
	}
	_ = cl.Close()
	if _, err := cl.CallTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("calling a closed client must fail fast, not hang")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	cl, err := Start(context.Background(), Config{Command: serverBin})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cl.Close(); err != nil {
		t.Fatalf("second Close must be a no-op: %v", err)
	}
}

// ---- manager ----

func newTestManager(t *testing.T) (*Manager, *registry.Registry) {
	t.Helper()
	dir := t.TempDir()
	v, err := keyvault.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := storage.NewJSON(filepath.Join(dir, "mcp"))
	reg := registry.New()
	return NewManager(st, v, reg, nil), reg
}

func TestManagerLifecycleRegistersProxies(t *testing.T) {
	mgr, reg := newTestManager(t)
	s := &Server{Name: "test", Command: serverBin}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 0 {
		t.Fatal("a created server must not register anything")
	}

	info, err := mgr.Start(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusRunning {
		t.Fatalf("status = %q", info.Status)
	}
	if info.ToolCount != 3 {
		t.Fatalf("tool count = %d, want 3", info.ToolCount)
	}
	names := map[string]bool{}
	for _, ti := range reg.List() {
		if ti.Source != registry.SourceMCP {
			t.Fatal("only MCP tools should be registered")
		}
		names[ti.Name] = true
	}
	// Prefixed names avoid collisions between servers exposing the same tool.
	if !names["mcp__"+s.ID+"__echo"] {
		t.Fatalf("expected prefixed tool name, got %v", names)
	}

	// A proxied call must actually reach the server.
	if _, err := reg.Execute(context.Background(), "mcp__"+s.ID+"__echo", map[string]interface{}{"text": "yo"}); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 0 {
		t.Fatal("stopping must unregister the proxied tools — orphans keep answering calls")
	}
}

func TestManagerStartIsIdempotent(t *testing.T) {
	mgr, _ := newTestManager(t)
	s := &Server{Name: "test", Command: serverBin}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	info, err := mgr.Start(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ToolCount != 3 {
		t.Fatalf("second Start duplicated tools: count=%d", info.ToolCount)
	}
}

func TestManagerStartFailureRecordsError(t *testing.T) {
	mgr, reg := newTestManager(t)
	s := &Server{Name: "bad", Command: "definitely-not-a-real-binary-xyz"}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), s.ID); err == nil {
		t.Fatal("starting a broken server must report the failure")
	}
	got, err := mgr.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusError || got.LastError == "" {
		t.Fatalf("failure not recorded: %+v", got)
	}
	// Nothing may be registered from a server that never connected.
	if len(reg.List()) != 0 {
		t.Fatal("a failed server must register no tools")
	}
}

func TestSecretEnvIsVaultedNotPersisted(t *testing.T) {
	mgr, _ := newTestManager(t)
	dir := t.TempDir()
	_ = dir
	s := &Server{
		Name:    "s",
		Command: serverBin,
		Env:     map[string]string{"GITHUB_TOKEN": "ghp_supersecret", "LOG_LEVEL": "debug"},
	}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The plaintext must not survive in either field.
	if _, ok := got.Env["GITHUB_TOKEN"]; ok {
		t.Fatal("secret left in plain env")
	}
	if strings.Contains(got.SecretEnv["GITHUB_TOKEN"], "supersecret") {
		t.Fatalf("secret not masked in the API view: %q", got.SecretEnv["GITHUB_TOKEN"])
	}
	if _, ok := got.Env["LOG_LEVEL"]; !ok {
		t.Fatal("a non-secret env var must stay plain")
	}

	// And the token must reach the child process, decrypted at spawn time.
	env, err := mgr.resolveEnv(got)
	if err != nil {
		t.Fatal(err)
	}
	if env["GITHUB_TOKEN"] != "ghp_supersecret" {
		t.Fatalf("secret not recovered for the child: %q", env["GITHUB_TOKEN"])
	}
}

func TestUpdateStopsServerOnCommandChange(t *testing.T) {
	mgr, reg := newTestManager(t)
	s := &Server{Name: "test", Command: serverBin}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	// Change the transport: the old process speaks for the old config.
	s2 := *s
	s2.Command = "other-binary"
	if err := mgr.Update(context.Background(), s.ID, &s2); err != nil {
		t.Fatal(err)
	}
	got, _ := mgr.Get(s.ID)
	if got.Status != StatusStopped {
		t.Fatalf("status after transport change = %q, want stopped", got.Status)
	}
	if len(reg.List()) != 0 {
		t.Fatal("old server's tools must be unregistered when the command changes")
	}
}

func TestUpdateKeepsRunningWhenOnlyNameChanges(t *testing.T) {
	mgr, reg := newTestManager(t)
	s := &Server{Name: "test", Command: serverBin}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	before := len(reg.List())
	s2 := *s
	s2.Name = "renamed"
	if err := mgr.Update(context.Background(), s.ID, &s2); err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != before {
		t.Fatal("a rename must not disturb the live connection")
	}
	got, _ := mgr.Get(s.ID)
	if got.Status != StatusRunning {
		t.Fatalf("status = %q, want running", got.Status)
	}
}

func TestDeleteRemovesVaultEntries(t *testing.T) {
	dir := t.TempDir()
	v, err := keyvault.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := storage.NewJSON(filepath.Join(dir, "mcp"))
	mgr := NewManager(st, v, registry.New(), nil)
	s := &Server{Name: "s", Command: serverBin, Env: map[string]string{"API_TOKEN": "tok-123"}}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got, _ := mgr.Get(s.ID)
	vid := got.EnvRefs["API_TOKEN"]
	if vid == "" {
		t.Fatal("no vault ref recorded for the secret")
	}
	if _, err := v.Get(vid); err != nil {
		t.Fatalf("secret not in vault: %v", err)
	}
	if err := mgr.Delete(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	// A deleted server must not leave a usable token behind on disk.
	if _, err := v.Get(vid); err == nil {
		t.Fatal("vault entry survived server deletion")
	}
}

func TestTestDoesNotRegisterTools(t *testing.T) {
	mgr, reg := newTestManager(t)
	res, err := mgr.Test(context.Background(), &Server{Command: serverBin})
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := res["ok"].(bool)
	if !ok {
		t.Fatalf("test failed: %+v", res)
	}
	if res["toolCount"].(int) != 3 {
		t.Fatalf("toolCount wrong: %+v", res)
	}
	if len(reg.List()) != 0 {
		t.Fatal("Test must be side-effect free")
	}
}

func TestIsSecretEnvKey(t *testing.T) {
	for _, k := range []string{"GITHUB_TOKEN", "api_key", "MY_SECRET", "DB_PASSWORD", "auth_header"} {
		if !IsSecretEnvKey(k) {
			t.Fatalf("%s should be treated as secret", k)
		}
	}
	for _, k := range []string{"LOG_LEVEL", "PATH", "HOME", "NODE_ENV"} {
		if IsSecretEnvKey(k) {
			t.Fatalf("%s should not be treated as secret", k)
		}
	}
}

func TestProxiedNameIsValid(t *testing.T) {
	name := proxiedName("my-server.v1", "search.docs")
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if !ok {
			t.Fatalf("proxied name %q contains an illegal char %q", name, r)
		}
	}
	if len(name) > 64 {
		t.Fatalf("proxied name too long: %d", len(name))
	}
}

func TestManagerListReturnsCopies(t *testing.T) {
	mgr, _ := newTestManager(t)
	s := &Server{Name: "s", Command: serverBin, Env: map[string]string{"A": "1"}}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	list := mgr.List()
	list[0].Name = "hacked"
	again, _ := mgr.Get(s.ID)
	if again.Name == "hacked" {
		t.Fatal("List must return copies")
	}
}

func TestProxyToolResolvesLiveClientAfterRestart(t *testing.T) {
	mgr, reg := newTestManager(t)
	s := &Server{Name: "s", Command: serverBin}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	name := "mcp__" + s.ID + "__echo"
	if _, err := reg.Execute(context.Background(), name, map[string]interface{}{"text": "a"}); err != nil {
		t.Fatal(err)
	}
	// Stop, then start again. A proxy that captured the old client pointer would
	// keep writing into a closed pipe from here on.
	if err := mgr.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Execute(context.Background(), name, map[string]interface{}{"text": "b"}); err != nil {
		t.Fatalf("proxy did not follow the restart: %v", err)
	}
}

func TestManagerSurvivesRequestContextCancellation(t *testing.T) {
	// Regression: Start derives the child process lifetime from its ctx. When the
	// manager passed an HTTP request ctx, the server was killed the instant the
	// response was written and every later call failed with "pipe is being
	// closed" — while the tool list still showed the tools as connected.
	mgr, reg := newTestManager(t)
	s := &Server{Name: "s", Command: serverBin}
	if err := mgr.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	// Simulate the HTTP handler: a context cancelled as soon as the handler
	// returns.
	reqCtx, cancel := context.WithCancel(context.Background())
	if _, err := mgr.Start(reqCtx, s.ID); err != nil {
		t.Fatal(err)
	}
	cancel() // the "response has been written" moment

	// The kill is asynchronous, so give it a moment — in production the next
	// tool call arrives on a later request, long after the process died.
	time.Sleep(500 * time.Millisecond)

	// The server must still be alive and callable.
	if !mgr.Info(s.ID).Running {
		t.Fatal("server was killed by the request context")
	}
	name := "mcp__" + s.ID + "__echo"
	if _, err := reg.Execute(context.Background(), name, map[string]interface{}{"text": "still here"}); err != nil {
		t.Fatalf("proxy broken after request context was cancelled: %v", err)
	}
}

func TestStopAll(t *testing.T) {
	mgr, reg := newTestManager(t)
	var ids []string
	for i := 0; i < 2; i++ {
		s := &Server{Name: string(rune('a' + i)), Command: serverBin}
		if err := mgr.Create(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.Start(context.Background(), s.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	mgr.StopAll()
	for _, id := range ids {
		if mgr.Info(id).Running {
			t.Fatalf("server %s still running after StopAll", id)
		}
	}
	if len(reg.List()) != 0 {
		t.Fatal("StopAll must unregister every proxied tool")
	}
}

// A server that completes the handshake and then never answers used to park the
// reader inside call() forever while holding callMu: the 120s CallTimeout was
// applied to a context nothing consulted, every later tools/call and /start
// blocked on that mutex, and the whole MCP surface wedged until restart.
func TestCallToolReturnsWhenTheServerNeverAnswers(t *testing.T) {
	cl, err := Start(context.Background(), Config{
		Command: serverBin,
		Env:     map[string]string{"MCPTEST_HANG": "1"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := cl.CallTool(ctx, "echo", map[string]interface{}{"text": "hi"}); err == nil {
		t.Fatal("a silent server produced no error")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the call ignored its deadline and returned after %s", took)
	}
}

// Two sequential calls must not deadlock on callMu when the first one is
// abandoned: the mutex is released with the call, not with the server.
func TestAbandonedCallReleasesTheLock(t *testing.T) {
	cl, err := Start(context.Background(), Config{
		Command: serverBin,
		Env:     map[string]string{"MCPTEST_SLOW_MS": "50"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cl.Close()

	// First call: already-expired deadline.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cl.CallTool(expired, "echo", map[string]interface{}{"text": "a"}); err == nil {
		t.Fatal("an expired deadline produced no error")
	}

	// Second call must still work (or fail cleanly because the client closed the
	// hung process), but it must not hang.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer c2()
		_, _ = cl.CallTool(ctx, "echo", map[string]interface{}{"text": "b"})
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the second call blocked: callMu was never released")
	}
}
