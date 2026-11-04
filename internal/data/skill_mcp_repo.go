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

// skillRecordModel 对应 skills 表：平台级提示词模板与参数定义。
type skillRecordModel struct {
	// ID Skill 主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Name Skill 名称。
	Name string
	// Version 版本号，与名称共同唯一。
	Version int
	// Description 描述。
	Description string
	// PromptTemplate 提示词模板。
	PromptTemplate string
	// ParametersJSON 参数定义 JSON Schema。
	ParametersJSON datatypes.JSON
	// Status 生命周期：draft/published/disabled。
	Status string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定 Skill 表名。
func (skillRecordModel) TableName() string { return "skills" }

// mcpRecordModel 对应 mcps 表：远端工具端点、鉴权类型与加密凭证。
type mcpRecordModel struct {
	// ID MCP 主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Name MCP 名称，全局唯一。
	Name string
	// Endpoint 远端 HTTPS 地址。
	Endpoint string
	// AuthType 鉴权类型：none/bearer/oauth/header。
	AuthType string
	// AuthCredentialsEncrypted 鉴权凭证密文，禁止明文入库。
	AuthCredentialsEncrypted []byte
	// KeyVersion 凭证密文对应的密钥版本。
	KeyVersion string
	// RiskLevel 风险等级。
	RiskLevel string
	// Status 启停状态。
	Status string
	// ToolTimeoutsJSON 工具超时覆盖：原始工具名或前缀映射到秒数。
	ToolTimeoutsJSON datatypes.JSON `gorm:"column:tool_timeouts"`
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定 MCP 表名。
func (mcpRecordModel) TableName() string { return "mcps" }

// CreateSkill 在 system scope 事务内创建 Skill 模板并追加审计事件。
func (r *CatalogRepository) CreateSkill(ctx context.Context, actorID uuid.UUID, skill biz.SkillTemplate, event biz.OutboxEvent) (biz.SkillTemplate, error) {
	record, err := fromBizSkill(skill)
	if err != nil {
		return biz.SkillTemplate{}, err
	}
	err = WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.SkillTemplate{}, catalogError("create skill", err)
	}
	return r.GetSkill(ctx, skill.ID)
}

// UpdateSkill 更新 Skill 模板。
func (r *CatalogRepository) UpdateSkill(ctx context.Context, actorID uuid.UUID, skill biz.SkillTemplate, event biz.OutboxEvent) (biz.SkillTemplate, error) {
	record, err := fromBizSkill(skill)
	if err != nil {
		return biz.SkillTemplate{}, err
	}
	err = WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Model(&skillRecordModel{}).Where("id = ?", skill.ID).Updates(map[string]any{
			"description": record.Description, "prompt_template": record.PromptTemplate,
			"parameters_json": record.ParametersJSON, "status": record.Status, "updated_at": time.Now().UTC(),
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
		return biz.SkillTemplate{}, catalogError("update skill", err)
	}
	return r.GetSkill(ctx, skill.ID)
}

// DeleteSkill 删除 Skill 模板。
func (r *CatalogRepository) DeleteSkill(ctx context.Context, actorID, skillID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Delete(&skillRecordModel{}, "id = ?", skillID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	return catalogError("delete skill", err)
}

// GetSkill 按 ID 读取 Skill 模板。
func (r *CatalogRepository) GetSkill(ctx context.Context, skillID uuid.UUID) (biz.SkillTemplate, error) {
	var record skillRecordModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ?", skillID).Error
	})
	if err != nil {
		return biz.SkillTemplate{}, catalogError("get skill", err)
	}
	return toBizSkill(record)
}

// ListSkills 列出全部 Skill 模板，最多 1000 条。
func (r *CatalogRepository) ListSkills(ctx context.Context) ([]biz.SkillTemplate, error) {
	var records []skillRecordModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Order("name, version DESC").Limit(1000).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list skills", err)
	}
	result := make([]biz.SkillTemplate, 0, len(records))
	for _, record := range records {
		skill, err := toBizSkill(record)
		if err != nil {
			return nil, err
		}
		result = append(result, skill)
	}
	return result, nil
}

// CreateMCP 创建 MCP 配置并加密保存凭证。
func (r *CatalogRepository) CreateMCP(ctx context.Context, actorID uuid.UUID, mcp biz.MCP, credential *biz.ProviderCredential, event biz.OutboxEvent) (biz.MCP, error) {
	record := mcpRecordModel{
		ID: mcp.ID, Name: mcp.Name, Endpoint: mcp.Endpoint, AuthType: mcp.AuthType,
		RiskLevel: mcp.RiskLevel, Status: mcp.Status, ToolTimeoutsJSON: marshalToolTimeouts(mcp.ToolTimeouts),
	}
	if credential != nil {
		record.AuthCredentialsEncrypted = credential.Encrypted
		record.KeyVersion = credential.KeyVersion
	}
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.MCP{}, catalogError("create mcp", err)
	}
	return r.GetMCP(ctx, mcp.ID)
}

