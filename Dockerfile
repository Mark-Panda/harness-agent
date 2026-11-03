# syntax=docker/dockerfile:1

FROM node:24.12.0-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26.10-alpine AS server-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web/dist ./internal/server/static/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-agent ./cmd/server \
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
COPY --from=server-builder /out/harness-agent /app/harness-agent
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
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget -qO- http://127.0.0.1:8080/health/ready || exit 1
ENTRYPOINT ["/app/harness-agent"]
