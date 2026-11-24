package data

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
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

type runCreationTransactionKey struct{}

type runCreationTransaction struct {
	db          *gorm.DB
	tx          *gorm.DB
	afterCommit []func(context.Context)
}

func runCreationTransactionFromContext(ctx context.Context, db *gorm.DB) *runCreationTransaction {
	transaction, _ := ctx.Value(runCreationTransactionKey{}).(*runCreationTransaction)
	if transaction != nil && transaction.db == db {
		return transaction
	}
	return nil
}

// NewRunCreationTransaction lets repositories sharing db join Run creation.
// A nested call uses a savepoint so a failed write can be rolled back independently.
func NewRunCreationTransaction(db *gorm.DB) func(context.Context, func(context.Context) error) error {
	return func(ctx context.Context, fn func(context.Context) error) error {
		parent := runCreationTransactionFromContext(ctx, db)
		conn := db
		if parent != nil {
			conn = parent.tx
		}
		transaction := &runCreationTransaction{db: db}
		if err := conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			transaction.tx = tx
			return fn(context.WithValue(ctx, runCreationTransactionKey{}, transaction))
		}); err != nil {
			return err
		}
		if parent != nil {
			parent.afterCommit = append(parent.afterCommit, transaction.afterCommit...)
		} else {
			for _, notify := range transaction.afterCommit {
				notify(context.WithoutCancel(ctx))
			}
		}
		return nil
	}
}

// OpenPostgres 打开 PostgreSQL 连接池并完成健康检查。
func OpenPostgres(dsn string, maxOpenConns int) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(withPostgresTimeouts(dsn)), &gorm.Config{
		TranslateError: true,
		Logger:         newQueryLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %v", redactDSNError(err, dsn))
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get postgres connection pool: %v", redactDSNError(err, dsn))
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
		return nil, fmt.Errorf("ping postgres: %v", redactDSNError(err, dsn))
	}
	return db, nil
}

// redactDSNError 移除错误文本中出现的 DSN 密码，避免启动日志泄露凭据。
func redactDSNError(err error, dsn string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if parsed, parseErr := url.Parse(dsn); parseErr == nil && parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			message = strings.ReplaceAll(message, password, "***")
			message = strings.ReplaceAll(message, url.QueryEscape(password), "***")
		}
	}
	return errors.New(message)
}

// WithScope 在单个事务内设置 RLS 会话变量后执行 fn，事务结束自动清理作用域。
// 关键步骤：所有 set_config 使用 is_local=true，确保作用域不会泄漏到连接池中的后续请求。
func WithScope(ctx context.Context, db *gorm.DB, scope Scope, fn func(*gorm.DB) error) error {
	transaction := runCreationTransactionFromContext(ctx, db)
	if transaction != nil {
		// Keep each repository's error handling local to its savepoint.
		db = transaction.tx
	}
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
		if scope.RegistrationEnabled || transaction != nil {
			value := ""
			if scope.RegistrationEnabled {
				value = "on"
			}
			settings = append(settings, struct {
				key   string
				value string
			}{key: "app.registration_enabled", value: value})
		}
		for _, setting := range settings {
			if err := tx.Exec("SELECT set_config(?, ?, true)", setting.key, setting.value).Error; err != nil {
				return fmt.Errorf("set database scope %s: %w", setting.key, err)
			}
		}
		return fn(tx)
	})
}
