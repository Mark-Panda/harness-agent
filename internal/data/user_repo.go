package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// userModel 对应 users 表：账号与凭据状态。
type userModel struct {
	// ID 用户主键，同时是私有数据的 RLS 隔离键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// Username 展示用用户名。
	Username string
	// UsernameNormalized 规范化用户名（小写去空白），唯一索引使用。
	UsernameNormalized string
	// PasswordHash Argon2id 密码哈希。
	PasswordHash string
	// Role 平台角色：user/admin。
	Role string
	// Status 账号状态：active/disabled。
	Status string
	// CredentialVersion 凭据版本，提升后所有令牌失效。
	CredentialVersion int64
	// CreatedAt 创建时间。
	CreatedAt time.Time
	// UpdatedAt 更新时间。
	UpdatedAt time.Time
}

// TableName 指定用户表名。
func (userModel) TableName() string {
	return "users"
}

// UserRepository 是用户持久化实现，读取使用应用连接池、认证流程使用受限认证连接池。
type UserRepository struct {
	appDB  *gorm.DB
	authDB *gorm.DB
}

// NewUserRepository 构造用户仓库。
func NewUserRepository(appDB, authDB *gorm.DB) *UserRepository {
	return &UserRepository{appDB: appDB, authDB: authDB}
}

// CreateUser 在注册作用域事务内创建用户并追加审计事件。
func (r *UserRepository) CreateUser(ctx context.Context, user biz.User, event biz.OutboxEvent) (biz.User, error) {
	model := userModel{
		ID:                 user.ID,
		Username:           user.Username,
		UsernameNormalized: biz.NormalizeUsername(user.Username),
		PasswordHash:       user.PasswordHash,
		Role:               string(user.Role),
		Status:             string(user.Status),
		CredentialVersion:  user.CredentialVersion,
	}
	if model.ID == uuid.Nil {
		model.ID = uuid.New()
	}
	if model.Role == "" {
		model.Role = string(biz.RoleUser)
	}
	if model.Status == "" {
		model.Status = string(biz.UserStatusActive)
	}
	if model.CredentialVersion == 0 {
		model.CredentialVersion = 1
	}
	if event.UserID == uuid.Nil {
		event.UserID = model.ID
	}
	if event.AggregateID == "" {
		event.AggregateID = model.ID.String()
	}
	err := WithScope(ctx, r.authDB, Scope{UserID: model.ID, RegistrationEnabled: true}, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return biz.User{}, biz.ErrUserAlreadyExists
	}
	if err != nil {
		return biz.User{}, fmt.Errorf("create user: %w", err)
	}
	return toBizUser(model), nil
}

// GetUserByID 按用户 ID 读取，只能读取当前作用域内的用户。
func (r *UserRepository) GetUserByID(ctx context.Context, userID uuid.UUID) (biz.User, error) {
	var model userModel
	err := WithScope(ctx, r.appDB, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.First(&model, "id = ?", userID).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return biz.User{}, biz.ErrUserNotFound
	}
	if err != nil {
		return biz.User{}, fmt.Errorf("get user by id: %w", err)
	}
	return toBizUser(model), nil
}

// GetUserByNormalizedUsername 按规范化用户名读取，仅允许登录作用域访问。
func (r *UserRepository) GetUserByNormalizedUsername(ctx context.Context, normalized string) (biz.User, error) {
	normalized = biz.NormalizeUsername(normalized)
	var model userModel
	err := WithScope(ctx, r.authDB, Scope{AuthUsername: normalized}, func(tx *gorm.DB) error {
		return tx.First(&model, "username_normalized = ?", normalized).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return biz.User{}, biz.ErrUserNotFound
	}
	if err != nil {
		return biz.User{}, fmt.Errorf("get user by username: %w", err)
	}
	return toBizUser(model), nil
}

// UpdateUser 更新密码哈希、状态与凭据版本，并与审计事件同事务提交。
// 只允许更新当前作用域用户的凭据字段，不接受角色或用户名变更，避免越权提升。
func (r *UserRepository) UpdateUser(ctx context.Context, user biz.User, event biz.OutboxEvent) (biz.User, error) {
	if user.ID == uuid.Nil {
		return biz.User{}, fmt.Errorf("%w: user id is required", biz.ErrInvalidConfig)
	}
	if event.UserID == uuid.Nil {
		event.UserID = user.ID
	}
	if event.AggregateID == "" {
		event.AggregateID = user.ID.String()
	}
	updatedAt := time.Now().UTC()
	err := WithScope(ctx, r.authDB, Scope{UserID: user.ID}, func(tx *gorm.DB) error {
		result := tx.Model(&userModel{}).Where("id = ?", user.ID).Updates(map[string]any{
			"password_hash": user.PasswordHash, "status": string(user.Status),
			"credential_version": user.CredentialVersion, "updated_at": updatedAt,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return biz.ErrUserNotFound
		}
		return appendOutbox(tx, event)
	})
	if errors.Is(err, biz.ErrUserNotFound) {
		return biz.User{}, biz.ErrUserNotFound
	}
	if err != nil {
		return biz.User{}, fmt.Errorf("update user: %w", err)
	}
	user.UpdatedAt = updatedAt
	return user, nil
}

// toBizUser 将数据库行转换为领域用户。
func toBizUser(model userModel) biz.User {
	return biz.User{
		ID:                model.ID,
		Username:          model.Username,
		PasswordHash:      model.PasswordHash,
		Role:              biz.Role(model.Role),
		Status:            biz.UserStatus(model.Status),
		CredentialVersion: model.CredentialVersion,
		CreatedAt:         model.CreatedAt,
		UpdatedAt:         model.UpdatedAt,
	}
}

var _ biz.UserRepository = (*UserRepository)(nil)
