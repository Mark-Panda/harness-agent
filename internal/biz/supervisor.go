package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SupervisorTaskStatus 是子任务生命周期状态。
type SupervisorTaskStatus string

const (
	// SupervisorTaskPending 等待执行。
	SupervisorTaskPending SupervisorTaskStatus = "pending"
	// SupervisorTaskRunning 执行中。
	SupervisorTaskRunning SupervisorTaskStatus = "running"
	// SupervisorTaskCompleted Worker 成功返回结果。
	SupervisorTaskCompleted SupervisorTaskStatus = "completed"
	// SupervisorTaskFailed Worker 失败，结果不得伪装成功。
	SupervisorTaskFailed SupervisorTaskStatus = "failed"
	// SupervisorTaskCancelled 父 Run 取消或预算超限导致终止。
	SupervisorTaskCancelled SupervisorTaskStatus = "cancelled"
	// SupervisorTaskUnknown Worker 崩溃导致结果未知。
	SupervisorTaskUnknown SupervisorTaskStatus = "unknown"
)

// 子任务领域错误。
var (
	// ErrSupervisorInvalid 委派 JSON 或 Worker 能力不合法。
	ErrSupervisorInvalid = errors.New("supervisor delegation is invalid")
	// ErrSupervisorAllFailed 全部 Worker 失败，主 Run 无法给出有效结果。
	ErrSupervisorAllFailed = errors.New("all supervisor workers failed")
	// ErrSupervisorTaskTerminal 子任务已终态，禁止迟到结果回写。
	ErrSupervisorTaskTerminal = errors.New("supervisor task is already terminal")
)

