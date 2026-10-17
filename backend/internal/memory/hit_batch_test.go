package memory

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
)

// countingStore counts Save calls so a test can prove recall is not one commit
// per hit.
type countingStore struct {
	storage.Store
	saves atomic.Int32
}

func (c *countingStore) Save(id string, v interface{}) error {
	c.saves.Add(1)
	return c.Store.Save(id, v)
}

// Recall bumps hit counters, and it used to persist each of them immediately:
// every chat turn (recall runs on every turn) wrote to the store once per hit,
// all under the service write lock. The counters are ranking hints, so batching
// them is free; earning a commit per hit was not.
func TestRecallBatchesHitCounterWrites(t *testing.T) {
	dir := t.TempDir()
	counted := &countingStore{Store: storage.NewJSON(filepath.Join(dir, "mem"))}
	s := New(counted, storage.NewJSON(filepath.Join(dir, "idx")),
		filepath.Join(dir, "memory"), retrieval.New(), nil, false)
	for _, o := range []*Service{s} {
		// Batch on size rather than time so the test does not sleep for seconds.
		o.hitFlushEvery = time.Hour
		o.hitFlushBatch = 8
	}

	const n = 40
	for i := 0; i < n; i++ {
		if _, err := s.Add(&Memory{Level: LevelL1, Content: "用户偏好编号 " + itoa(i) + " 的回答方式"}); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	before := counted.saves.Load()

	const recalls = 30
	var hits int
	for i := 0; i < recalls; i++ {
		got, err := s.Recall("回答方式", 5, ScopeGlobal)
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		hits += len(got)
	}
	if hits == 0 {
		t.Fatal("precondition: recall returned nothing")
	}

	writes := counted.saves.Load() - before
	// One write per hit would be ≥ hits; batching keeps it to hits/batch + 1.
	if int(writes) >= hits {
		t.Errorf("recall persisted %d times for %d hits: still one commit per hit", writes, hits)
	}
	if writes == 0 {
		t.Error("hit counters were never persisted at all")
	}

	// The counters still reach the store, and a later recall sees them.
	if err := s.flushHitsForTest(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	anyBumped := false
	for _, m := range s.List(ListFilter{Level: LevelL1}) {
		if m.HitCount > 0 {
			anyBumped = true
			break
		}
	}
	if !anyBumped {
		t.Error("no memory ended up with a hit count")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// flushHitsForTest forces a flush regardless of the time window.
func (s *Service) flushHitsForTest() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.hitsDirty {
		if m, ok := s.cache[id]; ok {
			if err := s.entities.Save(m.ID, m); err != nil {
				return err
			}
		}
	}
	s.hitsDirty = map[string]struct{}{}
	s.lastHitFlush = time.Now()
	return nil
}
