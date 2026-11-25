package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// userSkillModel 对应 user_skills 表：用户级技能模板。
type userSkillModel struct {
	ID               uuid.UUID      `gorm:"type:uuid;primaryKey"`
	UserID           uuid.UUID      `gorm:"type:uuid;not null"`
	Name             string         `gorm:"not null"`
	Version          int            `gorm:"not null"`
	Description      string         `gorm:"not null;default:''"`
	PromptTemplate   string         `gorm:"column:prompt_template;not null"`
	ParametersJSON   datatypes.JSON `gorm:"column:parameters_json;not null"`
	Status           string         `gorm:"not null"`
	Origin           string         `gorm:"not null"`
	SourceSessionIDs datatypes.JSON `gorm:"column:source_session_ids;not null"`
	Confidence       float64        `gorm:"not null"`
	BundleFormat     string         `gorm:"not null;default:''"`
	BundleEntrypoint string         `gorm:"not null;default:''"`
	BundleFileCount  int            `gorm:"not null;default:0"`
	BundleTotalBytes int64          `gorm:"not null;default:0"`
	BundleHasScripts bool           `gorm:"not null;default:false"`
	BundleExecutable bool           `gorm:"column:bundle_execution_enabled;not null;default:false"`
	BundleDigest     string         `gorm:"not null;default:''"`
	UserEditedAt     *time.Time     `gorm:"column:user_edited_at"`
	CreatedAt        time.Time      `gorm:"not null"`
	UpdatedAt        time.Time      `gorm:"not null"`
}

// TableName 指定表名。
func (userSkillModel) TableName() string { return "user_skills" }

// userSkillRevisionModel 对应 user_skill_revisions 表：技能原地更新前的历史快照。
type userSkillRevisionModel struct {
	ID               uuid.UUID      `gorm:"type:uuid;primaryKey"`
	UserID           uuid.UUID      `gorm:"type:uuid;not null"`
	SkillID          uuid.UUID      `gorm:"column:skill_id;type:uuid;not null"`
	Version          int            `gorm:"not null"`
	Description      string         `gorm:"not null;default:''"`
	PromptTemplate   string         `gorm:"column:prompt_template;not null"`
	ParametersJSON   datatypes.JSON `gorm:"column:parameters_json;not null"`
	SourceSessionIDs datatypes.JSON `gorm:"column:source_session_ids;not null"`
	Confidence       float64        `gorm:"not null"`
	CreatedAt        time.Time      `gorm:"not null"`
}

// TableName 指定表名。
func (userSkillRevisionModel) TableName() string { return "user_skill_revisions" }

// userSkillSynthesisSettingsModel 对应 user_skill_synthesis_settings 表。
type userSkillSynthesisSettingsModel struct {
	UserID             uuid.UUID  `gorm:"type:uuid;primaryKey"`
	Enabled            bool       `gorm:"not null;default:false"`
	ModelPolicyID      *uuid.UUID `gorm:"type:uuid"`
	ModelPolicyVersion *int
	CreatedAt          time.Time `gorm:"not null"`
	UpdatedAt          time.Time `gorm:"not null"`
}

// TableName 指定表名。
func (userSkillSynthesisSettingsModel) TableName() string { return "user_skill_synthesis_settings" }

// UserSkillRepository 是 user_skills 表的 GORM 实现。
type UserSkillRepository struct {
	db *gorm.DB
}

// NewUserSkillRepository 构造用户技能仓库。
func NewUserSkillRepository(db *gorm.DB) *UserSkillRepository {
	return &UserSkillRepository{db: db}
}

