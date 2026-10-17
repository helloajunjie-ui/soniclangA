// Package vector provides cosine-similarity search over stored embeddings.
package vector

import "sort"

// Scored is an id with its similarity score.
type Scored struct {
	ID    string
	Score float64
}

// Search returns the topK documents by cosine similarity to the query.
//
// A zero similarity is not a weak match, it means "no match at all" (empty or
// zero vector, dimension mismatch, or genuinely orthogonal text). Those must not
// be emitted: RRF ranks candidates by position, not by score, so a 0-scored row
// would still be promoted into the fused result and shown as a citation or a
// recalled memory that nothing actually matched.
func Search(query []float32, ids []string, embs map[string][]float32, topK int) []Scored {
	scores := map[string]float64{}
	for _, id := range ids {
		e, ok := embs[id]
		if !ok {
			continue
		}
		if sc := cosine(query, e); sc > 0 {
			scores[id] = sc
		}
	}
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

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt(na) * sqrt(nb))
}

func sqrt(x float64) float64 {
	if x == 0 {
		return 0
	}
	g := x
	for i := 0; i < 8; i++ {
		g = (g + x/g) / 2
	}
	return g
}
