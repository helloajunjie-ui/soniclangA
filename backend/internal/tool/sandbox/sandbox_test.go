package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
)

func newTestService(t *testing.T) (*Service, *registry.Registry) {
	t.Helper()
	st := storage.NewJSON(t.TempDir())
	reg := registry.New()
	return NewService(st, reg, nil), reg
}

// echoJS prints its input, so a passing smoke run proves the whole chain works.
const echoJS = "console.log(process.env.AGENTHUB_INPUT || '')"

func TestCreateIsDisabledByDefault(t *testing.T) {
	svc, reg := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本，用于验证脚本链路", Runtime: RuntimeJS, Code: echoJS, Enabled: true}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	// DESIGN §10: creating a tool must not imply running it.
	if got.Enabled {
		t.Fatal("a newly created script must not be enabled — saving a draft is not consent to execute code")
	}
	if _, ok := reg.Get("echo_tool"); ok {
		t.Fatal("a disabled script must not be registered as callable")
	}
}

func TestCreateRejectsBadRuntime(t *testing.T) {
	svc, _ := newTestService(t)
	for _, rt := range []string{"", "ruby", "sh"} {
		e := &ScriptToolEntity{Name: "t", Description: "测试用工具描述", Runtime: rt, Code: echoJS}
		if err := svc.Create(e); err == nil {
			t.Fatalf("runtime %q must be rejected", rt)
		}
	}
}

func TestEnableRequiresAdmission(t *testing.T) {
	svc, reg := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本，用于验证脚本链路", Runtime: RuntimeJS, Code: echoJS}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	// Enabling without the gate must be refused: an unproven tool is exactly the
	// half-finished product the gate exists to keep out.
	if _, err := svc.SetEnabled(e.ID, true); err == nil {
		t.Fatal("enabling without admission must fail")
	}
	if _, ok := reg.Get("echo_tool"); ok {
		t.Fatal("refused tool must not be registered")
	}
}

func TestAdmitThenEnableRegistersTool(t *testing.T) {
	svc, reg := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本，用于验证脚本链路", Runtime: RuntimeJS, Code: echoJS}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Admit(context.Background(), e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("admission failed: %+v", res)
	}
	if _, err := svc.SetEnabled(e.ID, true); err != nil {
		t.Fatalf("enable after passing admission failed: %v", err)
	}
	if _, ok := reg.Get("echo_tool"); !ok {
		t.Fatal("enabled+admitted tool must be registered")
	}
	out, err := reg.Execute(context.Background(), "echo_tool", map[string]interface{}{"input": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("tool output lost the input: %q", out)
	}
}

func TestAdmitFailsOnBrokenCode(t *testing.T) {
	svc, reg := newTestService(t)
	e := &ScriptToolEntity{Name: "broken_tool", Description: "故意包含语法错误的脚本", Runtime: RuntimeJS, Code: "this is ( not javascript"}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Admit(context.Background(), e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("a script that cannot run must not pass the smoke gate")
	}
	if res.Gate != "smoke" {
		t.Fatalf("expected smoke gate, got %q", res.Gate)
	}
	if _, err := svc.SetEnabled(e.ID, true); err == nil {
		t.Fatal("a tool that failed admission must not be enableable")
	}
	if _, ok := reg.Get("broken_tool"); ok {
		t.Fatal("failed tool must not be registered")
	}
}

func TestCodeChangeRevokesAdmission(t *testing.T) {
	svc, reg := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本，用于验证脚本链路", Runtime: RuntimeJS, Code: echoJS}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Admit(context.Background(), e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnabled(e.ID, true); err != nil {
		t.Fatal(err)
	}

	// Swap in malicious code. The previous verdict was for the *old* body, so
	// the tool must drop back to disabled and out of the registry.
	e2 := *e
	e2.Code = "console.log('pwned')"
	if err := svc.Update(e.ID, &e2); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(e.ID)
	if got.Enabled {
		t.Fatal("editing the code must revoke the enabled state")
	}
	if got.Admission != nil {
		t.Fatal("editing the code must clear the stale admission verdict")
	}
	if _, ok := reg.Get("echo_tool"); ok {
		t.Fatal("an unvetted edit must be pulled out of the registry immediately")
	}
	if _, err := svc.SetEnabled(e.ID, true); err == nil {
		t.Fatal("re-enabling without a fresh gate must fail")
	}
}