// fromBizUserSkill 将领域模型转为 GORM 模型。
func fromBizUserSkill(skill biz.UserSkill) (userSkillModel, error) {
	parameters := skill.Parameters
	if parameters == nil {
		parameters = map[string]any{}
	}
	parametersJSON, err := json.Marshal(parameters)
	if err != nil {
		return userSkillModel{}, err
	}
	sourceIDs := skill.SourceSessionIDs
	if sourceIDs == nil {
		sourceIDs = []uuid.UUID{}
	}
	sourceJSON, err := json.Marshal(sourceIDs)
	if err != nil {
		return userSkillModel{}, err
	}
	return userSkillModel{
		ID: skill.ID, UserID: skill.UserID, Name: skill.Name, Version: skill.Version,
		Description: skill.Description, PromptTemplate: skill.PromptTemplate,
		ParametersJSON: parametersJSON, Status: string(skill.Status), Origin: string(skill.Origin),
		SourceSessionIDs: sourceJSON, Confidence: skill.Confidence,
		BundleFormat: skill.Bundle.Format, BundleEntrypoint: skill.Bundle.Entrypoint,
		BundleFileCount: skill.Bundle.FileCount, BundleTotalBytes: skill.Bundle.TotalBytes,
		BundleHasScripts: skill.Bundle.HasScripts, BundleExecutable: skill.Bundle.ExecutionEnabled,
		BundleDigest: skill.Bundle.Digest, UserEditedAt: skill.UserEditedAt,
		CreatedAt: skill.CreatedAt, UpdatedAt: skill.UpdatedAt,
	}, nil
}

