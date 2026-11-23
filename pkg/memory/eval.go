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

// MRR 返回第一个相关结果倒数排名的平均值；没有相关结果命中时返回 0。
func MRR(ranked []string, relevant map[string]struct{}) float64 {
	if len(relevant) == 0 {
		return 1
	}
	for index, item := range ranked {
		if _, exists := relevant[item]; exists {
			return 1 / float64(index+1)
		}
	}
	return 0
}

// NDCGAtK 计算前 k 个结果的归一化折损累计增益；相关集合为空时返回 1。
func NDCGAtK(ranked []string, relevant map[string]struct{}, k int) float64 {
	if len(relevant) == 0 {
		return 1
	}
	if k <= 0 || len(ranked) == 0 {
		return 0
	}
	if k > len(ranked) {
		k = len(ranked)
	}
	dcg := 0.0
	for index := 0; index < k; index++ {
		if _, exists := relevant[ranked[index]]; exists {
			dcg += 1 / math.Log2(float64(index)+2)
		}
	}
	idealHits := len(relevant)
	if idealHits > k {
		idealHits = k
	}
	idcg := 0.0
	for index := 0; index < idealHits; index++ {
		idcg += 1 / math.Log2(float64(index)+2)
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
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

// SummaryClaimRecall 计算期望 claim 被摘要覆盖的比例；空集合返回 1。
func SummaryClaimRecall(summary string, expected []string) float64 {
	if len(expected) == 0 {
		return 1
	}
	normalized := normalizeClaim(summary)
	hits := 0
	for _, claim := range expected {
		if strings.Contains(normalized, normalizeClaim(claim)) {
			hits++
		}
	}
	return float64(hits) / float64(len(expected))
}

// UnsupportedClaimRate 计算摘要中出现禁止 claim 的比例；空集合返回 0。
func UnsupportedClaimRate(summary string, forbidden []string) float64 {
	if len(forbidden) == 0 {
		return 0
	}
	normalized := normalizeClaim(summary)
	hits := 0
	for _, claim := range forbidden {
		if strings.Contains(normalized, normalizeClaim(claim)) {
			hits++
		}
	}
	return float64(hits) / float64(len(forbidden))
}

// SpeakerAttributionAccuracy 校验 claim 是否出现在正确发言人的行内。
// expected 的 key 是发言人，value 是该发言人对应的 claim；空集合返回 1。
func SpeakerAttributionAccuracy(expected map[string]string, transcript string) float64 {
	if len(expected) == 0 {
		return 1
	}
	lines := strings.Split(transcript, "\n")
	hits := 0
	for speaker, claim := range expected {
		normalizedClaim := normalizeClaim(claim)
		for _, line := range lines {
			if strings.Contains(line, speaker) && strings.Contains(normalizeClaim(line), normalizedClaim) {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(len(expected))
}

// ConflictResolutionAccuracy 校验当前生效事实是否等于期望值；空集合返回 1。
func ConflictResolutionAccuracy(active map[string]string, expected map[string]string) float64 {
	if len(expected) == 0 {
		return 1
	}
	hits := 0
	for key, value := range expected {
		if active[key] == value {
			hits++
		}
	}
	return float64(hits) / float64(len(expected))
}

// normalizeClaim 统一大小写、标点与空白，降低 claim 精确匹配的格式噪声。
func normalizeClaim(text string) string {
	var builder strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			builder.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(builder.String())
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
		// CJK 连续字符按二元组切分：覆盖汉字、平假名、片假名与韩文，
		// 避免中日韩摘要因分词差异完全无法比较。
		if index+1 < len(runes) && isCJKRune(current) && isCJKRune(runes[index+1]) {
			tokens[string([]rune{current, runes[index+1]})] = struct{}{}
		}
	}
	if asciiBuilder.Len() >= 2 {
		tokens[asciiBuilder.String()] = struct{}{}
	}
	return tokens
}

// isCJKRune 判断字符是否属于需要按二元组切分的中日韩文字。
func isCJKRune(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
