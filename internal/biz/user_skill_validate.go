package biz

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

const (
	// maxSynthesizedSkillPromptTokens 自动技能正文的 Token 上限；运行时注入上限为 2000。
	maxSynthesizedSkillPromptTokens = 1800
	// maxSynthesizedSkillNameLength 标准 SKILL 名称的最大字符数。
	maxSynthesizedSkillNameLength = 64
	// minSynthesizedDescriptionRunes 自动技能描述的最小字符数。
	minSynthesizedDescriptionRunes = 20
	// maxSynthesizedDescriptionRunes 自动技能描述的最大字符数，对齐标准 SKILL。
	maxSynthesizedDescriptionRunes = 1024
	// minSynthesizedEvidenceQuoteRunes 单条证据引用的最小字符数。
	minSynthesizedEvidenceQuoteRunes = 8
)

var (
	// synthesizedSkillNamePattern 标准 SKILL 名称：小写字母/数字，用连字符分段。
	synthesizedSkillNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	// skillPlaceholderPattern 匹配提示词模板中的 {{参数名}} 占位符。
	skillPlaceholderPattern = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
	// sensitiveSkillPatterns 高置信敏感信息；命中即拒绝自动技能。
	sensitiveSkillPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{8,}`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}`),
		regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|access[_-]?token)\s*[:=]\s*[^\s,;]{3,}`),
		regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`),
	}
)

// validateSynthesizedSkill 校验自动提炼候选的格式、参数、证据与敏感信息。
// 校验通过时会规范化正文，补上一级标题与「何时使用」段。
func validateSynthesizedSkill(candidate *SkillCandidate, sessions []Session, messagesBySession map[uuid.UUID][]Message) error {
	if candidate == nil || !candidate.ShouldCreate {
		return nil
	}
	candidate.Name = strings.TrimSpace(candidate.Name)
	candidate.Description = strings.TrimSpace(candidate.Description)
	candidate.WhenToUse = strings.TrimSpace(candidate.WhenToUse)
	whenToUseRunes := utf8.RuneCountInString(candidate.WhenToUse)
	if whenToUseRunes < minSynthesizedDescriptionRunes || whenToUseRunes > maxSynthesizedDescriptionRunes {
		return invalid("skill when_to_use must contain between %d and %d characters", minSynthesizedDescriptionRunes, maxSynthesizedDescriptionRunes)
	}
	candidate.PromptTemplate = composeStandardSkillPrompt(candidate.Name, candidate.WhenToUse, candidate.PromptTemplate)
	if err := validateSynthesizedSkillStructure(*candidate); err != nil {
		return err
	}
	if err := validateSkillEvidence(candidate.Evidence, sessions, messagesBySession); err != nil {
		return err
	}
	return nil
}

// validateSynthesizedSkillStructure 只校验不依赖会话证据的结构部分，供 CreateAuto 兜底。
func validateSynthesizedSkillStructure(candidate SkillCandidate) error {
	name := strings.TrimSpace(candidate.Name)
	if !synthesizedSkillNamePattern.MatchString(name) || utf8.RuneCountInString(name) > maxSynthesizedSkillNameLength {
		return invalid("skill name must be kebab-case, 3-64 characters")
	}
	descriptionRunes := utf8.RuneCountInString(strings.TrimSpace(candidate.Description))
	if descriptionRunes < minSynthesizedDescriptionRunes || descriptionRunes > maxSynthesizedDescriptionRunes {
		return invalid("skill description must contain between %d and %d characters", minSynthesizedDescriptionRunes, maxSynthesizedDescriptionRunes)
	}
	prompt := strings.TrimSpace(candidate.PromptTemplate)
	if prompt == "" {
		return invalid("skill prompt template is required")
	}
	if react.EstimateTextTokens(prompt) > maxSynthesizedSkillPromptTokens {
		return invalid("skill prompt template exceeds %d tokens", maxSynthesizedSkillPromptTokens)
	}
	for _, headings := range [][]string{
		{"## 何时使用", "## 适用场景"},
		{"## 步骤", "## 流程"},
		{"## 输出格式", "## 输出"},
		{"## 约束", "## 禁止"},
	} {
		if !hasAnyHeading(prompt, headings...) {
			return invalid("skill prompt template is missing required heading %q", headings[0])
		}
	}
	if err := validateSkillParameters(candidate.Parameters, prompt); err != nil {
		return err
	}
	for _, text := range []string{name, candidate.Description, prompt} {
		if containsSensitiveSkillText(text) {
			return invalid("skill contains high-confidence sensitive information")
		}
	}
	return nil
}

