package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Mark-Panda/harness-agent/internal/platform/config"
	"github.com/Mark-Panda/harness-agent/internal/server"
	"github.com/Mark-Panda/harness-agent/internal/server/static"
	"github.com/go-kratos/kratos/v3"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	environment := os.Getenv("APP_ENV")
	if environment == "" {
		environment = "dev"
	}
	paths := []string{"configs/config.yaml"}
	overlay := fmt.Sprintf("configs/config.%s.yaml", environment)
	if _, err := os.Stat(overlay); err == nil {
		paths = append(paths, overlay)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect config overlay: %w", err)
	}
	cfg, err := config.Load(paths[0], paths[1:]...)
	if err != nil {
		return err
	}
	httpServer := server.NewHTTPServer(cfg.Server, static.Dist())
	app := kratos.New(
		kratos.Name("harness-agent"),
		kratos.Server(httpServer),
		kratos.StopTimeout(cfg.Server.ShutdownTimeout.Duration),
	)
	return app.Run()
}
