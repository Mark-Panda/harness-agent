package biz

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// UserStatus 用户账号状态。
type UserStatus string

const (
	// UserStatusActive 账号正常可用。
	UserStatusActive UserStatus = "active"
	// UserStatusDisabled 账号被禁用，所有令牌失效。
	UserStatusDisabled UserStatus = "disabled"
	// UserStatusDeleted 账号已匿名化删除，保留审计外键但禁止登录。
	UserStatusDeleted UserStatus = "deleted"
)

// User 是平台用户聚合，密码只保存 Argon2id 哈希，不保存明文。
type User struct {
	// ID 用户唯一标识，也是全部私有数据的隔离键。
	ID uuid.UUID
	// Username 展示用用户名。
	Username string
	// PasswordHash Argon2id 密码哈希。
	PasswordHash string
	// Role 平台角色，决定是否可管理平台配置。
	Role Role
	// Status 账号状态。
	Status UserStatus
	// CredentialVersion 凭据版本，提升后所有已签发令牌失效。
	CredentialVersion int64
	// CreatedAt 创建时间（UTC）。
	CreatedAt time.Time
	// UpdatedAt 最后更新时间（UTC）。
	UpdatedAt time.Time
}

// 用户领域错误。
var (
	// ErrUserNotFound 用户不存在。
	ErrUserNotFound = errors.New("user not found")
	// ErrUserAlreadyExists 用户名冲突。
	ErrUserAlreadyExists = errors.New("user already exists")
)

// NormalizeUsername 规范化用户名：去空白并转小写，用于唯一约束与登录匹配。
func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// UserRepository 定义用户持久化契约。
type UserRepository interface {
	// CreateUser 创建用户并与审计事件同事务提交。
	CreateUser(ctx context.Context, user User, event OutboxEvent) (User, error)
	// GetUserByID 按用户 ID 读取。
	GetUserByID(ctx context.Context, userID uuid.UUID) (User, error)
	// GetUserByNormalizedUsername 按规范化用户名读取，用于登录。
	GetUserByNormalizedUsername(ctx context.Context, normalized string) (User, error)
	// UpdateUser 更新密码哈希、状态与凭据版本；只能作用于当前用户且写入审计事件。
	UpdateUser(ctx context.Context, user User, event OutboxEvent) (User, error)
}
