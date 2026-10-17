package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
)

// fakeTool records what it was called with. The loop's whole job is "turn model
// tool calls into results without letting one failure end the turn", so the
// things worth pinning are: was it invoked at all (gate), with what arguments
// (malformed JSON), and what came back to the model on failure.
type fakeTool struct {
	name    string
	result  string
	err     error
	calls   int
	lastArg map[string]interface{}
}

func (f *fakeTool) Name() string                   { return f.name }
func (f *fakeTool) Description() string            { return "fake" }
func (f *fakeTool) Schema() map[string]interface{} { return nil }
func (f *fakeTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	f.calls++
	f.lastArg = args
	return f.result, f.err
}

func testRegistry(tools ...*fakeTool) *registry.Registry {
	reg := registry.New()
	for _, t := range tools {
		reg.Register(t)
	}
	return reg
}

func call(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Name: name, Arguments: args}
}

func TestExecuteReturnsToolOutput(t *testing.T) {
	tool := &fakeTool{name: "calculator", result: "42"}
	out := Execute(context.Background(), testRegistry(tool), []provider.ToolCall{
		call("c1", "calculator", `{"expr":"6*7"}`),
	}, nil)

	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	if out[0].ID != "c1" || out[0].Name != "calculator" {
		t.Errorf("result lost its identity: %+v", out[0])
	}
	if out[0].Result != "42" || out[0].Err != "" {
		t.Errorf("result = %q err = %q, want 42 / empty", out[0].Result, out[0].Err)
	}
	if tool.calls != 1 {
		t.Errorf("tool ran %d times, want 1", tool.calls)
	}
	if got := tool.lastArg["expr"]; got != "6*7" {
		t.Errorf("tool received expr = %v, want 6*7", got)
	}
}

// A denied call must not reach the tool at all. The permission tiers are only
// real if the gate is checked *before* Execute — checking afterwards would run
// the side effect and then report that it was not allowed.
func TestExecuteGateDenialSkipsTheTool(t *testing.T) {
	tool := &fakeTool{name: "write_file", result: "written"}
	gate := func(string, map[string]interface{}) error { return errors.New("需要授权") }

	out := Execute(context.Background(), testRegistry(tool), []provider.ToolCall{
		call("c1", "write_file", `{"path":"a.txt"}`),
	}, gate)

	if tool.calls != 0 {
		t.Fatalf("denied tool still ran %d times", tool.calls)
	}
	if len(out) != 1 {
		t.Fatalf("a denied call must still produce a result for the model, got %d", len(out))
	}
	if out[0].Err == "" {
		t.Error("denied result has no error text")
	}
	// The model has to be told this was a permission matter rather than a tool
	// bug, otherwise it retries the same call instead of asking the user.
	if !strings.Contains(out[0].Result, "授权") || !strings.Contains(out[0].Result, "需要授权") {
		t.Errorf("denial result does not explain itself: %q", out[0].Result)
	}
}

// A failing tool is reported, not thrown: the model needs the text to recover.
func TestExecuteToolErrorBecomesResult(t *testing.T) {
	tool := &fakeTool{name: "flaky", err: errors.New("boom")}
	out := Execute(context.Background(), testRegistry(tool), []provider.ToolCall{
		call("c1", "flaky", `{}`),
	}, nil)

	if out[0].Err != "boom" {
		t.Errorf("err = %q, want boom", out[0].Err)
	}
	if out[0].Result != "error: boom" {
		t.Errorf("result = %q, want %q", out[0].Result, "error: boom")
	}
}

// An unknown tool name (a hallucinated one) must come back as an error result
// rather than a panic or a silent empty string.
func TestExecuteUnknownToolIsAnErrorResult(t *testing.T) {
	out := Execute(context.Background(), testRegistry(), []provider.ToolCall{
		call("c1", "does_not_exist", `{}`),
	}, nil)

	if len(out) != 1 || out[0].Err == "" {
		t.Fatalf("unknown tool produced %+v, want one error result", out)
	}
	if !strings.Contains(out[0].Result, "not found") {
		t.Errorf("result = %q, want it to say the tool is not found", out[0].Result)
	}
}

// Models emit broken JSON for arguments fairly often. The loop must survive it
// (call with empty args) instead of aborting the turn, and it must still run the
// *following* calls — one bad call cannot cancel the rest.
func TestExecuteSurvivesMalformedArgumentsAndKeepsGoing(t *testing.T) {
	first := &fakeTool{name: "a", result: "a-ok"}
	second := &fakeTool{name: "b", result: "b-ok"}

	out := Execute(context.Background(), testRegistry(first, second), []provider.ToolCall{
		call("c1", "a", `{"unterminated":`),
		call("c2", "b", `{"fine":true}`),
	}, nil)

	if len(out) != 2 {
		t.Fatalf("got %d results, want 2", len(out))
	}
	if first.calls != 1 {
		t.Fatalf("malformed arguments stopped the call from running")
	}
	if len(first.lastArg) != 0 {
		t.Errorf("malformed arguments should decode to empty args, got %v", first.lastArg)
	}
	if out[1].Result != "b-ok" {
		t.Errorf("second call did not run: %+v", out[1])
	}
}

// Order matters: results are matched back to calls positionally.
func TestExecuteKeepsResultOrder(t *testing.T) {
	ok := &fakeTool{name: "ok", result: "fine"}
	bad := &fakeTool{name: "bad", err: errors.New("nope")}

	out := Execute(context.Background(), testRegistry(ok, bad), []provider.ToolCall{
		call("c1", "bad", `{}`),
		call("c2", "ok", `{}`),
		call("c3", "bad", `{}`),
	}, nil)

	wantIDs := []string{"c1", "c2", "c3"}
	if len(out) != len(wantIDs) {
		t.Fatalf("got %d results, want %d", len(out), len(wantIDs))
	}
	for i, want := range wantIDs {
		if out[i].ID != want {
			t.Errorf("result %d has id %q, want %q", i, out[i].ID, want)
		}
	}
	if out[1].Err != "" || out[0].Err == "" || out[2].Err == "" {
		t.Errorf("a failure leaked into a neighbouring result: %+v", out)
	}
}

// No gate means "run everything" — the full-permission path.
func TestExecuteNilGateRunsEverything(t *testing.T) {
	tool := &fakeTool{name: "write_file", result: "ok"}
	Execute(context.Background(), testRegistry(tool), []provider.ToolCall{
		call("c1", "write_file", `{}`),
	}, nil)

	if tool.calls != 1 {
		t.Fatalf("nil gate blocked the call (%d runs)", tool.calls)
	}
}
