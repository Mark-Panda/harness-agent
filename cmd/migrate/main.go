package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/Mark-Panda/harness-agent/internal/platform/config"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func main() {
	flag.Parse()
	if err := run(flag.Arg(0), flag.Arg(1)); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run(command, argument string) error {
	cfg, err := config.LoadEnvironment(os.Getenv("APP_ENV"))
	if err != nil {
		return err
	}
	if cfg.Data.Postgres.DSN == "" {
		return errors.New("DATABASE_URL is required")
	}
	m, err := migrate.New("file://migrations", cfg.Data.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer func() {
		sourceErr, databaseErr := m.Close()
		if sourceErr != nil || databaseErr != nil {
			slog.Warn("close migrator", "source_error", sourceErr, "database_error", databaseErr)
		}
	}()

	if command == "" {
		command = "up"
	}
	switch command {
	case "up":
		err = m.Up()
	case "steps":
		steps, parseErr := strconv.Atoi(argument)
		if parseErr != nil || steps <= 0 {
			return errors.New("steps requires a positive integer; destructive down migrations are not supported")
		}
		err = m.Steps(steps)
	case "force":
		// force 仅用于修复失败迁移留下的 dirty 标记；执行前必须确认对应版本 DDL 已回滚。
		version, parseErr := strconv.Atoi(argument)
		if parseErr != nil || version < 0 {
			return errors.New("force requires a non-negative version")
		}
		err = m.Force(version)
	case "version":
		var version uint
		var dirty bool
		version, dirty, err = m.Version()
		if err == nil {
			fmt.Printf("version=%d dirty=%t\n", version, dirty)
		}
	default:
		return fmt.Errorf("unsupported migration command %q", command)
	}
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	return err
}
