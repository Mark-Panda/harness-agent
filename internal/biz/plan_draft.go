package biz

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// PlanLimits 是 Planner 输出的硬限制，防止计划规模或依赖深度失控。
type PlanLimits struct {
	// MaxSteps 最大步骤数，硬上限 50。
	MaxSteps int
	// MaxDepth 最大依赖深度。
	MaxDepth int
	// MaxGoalBytes 目标最大字节数。
	MaxGoalBytes int
	// MaxDescriptionBytes 单步说明最大字节数。
	MaxDescriptionBytes int
	// MaxArgumentsBytes 单步工具参数最大字节数。
	MaxArgumentsBytes int
}

// DefaultPlanLimits 返回生产默认限制：50 步、依赖深度 5。
func DefaultPlanLimits() PlanLimits {
	return PlanLimits{
		MaxSteps: 50, MaxDepth: 5, MaxGoalBytes: 8192,
		MaxDescriptionBytes: 8192, MaxArgumentsBytes: 262144,
	}
}

// normalize 补齐零值限制，避免测试或调用方遗漏时失去边界。
func (l PlanLimits) normalize() PlanLimits {
	defaults := DefaultPlanLimits()
	if l.MaxSteps <= 0 || l.MaxSteps > defaults.MaxSteps {
		l.MaxSteps = defaults.MaxSteps
	}
	if l.MaxDepth <= 0 || l.MaxDepth > defaults.MaxDepth {
		l.MaxDepth = defaults.MaxDepth
	}
	if l.MaxGoalBytes <= 0 {
		l.MaxGoalBytes = defaults.MaxGoalBytes
	}
	if l.MaxDescriptionBytes <= 0 {
		l.MaxDescriptionBytes = defaults.MaxDescriptionBytes
	}
	if l.MaxArgumentsBytes <= 0 {
		l.MaxArgumentsBytes = defaults.MaxArgumentsBytes
	}
	return l
}

// PlanDraft 是 Planner 模型输出的结构化计划。
type PlanDraft struct {
	// Goal 计划目标。
	Goal string `json:"goal"`
	// Steps 有序步骤列表。
	Steps []PlanDraftStep `json:"steps"`
}

// PlanDraftStep 是 Planner 输出的单个步骤。
type PlanDraftStep struct {
	// StepNo 步骤序号，必须从 1 连续递增。
	StepNo int `json:"step_no"`
	// Title 步骤标题。
	Title string `json:"title"`
	// Description 步骤说明。
	Description string `json:"description"`
	// DependsOn 依赖的前置步骤序号。
	DependsOn []int `json:"depends_on"`
	// ToolName 可选工具名；为空表示纯模型步骤。
	ToolName string `json:"tool_name"`
	// Arguments 工具参数。
	Arguments map[string]any `json:"arguments"`
}

// ParsePlanDraft 解析并校验 Planner 输出；容忍 Markdown 代码围栏但不接受额外字段。
func ParsePlanDraft(raw string, limits PlanLimits) (PlanDraft, error) {
	limits = limits.normalize()
	payload, err := extractJSONObject(raw)
	if err != nil {
		return PlanDraft{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var draft PlanDraft
	if err := decoder.Decode(&draft); err != nil {
		return PlanDraft{}, fmt.Errorf("%w: decode plan draft: %v", ErrInvalidConfig, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return PlanDraft{}, fmt.Errorf("%w: trailing plan draft content", ErrInvalidConfig)
	}
	return validatePlanDraft(draft, limits)
}

// extractJSONObject 截取首个完整 JSON 对象，兼容模型输出的代码围栏与说明文字。
func extractJSONObject(raw string) ([]byte, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("%w: plan draft is not a JSON object", ErrInvalidConfig)
	}
	return []byte(raw[start : end+1]), nil
}

// validatePlanDraft 校验步骤数量、编号、依赖、深度与大小限制。
func validatePlanDraft(draft PlanDraft, limits PlanLimits) (PlanDraft, error) {
	draft.Goal = strings.TrimSpace(draft.Goal)
	if draft.Goal == "" || len(draft.Goal) > limits.MaxGoalBytes {
		return PlanDraft{}, fmt.Errorf("%w: plan goal must contain 1-%d bytes", ErrInvalidConfig, limits.MaxGoalBytes)
	}
	if len(draft.Steps) == 0 || len(draft.Steps) > limits.MaxSteps {
		return PlanDraft{}, fmt.Errorf("%w: plan must contain 1-%d steps", ErrInvalidConfig, limits.MaxSteps)
	}
	depths := make(map[int]int, len(draft.Steps))
	for index, step := range draft.Steps {
		expected := index + 1
		if step.StepNo != expected {
			return PlanDraft{}, fmt.Errorf("%w: steps must be numbered consecutively starting at 1, got %d at position %d", ErrInvalidConfig, step.StepNo, expected)
		}
		step.Title = strings.TrimSpace(step.Title)
		if step.Title == "" || len(step.Title) > 256 {
			return PlanDraft{}, fmt.Errorf("%w: step %d title must contain 1-256 bytes", ErrInvalidConfig, step.StepNo)
		}
		step.Description = strings.TrimSpace(step.Description)
		if len(step.Description) > limits.MaxDescriptionBytes {
			return PlanDraft{}, fmt.Errorf("%w: step %d description exceeds %d bytes", ErrInvalidConfig, step.StepNo, limits.MaxDescriptionBytes)
		}
		step.ToolName = strings.TrimSpace(step.ToolName)
		if len(step.ToolName) > 256 {
			return PlanDraft{}, fmt.Errorf("%w: step %d tool name exceeds 256 bytes", ErrInvalidConfig, step.StepNo)
		}
		if step.Arguments == nil {
			step.Arguments = map[string]any{}
		}
		argumentBytes, err := json.Marshal(step.Arguments)
		if err != nil {
			return PlanDraft{}, fmt.Errorf("%w: step %d arguments are not JSON encodable", ErrInvalidConfig, step.StepNo)
		}
		if len(argumentBytes) > limits.MaxArgumentsBytes {
			return PlanDraft{}, fmt.Errorf("%w: step %d arguments exceed %d bytes", ErrInvalidConfig, step.StepNo, limits.MaxArgumentsBytes)
		}
		seen := make(map[int]struct{}, len(step.DependsOn))
		depth := 1
		for _, dependency := range step.DependsOn {
			if dependency < 1 || dependency >= step.StepNo {
				return PlanDraft{}, fmt.Errorf("%w: step %d depends on invalid step %d", ErrInvalidConfig, step.StepNo, dependency)
			}
			if _, duplicated := seen[dependency]; duplicated {
				return PlanDraft{}, fmt.Errorf("%w: step %d has duplicated dependency %d", ErrInvalidConfig, step.StepNo, dependency)
			}
			seen[dependency] = struct{}{}
			if candidate := depths[dependency] + 1; candidate > depth {
				depth = candidate
			}
		}
		if depth > limits.MaxDepth {
			return PlanDraft{}, fmt.Errorf("%w: step %d dependency depth exceeds %d", ErrInvalidConfig, step.StepNo, limits.MaxDepth)
		}
		depths[step.StepNo] = depth
		draft.Steps[index] = step
	}
	return draft, nil
}
