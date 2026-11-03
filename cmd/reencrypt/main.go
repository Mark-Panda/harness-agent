// Command reencrypt 将 Provider/MCP 凭据从旧密钥版本重加密到当前活动密钥。
// 默认 dry-run，仅统计；显式 -apply 才写入，并逐行追加审计 Outbox 事件。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/internal/platform/config"
	"github.com/Mark-Panda/harness-agent/internal/platform/crypto"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func main() {
	apply := flag.Bool("apply", false, "actually re-encrypt credentials; default is dry-run")
	batch := flag.Int("batch", 100, "rows per batch")
	flag.Parse()
	if err := run(*apply, *batch); err != nil {
		slog.Error("credential re-encrypt failed", "error", err)
		os.Exit(1)
	}
}

// credentialRow 是一行待处理的密文凭据。
type credentialRow struct {
	// ID 资源主键。
	ID uuid.UUID
	// Ciphertext 当前密文。
	Ciphertext []byte
	// KeyVersion 当前密钥版本。
	KeyVersion string
}

// run 执行重加密。
func run(apply bool, batch int) error {
	cfg, err := config.LoadEnvironment(os.Getenv("APP_ENV"))
	if err != nil {
		return err
	}
	if cfg.Data.Postgres.DSN == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	previous := make([]crypto.Key, 0, len(cfg.Credentials.PreviousKeys))
	for _, key := range cfg.Credentials.PreviousKeys {
		previous = append(previous, crypto.Key{ID: key.ID, Secret: []byte(key.Secret)})
	}
	keyring, err := crypto.NewKeyring(crypto.Key{ID: cfg.Credentials.ActiveKeyID, Secret: []byte(cfg.Credentials.ActiveKeySecret)}, previous)
	if err != nil {
		return err
	}
	db, err := data.OpenPostgres(cfg.Data.Postgres.DSN, 2)
	if err != nil {
		return err
	}
	defer func() {
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	}()
	if batch <= 0 {
		batch = 100
	}
	for _, table := range []struct {
		name      string
		selectSQL string
		updateSQL string
		aggregate string
	}{
		{
			name: "llm_providers", aggregate: "llm_provider",
			selectSQL: "SELECT id, api_key_encrypted, key_version FROM llm_providers WHERE key_version <> ? ORDER BY id LIMIT ?",
			updateSQL: "UPDATE llm_providers SET api_key_encrypted = ?, key_version = ?, updated_at = ? WHERE id = ? AND key_version = ?",
		},
		{
			name: "mcps", aggregate: "mcp",
			selectSQL: "SELECT id, auth_credentials_encrypted, key_version FROM mcps WHERE key_version <> ? ORDER BY id LIMIT ?",
			updateSQL: "UPDATE mcps SET auth_credentials_encrypted = ?, key_version = ?, updated_at = ? WHERE id = ? AND key_version = ?",
		},
	} {
		processed, err := rotateTable(context.Background(), db, keyring, cfg.Credentials.ActiveKeyID, table.name, table.selectSQL, table.updateSQL, table.aggregate, apply, batch)
		if err != nil {
			return err
		}
		slog.Info("credential rotation", "table", table.name, "processed", processed, "apply", apply)
	}
	return nil
}

// rotateTable 按批次重加密单表的凭据列。
func rotateTable(ctx context.Context, db *gorm.DB, keyring *crypto.Keyring, activeKeyID, table, selectSQL, updateSQL, aggregateType string, apply bool, batch int) (int64, error) {
	var processed int64
	for {
		var rows []credentialRow
		err := data.WithScope(ctx, db, data.Scope{System: true}, func(tx *gorm.DB) error {
			return tx.Raw(selectSQL, activeKeyID, batch).Scan(&rows).Error
		})
		if err != nil {
			return processed, err
		}
		if len(rows) == 0 {
			return processed, nil
		}
		if !apply {
			return processed + int64(len(rows)), nil
		}
		for _, row := range rows {
			plaintext, err := keyring.Open(row.KeyVersion, row.Ciphertext)
			if err != nil {
				return processed, fmt.Errorf("decrypt %s %s: %w", table, row.ID, err)
			}
			ciphertext, newVersion, err := keyring.Seal(plaintext)
			if err != nil {
				return processed, fmt.Errorf("encrypt %s %s: %w", table, row.ID, err)
			}
			err = data.WithScope(ctx, db, data.Scope{System: true}, func(tx *gorm.DB) error {
				update := tx.Exec(updateSQL, ciphertext, newVersion, time.Now().UTC(), row.ID, row.KeyVersion)
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected == 0 {
					// 并发轮换已处理该行，跳过审计。
					return nil
				}
				return tx.Exec(`INSERT INTO outbox_events (event_type, aggregate_type, aggregate_id, dedupe_key, payload)
					VALUES ('credential.reencrypted', ?, ?, ?, ?)`,
					aggregateType, row.ID.String(), fmt.Sprintf("credential.reencrypted:%s:%s:%s", table, row.ID, newVersion),
					fmt.Sprintf(`{"from_key":%q,"to_key":%q}`, row.KeyVersion, newVersion)).Error
			})
			if err != nil {
				return processed, fmt.Errorf("update %s %s: %w", table, row.ID, err)
			}
			processed++
		}
	}
}
