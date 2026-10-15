// Package rrf fuses ranked result lists with Reciprocal Rank Fusion (k=60 by
// default, DESIGN §5.2 / §2.4).
package rrf

import "sort"

// Scored is an id with its fused score.
type Scored struct {
	ID    string
	Score float64
}

// Fuse combines ranked lists (highest rank first) into one ranked list.
func Fuse(k int, lists ...[]Scored) []Scored {
	if k <= 0 {
		k = 60
	}
	scores := map[string]float64{}
	for _, list := range lists {
		for rank, item := range list {
			scores[item.ID] += 1.0 / float64(k+rank+1)
		}
	}
	out := make([]Scored, 0, len(scores))
	for id, sc := range scores {
		out = append(out, Scored{ID: id, Score: sc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
