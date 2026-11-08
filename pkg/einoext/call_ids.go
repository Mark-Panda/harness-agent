package einoext

import (
	"strings"

	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// normalizeToolCallIDs assigns identities to calls in a newly generated completion.
// Provider IDs can repeat across completions or be empty. The assigned ID is shared
// by the assistant message, tool execution, and approval, and is preserved on replay.
func normalizeToolCallIDs(completion *react.Completion) {
	for index := range completion.ToolCalls {
		completion.ToolCalls[index].ID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
}
