package builtin

import (
	"context"
	"github.com/agenthub/agent-proxy/internal/audit"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shell_exec is the only builtin that can change the machine. It is gated by the
// permission tier (see the orchestrator's TestShellExecIsGatedByThePermissionTier)
// and every call is audited; the cases here cover the tool's own behaviour.

func TestExecuteEmptyCommand(t *testing.T) {
	if _, err := (shellExecTool{}).Execute(context.Background(), map[string]interface{}{}); err == nil {
		t.Fatal("empty command returned no error")
	}
}

// A benign command still runs and its stdout comes back.
func TestExecuteRunsOrdinaryCommand(t *testing.T) {
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("powershell not available")
	}
	out, err := shellExecTool{}.Execute(context.Background(), map[string]interface{}{
		"command": `Write-Output agent-hub-probe`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "agent-hub-probe") {
		t.Errorf("stdout = %q", out)
	}
}

// The SSRF guard is what keeps http_fetch from reading the local API or cloud
// metadata endpoints on the model's behalf.
func TestGuardSSRF(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1:8787/api/v1/settings",
		"http://localhost:8787/v1/models",
		"http://[::1]:8787/",
		"http://0.0.0.0/",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.1/",
		"http://10.0.0.5/",
	}
	for _, u := range blocked {
		if err := guardSSRF(u); err == nil {
			t.Errorf("not blocked: %s", u)
		}
	}
	allowed := []string{
		"https://example.com/page",
		"https://api.github.com/repos/x/y",
	}
	for _, u := range allowed {
		if err := guardSSRF(u); err != nil {
			t.Errorf("over-blocked %s: %v", u, err)
		}
	}
}

func TestCalculator(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"1+1", "2"},
		{"2*3+4", "10"},
		{"10/4", "2.5"},
		{"(1+2)*3", "9"},
	}
	for _, c := range cases {
		got, err := calcTool{}.Execute(context.Background(), map[string]interface{}{"expression": c.expr})
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s = %q, want it to contain %q", c.expr, got, c.want)
		}
	}
	if _, err := (calcTool{}).Execute(context.Background(), map[string]interface{}{"expression": "1+"}); err == nil {
		t.Error("invalid expression returned no error")
	}
}

func TestGetTimeMentionsNow(t *testing.T) {
	out, err := timeTool{}.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("get_time: %v", err)
	}
	// The contract is RFC3339 (the model parses it); a prose date would not sort.
	if _, err := time.Parse(time.RFC3339, out); err != nil {
		t.Errorf("get_time = %q, want RFC3339", out)
	}
}

// On the user's own machine the point is not to referee commands but to be able to
// say afterwards what ran. Every shell_exec call is appended to the audit log
// before it executes.
func TestShellExecIsAudited(t *testing.T) {
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("powershell not available")
	}
	dir := t.TempDir()
	a := audit.New(dir)
	tool := shellExecTool{audit: a}
	if _, err := tool.Execute(context.Background(), map[string]interface{}{
		"command": `Write-Output audit-probe`,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !strings.Contains(string(b), "tool.shell_exec") || !strings.Contains(string(b), "audit-probe") {
		t.Errorf("command was not audited: %s", string(b))
	}
}

func TestAuditTruncatesLongCommands(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncateForAudit(long)
	if len(got) > 320 || !strings.HasSuffix(got, "…(截断)") {
		t.Errorf("long command not truncated: len=%d tail=%q", len(got), got[len(got)-10:])
	}
	if truncateForAudit("  echo hi  ") != "echo hi" {
		t.Error("short command was altered")
	}
}
