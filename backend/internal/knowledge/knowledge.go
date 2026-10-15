// Package knowledge implements the local knowledge base / RAG layer
// (DESIGN §5 knowledge / §6.4 / §7). Pipeline: documents are parsed → chunked →
// optionally embedded (BYOK) → indexed into the shared retrieval kernel, which
// fuses BM25 + vectors with RRF. Retrieval returns citations that the
// orchestrator injects into the prompt and surfaces to the UI.
//
// Storage follows §7.1: vectors live in the per-collection index file
// (knowledge/index/{collId}.json), never inside Doc JSON, so document files do
// not bloat as corpora grow.
package knowledge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agenthub/agent-proxy/internal/knowledge/chunk"
	"github.com/agenthub/agent-proxy/internal/knowledge/parse"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Index lifecycle states.
const (
	StatusPending  = "pending"
	StatusBuilding = "building"
	StatusReady    = "ready"
	StatusError    = "error"
)

// Trust levels (已核实 / 参考 / 未验证).
const (
	TrustVerified   = "verified"
	TrustReference  = "reference"
	TrustUnverified = "unverified"
)

// Default chunking parameters (DESIGN §6.4).
const (
	DefaultChunkSize    = 800
	DefaultChunkOverlap = 80
)

// Collection is a knowledge base (§7 knowledge/collections/{id}.json).
type Collection struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Description         string `json:"description,omitempty"`
	EmbeddingProviderId string `json:"embeddingProviderId,omitempty"`
	EmbeddingModel      string `json:"embeddingModel,omitempty"`
	EmbeddingDim        int    `json:"embeddingDim,omitempty"`
	ChunkSize           int    `json:"chunkSize"`
	ChunkOverlap        int    `json:"chunkOverlap"`
	DocCount            int    `json:"docCount"`
	ChunkCount          int    `json:"chunkCount"`
	IndexStatus         string `json:"indexStatus"`
	CreatedAt           int64  `json:"createdAt"`
	UpdatedAt           int64  `json:"updatedAt"`
}

// Chunk is one retrievable unit (§7 Doc.chunks[]).
type Chunk struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
	Title string `json:"title,omitempty"`
	Page  int    `json:"page,omitempty"`
	Hash  string `json:"hash"`
}

// Doc is an uploaded document (§7 knowledge/docs/{collId}/{docId}.json).
// Embeddings are intentionally absent here — see §7.1.
type Doc struct {
	ID           string `json:"id"`
	CollectionID string `json:"collectionId"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	PageCount    int    `json:"pageCount"`
	Version      int    `json:"version"`
	Trust        string `json:"trust"`
	ChunkCount   int    `json:"chunkCount"`
	IndexStatus  string `json:"indexStatus"`
	Error        string `json:"error,omitempty"`
	// Warning records a non-fatal degradation — most importantly, vectorisation
	// falling back to BM25-only. Ingestion still reports StatusReady, so without
	// this the operator has no way to tell that semantic recall is switched off
	// on a document they believed was fully indexed.
	Warning   string  `json:"warning,omitempty"`
	Chunks    []Chunk `json:"chunks,omitempty"`
	CreatedAt int64   `json:"createdAt"`
	UpdatedAt int64   `json:"updatedAt"`
}

// IndexRecord is the authoritative vector/text record for one chunk, persisted
// in knowledge/index/{collId}.json (§7.1).
type IndexRecord struct {
	ChunkID   string    `json:"chunkId"` // "<docId>#<index>"
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding,omitempty"`
}

