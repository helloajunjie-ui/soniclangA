// Package bm25 implements a dependency-free BM25 index using 2-gram tokenization
// for CJK (zero word-segmentation, DESIGN §5.2 / §2.4). It returns id→score
// pairs; the retrieval package assembles Hits.
package bm25

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Scored is an id with its relevance score.
type Scored struct {
	ID    string
	Score float64
}

const k1 = 1.5
const b = 0.75

// Inverted is a BM25 inverted index.
type Inverted struct {
	df     map[string]int
	tf     map[string]map[string]int
	docLen map[string]int
	n      int
	avgdl  float64
}

var nonWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// Tokenize splits text into tokens: ASCII words kept whole, CJK runs split into
// overlapping 2-grams.
func Tokenize(text string) []string {
	text = strings.ToLower(text)
	fields := nonWord.Split(text, -1)
	var toks []string
	for _, f := range fields {
		if f == "" {
			continue
		}
		if isASCII(f) {
			toks = append(toks, f)
			continue
		}
		runes := []rune(f)
		if len(runes) == 1 {
			toks = append(toks, f)
			continue
		}
		for i := 0; i < len(runes)-1; i++ {
			toks = append(toks, string(runes[i:i+2]))
		}
	}
	return toks
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// Build constructs an inverted index from ids and texts.
func Build(ids, texts []string) *Inverted {
	inv := &Inverted{
		df:     map[string]int{},
		tf:     map[string]map[string]int{},
		docLen: map[string]int{},
		n:      len(ids),
	}
	total := 0
	for i, id := range ids {
		toks := Tokenize(texts[i])
		inv.docLen[id] = len(toks)
		total += len(toks)
		m := map[string]int{}
		for _, t := range toks {
			m[t]++
		}
		inv.tf[id] = m
		for t := range m {
			inv.df[t]++
		}
	}
	if inv.n > 0 {
		inv.avgdl = float64(total) / float64(inv.n)
	}
	return inv
}

// Search scores documents for a query and returns the topK by BM25.
func (inv *Inverted) Search(query string, topK int) []Scored {
	q := Tokenize(query)
	scores := map[string]float64{}
	for _, term := range q {
		dft, ok := inv.df[term]
		if !ok {
			continue
		}
		idf := math.Log((float64(inv.n-dft)+0.5)/(float64(dft)+0.5) + 1)
		for doc, f := range inv.tf {
			tfv, ok := f[term]
			if !ok {
				continue
			}
			denom := float64(tfv) * (k1*(1-b) + b*float64(inv.docLen[doc])/inv.avgdl)
			scores[doc] += idf * float64(tfv) * (k1 + 1) / (float64(tfv) + denom)
		}
	}
	return topScored(scores, topK)
}

func topScored(scores map[string]float64, topK int) []Scored {
	out := make([]Scored, 0, len(scores))
	for id, sc := range scores {
		out = append(out, Scored{ID: id, Score: sc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out
}
