package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/agenthub/agent-proxy/internal/provider"
)

// PlanStep is one planned step surfaced to the UI.
type PlanStep struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// planPrompt asks the model for a short, concrete plan before it starts acting.
// It is deliberately tool-agnostic: the plan is a map for the user, not a rigid
// script the model must follow, so steps are short and imperative.
const planPrompt = `你是任务规划助手。请把用户的目标拆成 3 到 6 个具体、可执行的步骤。
要求：
1. 每步用一句简短的中文动宾短语描述（不超过 20 字），例如「检查本机 Python 环境」「安装所需依赖」「编写并运行数据清洗脚本」。
2. 步骤要按实际执行顺序排列，能并行的可以并列。
3. 只输出 JSON，不要任何解释、不要 markdown 代码块。
4. JSON 格式：{"steps":["步骤一","步骤二","步骤三"]}`

// BuildPlan asks the model for a task plan. It is best-effort and bounded: any
// failure (timeout, unparseable output, provider error) returns nil so the main
// turn proceeds without a plan rather than blocking the user.
func (o *Orchestrator) BuildPlan(ctx context.Context, providerID, model, goal string) []PlanStep {
	rt, err := o.Providers.GetRuntime(providerID)
	if err != nil {
		return nil
	}
	planCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := rt.Chat(planCtx, []provider.Message{
		{Role: "system", Content: planPrompt},
		{Role: "user", Content: goal},
	}, provider.CallOptions{
		Model:       model,
		MaxTokens:   400,
		Temperature: 0.2,
		Stream:      false,
	})
	if err != nil || resp == nil {
		return nil
	}
	titles := parsePlanSteps(resp.Content)
	steps := make([]PlanStep, 0, len(titles))
	for i, t := range titles {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		steps = append(steps, PlanStep{ID: "s" + itoaPlan(i+1), Title: t})
	}
	return steps
}

// parsePlanSteps extracts step titles from a model response that may be a JSON
// object, a bare JSON array, or wrapped in a markdown code fence.
func parsePlanSteps(text string) []string {
	raw := strings.TrimSpace(text)
	// Strip a ```json ... ``` fence if present.
	if i := strings.Index(raw, "["); i >= 0 {
		j := strings.LastIndex(raw, "]")
		if j > i {
			var arr []string
			if err := json.Unmarshal([]byte(raw[i:j+1]), &arr); err == nil && len(arr) > 0 {
				return arr
			}
		}
	}
	var obj struct {
		Steps []string `json:"steps"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err == nil && len(obj.Steps) > 0 {
		return obj.Steps
	}
	return nil
}

func itoaPlan(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
