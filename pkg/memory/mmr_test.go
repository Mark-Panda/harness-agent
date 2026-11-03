package memory

import (
	"math"
	"testing"
)

// TestCosineSimilarity 验证基本相似度与非法输入。
func TestCosineSimilarity(t *testing.T) {
	if got := CosineSimilarity([]float32{1, 0}, []float32{1, 0}); math.Abs(got-1) > 1e-6 {
		t.Fatalf("identical similarity = %f", got)
	}
	if got := CosineSimilarity([]float32{1, 0}, []float32{0, 1}); math.Abs(got) > 1e-6 {
		t.Fatalf("orthogonal similarity = %f", got)
	}
	if got := CosineSimilarity([]float32{0, 0}, []float32{1, 0}); got != 0 {
		t.Fatalf("zero vector similarity = %f", got)
	}
	if got := CosineSimilarity([]float32{1}, []float32{1, 2}); got != 0 {
		t.Fatalf("dimension mismatch similarity = %f", got)
	}
}

// TestSelectMMRPrefersDiverseCandidates 验证 MMR 在相关性相近时选择多样性更高的候选。
func TestSelectMMRPrefersDiverseCandidates(t *testing.T) {
	candidates := []VectorCandidate{
		{ID: "a", Relevance: 0.95, Vector: []float32{1, 0}},
		{ID: "b", Relevance: 0.94, Vector: []float32{0.99, 0.05}},
		{ID: "c", Relevance: 0.80, Vector: []float32{0, 1}},
	}
	selected := SelectMMR(candidates, 2, 0.5)
	if len(selected) != 2 || selected[0].ID != "a" || selected[1].ID != "c" {
		t.Fatalf("selected = %#v", selected)
	}
}

// TestSelectMMRDeduplicatesAndLimits 验证重复 ID 去重、数量上限与确定性。
func TestSelectMMRDeduplicatesAndLimits(t *testing.T) {
	candidates := []VectorCandidate{
		{ID: "a", Relevance: 0.5, Vector: []float32{1, 0}},
		{ID: "a", Relevance: 0.9, Vector: []float32{1, 0}},
		{ID: "b", Relevance: 0.4, Vector: []float32{0, 1}},
		{ID: "c", Relevance: 0.3, Vector: []float32{1, 1}},
	}
	selected := SelectMMR(candidates, 10, 1)
	if len(selected) != 3 {
		t.Fatalf("selected = %#v", selected)
	}
	if selected[0].ID != "a" || selected[0].Relevance != 0.9 {
		t.Fatalf("duplicate was not merged: %#v", selected[0])
	}
	if result := SelectMMR(candidates, 2, 1); result[0].ID != "a" || result[1].ID != "b" {
		t.Fatalf("lambda=1 order = %#v", result)
	}
	if selected := SelectMMR(nil, 1, 0.5); selected != nil {
		t.Fatalf("nil candidates = %#v", selected)
	}
	if selected := SelectMMR(candidates, 0, 0.5); selected != nil {
		t.Fatalf("zero limit = %#v", selected)
	}
	if selected := SelectMMR(candidates, 1, 2); len(selected) != 1 || selected[0].Relevance < 0.9 {
		t.Fatalf("lambda > 1 = %#v", selected)
	}
	if selected := SelectMMR(candidates, 1, -1); len(selected) != 1 {
		t.Fatalf("lambda < 0 = %#v", selected)
	}
}