// toUserSkill 将 GORM 模型转为领域模型。
func toUserSkill(record userSkillModel) (biz.UserSkill, error) {
	var parameters map[string]any
	if len(record.ParametersJSON) > 0 {
		if err := json.Unmarshal(record.ParametersJSON, &parameters); err != nil {
			return biz.UserSkill{}, err
		}
	}
	if parameters == nil {
		parameters = map[string]any{}
	}
	var sourceIDs []uuid.UUID
	if len(record.SourceSessionIDs) > 0 {
		if err := json.Unmarshal(record.SourceSessionIDs, &sourceIDs); err != nil {
			return biz.UserSkill{}, err
		}
	}
	if sourceIDs == nil {
		sourceIDs = []uuid.UUID{}
	}
	return biz.UserSkill{
		ID: record.ID, UserID: record.UserID, Name: record.Name, Version: record.Version,
		Description: record.Description, PromptTemplate: record.PromptTemplate,
		Parameters: parameters, Status: biz.UserSkillStatus(record.Status), Origin: biz.UserSkillOrigin(record.Origin),
		SourceSessionIDs: sourceIDs, Confidence: record.Confidence,
		Bundle: biz.SkillBundleInfo{
			Format: record.BundleFormat, Entrypoint: record.BundleEntrypoint,
			FileCount: record.BundleFileCount, TotalBytes: record.BundleTotalBytes,
			HasScripts: record.BundleHasScripts, ExecutionEnabled: record.BundleExecutable,
			Digest: record.BundleDigest,
		},
		UserEditedAt: record.UserEditedAt,
		CreatedAt:    record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// CreateUserSkill 在用户作用域事务内创建技能并追加审计事件。
func (r *UserSkillRepository) CreateUserSkill(ctx context.Context, skill biz.UserSkill, event biz.OutboxEvent) (biz.UserSkill, error) {
	record, err := fromBizUserSkill(skill)
	if err != nil {
		return biz.UserSkill{}, err
	}
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	err = WithScope(ctx, r.db, Scope{UserID: skill.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.UserSkill{}, catalogError("create user skill", err)
	}
	return r.GetUserSkill(ctx, skill.UserID, skill.ID)
}

// maxUserSkillRevisions 是单个技能保留的历史快照上限。
const maxUserSkillRevisions = 50

// UpdateUserSkill 更新技能正文、来源会话与置信度，并在同一事务内保留更新前的历史快照。
// 状态流转由服务层保证。
func (r *UserSkillRepository) UpdateUserSkill(ctx context.Context, skill biz.UserSkill, event biz.OutboxEvent) (biz.UserSkill, error) {
	record, err := fromBizUserSkill(skill)
	if err != nil {
		return biz.UserSkill{}, err
	}
	err = WithScope(ctx, r.db, Scope{UserID: skill.UserID}, func(tx *gorm.DB) error {
		var current userSkillModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&current, "id = ? AND user_id = ?", skill.ID, skill.UserID).Error; err != nil {
			return err
		}
		// 内容未变化时只更新元数据，不产生无意义的历史快照。
		revisionInserted := false
		if current.Description != record.Description ||
			current.PromptTemplate != record.PromptTemplate ||
			!jsonContentEqual(current.ParametersJSON, record.ParametersJSON) {
			revision := userSkillRevisionModel{
				ID: uuid.New(), UserID: current.UserID, SkillID: current.ID, Version: current.Version,
				Description: current.Description, PromptTemplate: current.PromptTemplate,
				ParametersJSON: current.ParametersJSON, SourceSessionIDs: current.SourceSessionIDs,
				Confidence: current.Confidence, CreatedAt: time.Now().UTC(),
			}
			if err := tx.Create(&revision).Error; err != nil {
				return err
			}
			revisionInserted = true
		}
		if revisionInserted {
			// 每个技能只保留最近 maxUserSkillRevisions 条历史，避免长期无限增长。
			if err := tx.Exec(`
DELETE FROM user_skill_revisions
WHERE id IN (
  SELECT id FROM user_skill_revisions
  WHERE user_id = ? AND skill_id = ?
  ORDER BY created_at DESC
  OFFSET ?
)`, skill.UserID, skill.ID, maxUserSkillRevisions).Error; err != nil {
				return err
			}
		}
		result := tx.Model(&userSkillModel{}).Where("id = ? AND user_id = ?", skill.ID, skill.UserID).Updates(map[string]any{
			"description": record.Description, "prompt_template": record.PromptTemplate,
			"parameters_json": record.ParametersJSON, "source_session_ids": record.SourceSessionIDs,
			"confidence": record.Confidence, "user_edited_at": record.UserEditedAt,
			"updated_at": time.Now().UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.UserSkill{}, catalogError("update user skill", err)
	}
	return r.GetUserSkill(ctx, skill.UserID, skill.ID)
}

// GetUserSkill 按用户与技能 ID 读取。
func (r *UserSkillRepository) GetUserSkill(ctx context.Context, userID, skillID uuid.UUID) (biz.UserSkill, error) {
	var record userSkillModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("id = ? AND user_id = ?", skillID, userID).First(&record).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return biz.UserSkill{}, fmt.Errorf("%w: user skill not found", biz.ErrResourceNotFound)
		}
		return biz.UserSkill{}, catalogError("get user skill", err)
	}
	return toUserSkill(record)
}

// ListUserSkills 按更新时间倒序列出用户全部技能。
func (r *UserSkillRepository) ListUserSkills(ctx context.Context, userID uuid.UUID, limit int) ([]biz.UserSkill, error) {
	if limit <= 0 {
		limit = 50
	}
	var records []userSkillModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ?", userID).Order("updated_at DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list user skills", err)
	}
	result := make([]biz.UserSkill, 0, len(records))
	for _, record := range records {
		skill, err := toUserSkill(record)
		if err != nil {
			return nil, err
		}
		result = append(result, skill)
	}
	return result, nil
}

// ListUserSkillsByIDs 批量读取用户技能，用于 Agent 快照校验。
func (r *UserSkillRepository) ListUserSkillsByIDs(ctx context.Context, userID uuid.UUID, skillIDs []uuid.UUID) ([]biz.UserSkill, error) {
	if len(skillIDs) == 0 {
		return []biz.UserSkill{}, nil
	}
	var records []userSkillModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND id IN ?", userID, skillIDs).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list user skills by ids", err)
	}
	result := make([]biz.UserSkill, 0, len(records))
	for _, record := range records {
		skill, err := toUserSkill(record)
		if err != nil {
			return nil, err
		}
		result = append(result, skill)
	}
	return result, nil
}