// UpdateMCP 更新 MCP；credential 非空时同步替换密文与密钥版本。
func (r *CatalogRepository) UpdateMCP(ctx context.Context, actorID uuid.UUID, mcp biz.MCP, credential *biz.ProviderCredential, event biz.OutboxEvent) (biz.MCP, error) {
	updates := map[string]any{
		"name": mcp.Name, "endpoint": mcp.Endpoint, "auth_type": mcp.AuthType,
		"risk_level": mcp.RiskLevel, "status": mcp.Status,
		"tool_timeouts": marshalToolTimeouts(mcp.ToolTimeouts), "updated_at": time.Now().UTC(),
	}
	if credential != nil {
		updates["auth_credentials_encrypted"] = credential.Encrypted
		updates["key_version"] = credential.KeyVersion
	}
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Model(&mcpRecordModel{}).Where("id = ?", mcp.ID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.MCP{}, catalogError("update mcp", err)
	}
	return r.GetMCP(ctx, mcp.ID)
}

// DeleteMCP 删除 MCP 配置。
func (r *CatalogRepository) DeleteMCP(ctx context.Context, actorID, mcpID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Delete(&mcpRecordModel{}, "id = ?", mcpID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	return catalogError("delete mcp", err)
}

// GetMCP 读取 MCP 元数据，不返回凭证。
func (r *CatalogRepository) GetMCP(ctx context.Context, mcpID uuid.UUID) (biz.MCP, error) {
	var record mcpRecordModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ?", mcpID).Error
	})
	if err != nil {
		return biz.MCP{}, catalogError("get mcp", err)
	}
	return toBizMCP(record), nil
}

// ListMCPs 列出全部 MCP，最多 1000 条。
func (r *CatalogRepository) ListMCPs(ctx context.Context) ([]biz.MCP, error) {
	var records []mcpRecordModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Order("name, id").Limit(1000).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list mcps", err)
	}
	result := make([]biz.MCP, 0, len(records))
	for _, record := range records {
		result = append(result, toBizMCP(record))
	}
	return result, nil
}

// fromBizSkill 将 Skill 领域实体转换为数据库行。
func fromBizSkill(skill biz.SkillTemplate) (skillRecordModel, error) {
	parameters, err := json.Marshal(nonNilMap(skill.Parameters))
	if err != nil {
		return skillRecordModel{}, err
	}
	return skillRecordModel{
		ID: skill.ID, Name: skill.Name, Version: skill.Version, Description: skill.Description,
		PromptTemplate: skill.PromptTemplate, ParametersJSON: datatypes.JSON(parameters), Status: string(skill.Status),
	}, nil
}

// toBizSkill 将数据库行转换为 Skill 领域实体并解码参数 JSON。
func toBizSkill(record skillRecordModel) (biz.SkillTemplate, error) {
	var parameters map[string]any
	if err := json.Unmarshal(record.ParametersJSON, &parameters); err != nil {
		return biz.SkillTemplate{}, fmt.Errorf("decode skill parameters: %w", err)
	}
	return biz.SkillTemplate{
		ID: record.ID, Name: record.Name, Version: record.Version, Description: record.Description,
		PromptTemplate: record.PromptTemplate, Parameters: parameters, Status: biz.SkillStatus(record.Status),
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// toBizMCP 将数据库行转换为 MCP 领域实体，刻意不映射凭证密文。
func toBizMCP(record mcpRecordModel) biz.MCP {
	timeouts := map[string]int{}
	if len(record.ToolTimeoutsJSON) > 0 {
		_ = json.Unmarshal(record.ToolTimeoutsJSON, &timeouts)
	}
	return biz.MCP{
		ID: record.ID, Name: record.Name, Endpoint: record.Endpoint, AuthType: record.AuthType,
		KeyVersion: record.KeyVersion, RiskLevel: record.RiskLevel, Status: record.Status,
		ToolTimeouts: timeouts, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

// marshalToolTimeouts 把工具超时覆盖编码为 JSON；nil 视为空对象。
func marshalToolTimeouts(values map[string]int) datatypes.JSON {
	if values == nil {
		values = map[string]int{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return datatypes.JSON([]byte("{}"))
	}
	return datatypes.JSON(encoded)
}

// GetMCPCredential 读取 MCP 元数据与加密凭证，仅供执行 Worker 解密使用。
// 密文与密钥版本绝不返回给 HTTP/gRPC 调用方，也不得写入事件载荷。
func (r *CatalogRepository) GetMCPCredential(ctx context.Context, mcpID uuid.UUID) (biz.MCP, biz.ProviderCredential, error) {
	var record mcpRecordModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ?", mcpID).Error
	})
	if err != nil {
		return biz.MCP{}, biz.ProviderCredential{}, catalogError("get MCP credential", err)
	}
	credential := biz.ProviderCredential{
		Encrypted: append([]byte(nil), record.AuthCredentialsEncrypted...), KeyVersion: record.KeyVersion,
	}
	return toBizMCP(record), credential, nil
}

var _ biz.SkillRepository = (*CatalogRepository)(nil)
var _ biz.MCPRepository = (*CatalogRepository)(nil)
var _ biz.MCPCredentialRepository = (*CatalogRepository)(nil)
