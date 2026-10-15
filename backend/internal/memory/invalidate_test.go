package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
)

// 失效 is the reversible counterpart to deleting a memory: the record stays so
// the user can see what was dropped, but it must stop being recalled at once.
// "At once" is the part that is easy to get wrong — an invalid_at set to now
// used to fall through a strict < comparison and stay live for another tick.
func TestInvalidateStopsRecallImmediately(t *testing.T) {
	s := newTestService(t)
	m, err := s.Add(&Memory{Level: LevelL1, Scope: ScopeGlobal, Content: "用户偏好简洁的回答"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	if hits, _ := s.Recall("简洁", 5, ScopeGlobal); len(hits) == 0 {
		t.Fatal("precondition: the memory should be recallable before invalidation")
	}

	if err := s.Invalidate(m.ID); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if hits, _ := s.Recall("简洁", 5, ScopeGlobal); len(hits) != 0 {
		t.Fatalf("an invalidated memory came back from recall: %+v", hits)
	}

	// Still readable: the point is to stop using it, not to destroy it.
	got, err := s.Get(m.ID)
	if err != nil {
		t.Fatalf("an invalidated memory should still be readable: %v", err)
	}
	if got.InvalidAt == 0 {
		t.Fatal("invalid_at was not set")
	}
}

// recallable is the single predicate behind every recall path, so the boundary
// cases are worth pinning: the invalidated entry must be gone at exactly its
// invalid_at, and a future-dated one must still be usable until then.
func TestRecallableBoundary(t *testing.T) {
	now := time.Now().UnixMilli()
	cases := []struct {
		name string
		m    Memory
		want bool
	}{
		{"plain", Memory{Enabled: true}, true},
		{"disabled", Memory{Enabled: false}, false},
		{"superseded", Memory{Enabled: true, SupersededBy: "other"}, false},
		{"invalidated this instant", Memory{Enabled: true, InvalidAt: now}, false},
		{"invalidated earlier", Memory{Enabled: true, InvalidAt: now - 1}, false},
		{"expires in a minute", Memory{Enabled: true, InvalidAt: now + 60_000}, true},
	}
	for _, c := range cases {
		if got := c.m.recallable(now); got != c.want {
			t.Errorf("%s: recallable = %v, want %v", c.name, got, c.want)
		}
	}
}

// Deleting is irreversible and the store is local, so without an audit line
// there is no way afterwards to tell "the assistant forgot something" from a
// bug. The log must also stay desensitised: it answers who/when, and copying
// the memory content into a second plaintext file would defeat the point.
func TestDestructiveMemoryOpsAreAudited(t *testing.T) {
	root := t.TempDir()
	s := New(storage.NewJSON(root+"/memories"), storage.NewJSON(root+"/index"),
		root+"/memory", retrieval.New(), nil, false)
	s.SetAudit(audit.New(root))

	const secret = "用户的银行密码在抽屉里"
	m, err := s.Add(&Memory{Level: LevelL1, Scope: ScopeGlobal, Content: secret})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.Invalidate(m.ID); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if err := s.Delete(m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(root, "audit.log"))
	if err != nil {
		t.Fatalf("audit log missing: %v", err)
	}
	log := string(b)
	for _, want := range []string{"memory.invalidate", "memory.delete", m.ID} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log is missing %q\ngot: %s", want, log)
		}
	}
	if strings.Contains(log, secret) {
		t.Fatalf("audit log leaked the memory content: %s", log)
	}
}
