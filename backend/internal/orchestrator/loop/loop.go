// Package loop runs the tool-execution cycle: it turns model-issued tool calls
// into results via the registry (DESIGN §5 orchestrator/loop).
package loop

import (
	"context"
	"encoding/json"

	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
)

// Result is one executed tool call's outcome.
type Result struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Result string `json:"result"`
	Err    string `json:"err,omitempty"`
}

// Gate decides whether a tool call may run. Returning an error blocks the call
// and feeds the reason back to the model as the tool result, so a denied call
// never silently succeeds.
type Gate func(name string, args map[string]interface{}) error

// Execute runs every tool call and returns its results (failures are captured,
// never thrown — the model sees the error text and can recover).
//
// gate may be nil (run everything). When set, every call is checked first and
// denied calls are returned as results rather than executed — this is how the
// permission levels (ask/auto/full) are enforced.
func Execute(ctx context.Context, reg *registry.Registry, calls []provider.ToolCall, gate Gate) []Result {
	out := make([]Result, 0, len(calls))
	for _, c := range calls {
		r := Result{ID: c.ID, Name: c.Name}
		args := map[string]interface{}{}
		if c.Arguments != "" {
			_ = json.Unmarshal([]byte(c.Arguments), &args)
		}
		if gate != nil {
			if err := gate(c.Name, args); err != nil {
				r.Err = err.Error()
				r.Result = "error: 需要用户授权 — " + err.Error()
				out = append(out, r)
				continue
			}
		}
		res, err := reg.Execute(ctx, c.Name, args)
		if err != nil {
			r.Err = err.Error()
			r.Result = "error: " + err.Error()
		} else {
			r.Result = res
		}
		out = append(out, r)
	}
	return out
}
