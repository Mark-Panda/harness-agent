package data

import (
	"context"
	"errors"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// feishuBotModel 对应 feishu_bots 表：管理员登记的飞书机器人。
type feishuBotModel struct {
	// ID 机器人主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Name 机器人展示名称。
	Name string
	// AppID 飞书应用 App ID。
	AppID string
	// BotOpenID 机器人 open_id。
	BotOpenID string
	// KeyVersion 凭据密文密钥版本。
	KeyVersion string
	// Credentials 应用凭据密文。
	Credentials []byte `gorm:"type:bytea"`
	// Status 启停状态。
	Status string
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定机器人表名。
func (feishuBotModel) TableName() string { return "feishu_bots" }

// agentFeishuBotModel 对应 agent_feishu_bots 表：机器人到助手的唯一绑定。
type agentFeishuBotModel struct {
	// BotID 机器人主键，保证一个机器人只能绑定一个助手。
	BotID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 助手所属用户。
	UserID uuid.UUID `gorm:"type:uuid"`
	// AgentID 绑定的助手版本。
	AgentID uuid.UUID `gorm:"type:uuid"`
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定助手机器人绑定表名。
func (agentFeishuBotModel) TableName() string { return "agent_feishu_bots" }

// feishuBotBindingView 是绑定联表查询的扁平结果。
type feishuBotBindingView struct {
	// BotID 机器人 ID。
	BotID uuid.UUID `gorm:"column:bot_id"`
	// UserID 助手所属用户。
	UserID uuid.UUID `gorm:"column:user_id"`
	// AgentID 助手 ID。
	AgentID uuid.UUID `gorm:"column:agent_id"`
	// AgentName 助手名称。
	AgentName string `gorm:"column:agent_name"`
	// AgentVersion 助手版本号。
	AgentVersion int `gorm:"column:agent_version"`
	// Username 助手所属用户名。
	Username string `gorm:"column:username"`
}

// FeishuBotRepository 是机器人配置与绑定的 GORM 实现，全部使用系统作用域读写。
type FeishuBotRepository struct {
	db *gorm.DB
}

// NewFeishuBotRepository 构造机器人仓库。
func NewFeishuBotRepository(db *gorm.DB) *FeishuBotRepository {
	return &FeishuBotRepository{db: db}
}

// CreateFeishuBot 在系统作用域内创建机器人并写入审计事件。
func (r *FeishuBotRepository) CreateFeishuBot(ctx context.Context, actorID uuid.UUID, bot biz.FeishuBot, credential biz.ProviderCredential, event biz.OutboxEvent) (biz.FeishuBot, error) {
	record := feishuBotModel{
		ID: bot.ID, Name: bot.Name, AppID: bot.AppID, BotOpenID: bot.BotOpenID,
		KeyVersion: credential.KeyVersion, Credentials: credential.Encrypted, Status: string(bot.Status),
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.FeishuBot{}, catalogError("create feishu bot", err)
	}
	return r.GetFeishuBot(ctx, bot.ID)
}

// UpdateFeishuBot 更新机器人元数据；凭证为空时保留原密文。
func (r *FeishuBotRepository) UpdateFeishuBot(ctx context.Context, actorID uuid.UUID, bot biz.FeishuBot, credential *biz.ProviderCredential, event biz.OutboxEvent) (biz.FeishuBot, error) {
	updates := map[string]any{
		"name": bot.Name, "app_id": bot.AppID, "bot_open_id": bot.BotOpenID,
		"status": string(bot.Status), "updated_at": time.Now().UTC(),
	}
	if credential != nil {
		updates["key_version"] = credential.KeyVersion
		updates["credentials"] = credential.Encrypted
	}
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Model(&feishuBotModel{}).Where("id = ?", bot.ID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return biz.FeishuBot{}, catalogError("update feishu bot", err)
	}
	return r.GetFeishuBot(ctx, bot.ID)
}

// DeleteFeishuBot 删除机器人；绑定关系由外键级联解除。
func (r *FeishuBotRepository) DeleteFeishuBot(ctx context.Context, actorID, botID uuid.UUID, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.Delete(&feishuBotModel{}, "id = ?", botID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	return catalogError("delete feishu bot", err)
}

// GetFeishuBot 读取机器人元数据，不返回凭据。
func (r *FeishuBotRepository) GetFeishuBot(ctx context.Context, botID uuid.UUID) (biz.FeishuBot, error) {
	var record feishuBotModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ?", botID).Error
	})
	if err != nil {
		return biz.FeishuBot{}, catalogError("get feishu bot", err)
	}
	return toBizFeishuBot(record), nil
}

// ListFeishuBots 列出全部机器人并附带绑定信息，供管理员管理台使用。
func (r *FeishuBotRepository) ListFeishuBots(ctx context.Context) ([]biz.FeishuBot, error) {
	return r.listBots(ctx, false, uuid.Nil)
}

// ListFeishuBotsForUser 列出机器人；停用机器人仍会返回，便于已绑定用户查看并解绑。
func (r *FeishuBotRepository) ListFeishuBotsForUser(ctx context.Context, userID uuid.UUID) ([]biz.FeishuBot, error) {
	return r.listBots(ctx, false, userID)
}

// listBots 查询机器人列表并合并绑定信息；onlyEnabled 为 true 时只返回可用机器人。
func (r *FeishuBotRepository) listBots(ctx context.Context, onlyEnabled bool, viewerID uuid.UUID) ([]biz.FeishuBot, error) {
	var records []feishuBotModel
	var views []feishuBotBindingView
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		query := tx.Order("created_at DESC")
		if onlyEnabled {
			query = query.Where("status = ?", string(biz.FeishuBotEnabled))
		}
		if err := query.Find(&records).Error; err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		return tx.Table("agent_feishu_bots AS b").
			Select("b.bot_id, b.user_id, b.agent_id, a.name AS agent_name, a.version AS agent_version, u.username").
			Joins("JOIN agents a ON a.id = b.agent_id").
			Joins("JOIN users u ON u.id = b.user_id").
			Scan(&views).Error
	})
	if err != nil {
		return nil, catalogError("list feishu bots", err)
	}
	viewsByBot := make(map[uuid.UUID]feishuBotBindingView, len(views))
	for _, view := range views {
		viewsByBot[view.BotID] = view
	}
	result := make([]biz.FeishuBot, 0, len(records))
	for _, record := range records {
		bot := toBizFeishuBot(record)
		view, bound := viewsByBot[record.ID]
		if bound {
			bot.BoundUserID = &view.UserID
			// 只有绑定者本人能看到具体助手；其他用户仅知道该机器人已被占用。
			if viewerID == uuid.Nil || view.UserID == viewerID {
				bot.BoundAgentID = &view.AgentID
				bot.BoundAgentName = view.AgentName
				bot.BoundAgentVersion = &view.AgentVersion
				bot.BoundUsername = view.Username
			}
		}
		result = append(result, bot)
	}
	return result, nil
}

