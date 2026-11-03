// Package memory 的向量多样性选择：MMR 在相关性与冗余度之间做确定性权衡。
package memory

import (
	"math"
	"sort"
)

// VectorCandidate 是长期记忆检索候选项。
type VectorCandidate struct {
	// ID 候选唯一标识，用于去重与确定性排序。
	ID string
	// Relevance 与查询的余弦相似度，取值 [-1,1]，通常已转换为 [0,1]。
	Relevance float64
	// Vector 候选向量，与查询向量维度一致；缺失时视为与任何候选不相似。
	Vector []float32
}

// CosineSimilarity 计算两个向量的余弦相似度；维度不一致或零向量返回 0。
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for index := range a {
		dot += float64(a[index]) * float64(b[index])
		normA += float64(a[index]) * float64(a[index])
		normB += float64(b[index]) * float64(b[index])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// SelectMMR 使用 Maximal Marginal Relevance 选择最多 limit 个候选。
// lambda 越接近 1 越偏重相关性，越接近 0 越偏重多样性；非法值会被夹到 [0,1]。
// 相同输入始终产生相同顺序，便于评测与重放。
func SelectMMR(candidates []VectorCandidate, limit int, lambda float64) []VectorCandidate {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}
	if lambda < 0 {
		lambda = 0
	}
	if lambda > 1 {
		lambda = 1
	}
	// 按 ID 去重，保留相关性更高的副本；同分时按 ID 升序保证确定性。
	unique := make(map[string]VectorCandidate, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID == "" {
			continue
		}
		existing, exists := unique[candidate.ID]
		if !exists || candidate.Relevance > existing.Relevance {
			unique[candidate.ID] = candidate
		}
	}
	pool := make([]VectorCandidate, 0, len(unique))
	for _, candidate := range unique {
		pool = append(pool, candidate)
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].Relevance == pool[j].Relevance {
			return pool[i].ID < pool[j].ID
		}
		return pool[i].Relevance > pool[j].Relevance
	})
	if limit > len(pool) {
		limit = len(pool)
	}
	selected := make([]VectorCandidate, 0, limit)
	remaining := make([]VectorCandidate, len(pool))
	copy(remaining, pool)
	// 每轮选择 MMR 分数最高的候选；并列时取相关性更高、再按 ID 排序。
	for len(selected) < limit {
		bestIndex := 0
		bestScore := math.Inf(-1)
		for index, candidate := range remaining {
			maxSimilarity := 0.0
			for _, chosen := range selected {
				similarity := CosineSimilarity(candidate.Vector, chosen.Vector)
				if similarity > maxSimilarity {
					maxSimilarity = similarity
				}
			}
			score := lambda*candidate.Relevance - (1-lambda)*maxSimilarity
			if score > bestScore || (score == bestScore && candidate.Relevance > remaining[bestIndex].Relevance) {
				bestScore = score
				bestIndex = index
			}
		}
		selected = append(selected, remaining[bestIndex])
		remaining = append(remaining[:bestIndex], remaining[bestIndex+1:]...)
	}
	return selected
}
