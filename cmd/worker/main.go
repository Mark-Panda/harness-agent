// Command worker 只启动持久化任务消费者，不监听 HTTP/gRPC。
// 与 api 共用同一镜像和配置，通过 WORKER_ROLE=worker 或本入口区分进程角色。
package main

import (
	"log/slog"
	"os"

	"github.com/Mark-Panda/harness-agent/internal/bootstrap"
)

func main() {
	if err := bootstrap.Run(bootstrap.RoleWorker); err != nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}