// GetFeishuBotCredential 读取机器人元数据与加密凭据。
func (r *FeishuBotRepository) GetFeishuBotCredential(ctx context.Context, botID uuid.UUID) (biz.FeishuBot, biz.ProviderCredential, error) {
	var record feishuBotModel
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.First(&record, "id = ?", botID).Error
	})
	if err != nil {
		return biz.FeishuBot{}, biz.ProviderCredential{}, catalogError("get feishu bot credential", err)
	}
	return toBizFeishuBot(record), biz.ProviderCredential{Encrypted: record.Credentials, KeyVersion: record.KeyVersion}, nil
}

// LookupFeishuBot 按 App ID 读取机器人配置，未登记时 found 为 false。
func (r *FeishuBotRepository) LookupFeishuBot(ctx context.Context, appID string) (biz.FeishuBot, bool, error) {
	var record feishuBotModel
	found := false
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.First(&record, "app_id = ?", appID)
		if result.Error == nil {
			found = true
			return nil
		}
		if result.Error == gorm.ErrRecordNotFound || errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		return result.Error
	})
	if err != nil {
		return biz.FeishuBot{}, false, catalogError("lookup feishu bot", err)
	}
	return toBizFeishuBot(record), found, nil
}

// FindFeishuBotBinding 按机器人 ID 查找绑定助手及其发布状态。
func (r *FeishuBotRepository) FindFeishuBotBinding(ctx context.Context, botID uuid.UUID) (biz.FeishuBotBinding, bool, error) {
	var binding agentFeishuBotModel
	var bot feishuBotModel
	var agent agentModel
	found := false
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		result := tx.First(&binding, "bot_id = ?", botID)
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return nil
			}
			return result.Error
		}
		if err := tx.First(&bot, "id = ?", botID).Error; err != nil {
			return err
		}
		if err := tx.First(&agent, "id = ?", binding.AgentID).Error; err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return biz.FeishuBotBinding{}, false, catalogError("find feishu bot binding", err)
	}
	if !found {
		return biz.FeishuBotBinding{}, false, nil
	}
	return biz.FeishuBotBinding{
		BotID: botID, BotName: bot.Name, BotOpenID: bot.BotOpenID, UserID: binding.UserID,
		AgentID: binding.AgentID, AgentName: agent.Name, AgentVersion: agent.Version,
		AgentStatus: biz.AgentStatus(agent.Status), AgentMode: biz.AgentMode(agent.Mode),
	}, true, nil
}

// BindFeishuBot 在用户作用域内创建绑定；机器人或助手已被占用时返回冲突。
func (r *FeishuBotRepository) BindFeishuBot(ctx context.Context, binding biz.FeishuBotBinding, event biz.OutboxEvent) error {
	record := agentFeishuBotModel{
		BotID: binding.BotID, UserID: binding.UserID, AgentID: binding.AgentID,
	}
	err := WithScope(ctx, r.db, Scope{UserID: binding.UserID}, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	return catalogError("bind feishu bot", err)
}

// UnbindFeishuBot 在用户作用域内解除助手的机器人绑定。
func (r *FeishuBotRepository) UnbindFeishuBot(ctx context.Context, userID, agentID uuid.UUID, event biz.OutboxEvent) (int, error) {
	var affected int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Delete(&agentFeishuBotModel{}, "agent_id = ? AND user_id = ?", agentID, userID)
		if result.Error != nil {
			return result.Error
		}
		affected = result.RowsAffected
		if affected == 0 {
			return nil
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return 0, catalogError("unbind feishu bot", err)
	}
	return int(affected), nil
}

// toBizFeishuBot 将数据库模型转换为领域实体，不携带凭据。
func toBizFeishuBot(record feishuBotModel) biz.FeishuBot {
	return biz.FeishuBot{
		ID: record.ID, Name: record.Name, AppID: record.AppID, BotOpenID: record.BotOpenID,
		KeyVersion: record.KeyVersion, Status: biz.FeishuBotStatus(record.Status),
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

// 编译期断言机器人仓库实现领域契约。
var (
	_ biz.FeishuBotRepository        = (*FeishuBotRepository)(nil)
	_ biz.FeishuBotBindingRepository = (*FeishuBotRepository)(nil)
	_ biz.FeishuBotRouter            = (*FeishuBotRepository)(nil)
	_ biz.FeishuBotCredentialReader  = (*FeishuBotRepository)(nil)
)
