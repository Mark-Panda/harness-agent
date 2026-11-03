// Package memory 的记忆质量评测工具：提供可复现的检索、多样性与摘要忠实度指标。
package memory

import (
	"math"
	"strings"
	"unicode"
)

// PrecisionAtK 计算前 k 个结果中相关结果的比例；k 超过结果数时按实际数量计算。
func PrecisionAtK(ranked []string, relevant map[string]struct{}, k int) float64 {
	if k <= 0 || len(ranked) == 0 {
		return 0
	}
	if k > len(ranked) {
		k = len(ranked)
	}
	hits := 0
	for index := 0; index < k; index++ {
		if _, exists := relevant[ranked[index]]; exists {
			hits++
		}
	}
	return float64(hits) / float64(k)
}

// RecallAtK 计算前 k 个结果覆盖相关集合的比例；相关集合为空时返回 1。
func RecallAtK(ranked []string, relevant map[string]struct{}, k int) float64 {
	if len(relevant) == 0 {
		return 1
	}
	if k <= 0 || len(ranked) == 0 {
		return 0
	}
	if k > len(ranked) {
		k = len(ranked)
	}
	hits := 0
	found := make(map[string]struct{}, k)
	for index := 0; index < k; index++ {
		if _, exists := relevant[ranked[index]]; !exists {
			continue
		}
		if _, seen := found[ranked[index]]; seen {
			continue
		}
		found[ranked[index]] = struct{}{}
		hits++
	}
	return float64(hits) / float64(len(relevant))
}

// MMRDiversity 返回候选集合的平均两两余弦距离，用于衡量 MMR 多样性。
// 少于两个候选或向量缺失时返回 0。
func MMRDiversity(vectors [][]float32) float64 {
	if len(vectors) < 2 {
		return 0
	}
	pairs, total := 0, 0.0
	for i := 0; i < len(vectors); i++ {
		for j := i + 1; j < len(vectors); j++ {
			similarity := CosineSimilarity(vectors[i], vectors[j])
			if math.IsNaN(similarity) {
				continue
			}
			total += 1 - similarity
			pairs++
		}
	}
	if pairs == 0 {
		return 0
	}
	return total / float64(pairs)
}

// SummaryFaithfulness 计算摘要词元被来源文本覆盖的比例。
// ASCII 按单词切分，CJK 按二元组切分；数值、专名等未在来源出现的词元会降低忠实度。
func SummaryFaithfulness(summary string, sources []string) float64 {
	summaryTokens := contentTokens(summary)
	if len(summaryTokens) == 0 {
		return 1
	}
	sourceTokens := make(map[string]struct{})
	for _, source := range sources {
		for token := range contentTokens(source) {
			sourceTokens[token] = struct{}{}
		}
	}
	covered := 0
	for token := range summaryTokens {
		if _, exists := sourceTokens[token]; exists {
			covered++
		}
	}
	return float64(covered) / float64(len(summaryTokens))
}

// SpeakerAttributionRate 计算预期发言人在摘要/记录中被标注的比例。
func SpeakerAttributionRate(transcript string, speakers []string) float64 {
	if len(speakers) == 0 {
		return 1
	}
	seen := 0
	for _, speaker := range speakers {
		if speaker != "" && strings.Contains(transcript, speaker) {
			seen++
		}
	}
	return float64(seen) / float64(len(speakers))
}

// contentTokens 提取用于忠实度比较的词元集合。
func contentTokens(text string) map[string]struct{} {
	tokens := make(map[string]struct{})
	var asciiBuilder strings.Builder
	runes := []rune(strings.ToLower(text))
	for index := 0; index < len(runes); index++ {
		current := runes[index]
		if current < unicode.MaxASCII && (unicode.IsLetter(current) || unicode.IsDigit(current)) {
			asciiBuilder.WriteRune(current)
			continue
		}
		if asciiBuilder.Len() >= 2 {
			tokens[asciiBuilder.String()] = struct{}{}
		}
		asciiBuilder.Reset()
		// CJK 连续字符按二元组切分，避免中文摘要因分词差异完全无法比较。
		if index+1 < len(runes) && unicode.Is(unicode.Han, current) && unicode.Is(unicode.Han, runes[index+1]) {
			tokens[string([]rune{current, runes[index+1]})] = struct{}{}
		}
	}
	if asciiBuilder.Len() >= 2 {
		tokens[asciiBuilder.String()] = struct{}{}
	}
	return tokens
}
