package methodology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/storage"
)

// Standing rules are injected into every single turn, so the library needs a
// ceiling even though it is local and the user owns it. Past a point the rules
// crowd out the question the user actually asked, and the symptom looks like
// "the assistant stopped listening" rather than "I wrote forty rules".
func TestStandingRuleBudget(t *testing.T) {
	s := newTestService(t)
	for i := 0; i < MaxStandingEntries+5; i++ {
		err := s.Create(&Methodology{Name: "常驻", Content: "内容", Always: true})
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), "常驻原则最多") {
			t.Fatalf("unexpected error once the budget filled up: %v", err)
		}
		if got := s.standingCount(""); got > MaxStandingEntries {
			t.Fatalf("standing rules exceeded the cap: %d > %d", got, MaxStandingEntries)
		}
		return
	}
	t.Fatalf("the standing budget never triggered (cap %d)", MaxStandingEntries)
}

// The cap counts runes, not bytes. A byte cap silently gives a Chinese entry
// roughly a third of the room an English one gets, which is invisible until a
// user's legitimate long entry is refused for no reason they can see.
func TestContentLengthCapCountsRunes(t *testing.T) {
	s := newTestService(t)
	tooLong := strings.Repeat("规", MaxContentRunes+1)
	if err := s.Create(&Methodology{Name: "长", Content: tooLong}); err == nil ||
		!strings.Contains(err.Error(), "超过单条上限") {
		t.Fatalf("oversized entry was accepted: %v", err)
	}
	atLimit := strings.Repeat("规", MaxContentRunes)
	if err := s.Create(&Methodology{Name: "刚好", Content: atLimit}); err != nil {
		t.Fatalf("an entry exactly at the limit was rejected: %v", err)
	}
}

// Learn() stores distilled rules with Status already set and Enabled false, and
// Create's "new entry" default must not switch them on. Otherwise text the app
// did not author goes straight into the system prompt without anyone reading
// it — the one guardrail standing between a poisoned document and a permanent
// instruction.
func TestCreateDoesNotSwitchOnAProposedEntry(t *testing.T) {
	s := newTestService(t)
	m := &Methodology{Name: "蒸馏出来的一条", Content: "内容", Status: StatusActive}
	if err := s.Create(m); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.Get(m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Enabled {
		t.Fatal("a deliberately disabled entry was switched on by Create")
	}
}

// Rules steer every answer, so "why does it behave like this now" has to be
// answerable after the fact. The log stays desensitised: it records who changed
// what and when, not the rule text.
func TestRuleMutationsAreAudited(t *testing.T) {
	dir := t.TempDir()
	s := New(storage.NewJSON(dir))
	s.SetAudit(audit.New(dir))

	m := &Methodology{Name: "临时规则", Content: "回答前先确认单位"}
	if err := s.Create(m); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Toggle(m.ID); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	if err := s.Delete(m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("audit log missing: %v", err)
	}
	log := string(b)
	for _, want := range []string{"methodology.create", "methodology.toggle", "methodology.delete", m.ID} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log is missing %q\ngot: %s", want, log)
		}
	}
	if strings.Contains(log, "回答前先确认单位") {
		t.Fatalf("audit log leaked the rule text: %s", log)
	}
}
