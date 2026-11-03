# harness-agent

工程级全栈多用户 Agent 系统，当前处于阶段 1 数据库、鉴权与隔离基座。

## 本地运行

```bash
export DATABASE_URL=postgres://harness_app:harness@localhost:5432/harness?sslmode=disable
export AUTH_DATABASE_URL=postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable
export JWT_SIGNING_KEY_ID=dev
export JWT_SIGNING_KEY_SECRET=dev-only-jwt-secret-01234567890123456789
export REFRESH_TOKEN_PEPPER=dev-only-refresh-pepper-0123456789012345
export AUDIT_PSEUDONYM_KEY=dev-only-audit-key-01234567890123456789
go run ./cmd/server
```

另开终端运行前端开发服务：

```bash
npm --prefix web install
npm --prefix web run dev
```

数据库迁移：

```bash
go run ./cmd/migrate up
go run ./cmd/migrate version
```

Docker 环境会先运行迁移，再启动应用。

## 验证与构建

```bash
make generate-check
make lint
make test-unit
make test-race
make test-integration
make test-contract
make test-breaking
make test-license
make test-migration
make test-coverage
make test-web
make build
```

## Docker 开发环境

```bash
make docker-up
make smoke
```

服务地址：Web `http://localhost:8080`，Jaeger `http://localhost:16686`，Prometheus `http://localhost:9090`。

停止环境：

```bash
make docker-stop
```

生产密钥只通过环境变量或 Secret Manager 注入，不得写入 YAML 或镜像。
