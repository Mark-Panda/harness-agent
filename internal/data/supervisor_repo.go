package data

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// supervisorTaskModel 对应 supervisor_tasks 表：Worker 委派与结果事实。
type supervisorTaskModel struct {
	// ID 任务主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 原始用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// ParentRunID 主 Run。
	ParentRunID uuid.UUID `gorm:"type:uuid"`
	// ChildSessionID 独立子会话。
	ChildSessionID uuid.UUID `gorm:"type:uuid"`
	// WorkerName Worker 名称。
	WorkerName string
	// WorkerIndex Worker 序号。
	WorkerIndex int
	// Task 委派描述。
	Task string
	// ToolScope 工具名交集 JSON 数组。
	ToolScope datatypes.JSON
	// Status 任务状态。
	Status string
	// Attempts 已领取次数。
	Attempts int
	// MaxAttempts 最大领取次数。
	MaxAttempts int
	// Result 结果快照。
	Result datatypes.JSON `gorm:"column:result_json"`
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

// TableName 指定子任务表名。
func (supervisorTaskModel) TableName() string { return "supervisor_tasks" }

// SupervisorTaskRepository 是 supervisor_tasks 的 GORM 实现，所有查询强制用户隔离。
type SupervisorTaskRepository struct {
	db *gorm.DB
}

// NewSupervisorTaskRepository 构造子任务仓库。
func NewSupervisorTaskRepository(db *gorm.DB) *SupervisorTaskRepository {
	return &SupervisorTaskRepository{db: db}
}

// CreateTasks 在同一事务内创建全部子任务与审计事件。
func (r *SupervisorTaskRepository) CreateTasks(ctx context.Context, userID, parentRunID uuid.UUID, tasks []biz.SupervisorTask, event biz.OutboxEvent) ([]biz.SupervisorTask, error) {
	if len(tasks) == 0 {
		return []biz.SupervisorTask{}, nil
	}
	records, err := fromBizSupervisorTasks(userID, parentRunID, tasks)
	if err != nil {
		return nil, err
	}
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.Create(&records).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	}); err != nil {
		return nil, catalogError("create supervisor tasks", err)
	}
	result := make([]biz.SupervisorTask, 0, len(records))
	for _, record := range records {
		task, err := toBizSupervisorTask(record)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, nil
}

// ListTasks 按 Worker 序号列出主 Run 的子任务。
func (r *SupervisorTaskRepository) ListTasks(ctx context.Context, userID, parentRunID uuid.UUID) ([]biz.SupervisorTask, error) {
	var records []supervisorTaskModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND parent_run_id = ?", userID, parentRunID).
			Order("worker_index").Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list supervisor tasks", err)
	}
	tasks := make([]biz.SupervisorTask, 0, len(records))
	for _, record := range records {
		task, err := toBizSupervisorTask(record)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// StartTask 以 pending 状态为前置条件领取任务并写入 attempts。
func (r *SupervisorTaskRepository) StartTask(ctx context.Context, userID, taskID uuid.UUID, now time.Time) (biz.SupervisorTask, error) {
	var record supervisorTaskModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Raw(`
UPDATE supervisor_tasks
SET status = 'running', attempts = attempts + 1, started_at = COALESCE(started_at, ?), updated_at = ?
WHERE id = ? AND user_id = ? AND status = 'pending' AND attempts < max_attempts
RETURNING *`, now.UTC(), now.UTC(), taskID, userID).Scan(&record).Error
	}); err != nil {
		return biz.SupervisorTask{}, catalogError("start supervisor task", err)
	}
	if record.ID == uuid.Nil {
		return biz.SupervisorTask{}, fmt.Errorf("%w: task is not claimable", biz.ErrResourceConflict)
	}
	return toBizSupervisorTask(record)
}

