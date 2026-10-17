package handler

import (
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/job"
	"github.com/agenthub/agent-proxy/internal/memory"
	"github.com/agenthub/agent-proxy/internal/memory/consolidate"
)

// The L1→L2→L3 pipeline had exactly one trigger: the manual endpoint. Nothing
// scheduled it, so "自动巩固" never happened — L1 accumulated and the scenes and
// persona never refreshed. These cases pin the scheduler's decision, which is the
// part that has to be right: it decides when to spend LLM calls.
func TestConsolidationDue(t *testing.T) {
	now := time.Now().UnixMilli()
	l1 := func(n int, base int64) []*memory.Memory {
		out := make([]*memory.Memory, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, &memory.Memory{
				ID: "m" + string(rune('a'+i)), Level: memory.LevelL1,
				Enabled: true, CreatedAt: base + int64(i),
			})
		}
		return out
	}

	cases := []struct {
		name   string
		l1     []*memory.Memory
		jobs   []job.JobStatus
		minNew int
		want   bool
	}{
		{
			name: "below the batch threshold",
			l1:   l1(10, now-1000),
			want: false,
		},
		{
			name: "never consolidated, enough material",
			l1:   l1(25, now-1000),
			want: true,
		},
		{
			name: "consolidated after everything existing",
			l1:   l1(25, now-10_000),
			jobs: []job.JobStatus{{Type: consolidate.TypeConsolidate, Status: "done", UpdatedAt: now - 1000}},
			want: false,
		},
		{
			name: "enough new facts since the last run",
			// 25 facts created after the last successful run.
			l1:   l1(25, now-500),
			jobs: []job.JobStatus{{Type: consolidate.TypeConsolidate, Status: "done", UpdatedAt: now - 1000}},
			want: true,
		},
		{
			name: "a failed run does not count as a baseline",
			l1:   l1(25, now-500),
			jobs: []job.JobStatus{{Type: consolidate.TypeConsolidate, Status: "failed", UpdatedAt: now - 100}},
			want: true,
		},
		{
			name: "too few facts overall for the pipeline",
			l1:   l1(2, now-1000),
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := consolidationDue(c.l1, c.jobs, c.minNew); got != c.want {
				t.Errorf("consolidationDue = %v, want %v", got, c.want)
			}
		})
	}
}

// Disabled and superseded entries must not count as material: they are invisible
// to consolidation, so counting them would trigger runs with nothing to fold in.
func TestConsolidationDueIgnoresRetiredEntries(t *testing.T) {
	now := time.Now().UnixMilli()
	var l1 []*memory.Memory
	for i := 0; i < 20; i++ {
		l1 = append(l1, &memory.Memory{ID: "on", Level: memory.LevelL1, Enabled: true, CreatedAt: now})
	}
	for i := 0; i < 20; i++ {
		l1 = append(l1, &memory.Memory{ID: "off", Level: memory.LevelL1, Enabled: false, CreatedAt: now})
		l1 = append(l1, &memory.Memory{ID: "old", Level: memory.LevelL1, Enabled: true, SupersededBy: "x", CreatedAt: now})
	}
	// 20 live entries is exactly minNew, so a scheduler that counted the 40 retired
	// ones would pass a higher threshold incorrectly.
	if !consolidationDue(l1, nil, 20) {
		t.Error("live entries were not counted")
	}
	if consolidationDue(l1, nil, 60) {
		t.Error("retired entries were counted as material")
	}
}
