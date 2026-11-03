package data

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Scope 描述一次数据库事务的行级安全（RLS）作用域。
// 所有非系统查询必须显式携带 UserID，由 WithScope 注入 session 变量后由 PostgreSQL 策略强制隔离。
type Scope struct {
	// System 是否以系统身份运行（平台配置读写或后台任务），对应 app.system=on。
	System bool
	// UserID 用户隔离键，对应 app.user_id。
	UserID uuid.UUID
	// AuthUsername 登录流程使用的规范化用户名。
	AuthUsername string
	// RegistrationEnabled 注册流程开关，允许插入新用户行。
	RegistrationEnabled bool
	// RefreshTokenHash 刷新令牌哈希，限定认证流程只能读取对应令牌。
	RefreshTokenHash []byte
	// IdentityProvider 外部身份提供方（如 feishu）。
	IdentityProvider string
	// IdentityTenant 外部身份租户标识。
	IdentityTenant string
	// IdentitySubject 外部身份主体标识。
	IdentitySubject string
}

// OpenPostgres 打开 PostgreSQL 连接池并完成健康检查。
func OpenPostgres(dsn string, maxOpenConns int) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(withPostgresTimeouts(dsn)), &gorm.Config{
		TranslateError: true,
		Logger:         newQueryLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get postgres connection pool: %w", err)
	}
	// 连接池设置上限，避免单实例耗尽数据库连接。
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}

// WithScope 在单个事务内设置 RLS 会话变量后执行 fn，事务结束自动清理作用域。
// 关键步骤：所有 set_config 使用 is_local=true，确保作用域不会泄漏到连接池中的后续请求。
func WithScope(ctx context.Context, db *gorm.DB, scope Scope, fn func(*gorm.DB) error) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		settings := []struct {
			key   string
			value string
		}{
			{key: "app.system", value: ""},
			{key: "app.user_id", value: scope.UserID.String()},
			{key: "app.auth_user_name", value: scope.AuthUsername},
			{key: "app.identity_provider", value: scope.IdentityProvider},
			{key: "app.identity_tenant", value: scope.IdentityTenant},
			{key: "app.identity_subject", value: scope.IdentitySubject},
			{key: "app.refresh_token_hash", value: hex.EncodeToString(scope.RefreshTokenHash)},
		}
		// 空用户 ID 必须置为空串，避免 nil UUID 被误判为有效隔离键。
		if scope.UserID == uuid.Nil {
			settings[1].value = ""
		}
		if scope.System {
			settings[0].value = "on"
		}
		if scope.RegistrationEnabled {
			settings = append(settings, struct {
				key   string
				value string
			}{key: "app.registration_enabled", value: "on"})
		}
		for _, setting := range settings {
			if err := tx.Exec("SELECT set_config(?, ?, true)", setting.key, setting.value).Error; err != nil {
				return fmt.Errorf("set database scope %s: %w", setting.key, err)
			}
		}
		return fn(tx)
	})
}
