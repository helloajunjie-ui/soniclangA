// Package vector provides cosine-similarity search over stored embeddings.
package vector

import "sort"

// Scored is an id with its similarity score.
type Scored struct {
	ID    string
	Score float64
}

// Search returns the topK documents by cosine similarity to the query.
func Search(query []float32, ids []string, embs map[string][]float32, topK int) []Scored {
	scores := map[string]float64{}
	for _, id := range ids {
		e, ok := embs[id]
		if !ok {
			continue
		}
		scores[id] = cosine(query, e)
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