// validateSkillParameters 校验参数 JSON Schema 与正文占位符一致。
func validateSkillParameters(parameters map[string]any, prompt string) error {
	placeholders := skillPlaceholders(prompt)
	if len(parameters) == 0 {
		if len(placeholders) > 0 {
			return invalid("skill prompt uses placeholders but parameters are empty")
		}
		return nil
	}
	if value, ok := parameters["type"]; !ok || strings.TrimSpace(fmt.Sprint(value)) != "object" {
		return invalid("skill parameters must declare type=object")
	}
	rawProperties, ok := parameters["properties"].(map[string]any)
	if !ok {
		return invalid("skill parameters must contain a properties object")
	}
	required, err := stringListValue(parameters["required"])
	if err != nil {
		return err
	}
	for _, name := range required {
		if _, ok := rawProperties[name]; !ok {
			return invalid("required parameter %q is not declared in properties", name)
		}
	}
	for name, rawProperty := range rawProperties {
		property, ok := rawProperty.(map[string]any)
		if !ok {
			return invalid("parameter %q must be an object", name)
		}
		if value, ok := property["type"].(string); !ok || strings.TrimSpace(value) == "" {
			return invalid("parameter %q must declare a type", name)
		}
		if value, ok := property["description"].(string); !ok || strings.TrimSpace(value) == "" {
			return invalid("parameter %q must declare a description", name)
		}
	}
	for name := range placeholders {
		if _, ok := rawProperties[name]; !ok {
			return invalid("placeholder %q is not declared in parameters", name)
		}
	}
	return nil
}

// stringListValue 兼容 []string 与 []any 两种 JSON 解码结果。
func stringListValue(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	switch items := value.(type) {
	case []string:
		return items, nil
	case []any:
		result := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, invalid("required parameters must be strings")
			}
			result = append(result, text)
		}
		return result, nil
	default:
		return nil, invalid("required parameters must be an array")
	}
}

// skillPlaceholders 提取正文中的占位符名称。
func skillPlaceholders(prompt string) map[string]struct{} {
	matches := skillPlaceholderPattern.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return nil
	}
	names := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		names[match[1]] = struct{}{}
	}
	return names
}

// synthesizedSkillWarnings 返回不阻断创建、但需要写入执行记录的提示。
func synthesizedSkillWarnings(candidate SkillCandidate) []string {
	var warnings []string
	if utf8.RuneCountInString(strings.TrimSpace(candidate.Description)) > 500 {
		warnings = append(warnings, "技能描述超过 500 字符，运行时只保留前 300 Token")
	}
	if utf8.RuneCountInString(strings.TrimSpace(candidate.WhenToUse)) > 500 {
		warnings = append(warnings, "when_to_use 超过 500 字符，运行时描述会被截断")
	}
	required, err := stringListValue(candidate.Parameters["required"])
	if err == nil {
		placeholders := skillPlaceholders(candidate.PromptTemplate)
		for _, name := range required {
			if _, ok := placeholders[name]; !ok {
				warnings = append(warnings, fmt.Sprintf("必填参数 %q 未在正文中使用", name))
			}
		}
	}
	return warnings
}

// validateSkillEvidence 校验证据条目确实来自提供的会话消息。
func validateSkillEvidence(evidence []SkillEvidence, sessions []Session, messagesBySession map[uuid.UUID][]Message) error {
	if len(evidence) < 2 {
		return invalid("skill requires at least 2 evidence quotes")
	}
	titles := make(map[string]uuid.UUID, len(sessions))
	for _, session := range sessions {
		title := strings.TrimSpace(session.Title)
		if title == "" {
			title = "(无标题)"
		}
		titles[normalizeEvidenceText(title)] = session.ID
	}
	for _, item := range evidence {
		quote := normalizeEvidenceText(item.Quote)
		if utf8.RuneCountInString(quote) < minSynthesizedEvidenceQuoteRunes {
			return invalid("evidence quote must contain at least %d characters", minSynthesizedEvidenceQuoteRunes)
		}
		sessionID, ok := titles[normalizeEvidenceText(item.Session)]
		if !ok {
			return invalid("evidence session %q is not part of this synthesis run", item.Session)
		}
		found := false
		for _, message := range messagesBySession[sessionID] {
			if strings.Contains(normalizeEvidenceText(message.Content), quote) {
				found = true
				break
			}
		}
		if !found {
			return invalid("evidence quote is not found in session %q", item.Session)
		}
	}
	return nil
}

// normalizeEvidenceText 统一空白与大小写，降低模型引用时的格式差异。
func normalizeEvidenceText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// containsSensitiveSkillText 判断文本是否命中高置信敏感信息模式。
func containsSensitiveSkillText(text string) bool {
	for _, pattern := range sensitiveSkillPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

// hasAnyHeading 判断正文是否包含任一指定 Markdown 标题。
func hasAnyHeading(prompt string, headings ...string) bool {
	for _, line := range strings.Split(prompt, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, heading := range headings {
			if strings.HasPrefix(trimmed, heading) {
				return true
			}
		}
	}
	return false
}

// composeStandardSkillPrompt 补齐一级标题与「何时使用」段，保证正文符合最小标准结构。
func composeStandardSkillPrompt(name, whenToUse, prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}
	if !strings.HasPrefix(prompt, "# ") {
		prompt = "# " + strings.TrimSpace(name) + "\n\n" + prompt
	}
	if strings.TrimSpace(whenToUse) != "" && !hasAnyHeading(prompt, "## 何时使用", "## 适用场景") {
		lines := strings.SplitN(prompt, "\n", 2)
		section := "\n\n## 何时使用\n" + strings.TrimSpace(whenToUse)
		if len(lines) == 2 {
			prompt = lines[0] + section + "\n\n" + strings.TrimSpace(lines[1])
		} else {
			prompt += section
		}
	}
	return strings.TrimSpace(prompt)
}