// SupervisorTask 是一次 Worker 委派的任务事实。
type SupervisorTask struct {
	// ID 任务主键。
	ID uuid.UUID
	// UserID 原始用户，RLS 隔离键。
	UserID uuid.UUID
	// ParentRunID 主 Run。
	ParentRunID uuid.UUID
	// ChildSessionID 子任务独立会话。
	ChildSessionID uuid.UUID
	// WorkerName Worker 名称。
	WorkerName string
	// WorkerIndex Worker 序号。
	WorkerIndex int
	// Task 委派任务描述。
	Task string
	// ToolScope 允许使用的工具名交集。
	ToolScope []string
	// Status 任务状态。
	Status SupervisorTaskStatus
	// Attempts 已领取次数。
	Attempts int
	// MaxAttempts 最大领取次数。
	MaxAttempts int
	// Result 结果快照。
	Result map[string]any
	// ErrorCode 失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏失败原因。
	ErrorMessage string
	// Depth 委派深度。
	Depth int
	// StartedAt 开始时间。
	StartedAt *time.Time
	// FinishedAt 终态时间。
	FinishedAt *time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// SupervisorTaskRepository 定义 Supervisor 子任务持久化契约；所有查询强制携带 user_id。
type SupervisorTaskRepository interface {
	// CreateTasks 在同一事务内创建全部子任务与审计事件。
	CreateTasks(ctx context.Context, userID, parentRunID uuid.UUID, tasks []SupervisorTask, event OutboxEvent) ([]SupervisorTask, error)
	// ListTasks 按 Worker 序号列出主 Run 的子任务。
	ListTasks(ctx context.Context, userID, parentRunID uuid.UUID) ([]SupervisorTask, error)
	// StartTask 以 pending 状态为前置条件领取任务，写入 attempts 与开始时间。
	StartTask(ctx context.Context, userID, taskID uuid.UUID, now time.Time) (SupervisorTask, error)
	// FinishTask 以 running 状态为前置条件推进终态，阻止迟到结果回写。
	FinishTask(ctx context.Context, userID, taskID uuid.UUID, status SupervisorTaskStatus, result map[string]any, errorCode, errorMessage string, now time.Time) (SupervisorTask, error)
	// CancelPendingTasks 取消主 Run 下所有待执行任务，用于预算超限或父取消。
	CancelPendingTasks(ctx context.Context, userID, parentRunID uuid.UUID, reason string, now time.Time) error
}

// SupervisorWorker 是 Planner 输出的单个 Worker 委派。
type SupervisorWorker struct {
	// Name Worker 名称，同一委派内唯一。
	Name string `json:"name"`
	// Task Worker 任务描述。
	Task string `json:"task"`
	// Tools 请求使用的工具名，必须与主 Agent 可用工具取交集。
	Tools []string `json:"tools"`
}

// SupervisorDraft 是 Supervisor 模型输出的委派 JSON。
type SupervisorDraft struct {
	// Workers Worker 列表。
	Workers []SupervisorWorker `json:"workers"`
}

// ParseSupervisorDraft 解析并校验 Supervisor 委派 JSON；最多 4 个 Worker。
func ParseSupervisorDraft(raw string, maxWorkers int) (SupervisorDraft, error) {
	if maxWorkers <= 0 || maxWorkers > 4 {
		maxWorkers = 4
	}
	payload, err := extractJSONObject(raw)
	if err != nil {
		return SupervisorDraft{}, err
	}
	decoder := jsonNewDecoder(payload)
	var draft SupervisorDraft
	if err := decoder.Decode(&draft); err != nil {
		return SupervisorDraft{}, fmt.Errorf("%w: decode supervisor draft: %v", ErrSupervisorInvalid, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return SupervisorDraft{}, err
	}
	if len(draft.Workers) == 0 || len(draft.Workers) > maxWorkers {
		return SupervisorDraft{}, fmt.Errorf("%w: workers must contain 1-%d items", ErrSupervisorInvalid, maxWorkers)
	}
	names := make(map[string]struct{}, len(draft.Workers))
	for index := range draft.Workers {
		worker := &draft.Workers[index]
		worker.Name = strings.TrimSpace(worker.Name)
		worker.Task = strings.TrimSpace(worker.Task)
		if worker.Name == "" || len(worker.Name) > 256 {
			return SupervisorDraft{}, fmt.Errorf("%w: worker %d name must contain 1-256 bytes", ErrSupervisorInvalid, index+1)
		}
		if _, duplicated := names[worker.Name]; duplicated {
			return SupervisorDraft{}, fmt.Errorf("%w: duplicated worker name %q", ErrSupervisorInvalid, worker.Name)
		}
		names[worker.Name] = struct{}{}
		if worker.Task == "" || len(worker.Task) > 8192 {
			return SupervisorDraft{}, fmt.Errorf("%w: worker %q task must contain 1-8192 bytes", ErrSupervisorInvalid, worker.Name)
		}
		if len(worker.Tools) > 32 {
			return SupervisorDraft{}, fmt.Errorf("%w: worker %q requests too many tools", ErrSupervisorInvalid, worker.Name)
		}
		seen := make(map[string]struct{}, len(worker.Tools))
		for _, tool := range worker.Tools {
			tool = strings.TrimSpace(tool)
			if tool == "" || len(tool) > 256 {
				return SupervisorDraft{}, fmt.Errorf("%w: worker %q requests an invalid tool", ErrSupervisorInvalid, worker.Name)
			}
			if _, duplicated := seen[tool]; duplicated {
				return SupervisorDraft{}, fmt.Errorf("%w: worker %q requests duplicated tool %q", ErrSupervisorInvalid, worker.Name, tool)
			}
			seen[tool] = struct{}{}
		}
	}
	return draft, nil
}

// SupervisorTaskTerminal 判断子任务是否终态。
func SupervisorTaskTerminal(status SupervisorTaskStatus) bool {
	switch status {
	case SupervisorTaskCompleted, SupervisorTaskFailed, SupervisorTaskCancelled:
		return true
	default:
		return false
	}
}

// jsonNewDecoder 构造严格 JSON 解码器，拒绝未知字段。
func jsonNewDecoder(payload []byte) *json.Decoder {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	return decoder
}

// ensureJSONEOF 确认 JSON 对象后没有多余内容。
func ensureJSONEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing supervisor draft content", ErrSupervisorInvalid)
	}
	return nil
}
