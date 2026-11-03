package biz

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Role 平台角色。
type Role string

const (
	// RoleUser 普通用户，只能访问自己的 Agent、会话与消息。
	RoleUser Role = "user"
	// RoleAdmin 管理员，可管理平台级 Provider、模型与策略。
	RoleAdmin Role = "admin"
)

// Principal 是已认证调用者身份，只由 JWT 或可信飞书映射产生，不接受客户端自报 user_id。
type Principal struct {
	// UserID 认证后的用户 ID。
	UserID uuid.UUID
	// Role 平台角色。
	Role Role
}

// NewPrincipal 构造并校验调用者身份。
func NewPrincipal(userID uuid.UUID, role Role) (Principal, error) {
	if userID == uuid.Nil {
		return Principal{}, errors.New("principal user id is required")
	}
	if role != RoleUser && role != RoleAdmin {
		return Principal{}, errors.New("invalid principal role")
	}
	return Principal{UserID: userID, Role: role}, nil
}

// CanAccessUser 判断调用者是否可直接访问目标用户的数据（管理员不隐式获得用户内容访问权）。
func (p Principal) CanAccessUser(userID uuid.UUID) bool {
	return p.UserID != uuid.Nil && p.UserID == userID
}

// principalContextKey 是 Context 中身份值的私有键类型，防止跨包冲突。
type principalContextKey struct{}

// WithPrincipal 把认证身份注入 Context，供 Service 与 Data 层读取。
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFromContext 从 Context 读取身份，未认证时返回 false。
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok && principal.UserID != uuid.Nil
}
