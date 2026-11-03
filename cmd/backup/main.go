package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/internal/platform/backup"
	"gorm.io/gorm"
)

// main 是备份工具入口，支持 encrypt/decrypt/stats/verify 四个子命令。
func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: harness-backup <encrypt|decrypt|stats|verify> [flags]")
	}
	var err error
	switch os.Args[1] {
	case "encrypt":
		err = runEncrypt(os.Args[2:])
	case "decrypt":
		err = runDecrypt(os.Args[2:])
	case "stats":
		err = runStats(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		log.Printf("backup command failed: %v", err)
		os.Exit(1)
	}
}

// runEncrypt 加密备份文件并输出校验元数据。
func runEncrypt(args []string) error {
	flags := flag.NewFlagSet("encrypt", flag.ExitOnError)
	input := flags.String("in", "", "plaintext backup file")
	output := flags.String("out", "", "encrypted backup file")
	keyFile := flags.String("key-file", os.Getenv("BACKUP_ENCRYPTION_KEY_FILE"), "key file with 32-byte base64/hex key")
	keyID := flags.String("key-id", os.Getenv("BACKUP_KEY_ID"), "key identifier recorded in logs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || *output == "" {
		return errors.New("-in and -out are required")
	}
	key, err := backup.LoadKeyFile(*keyFile)
	if err != nil {
		return err
	}
	source, err := os.Open(*input)
	if err != nil {
		return fmt.Errorf("open plaintext backup: %w", err)
	}
	defer source.Close()
	dst, err := os.Create(*output)
	if err != nil {
		return fmt.Errorf("create encrypted backup: %w", err)
	}
	// 加密失败时删除半成品，避免把不完整备份当作可恢复制品。
	removeOnError := true
	defer func() {
		_ = dst.Close()
		if removeOnError {
			_ = os.Remove(*output)
		}
	}()
	if err := backup.Encrypt(dst, source, key, backup.DefaultChunkSize); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close encrypted backup: %w", err)
	}
	removeOnError = false
	plaintextHash, err := backup.HashFile(*input)
	if err != nil {
		return err
	}
	ciphertextHash, err := backup.HashFile(*output)
	if err != nil {
		return err
	}
	plaintextInfo, err := os.Stat(*input)
	if err != nil {
		return err
	}
	ciphertextInfo, err := os.Stat(*output)
	if err != nil {
		return err
	}
	return writeJSON(map[string]any{
		"key_id": *keyID, "algorithm": backup.Algorithm,
		"plaintext_sha256": plaintextHash, "ciphertext_sha256": ciphertextHash,
		"plaintext_bytes": plaintextInfo.Size(), "ciphertext_bytes": ciphertextInfo.Size(),
	})
}

// runDecrypt 解密备份并输出明文校验信息。
func runDecrypt(args []string) error {
	flags := flag.NewFlagSet("decrypt", flag.ExitOnError)
	input := flags.String("in", "", "encrypted backup file")
	output := flags.String("out", "", "plaintext backup file")
	keyFile := flags.String("key-file", os.Getenv("BACKUP_ENCRYPTION_KEY_FILE"), "key file with 32-byte base64/hex key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || *output == "" {
		return errors.New("-in and -out are required")
	}
	key, err := backup.LoadKeyFile(*keyFile)
	if err != nil {
		return err
	}
	source, err := os.Open(*input)
	if err != nil {
		return fmt.Errorf("open encrypted backup: %w", err)
	}
	defer source.Close()
	dst, err := os.Create(*output)
	if err != nil {
		return fmt.Errorf("create plaintext backup: %w", err)
	}
	if err := backup.Decrypt(dst, source, key); err != nil {
		_ = dst.Close()
		_ = os.Remove(*output)
		return err
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close plaintext backup: %w", err)
	}
	hash, err := backup.HashFile(*output)
	if err != nil {
		return err
	}
	info, err := os.Stat(*output)
	if err != nil {
		return err
	}
	return writeJSON(map[string]any{"plaintext_sha256": hash, "plaintext_bytes": info.Size()})
}

// runStats 采集当前数据库恢复验收基线。
func runStats(args []string) error {
	flags := flag.NewFlagSet("stats", flag.ExitOnError)
	dsn := flags.String("dsn", os.Getenv("BACKUP_DATABASE_URL"), "admin database DSN")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return errors.New("-dsn or BACKUP_DATABASE_URL is required")
	}
	db, err := data.OpenPostgres(*dsn, 5)
	if err != nil {
		return err
	}
	defer closeGorm(db)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stats, err := backup.CaptureStats(ctx, db)
	if err != nil {
		return err
	}
	return writeJSON(stats)
}

// runVerify 校验备份与恢复目标一致；退出码非零表示验收失败。
func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	dsn := flags.String("dsn", os.Getenv("BACKUP_TARGET_DATABASE_URL"), "restored database DSN")
	manifestPath := flags.String("manifest", "", "manifest file")
	plaintextPath := flags.String("plaintext", "", "optional plaintext dump to hash")
	ciphertextPath := flags.String("ciphertext", "", "optional encrypted dump to hash")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dsn == "" || *manifestPath == "" {
		return errors.New("-dsn and -manifest are required")
	}
	manifest, err := backup.ReadManifest(*manifestPath)
	if err != nil {
		return err
	}
	db, err := data.OpenPostgres(*dsn, 5)
	if err != nil {
		return err
	}
	defer closeGorm(db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := backup.Verify(ctx, db, manifest, *plaintextPath, *ciphertextPath); err != nil {
		return err
	}
	return writeJSON(map[string]any{
		"status": "ok", "migration_version": manifest.Stats.MigrationVersion,
		"tables": len(manifest.Stats.TableCounts), "rls_tables": len(manifest.Stats.RLSEnabledTables),
	})
}

// writeJSON 把结果写入 stdout，供脚本合并到 Manifest。
func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

// closeGorm 关闭 GORM 连接池。
func closeGorm(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
}
