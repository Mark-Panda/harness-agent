package main

import (
	"log/slog"
	"os"

	"github.com/Mark-Panda/harness-agent/internal/bootstrap"
)

func main() {
	if err := bootstrap.Run(os.Getenv("WORKER_ROLE")); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
