package vector

import "testing"

// Zero similarity must not be emitted as a candidate. The fusion stage ranks by
// position, so a 0-scored row would still be promoted into the result and served
// as a citation or as a recalled memory that nothing matched.
func TestZeroSimilarityIsNotACandidate(t *testing.T) {
	embs := map[string][]float32{
		"match":     {1, 0, 0},
		"also":      {0.9, 0.1, 0},
		"nil":       nil,
		"zero":      {0, 0, 0},
		"otherDim":  {1, 0},
		"orthognal": {0, 1, 0},
	}
	ids := []string{"match", "also", "nil", "zero", "otherDim", "orthognal"}

	got := Search([]float32{1, 0, 0}, ids, embs, 10)
	if len(got) != 2 {
		t.Fatalf("got %d candidates (%v), want 2 real matches", len(got), got)
	}
	if got[0].ID != "match" || got[0].Score < 0.99 {
		t.Errorf("best hit = %+v, want match ≈ 1.0", got[0])
	}
	if got[1].ID != "also" {
		t.Errorf("second hit = %q, want also", got[1].ID)
	}
	for _, h := range got {
		if h.Score <= 0 {
			t.Errorf("%q was returned with score %v", h.ID, h.Score)
		}
	}
}

func TestTopKLimitsCandidates(t *testing.T) {
	embs := map[string][]float32{
		"a": {1, 0},
		"b": {0.9, 0.1},
		"c": {0.8, 0.2},
	}
	got := Search([]float32{1, 0}, []string{"a", "b", "c"}, embs, 2)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("order = %s,%s, want a,b", got[0].ID, got[1].ID)
	}
}
