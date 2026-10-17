// Package retrieval is the shared retrieval kernel used by knowledge and memory
// (DESIGN §5 retrieval / §5.2). It owns no business data — callers hand it
// records {id, text, embedding} and a namespace; it builds a BM25 (2-gram)
// inverted index plus a vector index and fuses them with RRF. Token estimation
// and fair-split budgeting are fixed here so every consumer shares one impl.
package retrieval

import (
	"sort"
	"sync"

	"github.com/agenthub/agent-proxy/internal/retrieval/bm25"
	"github.com/agenthub/agent-proxy/internal/retrieval/rrf"
	"github.com/agenthub/agent-proxy/internal/retrieval/token"
	"github.com/agenthub/agent-proxy/internal/retrieval/vector"
)

// Record is one indexed item.
type Record struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding"`
}

// Hit is a retrieval result.
type Hit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Text  string  `json:"text"`
}

// Item is a budgetable unit for SplitFairly.
type Item struct {
	ID     string      `json:"id"`
	Text   string      `json:"text"`
	Tokens int         `json:"tokens"`
	Data   interface{} `json:"-"`
}

// Retrieval is the shared retrieval contract (DESIGN §5.2).
type Retrieval interface {
	BuildIndex(ns string, records []Record) error
	SearchBM25(ns, query string, topK int) []Hit
	SearchVector(ns string, queryEmbedding []float32, topK int) []Hit
	RRF(bm25, vec []Hit, topK int) []Hit
	Estimate(text string) int
	SplitFairly(items []Item, budget int) []Item
	Refresh(ns, id string, text string, embedding []float32) error
	Remove(ns, id string) error
}

type index struct {
	records map[string]Record
	ids     []string
	embs    map[string][]float32
	inv     *bm25.Inverted
}

// Service implements Retrieval with in-memory indexes (rebuilt from records on
// startup by callers; M2 keeps indexes in memory, see DESIGN §7.1 escape hatch).
type Service struct {
	mu      sync.RWMutex
	indexes map[string]*index
}

// New constructs an empty retrieval Service.
func New() *Service {
	return &Service{indexes: map[string]*index{}}
}

// BuildIndex (re)builds the index for a namespace.
func (s *Service) BuildIndex(ns string, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := &index{records: map[string]Record{}, embs: map[string][]float32{}}
	for _, r := range records {
		idx.records[r.ID] = r
		idx.embs[r.ID] = r.Embedding
	}
	idx.rebuild()
	s.indexes[ns] = idx
	return nil
}

func (idx *index) rebuild() {
	ids := make([]string, 0, len(idx.records))
	texts := make([]string, 0, len(idx.records))
	for id, r := range idx.records {
		ids = append(ids, id)
		texts = append(texts, r.Text)
	}
	idx.ids = ids
	idx.inv = bm25.Build(ids, texts)
}

// SearchBM25 runs keyword search over the BM25 index.
//
// The read lock is held for the whole search, not just the map lookup: Refresh
// and Remove rewrite the index in place under the write lock (records, ids, inv),
// and toHits reads those fields. Dropping the lock early let a chat turn's recall
// run concurrently with an ingest or consolidation job's Refresh — Go treats a
// concurrent map read and map write as unrecoverable, so one overlap killed the
// process. Searches are short; serialising them against a rare rebuild is cheap.
func (s *Service) SearchBM25(ns, query string, topK int) []Hit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := s.indexes[ns]
	if idx == nil {
		return nil
	}
	scored := idx.inv.Search(query, topK)
	list := make([]idScore, 0, len(scored))
	for _, sc := range scored {
		list = append(list, idScore{ID: sc.ID, Score: sc.Score})
	}
	return s.toHits(idx, list)
}

// SearchVector runs semantic search over the embedding index (same locking
// rule as SearchBM25: the index it reads is mutated in place by writers).
func (s *Service) SearchVector(ns string, queryEmbedding []float32, topK int) []Hit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := s.indexes[ns]
	if idx == nil {
		return nil
	}
	scored := vector.Search(queryEmbedding, idx.ids, idx.embs, topK)
	list := make([]idScore, 0, len(scored))
	for _, sc := range scored {
		list = append(list, idScore{ID: sc.ID, Score: sc.Score})
	}
	return s.toHits(idx, list)
}

// RRF fuses BM25 and vector hits via Reciprocal Rank Fusion (k=60).
func (s *Service) RRF(bm25Hits, vecHits []Hit, topK int) []Hit {
	var bScores, vScores []rrf.Scored
	textOf := map[string]string{}
	for _, h := range bm25Hits {
		bScores = append(bScores, rrf.Scored{ID: h.ID, Score: h.Score})
		textOf[h.ID] = h.Text
	}
	for _, h := range vecHits {
		vScores = append(vScores, rrf.Scored{ID: h.ID, Score: h.Score})
		textOf[h.ID] = h.Text
	}
	fused := rrf.Fuse(60, bScores, vScores)
	if topK > 0 && len(fused) > topK {
		fused = fused[:topK]
	}
	out := make([]Hit, 0, len(fused))
	for _, f := range fused {
		out = append(out, Hit{ID: f.ID, Score: f.Score, Text: textOf[f.ID]})
	}
	return out
}

type idScore struct {
	ID    string
	Score float64
}

func (s *Service) toHits(idx *index, list []idScore) []Hit {
	out := make([]Hit, 0, len(list))
	for _, sc := range list {
		out = append(out, Hit{ID: sc.ID, Score: sc.Score, Text: idx.records[sc.ID].Text})
	}
	return out
}

// Estimate returns the token estimate for text (fixed heuristic).
func (s *Service) Estimate(text string) int {
	return token.Estimate(text)
}

// SplitFairly keeps whole items until the token budget is exceeded (never
// splits an item, DESIGN §5.2).
func (s *Service) SplitFairly(items []Item, budget int) []Item {
	var out []Item
	used := 0
	for _, it := range items {
		if used+it.Tokens > budget && len(out) > 0 {
			break
		}
		out = append(out, it)
		used += it.Tokens
	}
	return out
}

// Refresh updates one record and rebuilds the namespace index.
func (s *Service) Refresh(ns, id string, text string, embedding []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexes[ns]
	if idx == nil {
		idx = &index{records: map[string]Record{}, embs: map[string][]float32{}}
		s.indexes[ns] = idx
	}
	idx.records[id] = Record{ID: id, Text: text, Embedding: embedding}
	idx.embs[id] = embedding
	idx.rebuild()
	return nil
}

// Remove deletes one record and rebuilds the namespace index.
func (s *Service) Remove(ns, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexes[ns]
	if idx == nil {
		return nil
	}
	delete(idx.records, id)
	delete(idx.embs, id)
	idx.rebuild()
	return nil
}

// Namespaces lists the indexed namespaces.
func (s *Service) Namespaces() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.indexes))
	for ns := range s.indexes {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}
