# syntax=docker/dockerfile:1

FROM node:24.12.0-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26.10-alpine AS server-builder
# 构建期模块代理可通过 --build-arg 覆盖，默认使用国内镜像避免 proxy.golang.org 不可达。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web/dist ./internal/server/static/dist
# 构建镜像内全部可执行文件。前两个是常驻服务，其余均为一次性运维命令：
#   harness-agent     API 服务（HTTP/gRPC/SSE 与静态资源）；WORKER_ROLE=all 时在进程内同时启动 Worker
#   harness-worker    任务消费者，不监听 HTTP；8 种 Worker 按 WORKER_COMPONENTS / WORKER_POOLS 运行
#   harness-migrate   数据库迁移，执行完即退出
#   harness-backup    备份工具，支持 encrypt/decrypt/stats/verify 子命令
#   harness-retention 清理超过保留期的脱敏执行记录与审计数据，默认 dry-run，-apply 才删除
#   harness-admin     创建首个管理员账号，密码只从标准输入读取
#   harness-reencrypt 将 Provider/MCP 凭据从旧密钥版本重加密到当前活动密钥，默认 dry-run
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-agent ./cmd/server \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-migrate ./cmd/migrate \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-backup ./cmd/backup \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-retention ./cmd/retention \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-admin ./cmd/admin \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-reencrypt ./cmd/reencrypt

FROM alpine:3.22
RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -S -g 10001 harness \
    && adduser -S -D -H -u 10001 -G harness harness
WORKDIR /app
# 常驻服务入口：默认启动 API；Worker 由编排系统覆盖 command/entrypoint 为 /app/harness-worker。
COPY --from=server-builder /out/harness-agent /app/harness-agent
COPY --from=server-builder /out/harness-worker /app/harness-worker
# 一次性运维命令入口：迁移、备份、保留期清理、管理员初始化、凭据重加密，执行完即退出。
COPY --from=server-builder /out/harness-migrate /app/harness-migrate
COPY --from=server-builder /out/harness-backup /app/harness-backup
COPY --from=server-builder /out/harness-retention /app/harness-retention
COPY --from=server-builder /out/harness-admin /app/harness-admin
COPY --from=server-builder /out/harness-reencrypt /app/harness-reencrypt
COPY configs /app/configs
COPY migrations /app/migrations
RUN chown -R harness:harness /app
USER harness
ENV APP_ENV=production TZ=Asia/Shanghai
EXPOSE 8080
# 健康检查直接访问 readiness 端点，便于容器编排判断可服务状态。
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget -qO- http://127.0.0.1:8080/health/ready | grep -Eq '^[{].*"status":"(ready|degraded)"[}]$'
ENTRYPOINT ["/app/harness-agent"]