// ListUserSkillRevisions 按时间倒序读取技能历史快照。
func (r *UserSkillRepository) ListUserSkillRevisions(ctx context.Context, userID, skillID uuid.UUID, limit int) ([]biz.UserSkillRevision, error) {
	if limit <= 0 {
		limit = 20
	}
	var records []userSkillRevisionModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ? AND skill_id = ?", userID, skillID).
			Order("created_at DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list user skill revisions", err)
	}
	revisions := make([]biz.UserSkillRevision, 0, len(records))
	for _, record := range records {
		revision, err := toBizUserSkillRevision(record)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, nil
}

// GetUserSkillRevision 读取单条技能历史快照。
func (r *UserSkillRepository) GetUserSkillRevision(ctx context.Context, userID, skillID, revisionID uuid.UUID) (biz.UserSkillRevision, error) {
	var record userSkillRevisionModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("id = ? AND user_id = ? AND skill_id = ?", revisionID, userID, skillID).First(&record).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return biz.UserSkillRevision{}, fmt.Errorf("%w: skill revision not found", biz.ErrResourceNotFound)
		}
		return biz.UserSkillRevision{}, catalogError("get user skill revision", err)
	}
	return toBizUserSkillRevision(record)
}

// toBizUserSkillRevision 把历史快照行转换为领域结构。
func toBizUserSkillRevision(record userSkillRevisionModel) (biz.UserSkillRevision, error) {
	var parameters map[string]any
	if len(record.ParametersJSON) > 0 {
		if err := json.Unmarshal(record.ParametersJSON, &parameters); err != nil {
			return biz.UserSkillRevision{}, err
		}
	}
	if parameters == nil {
		parameters = map[string]any{}
	}
	var sourceIDs []uuid.UUID
	if len(record.SourceSessionIDs) > 0 {
		if err := json.Unmarshal(record.SourceSessionIDs, &sourceIDs); err != nil {
			return biz.UserSkillRevision{}, err
		}
	}
	if sourceIDs == nil {
		sourceIDs = []uuid.UUID{}
	}
	return biz.UserSkillRevision{
		ID: record.ID, UserID: record.UserID, SkillID: record.SkillID, Version: record.Version,
		Description: record.Description, PromptTemplate: record.PromptTemplate, Parameters: parameters,
		SourceSessionIDs: sourceIDs, Confidence: record.Confidence, CreatedAt: record.CreatedAt,
	}, nil
}

