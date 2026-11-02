# harness-agent

工程级全栈多用户 Agent 系统，当前处于阶段 0 工程基线。

## 本地运行

```bash
go run ./cmd/server
```

另开终端运行前端开发服务：

```bash
npm --prefix web install
npm --prefix web run dev
```

## 验证与构建

```bash
make generate-check
make lint
make test-unit
make test-race
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