func TestDescriptionOnlyEditKeepsAdmission(t *testing.T) {
	svc, _ := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本，用于验证脚本链路", Runtime: RuntimeJS, Code: echoJS}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Admit(context.Background(), e.ID); err != nil {
		t.Fatal(err)
	}
	e2 := *e
	e2.Description = "回显输入文本（描述已更新）"
	if err := svc.Update(e.ID, &e2); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(e.ID)
	// The code did not change, so the verdict still applies — re-gating on every
	// typo would make the gate noise rather than a safety property.
	if got.Admission == nil || !got.Admission.OK {
		t.Fatal("a description-only edit must not discard a valid verdict")
	}
}

func TestDisableUnregistersTool(t *testing.T) {
	svc, reg := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本，用于验证脚本链路", Runtime: RuntimeJS, Code: echoJS}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Admit(context.Background(), e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnabled(e.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnabled(e.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("echo_tool"); ok {
		t.Fatal("disabling must unregister the tool")
	}
}

func TestDeleteUnregistersTool(t *testing.T) {
	svc, reg := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本，用于验证脚本链路", Runtime: RuntimeJS, Code: echoJS}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Admit(context.Background(), e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnabled(e.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("echo_tool"); ok {
		t.Fatal("deleting must unregister the tool — an orphan keeps answering calls")
	}
	if _, err := svc.Get(e.ID); err == nil {
		t.Fatal("deleted script must be gone from the store")
	}
}

func TestDisabledToolRefusesExecution(t *testing.T) {
	st := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本", Runtime: RuntimeJS, Code: echoJS, Enabled: false}
	tool := NewScriptTool(*st)
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"input": "x"}); err == nil {
		t.Fatal("a disabled tool must refuse to run even if invoked directly")
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	e := &ScriptToolEntity{
		Name: "slow_tool", Description: "故意超时的脚本", Runtime: RuntimeJS,
		Code: "while(true){}", TimeoutSeconds: 1,
	}
	tool := buildTool(e, true)
	start := time.Now()
	_, err := tool.Execute(context.Background(), map[string]interface{}{"input": "x"})
	if err == nil {
		t.Fatal("an infinite script must be killed")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("expected a timeout error, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout not enforced promptly: took %v", elapsed)
	}
}

func TestArgEnvInjection(t *testing.T) {
	// Declared args must reach the script, otherwise a schema promising
	// parameters silently delivers nothing.
	code := "console.log(process.env.AGENTHUB_ARG_CITY || 'none')"
	st := &ScriptToolEntity{
		Name: "city_tool", Description: "回显城市参数", Runtime: RuntimeJS, Code: code,
		Args: []string{"city"},
	}
	tool := buildTool(st, true)
	out, err := tool.Execute(context.Background(), map[string]interface{}{"city": "Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Shanghai") {
		t.Fatalf("declared arg not delivered: %q", out)
	}
}

func TestListReturnsCopies(t *testing.T) {
	svc, _ := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本", Runtime: RuntimeJS, Code: echoJS}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	list := svc.List()
	if len(list) != 1 {
		t.Fatalf("want 1, got %d", len(list))
	}
	// Mutating a returned entity must not corrupt the cache.
	list[0].Name = "hacked"
	again, _ := svc.Get(e.ID)
	if again.Name != "echo_tool" {
		t.Fatal("List must return copies, not live cache pointers")
	}
}

func TestRunRecordsOutcome(t *testing.T) {
	svc, _ := newTestService(t)
	e := &ScriptToolEntity{Name: "echo_tool", Description: "回显输入文本", Runtime: RuntimeJS, Code: echoJS}
	if err := svc.Create(e); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.Run(context.Background(), e.ID, "ping")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.OK || !strings.Contains(rec.Output, "ping") {
		t.Fatalf("run record wrong: %+v", rec)
	}
	got, _ := svc.Get(e.ID)
	if got.LastRun == nil {
		t.Fatal("run outcome must be persisted so the UI can show it")
	}
}