// jsonContentEqual 按语义比较两段 JSON，避免 JSONB 文本格式差异造成误判。
func jsonContentEqual(left, right datatypes.JSON) bool {
	if string(left) == string(right) {
		return true
	}
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

// PublishUserSkill 将技能状态置为 published。
func (r *UserSkillRepository) PublishUserSkill(ctx context.Context, userID, skillID uuid.UUID, event biz.OutboxEvent) (biz.UserSkill, error) {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&userSkillModel{}).Where("id = ? AND user_id = ?", skillID, userID).Updates(map[string]any{
			"status": string(biz.UserSkillPublished), "updated_at": time.Now().UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.UserSkill{}, catalogError("publish user skill", err)
	}
	return r.GetUserSkill(ctx, userID, skillID)
}

// DisableUserSkill 将技能状态置为 disabled。
func (r *UserSkillRepository) DisableUserSkill(ctx context.Context, userID, skillID uuid.UUID, event biz.OutboxEvent) (biz.UserSkill, error) {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&userSkillModel{}).Where("id = ? AND user_id = ?", skillID, userID).Updates(map[string]any{
			"status": string(biz.UserSkillDisabled), "updated_at": time.Now().UTC(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.UserSkill{}, catalogError("disable user skill", err)
	}
	return r.GetUserSkill(ctx, userID, skillID)
}

// DeleteUserSkill 删除技能。
func (r *UserSkillRepository) DeleteUserSkill(ctx context.Context, userID, skillID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Where("id = ? AND user_id = ?", skillID, userID).Delete(&userSkillModel{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return catalogError("delete user skill", err)
	}
	return nil
}

// GetUserSkillSynthesisSettings 读取当前用户设置；不存在时返回默认关闭状态。
func (r *UserSkillRepository) GetUserSkillSynthesisSettings(ctx context.Context, userID uuid.UUID) (biz.UserSkillSynthesisSettings, error) {
	var record userSkillSynthesisSettingsModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ?", userID).First(&record).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return biz.UserSkillSynthesisSettings{UserID: userID}, nil
	}
	if err != nil {
		return biz.UserSkillSynthesisSettings{}, catalogError("get user skill synthesis settings", err)
	}
	return biz.UserSkillSynthesisSettings{
		UserID: record.UserID, Enabled: record.Enabled,
		ModelPolicyID: record.ModelPolicyID, ModelPolicyVersion: record.ModelPolicyVersion,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// UpsertUserSkillSynthesisSettings 完整替换当前用户设置并追加审计事件。
func (r *UserSkillRepository) UpsertUserSkillSynthesisSettings(ctx context.Context, settings biz.UserSkillSynthesisSettings, event biz.OutboxEvent) (biz.UserSkillSynthesisSettings, error) {
	now := time.Now().UTC()
	record := userSkillSynthesisSettingsModel{
		UserID: settings.UserID, Enabled: settings.Enabled,
		ModelPolicyID: settings.ModelPolicyID, ModelPolicyVersion: settings.ModelPolicyVersion,
		CreatedAt: now, UpdatedAt: now,
	}
	err := WithScope(ctx, r.db, Scope{UserID: settings.UserID}, func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"enabled", "model_policy_id", "model_policy_version", "updated_at",
			}),
		}).Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.UserSkillSynthesisSettings{}, catalogError("upsert user skill synthesis settings", err)
	}
	return r.GetUserSkillSynthesisSettings(ctx, settings.UserID)
}

// ListEnabledUserIDs 在系统任务作用域内批量筛选已开启提炼的用户。
func (r *UserSkillRepository) ListEnabledUserIDs(ctx context.Context, userIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(userIDs) == 0 {
		return []uuid.UUID{}, nil
	}
	var enabled []uuid.UUID
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Model(&userSkillSynthesisSettingsModel{}).
			Where("user_id IN ? AND enabled = ?", userIDs, true).
			Pluck("user_id", &enabled).Error
	})
	if err != nil {
		return nil, catalogError("list enabled skill synthesis users", err)
	}
	return enabled, nil
}

// EnqueueSkillSynthesis 幂等入队技能提炼任务；去重键为 skill-synth:{user_id}:{date}。
func (r *JobRepository) EnqueueSkillSynthesis(ctx context.Context, task biz.SkillSynthesisTask, availableAt time.Time) error {
	if task.UserID == uuid.Nil || task.Date == "" {
		return fmt.Errorf("%w: skill synthesis task requires user id and date", biz.ErrInvalidConfig)
	}
	if availableAt.IsZero() {
		availableAt = time.Now().UTC()
	}
	payload, err := json.Marshal(biz.EncodeSkillSynthesisTask(task))
	if err != nil {
		return err
	}
	dedupe := "skill-synth:" + task.UserID.String() + ":" + task.Date
	if requestID := strings.TrimSpace(task.RequestID); requestID != "" {
		// 手动触发使用独立去重后缀，避免被当天已完成的每日任务吸收。
		dedupe += ":" + requestID
	}
	record := jobModel{
		ID: uuid.New(), UserID: &task.UserID, JobType: biz.SkillSynthesisJobType, DedupeKey: &dedupe,
		Payload: datatypes.JSON(payload), Status: "pending", MaxAttempts: 4,
		AvailableAt: availableAt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	return WithScope(ctx, r.db, Scope{UserID: task.UserID}, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
	})
}
