package retrieval

import (
	"fmt"
	"sync"
	"testing"
)

// Every chat turn calls SearchBM25 (memory recall + knowledge search) while
// ingest and consolidation jobs call Refresh from their own goroutines. Search
// used to release the lock after looking the index up and then read its records,
// ids and inverted index — so one overlap with a rebuild was a Go fatal
// "concurrent map read and map write", which takes the whole process down rather
// than failing the request.
//
// Run with -race (the suite does) to make this deterministic; without it the test
// still exercises the same windows.
func TestSearchConcurrentWithRefreshIsSafe(t *testing.T) {
	s := New()
	for i := 0; i < 64; i++ {
		if err := s.Refresh("ns", fmt.Sprintf("seed-%d", i), fmt.Sprintf("初始文本 %d", i), nil); err != nil {
			t.Fatalf("seed refresh: %v", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Reader: the chat-turn side.
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			hits := s.SearchBM25("ns", "初始文本", 5)
			if len(hits) == 0 {
				t.Errorf("search returned nothing while the index was being rebuilt")
				return
			}
		}
	}()

	// Writer: an ingest/consolidation job updating and removing records.
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			id := fmt.Sprintf("job-%d", i)
			if err := s.Refresh("ns", id, "新增文本 "+id, nil); err != nil {
				t.Errorf("refresh: %v", err)
				return
			}
			if err := s.Remove("ns", id); err != nil {
				t.Errorf("remove: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// A namespace that was never indexed must return nothing rather than block on the
// lock or panic.
func TestSearchOnUnknownNamespace(t *testing.T) {
	s := New()
	if hits := s.SearchBM25("nope", "任何", 5); len(hits) != 0 {
		t.Errorf("got %d hits for an unknown namespace", len(hits))
	}
	if hits := s.SearchVector("nope", []float32{1, 0}, 5); len(hits) != 0 {
		t.Errorf("got %d hits for an unknown namespace", len(hits))
	}
}
