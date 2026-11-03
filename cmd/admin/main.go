// Command admin 创建首个管理员账号；密码只从标准输入读取，避免进入 shell 历史或配置。
// 用法：bin/harness-admin -username admin < /path/to/password-file
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/Mark-Panda/harness-agent/internal/platform/auth"
	"github.com/Mark-Panda/harness-agent/internal/platform/config"
	"github.com/google/uuid"
)

func main() {
	username := flag.String("username", "", "管理员用户名")
	flag.Parse()
	if err := run(*username); err != nil {
		slog.Error("admin bootstrap failed", "error", err)
		os.Exit(1)
	}
}

// run 读取密码并创建管理员账号。
func run(username string) error {
	username = biz.NormalizeUsername(username)
	if len(username) < 3 || len(username) > 64 {
		return fmt.Errorf("username must contain between 3 and 64 characters")
	}
	password, err := readPassword()
	if err != nil {
		return err
	}
	cfg, err := config.LoadEnvironment(os.Getenv("APP_ENV"))
	if err != nil {
		return err
	}
	if cfg.Data.Postgres.DSN == "" || cfg.Data.Postgres.AuthDSN == "" {
		return fmt.Errorf("DATABASE_URL and AUTH_DATABASE_URL are required")
	}
	appDB, err := data.OpenPostgres(cfg.Data.Postgres.DSN, 2)
	if err != nil {
		return err
	}
	defer func() {
		sqlDB, closeErr := appDB.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	}()
	authDB, err := data.OpenPostgres(cfg.Data.Postgres.AuthDSN, 2)
	if err != nil {
		return err
	}
	defer func() {
		sqlDB, closeErr := authDB.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	}()
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	user := biz.User{
		ID: uuid.New(), Username: username, PasswordHash: passwordHash,
		Role: biz.RoleAdmin, Status: biz.UserStatusActive, CredentialVersion: 1,
	}
	event := biz.OutboxEvent{
		UserID: user.ID, EventType: "auth.admin.bootstrapped", AggregateType: "user",
		AggregateID: user.ID.String(), DedupeKey: "auth.admin.bootstrapped:" + user.ID.String(),
		Payload: map[string]any{"outcome": "success", "source": "admin-cli"},
	}
	created, err := data.NewUserRepository(appDB, authDB).CreateUser(context.Background(), user, event)
	if err != nil {
		return err
	}
	slog.Info("admin created", "user_id", created.ID, "username", created.Username)
	return nil
}

// readPassword 从标准输入读取单行密码；优先使用 ADMIN_PASSWORD 便于自动化注入。
func readPassword() (string, error) {
	if value := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD")); value != "" {
		return value, nil
	}
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		value := strings.TrimSpace(scanner.Text())
		if value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("admin password is required via ADMIN_PASSWORD or stdin")
}
