// Package loop runs the tool-execution cycle: it turns model-issued tool calls
// into results via the registry (DESIGN §5 orchestrator/loop).
package loop

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agenthub/agent-proxy/internal/logx"
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
				logx.Warnf("tool %s 被权限拦截: %v", c.Name, err)
				out = append(out, r)
				continue
			}
		}
		start := time.Now()
		res, err := reg.Execute(ctx, c.Name, args)
		dur := time.Since(start).Round(time.Millisecond)
		if err != nil {
			r.Err = err.Error()
			r.Result = "error: " + err.Error()
			logx.Warnf("tool %s 失败 (%s): %v", c.Name, dur, err)
		} else {
			r.Result = res
			logx.Infof("tool %s 完成 (%s), 输出 %d 字符", c.Name, dur, len(res))
		}
		out = append(out, r)
	}
	return out
}
