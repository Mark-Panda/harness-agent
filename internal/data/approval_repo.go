package data

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// toolApprovalModel 对应 tool_approvals 表：中高风险工具人工审批与执行事实。
type toolApprovalModel struct {
	// ID 审批主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// RunID 所属 Run。
	RunID uuid.UUID `gorm:"type:uuid"`
	// StepID 请求审批的工具步骤。
	StepID *uuid.UUID `gorm:"type:uuid"`
	// SessionID 发起工具调用的会话，零值表示主会话。
	SessionID *uuid.UUID `gorm:"type:uuid"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// ToolCallID 模型产生的工具调用 ID。
	ToolCallID string
	// ToolName 规范化工具名。
	ToolName string
	// MCPServerID 是审批时选中的 MCP 服务 ID，旧审批为空。
	MCPServerID *uuid.UUID `gorm:"column:mcp_server_id;type:uuid"`
	// MCPToolName 是发送给远端的原始工具名。
	MCPToolName string `gorm:"column:mcp_tool_name"`
	// MCPEndpointFingerprint 用于拒绝指向新端点的旧审批。
	MCPEndpointFingerprint string `gorm:"column:mcp_endpoint_fingerprint"`
	// Arguments 待确认参数快照。
	Arguments datatypes.JSON
	// RiskLevel 风险等级。
	RiskLevel string
	// Status 审批状态。
	Status string
	// IdempotencyKey 工具执行幂等键。
	IdempotencyKey string
	// ResultJSON 工具执行结果快照。
	ResultJSON datatypes.JSON
	// ErrorCode 执行失败业务码。
	ErrorCode string
	// ErrorMessage 脱敏失败原因。
	ErrorMessage string
	// DecidedBy 决策人用户 ID。
	DecidedBy *uuid.UUID `gorm:"type:uuid"`
	// DecidedAt 决策时间。
	DecidedAt *time.Time
	// DeliveredAt 结果回填时间。
	DeliveredAt *time.Time
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定审批表名。
func (toolApprovalModel) TableName() string { return "tool_approvals" }

// ToolApprovalRepository 是审批表的 GORM 实现，所有查询强制携带 user_id。
type ToolApprovalRepository struct {
	db *gorm.DB
}

// NewToolApprovalRepository 构造审批仓库。
func NewToolApprovalRepository(db *gorm.DB) *ToolApprovalRepository {
	return &ToolApprovalRepository{db: db}
}

// CreateApproval 幂等创建审批；同一 Run 与 tool_call_id 已存在时返回既有记录。
func (r *ToolApprovalRepository) CreateApproval(ctx context.Context, approval biz.ToolApproval) (biz.ToolApproval, error) {
	if approval.RunID == uuid.Nil || approval.UserID == uuid.Nil || approval.ToolCallID == "" {
		return biz.ToolApproval{}, fmt.Errorf("%w: approval requires run, user and tool call ids", biz.ErrInvalidConfig)
	}
	if approval.ID == uuid.Nil {
		approval.ID = uuid.New()
	}
	if approval.Status == "" {
		approval.Status = biz.ApprovalPending
	}
	if approval.RiskLevel == "" {
		approval.RiskLevel = "high"
	}
	if approval.IdempotencyKey == "" {
		return biz.ToolApproval{}, fmt.Errorf("%w: approval idempotency key is required", biz.ErrInvalidConfig)
	}
	var mcpServerID *uuid.UUID
	target := approval.MCPBinding
	if target.ServerID != uuid.Nil || target.OriginalName != "" || target.EndpointFingerprint != "" {
		if target.ServerID == uuid.Nil || target.OriginalName == "" || target.EndpointFingerprint == "" {
			return biz.ToolApproval{}, fmt.Errorf("%w: approval MCP target is incomplete", biz.ErrInvalidConfig)
		}
		mcpServerID = &target.ServerID
	}
	arguments := bytes.TrimSpace(approval.Arguments)
	if !json.Valid(arguments) || (arguments[0] != '{' && arguments[0] != '[') {
		return biz.ToolApproval{}, fmt.Errorf("%w: approval arguments must be a JSON object or array", biz.ErrInvalidConfig)
	}
	var result datatypes.JSON
	if len(approval.ResultJSON) > 0 && json.Valid(approval.ResultJSON) {
		result = datatypes.JSON(approval.ResultJSON)
	}
	now := time.Now().UTC()
	var sessionID *uuid.UUID
	if approval.SessionID != uuid.Nil {
		value := approval.SessionID
		sessionID = &value
	}
	record := toolApprovalModel{
		ID: approval.ID, RunID: approval.RunID, StepID: approval.StepID, SessionID: sessionID, UserID: approval.UserID,
		ToolCallID: approval.ToolCallID, ToolName: approval.ToolName, Arguments: datatypes.JSON(arguments),
		MCPServerID: mcpServerID, MCPToolName: target.OriginalName, MCPEndpointFingerprint: target.EndpointFingerprint,
		RiskLevel: approval.RiskLevel, Status: string(approval.Status), IdempotencyKey: approval.IdempotencyKey,
		ResultJSON: result, ErrorCode: approval.ErrorCode, ErrorMessage: approval.ErrorMessage,
		DecidedBy: approval.DecidedBy, DecidedAt: approval.DecidedAt, DeliveredAt: approval.DeliveredAt,
		CreatedAt: now, UpdatedAt: now,
	}
	if approval.CreatedAt.IsZero() {
		approval.CreatedAt = now
	}
	var stored toolApprovalModel
	err := WithScope(ctx, r.db, Scope{UserID: approval.UserID}, func(tx *gorm.DB) error {
		// 唯一键 (run_id, tool_call_id) 保证模型重放同一调用时不会产生多条审批。
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "run_id"}, {Name: "tool_call_id"}},
			DoNothing: true,
		}).Create(&record)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			// Legacy approvals omit session_id for the Run's main session. Compare the
			// effective session and let PostgreSQL compare JSONB without float rounding.
			err := tx.Where("user_id = ? AND run_id = ? AND tool_call_id = ?", approval.UserID, approval.RunID, approval.ToolCallID).
				Where("tool_name = ? AND risk_level = ? AND arguments = ?::jsonb", approval.ToolName, approval.RiskLevel, string(arguments)).
				Where("mcp_server_id IS NOT DISTINCT FROM ?::uuid AND mcp_tool_name = ? AND mcp_endpoint_fingerprint = ?",
					mcpServerID, target.OriginalName, target.EndpointFingerprint).
				Where(`EXISTS (
					SELECT 1 FROM agent_runs AS ar
					WHERE ar.id = tool_approvals.run_id AND ar.user_id = tool_approvals.user_id
					  AND COALESCE(tool_approvals.session_id, ar.session_id) = COALESCE(?::uuid, ar.session_id)
				)`, sessionID).
				First(&stored).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: approval tool call %q reused with different execution details",
					biz.ErrResourceConflict, approval.ToolCallID)
			}
			return err
		}
		stored = record
		return nil
	})
	if err != nil {
		return biz.ToolApproval{}, catalogError("create tool approval", err)
	}
	return toBizApproval(stored)
}

// GetApproval 按用户与审批 ID 读取。
func (r *ToolApprovalRepository) GetApproval(ctx context.Context, userID, approvalID uuid.UUID) (biz.ToolApproval, error) {
	var record toolApprovalModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ? AND user_id = ?", approvalID, userID).Error
	})
	if err != nil {
		return biz.ToolApproval{}, catalogError("get tool approval", err)
	}
	return toBizApproval(record)
}

// ListApprovals 按创建时间升序返回 Run 的全部审批。
func (r *ToolApprovalRepository) ListApprovals(ctx context.Context, userID, runID uuid.UUID) ([]biz.ToolApproval, error) {
	var records []toolApprovalModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND run_id = ?", userID, runID).
			Order("created_at, id").Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list tool approvals", err)
	}
	approvals := make([]biz.ToolApproval, 0, len(records))
	for _, record := range records {
		approval, err := toBizApproval(record)
		if err != nil {
			return nil, err
		}
		approvals = append(approvals, approval)
	}
	return approvals, nil
}

// CountPendingApprovals 统计 Run 中仍待决策的审批数量。
func (r *ToolApprovalRepository) CountPendingApprovals(ctx context.Context, userID, runID uuid.UUID) (int, error) {
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&toolApprovalModel{}).
			Where("user_id = ? AND run_id = ? AND status = ?", userID, runID, string(biz.ApprovalPending)).
			Count(&count).Error
	})
	if err != nil {
		return 0, catalogError("count pending tool approvals", err)
	}
	return int(count), nil
}

// DecideApproval 以 CAS 方式把 pending 推进到目标状态；重复相同决策幂等返回。
func (r *ToolApprovalRepository) DecideApproval(ctx context.Context, userID, approvalID uuid.UUID, decision biz.ApprovalDecision, decidedBy uuid.UUID, now time.Time) (biz.ToolApproval, error) {
	if decidedBy != userID {
		return biz.ToolApproval{}, biz.ErrForbidden
	}
	var target biz.ApprovalStatus
	switch decision {
	case biz.ApprovalDecisionApprove:
		target = biz.ApprovalApproved
	case biz.ApprovalDecisionDeny:
		target = biz.ApprovalDenied
	default:
		return biz.ToolApproval{}, fmt.Errorf("%w: unsupported approval decision", biz.ErrInvalidConfig)
	}
	var record toolApprovalModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.First(&record, "id = ? AND user_id = ?", approvalID, userID).Error; err != nil {
			return err
		}
		if biz.ApprovalStatus(record.Status) == target {
			// 同一决策重试直接返回既有结果，客户端超时重放是安全的。
			return nil
		}
		if biz.ApprovalStatus(record.Status) != biz.ApprovalPending {
			return fmt.Errorf("%w: approval is already %s", biz.ErrResourceConflict, record.Status)
		}
		result := tx.Model(&toolApprovalModel{}).
			Where("id = ? AND user_id = ? AND status = ?", approvalID, userID, string(biz.ApprovalPending)).
			Updates(map[string]any{
				"status": string(target), "decided_by": decidedBy, "decided_at": now, "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: approval status changed concurrently", biz.ErrResourceConflict)
		}
		return tx.First(&record, "id = ? AND user_id = ?", approvalID, userID).Error
	})
	if err != nil {
		return biz.ToolApproval{}, catalogError("decide tool approval", err)
	}
	return toBizApproval(record)
}

// ClaimApprovalExecution atomically authorizes dispatch against the live Run and Plan.
func (r *ToolApprovalRepository) ClaimApprovalExecution(ctx context.Context, userID, runID, approvalID uuid.UUID, fence biz.RunFence, now time.Time) (biz.ToolApproval, error) {
	var record toolApprovalModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		mode, err := lockRunningRunLease(tx, userID, runID, fence, now)
		if err != nil {
			return err
		}
		claim := tx.Model(&toolApprovalModel{}).
			Where("id = ? AND user_id = ? AND run_id = ? AND status = ?", approvalID, userID, runID, biz.ApprovalApproved)
		if mode == biz.AgentModePlan {
			plan, err := lockLatestRunPlan(tx, userID, runID)
			if err != nil {
				return err
			}
			switch biz.PlanStatus(plan.Status) {
			case biz.PlanPaused:
				return biz.ErrPlanPaused
			case biz.PlanCancelled:
				return biz.ErrPlanCancelled
			case biz.PlanExecuting, biz.PlanWaitingApproval:
				claim = claim.Where(`EXISTS (
					SELECT 1 FROM plan_steps
					WHERE plan_id = ? AND user_id = ? AND status = 'waiting_approval'
					  AND tool_approvals.tool_call_id = 'plan-step-' || plan_steps.id::text
				)`, plan.ID, userID)
			default:
				return fmt.Errorf("%w: plan is not accepting tool execution", biz.ErrResourceConflict)
			}
		}
		result := claim.
			Updates(map[string]any{"status": string(biz.ApprovalExecuting), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: approval is not approved for execution", biz.ErrResourceConflict)
		}
		return tx.First(&record, "id = ? AND user_id = ? AND run_id = ?", approvalID, userID, runID).Error
	})
	if err != nil {
		return biz.ToolApproval{}, catalogError("claim tool approval execution", err)
	}
	return toBizApproval(record)
}

// FinishApprovalExecution 以 CAS 方式把 executing 推进到 executed/failed，并保存结果或脱敏错误。
func (r *ToolApprovalRepository) FinishApprovalExecution(ctx context.Context, userID, approvalID uuid.UUID, status biz.ApprovalStatus, result json.RawMessage, errorCode, errorMessage string, now time.Time) (biz.ToolApproval, error) {
	if status != biz.ApprovalExecuted && status != biz.ApprovalFailed {
		return biz.ToolApproval{}, fmt.Errorf("%w: invalid approval execution result status", biz.ErrInvalidConfig)
	}
	var record toolApprovalModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.First(&record, "id = ? AND user_id = ?", approvalID, userID).Error; err != nil {
			return err
		}
		current := biz.ApprovalStatus(record.Status)
		if current == status {
			// 执行结果已写入时重复结算幂等返回，避免恢复流程覆盖既有事实。
			return nil
		}
		if err := biz.TransitionApprovalStatus(current, status); err != nil {
			return err
		}
		if len(errorMessage) > 512 {
			errorMessage = strings.ToValidUTF8(errorMessage[:512], "")
		}
		updates := map[string]any{
			"status": string(status), "error_code": errorCode, "error_message": errorMessage, "updated_at": now,
		}
		if status == biz.ApprovalExecuted && len(result) > 0 && json.Valid(result) {
			updates["result_json"] = datatypes.JSON(result)
		}
		resultRow := tx.Model(&toolApprovalModel{}).
			Where("id = ? AND user_id = ? AND status = ?", approvalID, userID, string(biz.ApprovalExecuting)).
			Updates(updates)
		if resultRow.Error != nil {
			return resultRow.Error
		}
		if resultRow.RowsAffected != 1 {
			return fmt.Errorf("%w: approval execution changed concurrently", biz.ErrResourceConflict)
		}
		return tx.First(&record, "id = ? AND user_id = ?", approvalID, userID).Error
	})
	if err != nil {
		return biz.ToolApproval{}, catalogError("finish tool approval execution", err)
	}
	return toBizApproval(record)
}

// MarkApprovalDelivered 记录结果已回填为 tool 消息；终态之外的审批不允许标记。
func (r *ToolApprovalRepository) MarkApprovalDelivered(ctx context.Context, userID, approvalID uuid.UUID, now time.Time) (biz.ToolApproval, error) {
	var record toolApprovalModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		if err := tx.First(&record, "id = ? AND user_id = ?", approvalID, userID).Error; err != nil {
			return err
		}
		if record.DeliveredAt != nil {
			return nil
		}
		if !biz.ApprovalTerminal(biz.ApprovalStatus(record.Status)) {
			return fmt.Errorf("%w: approval result is not ready for delivery", biz.ErrResourceConflict)
		}
		result := tx.Model(&toolApprovalModel{}).
			Where("id = ? AND user_id = ? AND delivered_at IS NULL AND status IN ?", approvalID, userID,
				[]string{string(biz.ApprovalExecuted), string(biz.ApprovalFailed), string(biz.ApprovalDenied)}).
			Updates(map[string]any{"delivered_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: approval delivery changed concurrently", biz.ErrResourceConflict)
		}
		return tx.First(&record, "id = ? AND user_id = ?", approvalID, userID).Error
	})
	if err != nil {
		return biz.ToolApproval{}, catalogError("mark tool approval delivered", err)
	}
	return toBizApproval(record)
}

// toBizApproval 将数据库行转换为领域审批。
func toBizApproval(record toolApprovalModel) (biz.ToolApproval, error) {
	approval := biz.ToolApproval{
		ID: record.ID, RunID: record.RunID, StepID: record.StepID, UserID: record.UserID,
		ToolCallID: record.ToolCallID, ToolName: record.ToolName, Arguments: json.RawMessage(record.Arguments),
		RiskLevel: record.RiskLevel, Status: biz.ApprovalStatus(record.Status), IdempotencyKey: record.IdempotencyKey,
		ResultJSON: json.RawMessage(record.ResultJSON), ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		DecidedBy: record.DecidedBy, DecidedAt: record.DecidedAt, DeliveredAt: record.DeliveredAt,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if record.SessionID != nil {
		approval.SessionID = *record.SessionID
	}
	if record.MCPServerID != nil {
		approval.MCPBinding.ServerID = *record.MCPServerID
	}
	approval.MCPBinding.OriginalName = record.MCPToolName
	approval.MCPBinding.EndpointFingerprint = record.MCPEndpointFingerprint
	return approval, nil
}

// 编译期断言接口实现。
var _ biz.ToolApprovalRepository = (*ToolApprovalRepository)(nil)
