package biz

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SkillStatus Skill 模板生命周期。
type SkillStatus string

const (
	// SkillDraft 草稿，可编辑。
	SkillDraft SkillStatus = "draft"
	// SkillPublished 已发布，可被 Agent 绑定。
	SkillPublished SkillStatus = "published"
	// SkillDisabled 已停用。
	SkillDisabled SkillStatus = "disabled"
)

// SkillTemplate 是平台级 Skill 模板，供 Agent 复用提示词与参数定义。
type SkillTemplate struct {
	// ID Skill 唯一标识。
	ID uuid.UUID
	// Name Skill 名称。
	Name string
	// Version 版本号，与名称共同唯一。
	Version int
	// Description Skill 描述。
	Description string
	// PromptTemplate 提示词模板，上限 256KB。
	PromptTemplate string
	// Parameters 参数定义 JSON Schema。
	Parameters map[string]any
	// Status 生命周期状态。
	Status SkillStatus
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 更新时间（UTC）。
	UpdatedAt time.Time
}

// SkillRepository 定义 Skill 模板持久化契约，写操作与审计事件同事务提交。
type SkillRepository interface {
	// CreateSkill 创建 Skill 模板并写入审计事件。
	CreateSkill(ctx context.Context, actorID uuid.UUID, skill SkillTemplate, event OutboxEvent) (SkillTemplate, error)
	// UpdateSkill 更新 Skill 模板。
	UpdateSkill(ctx context.Context, actorID uuid.UUID, skill SkillTemplate, event OutboxEvent) (SkillTemplate, error)
	// DeleteSkill 删除 Skill 模板。
	DeleteSkill(ctx context.Context, actorID, skillID uuid.UUID, event OutboxEvent) error
	// GetSkill 按 ID 读取 Skill 模板。
	GetSkill(ctx context.Context, skillID uuid.UUID) (SkillTemplate, error)
	// ListSkills 列出全部 Skill 模板。
	ListSkills(ctx context.Context) ([]SkillTemplate, error)
}

// SkillService 实现平台级 Skill 管理，仅管理员可写。
type SkillService struct {
	repo SkillRepository
}

// NewSkillService 构造 Skill 服务。
func NewSkillService(repo SkillRepository) (*SkillService, error) {
	if repo == nil {
		return nil, invalid("skill repository is required")
	}
	return &SkillService{repo: repo}, nil
}

// Create 创建 Skill 草稿，版本固定为 1。
func (s *SkillService) Create(ctx context.Context, principal Principal, skill SkillTemplate) (SkillTemplate, error) {
	if err := RequireAdmin(principal); err != nil {
		return SkillTemplate{}, err
	}
	skill.ID = uuid.New()
	skill.Version = 1
	skill.Status = SkillDraft
	if err := validateSkill(&skill); err != nil {
		return SkillTemplate{}, err
	}
	return s.repo.CreateSkill(ctx, principal.UserID, skill, mutationEvent(principal.UserID, "skill.created", "skill", skill.ID))
}

// Update 更新 Skill 模板；已发布模板不允许原地修改。
func (s *SkillService) Update(ctx context.Context, principal Principal, skill SkillTemplate) (SkillTemplate, error) {
	if err := RequireAdmin(principal); err != nil {
		return SkillTemplate{}, err
	}
	current, err := s.repo.GetSkill(ctx, skill.ID)
	if err != nil {
		return SkillTemplate{}, err
	}
	if current.Status == SkillPublished {
		return SkillTemplate{}, fmt.Errorf("%w: published skills are immutable", ErrResourceConflict)
	}
	// 名称与版本由服务端保持，防止通过更新绕过唯一约束或篡改历史版本。
	skill.Name = current.Name
	skill.Version = current.Version
	if skill.Status == "" {
		skill.Status = current.Status
	}
	if err := validateSkill(&skill); err != nil {
		return SkillTemplate{}, err
	}
	return s.repo.UpdateSkill(ctx, principal.UserID, skill, mutationEvent(principal.UserID, "skill.updated", "skill", skill.ID))
}

// Delete 删除 Skill 模板。
func (s *SkillService) Delete(ctx context.Context, principal Principal, skillID uuid.UUID) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	return s.repo.DeleteSkill(ctx, principal.UserID, skillID, mutationEvent(principal.UserID, "skill.deleted", "skill", skillID))
}

// Get 读取 Skill 模板。
func (s *SkillService) Get(ctx context.Context, principal Principal, skillID uuid.UUID) (SkillTemplate, error) {
	if err := RequireAdmin(principal); err != nil {
		return SkillTemplate{}, err
	}
	return s.repo.GetSkill(ctx, skillID)
}

// List 列出全部 Skill 模板。
func (s *SkillService) List(ctx context.Context, principal Principal) ([]SkillTemplate, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	return s.repo.ListSkills(ctx)
}

// validateSkill 校验名称、模板长度、状态与参数 JSON 大小。
func validateSkill(skill *SkillTemplate) error {
	skill.Name = strings.TrimSpace(skill.Name)
	if skill.ID == uuid.Nil || skill.Name == "" || len(skill.Name) > 128 || skill.Version <= 0 {
		return invalid("skill id, name and positive version are required")
	}
	if strings.TrimSpace(skill.PromptTemplate) == "" || len(skill.PromptTemplate) > 256*1024 {
		return invalid("skill prompt template must contain between 1 and 262144 bytes")
	}
	if len(skill.Description) > 4096 {
		return invalid("skill description exceeds 4096 bytes")
	}
	if skill.Status != SkillDraft && skill.Status != SkillPublished && skill.Status != SkillDisabled {
		return invalid("unsupported skill status")
	}
	if err := validateJSONSize(skill.Parameters, 64*1024); err != nil {
		return err
	}
	return nil
}