// Citation is a retrieval hit surfaced to the model and UI (§15.1 context event).
type Citation struct {
	Collection string  `json:"collection"`
	Doc        string  `json:"doc"`
	DocName    string  `json:"docName,omitempty"`
	Chunk      int     `json:"chunk"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	Trust      string  `json:"trust"`
}

// Service manages collections, documents and the retrieval index.
type Service struct {
	collections storage.Store
	indexStore  storage.Store
	baseDir     string
	dataDir     string // root data dir, used to open per-collection stores
	driver      string // persistence driver (json | sqlite)
	retrieval   *retrieval.Service
	providers   *provider.Service
	mu          sync.RWMutex
	cols        map[string]*Collection
}

// New constructs the knowledge Service and rebuilds in-memory indexes.
func New(colStore storage.Store, idxStore storage.Store, baseDir, dataDir, driver string,
	retr *retrieval.Service, prov *provider.Service) *Service {
	s := &Service{
		collections: colStore,
		indexStore:  idxStore,
		baseDir:     baseDir,
		dataDir:     dataDir,
		driver:      driver,
		retrieval:   retr,
		providers:   prov,
		cols:        map[string]*Collection{},
	}
	s.load()
	return s
}

func (s *Service) load() {
	ids, err := s.collections.IDs()
	if err != nil {
		return
	}
	for _, id := range ids {
		var c Collection
		if err := s.collections.Load(id, &c); err != nil {
			continue
		}
		s.cols[id] = &c
		// rebuild the retrieval index from the persisted vectors
		var recs []retrieval.Record
		var persisted []IndexRecord
		if err := s.indexStore.Load(id, &persisted); err == nil {
			for _, r := range persisted {
				recs = append(recs, retrieval.Record{ID: r.ChunkID, Text: r.Text, Embedding: r.Embedding})
			}
		}
		_ = s.retrieval.BuildIndex(ns(id), recs)
	}
}

// docStore returns the per-collection document store (created on demand).
func (s *Service) docStore(collID string) storage.Store {
	dir := filepath.Join(s.baseDir, "docs", collID)
	st, err := storage.Open(s.dataDir, "knowledge/docs/"+collID, dir, s.driver)
	if err != nil {
		// Open already falls back to JSON; surface the warning once.
		fmt.Fprintf(os.Stderr, "knowledge: %v\n", err)
	}
	return st
}

func ns(collID string) string { return "kb:" + collID }

// KindFromName infers the document kind (md/txt/html/csv/docx/pdf) from a
// filename; unknown extensions fall back to txt.
func KindFromName(name string) string { return parse.KindFromName(name) }

// ---- collections ----

// List returns all collections.
func (s *Service) List() []*Collection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Collection, 0, len(s.cols))
	for _, c := range s.cols {
		out = append(out, c)
	}
	return out
}

// Get returns a collection by id.
func (s *Service) Get(id string) (*Collection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cols[id]
	if !ok {
		return nil, fmt.Errorf("collection %s not found", id)
	}
	return c, nil
}

// Create persists a new collection, applying chunking defaults.
func (s *Service) Create(c *Collection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == "" {
		c.ID = util.NewID()
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = DefaultChunkSize
	}
	if c.ChunkOverlap < 0 {
		c.ChunkOverlap = DefaultChunkOverlap
	}
	now := time.Now().UnixMilli()
	c.CreatedAt, c.UpdatedAt = now, now
	c.IndexStatus = StatusReady
	if err := s.collections.Save(c.ID, c); err != nil {
		return err
	}
	s.cols[c.ID] = c
	return nil
}

// Delete removes a collection, its documents and its index.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.RemoveAll(filepath.Join(s.baseDir, "docs", id))
	_ = s.indexStore.Remove(id)
	if s.retrieval != nil {
		_ = s.retrieval.Remove(ns(id), id)
	}
	_ = s.collections.Remove(id)
	delete(s.cols, id)
	return nil
}

// ---- documents ----

// CreateDoc stores an uploaded document as pending ingestion.
func (s *Service) CreateDoc(collID, name string, kind string, data []byte) (*Doc, error) {
	if _, err := s.Get(collID); err != nil {
		return nil, err
	}
	if kind == "" {
		kind = parse.KindFromName(name)
	}
	now := time.Now().UnixMilli()
	d := &Doc{
		ID:           util.NewID(),
		CollectionID: collID,
		Name:         name,
		Type:         kind,
		Size:         int64(len(data)),
		Version:      1,
		Trust:        TrustUnverified,
		IndexStatus:  StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.docStore(collID).Save(d.ID, d); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if c, ok := s.cols[collID]; ok {
		c.DocCount++
		c.UpdatedAt = now
		_ = s.collections.Save(collID, c)
	}
	s.mu.Unlock()
	return d, nil
}

// Ingest runs the parse → chunk → embed → index pipeline for a document.
// progress is called with a 0-100 percentage and a stage message; it may be nil.
func (s *Service) Ingest(ctx context.Context, collID string, d *Doc, data []byte, progress func(int, string)) error {
	report := func(p int, m string) {
		if progress != nil {
			progress(p, m)
		}
	}
	col, err := s.Get(collID)
	if err != nil {
		return err
	}
	ds := s.docStore(collID)
	mark := func(status, errMsg string) {
		d.IndexStatus = status
		d.Error = errMsg
		d.UpdatedAt = time.Now().UnixMilli()
		_ = ds.Save(d.ID, d)
	}

	report(5, "解析文档")
	res, err := parse.Parse(d.Type, data)
	if err != nil {
		mark(StatusError, err.Error())
		return err
	}
	d.PageCount = res.PageCount

	report(30, "分块")
	chunks := chunk.Split(res.Text, chunk.Options{Size: col.ChunkSize, Overlap: col.ChunkOverlap})
	if len(chunks) == 0 {
		e := fmt.Errorf("分块结果为空")
		mark(StatusError, e.Error())
		return e
	}

	report(45, "向量化")
	var embs [][]float32
	var embedWarning string
	if col.EmbeddingProviderId != "" && col.EmbeddingModel != "" && s.providers != nil {
		texts := make([]string, 0, len(chunks))
		for _, c := range chunks {
			texts = append(texts, c.Text)
		}
		if rt, e := s.providers.GetRuntime(col.EmbeddingProviderId); e == nil {
			ctx2, cancel := context.WithTimeout(ctx, 120*time.Second)
			embs, err = rt.Embeddings(ctx2, col.EmbeddingModel, texts)
			cancel()
			if err != nil {
				// embedding failure is non-fatal: BM25 still answers (§6.4 兜底).
				// The reason is surfaced on the document, otherwise the collection
				// silently runs keyword-only and the operator cannot tell why
				// semantic recall feels weak.
				embs = nil
				embedWarning = "向量化失败，已降级为 BM25 关键词检索（语义召回未启用）：" + err.Error()
				err = nil
			}
			report(80, "写入向量索引")
		} else {
			embedWarning = "无法获取 embedding 服务商运行时，已降级为 BM25 关键词检索：" + e.Error()
		}
	}

	// build chunk records + index records
	cs := make([]Chunk, 0, len(chunks))
	recs := make([]IndexRecord, 0, len(chunks))
	for i, c := range chunks {
		cid := fmt.Sprintf("%s#%d", d.ID, i)
		cs = append(cs, Chunk{Index: c.Index, Text: c.Text, Title: c.Title, Hash: c.Hash})
		r := IndexRecord{ChunkID: cid, Text: c.Text}
		if i < len(embs) {
			r.Embedding = embs[i]
			if col.EmbeddingDim == 0 {
				col.EmbeddingDim = len(embs[i])
			}
		}
		recs = append(recs, r)
	}

	report(90, "构建索引")
	// merge with existing index records of other docs in this collection
	var all []IndexRecord
	if existing, e := s.loadIndex(collID); e == nil {
		prefix := d.ID + "#"
		for _, r := range existing {
			if !strings.HasPrefix(r.ChunkID, prefix) {
				all = append(all, r)
			}
		}
	}
	all = append(all, recs...)
	if err := s.indexStore.Save(collID, all); err != nil {
		mark(StatusError, err.Error())
		return err
	}

	var rr []retrieval.Record
	for _, r := range all {
		rr = append(rr, retrieval.Record{ID: r.ChunkID, Text: r.Text, Embedding: r.Embedding})
	}
	_ = s.retrieval.BuildIndex(ns(collID), rr)

	d.Chunks = cs
	d.ChunkCount = len(cs)
	d.IndexStatus = StatusReady
	d.Error = ""
	d.Warning = embedWarning
	d.UpdatedAt = time.Now().UnixMilli()
	if err := ds.Save(d.ID, d); err != nil {
		return err
	}

	s.mu.Lock()
	if c, ok := s.cols[collID]; ok {
		c.ChunkCount = s.countChunks(collID)
		c.IndexStatus = StatusReady
		c.EmbeddingDim = col.EmbeddingDim
		c.UpdatedAt = time.Now().UnixMilli()
		_ = s.collections.Save(collID, c)
	}
	s.mu.Unlock()

	report(100, "完成")
	return nil
}

func (s *Service) loadIndex(collID string) ([]IndexRecord, error) {
	var out []IndexRecord
	err := s.indexStore.Load(collID, &out)
	return out, err
}

func (s *Service) countChunks(collID string) int {
	recs, err := s.loadIndex(collID)
	if err != nil {
		return 0
	}
	return len(recs)
}

// ListDocs returns all documents in a collection.
func (s *Service) ListDocs(collID string) []*Doc {
	ds := s.docStore(collID)
	ids, err := ds.IDs()
	if err != nil {
		return nil
	}
	out := make([]*Doc, 0, len(ids))
	for _, id := range ids {
		var d Doc
		if ds.Load(id, &d) == nil {
			out = append(out, &d)
		}
	}
	return out
}

// GetDoc returns one document.
func (s *Service) GetDoc(collID, docID string) (*Doc, error) {
	var d Doc
	if err := s.docStore(collID).Load(docID, &d); err != nil {
		return nil, fmt.Errorf("document %s not found", docID)
	}
	return &d, nil
}

// DeleteDoc removes a document and its chunks from the index.
func (s *Service) DeleteDoc(collID, docID string) error {
	ds := s.docStore(collID)
	if existing, err := s.loadIndex(collID); err == nil {
		prefix := docID + "#"
		kept := existing[:0]
		for _, r := range existing {
			if !strings.HasPrefix(r.ChunkID, prefix) {
				kept = append(kept, r)
			}
		}
		_ = s.indexStore.Save(collID, kept)
		var rr []retrieval.Record
		for _, r := range kept {
			rr = append(rr, retrieval.Record{ID: r.ChunkID, Text: r.Text, Embedding: r.Embedding})
		}
		_ = s.retrieval.BuildIndex(ns(collID), rr)
	}
	if err := ds.Remove(docID); err != nil {
		return err
	}
	s.mu.Lock()
	if c, ok := s.cols[collID]; ok {
		if c.DocCount > 0 {
			c.DocCount--
		}
		c.ChunkCount = s.countChunks(collID)
		c.UpdatedAt = time.Now().UnixMilli()
		_ = s.collections.Save(collID, c)
	}
	s.mu.Unlock()
	return nil
}

// ---- retrieval ----

// Search runs dual-path retrieval (BM25 + vectors fused by RRF) over a
// collection, returning citations. Falls back to BM25-only when no embedding
// model is configured (DESIGN §6.4).
func (s *Service) Search(collID, query string, topK int) []Citation {
	if s.retrieval == nil || query == "" {
		return nil
	}
	col, err := s.Get(collID)
	if err != nil {
		return nil
	}
	bm := s.retrieval.SearchBM25(ns(collID), query, topK)
	var vec []retrieval.Hit
	if col.EmbeddingProviderId != "" && col.EmbeddingModel != "" && s.providers != nil {
		if rt, e := s.providers.GetRuntime(col.EmbeddingProviderId); e == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			embs, ee := rt.Embeddings(ctx, col.EmbeddingModel, []string{query})
			cancel()
			if ee == nil && len(embs) > 0 {
				vec = s.retrieval.SearchVector(ns(collID), embs[0], topK)
			}
		}
	}
	fused := s.retrieval.RRF(bm, vec, topK)
	return s.toCitations(collID, fused)
}

// SearchCollections searches several collections and merges the best hits.
func (s *Service) SearchCollections(ids []string, query string, topK int) []Citation {
	var all []Citation
	for _, id := range ids {
		all = append(all, s.Search(id, query, topK)...)
	}
	// keep the strongest hits across collections
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].Score > all[j-1].Score; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if topK > 0 && len(all) > topK {
		all = all[:topK]
	}
	return all
}

func (s *Service) toCitations(collID string, hits []retrieval.Hit) []Citation {
	out := make([]Citation, 0, len(hits))
	ds := s.docStore(collID)
	nameCache := map[string]string{}
	trustCache := map[string]string{}
	for _, h := range hits {
		docID, idx := splitChunkID(h.ID)
		name, ok := nameCache[docID]
		trust := trustCache[docID]
		if !ok {
			var d Doc
			if ds.Load(docID, &d) == nil {
				name = d.Name
				trust = d.Trust
			} else {
				name, trust = docID, TrustUnverified
			}
			nameCache[docID] = name
			trustCache[docID] = trust
		}
		out = append(out, Citation{
			Collection: collID,
			Doc:        docID,
			DocName:    name,
			Chunk:      idx,
			Text:       h.Text,
			Score:      h.Score,
			Trust:      trust,
		})
	}
	return out
}

func splitChunkID(id string) (string, int) {
	i := strings.LastIndex(id, "#")
	if i < 0 {
		return id, 0
	}
	n := 0
	for _, r := range id[i+1:] {
		n = n*10 + int(r-'0')
	}
	return id[:i], n
}
