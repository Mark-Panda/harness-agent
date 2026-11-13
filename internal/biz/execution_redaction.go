package biz

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ExecutionRedactionVersion identifies payloads sanitized before storage.
const ExecutionRedactionVersion = 2

var executionSecretAssignment = regexp.MustCompile(`(?i)(\b(?:password|passwd|client[_-]?secret|secret|access[_-]?token|refresh[_-]?token|token|authorization|api[_-]?key|credential|private[_-]?key|access[_-]?key)["']?\s*[:=]\s*)(?:\[REDACTED\]|"(?:[^"\\]|\\.)*(?:"|$)|'(?:[^'\\]|\\.)*(?:'|$)|(?:(?:Bearer|Basic|Token)[ \t]+)?[^,}\]\s]+)`)

// RedactExecutionText removes structured and text-form credentials from an audit copy.
// Tool arguments and results passed to the caller are kept separate from this copy.
func RedactExecutionText(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if json.Valid([]byte(trimmed)) {
			decoder := json.NewDecoder(strings.NewReader(trimmed))
			decoder.UseNumber()
			var value any
			if decoder.Decode(&value) == nil {
				// Re-encode even if no parsed value changed. The original can contain
				// duplicate keys that the decoder discarded.
				if encoded, err := json.Marshal(redactExecutionValue(value)); err == nil {
					return string(encoded)
				}
			}
		}
	}
	return RedactExportText(executionSecretAssignment.ReplaceAllString(text, "${1}"+exportRedactedMarker))
}

// The decoder owns its values, so changing them never affects the tool transcript.
func redactExecutionValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if isExportSensitiveKey(normalized) || normalized == "privatekey" || normalized == "accesskey" {
				typed[key] = exportRedactedMarker
				continue
			}
			typed[key] = redactExecutionValue(item)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = redactExecutionValue(item)
		}
		return typed
	case string:
		return RedactExecutionText(typed)
	default:
		return value
	}
}

func captureExecutionText(text string, maximum int64) (string, bool) {
	return truncatePayloadText(RedactExecutionText(text), maximum)
}
