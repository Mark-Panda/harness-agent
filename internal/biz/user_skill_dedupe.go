package biz

import (
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// normalizeSkillName 统一技能名称比较形式：小写，连字符/下划线/空白统一为连字符。
func normalizeSkillName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var builder strings.Builder
	lastSeparator := false
	for _, r := range name {
		switch {
		case r == '-' || r == '_' || unicode.IsSpace(r):
			if builder.Len() > 0 && !lastSeparator {
				builder.WriteByte('-')
				lastSeparator = true
			}
		default:
			builder.WriteRune(r)
			lastSeparator = false
		}
	}
	return strings.Trim(builder.String(), "-")
}

// skillTokenSet 提取用于相似度比较的 Token：ASCII 小写单词 + CJK 相邻二字组。
func skillTokenSet(text string) map[string]struct{} {
	tokens := map[string]struct{}{}
	var word strings.Builder
	var previousHan rune
	flushWord := func() {
		if word.Len() >= 2 {
			tokens[strings.ToLower(word.String())] = struct{}{}
		}
		word.Reset()
	}
	for _, r := range text {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			word.WriteRune(r)
			previousHan = 0
			continue
		}
		flushWord()
		if unicode.Is(unicode.Han, r) {
			if previousHan != 0 {
				tokens[string([]rune{previousHan, r})] = struct{}{}
			}
			previousHan = r
			continue
		}
		previousHan = 0
	}
	flushWord()
	return tokens
}

// tokenContainment 返回两个 Token 集合的交集占较小集合的比例。
func tokenContainment(left, right map[string]struct{}) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	smaller := len(left)
	if len(right) < smaller {
		smaller = len(right)
	}
	intersection := 0
	for token := range left {
		if _, ok := right[token]; ok {
			intersection++
		}
	}
	return float64(intersection) / float64(smaller)
}

// findDuplicateSkill 按名称、来源会话与正文/描述相似度判定候选是否重复。
func findDuplicateSkill(candidate SkillCandidate, sourceSessionIDs []uuid.UUID, existing []UserSkill) (UserSkill, bool) {
	candidateName := normalizeSkillName(candidate.Name)
	candidatePrompt := skillTokenSet(candidate.PromptTemplate)
	candidateDescription := skillTokenSet(candidate.Description)
	candidateSources := sessionIDSet(sourceSessionIDs)
	for _, skill := range existing {
		if candidateName != "" && normalizeSkillName(skill.Name) == candidateName {
			return skill, true
		}
		existingPrompt := skillTokenSet(skill.PromptTemplate)
		if len(candidatePrompt) < 20 || len(existingPrompt) < 20 {
			continue
		}
		promptOverlap := tokenContainment(candidatePrompt, existingPrompt)
		sourceOverlap := sessionSetOverlap(candidateSources, sessionIDSet(skill.SourceSessionIDs))
		if sourceOverlap >= 0.75 && promptOverlap >= 0.2 {
			return skill, true
		}
		if sourceOverlap >= 0.5 && promptOverlap >= 0.35 {
			return skill, true
		}
		if promptOverlap >= 0.5 && tokenContainment(candidateDescription, skillTokenSet(skill.Description)) >= 0.8 {
			return skill, true
		}
	}
	return UserSkill{}, false
}

// sessionIDSet 把会话 ID 列表转为集合。
func sessionIDSet(ids []uuid.UUID) map[uuid.UUID]struct{} {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if id != uuid.Nil {
			set[id] = struct{}{}
		}
	}
	return set
}

// sessionSetOverlap 返回两个会话集合的交集占较小集合的比例。
func sessionSetOverlap(left, right map[uuid.UUID]struct{}) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	smaller := len(left)
	if len(right) < smaller {
		smaller = len(right)
	}
	intersection := 0
	for id := range left {
		if _, ok := right[id]; ok {
			intersection++
		}
	}
	return float64(intersection) / float64(smaller)
}