// FinishTask 以 running 状态为前置条件推进终态，迟到结果被拒绝。
func (r *SupervisorTaskRepository) FinishTask(ctx context.Context, userID, taskID uuid.UUID, status biz.SupervisorTaskStatus, result map[string]any, errorCode, errorMessage string, now time.Time) (biz.SupervisorTask, error) {
	if !biz.SupervisorTaskTerminal(status) {
		return biz.SupervisorTask{}, fmt.Errorf("%w: unsupported terminal status %s", biz.ErrInvalidConfig, status)
	}
	encoded, err := encodeOptionalJSON(result)
	if err != nil {
		return biz.SupervisorTask{}, err
	}
	var record supervisorTaskModel
	if err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Raw(`
UPDATE supervisor_tasks
SET status = ?, result_json = ?, error_code = ?, error_message = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND user_id = ? AND status = 'running'
RETURNING *`, string(status), encoded, errorCode, errorMessage, now.UTC(), now.UTC(), taskID, userID).Scan(&record).Error
	}); err != nil {
		return biz.SupervisorTask{}, catalogError("finish supervisor task", err)
	}
	if record.ID == uuid.Nil {
		return biz.SupervisorTask{}, fmt.Errorf("%w: task is not running", biz.ErrResourceConflict)
	}
	return toBizSupervisorTask(record)
}

// CancelPendingTasks 取消主 Run 下所有待执行任务。
func (r *SupervisorTaskRepository) CancelPendingTasks(ctx context.Context, userID, parentRunID uuid.UUID, reason string, now time.Time) error {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&supervisorTaskModel{}).
			Where("user_id = ? AND parent_run_id = ? AND status = 'pending'", userID, parentRunID).
			Updates(map[string]any{
				"status": string(biz.SupervisorTaskCancelled), "error_code": "parent_cancelled",
				"error_message": reason, "finished_at": now.UTC(), "updated_at": now.UTC(),
			}).Error
	})
	if err != nil {
		return catalogError("cancel pending supervisor tasks", err)
	}
	return nil
}

// fromBizSupervisorTasks 将领域任务转换为数据库行。
func fromBizSupervisorTasks(userID, parentRunID uuid.UUID, tasks []biz.SupervisorTask) ([]supervisorTaskModel, error) {
	records := make([]supervisorTaskModel, 0, len(tasks))
	for _, task := range tasks {
		scope, err := json.Marshal(nonNilStrings(task.ToolScope))
		if err != nil {
			return nil, err
		}
		result, err := encodeOptionalJSON(task.Result)
		if err != nil {
			return nil, err
		}
		maxAttempts := task.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 1
		}
		createdAt := task.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		records = append(records, supervisorTaskModel{
			ID: task.ID, UserID: userID, ParentRunID: parentRunID, ChildSessionID: task.ChildSessionID,
			WorkerName: task.WorkerName, WorkerIndex: task.WorkerIndex, Task: task.Task,
			ToolScope: datatypes.JSON(scope), Status: string(task.Status), Attempts: task.Attempts,
			MaxAttempts: maxAttempts, Result: result, ErrorCode: task.ErrorCode, ErrorMessage: task.ErrorMessage,
			Depth: task.Depth, StartedAt: task.StartedAt, FinishedAt: task.FinishedAt, CreatedAt: createdAt, UpdatedAt: createdAt,
		})
	}
	return records, nil
}

// toBizSupervisorTask 将数据库行转换为领域任务。
func toBizSupervisorTask(record supervisorTaskModel) (biz.SupervisorTask, error) {
	var scope []string
	if len(record.ToolScope) > 0 {
		if err := json.Unmarshal(record.ToolScope, &scope); err != nil {
			return biz.SupervisorTask{}, fmt.Errorf("decode supervisor tool scope: %w", err)
		}
	}
	var result map[string]any
	if len(record.Result) > 0 {
		if err := json.Unmarshal(record.Result, &result); err != nil {
			return biz.SupervisorTask{}, fmt.Errorf("decode supervisor result: %w", err)
		}
	}
	return biz.SupervisorTask{
		ID: record.ID, UserID: record.UserID, ParentRunID: record.ParentRunID, ChildSessionID: record.ChildSessionID,
		WorkerName: record.WorkerName, WorkerIndex: record.WorkerIndex, Task: record.Task, ToolScope: scope,
		Status: biz.SupervisorTaskStatus(record.Status), Attempts: record.Attempts, MaxAttempts: record.MaxAttempts,
		Result: result, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage, Depth: record.Depth,
		StartedAt: record.StartedAt, FinishedAt: record.FinishedAt, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// nonNilStrings 保证工具交集编码为 JSON 数组。
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

var _ biz.SupervisorTaskRepository = (*SupervisorTaskRepository)(nil)
