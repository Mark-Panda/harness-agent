SHELL := /bin/sh

.PHONY: generate generate-check lint test-unit test-race test-integration test-contract test-breaking test-license test-migration test-coverage test-quality test-web web-install web-build build docker-up docker-stop smoke test-chaos backup-check test-security

generate:
	buf generate
	go generate ./internal/service

generate-check: generate
	git diff --exit-code -- api internal/service/mocks

lint:
	@test -z "$$(gofmt -l api cmd internal pkg)"
	go vet ./...
	buf lint
	$(MAKE) web-install
	npm --prefix web run typecheck

test-unit:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	# 集成测试共享真实 PostgreSQL，串行执行包避免备份基线被其他包的并发写入干扰。
	go test -p 1 -tags=integration ./...

test-contract:
	buf build

# Protobuf 兼容性：与 PR 目标分支或上一提交对比，禁止破坏性变更合入。
test-breaking:
	./scripts/buf_breaking.sh

# 许可证检查：Go 模块与前端依赖均需命中白名单。
test-license:
	./scripts/license_check.sh

# 迁移守卫：静态校验 expand-contract 规则、版本连续性与已发布迁移校验和。
test-migration:
	go test -count=1 -run 'TestMigrations|TestMigrationRules|TestMigrationChecksums' ./internal/data

# 故障注入测试：覆盖依赖不可用、慢调用、连接池耗尽与恢复路径，不依赖真实外部服务。
test-chaos:
	go test -count=1 -run 'TestReadiness|TestRouterSlowProviderTimesOutAndSwitches|TestPrepareMCPTools|TestClientSlowUpstreamFailsFast|TestHotMessageStoreRecoversAfterRedisRestart|TestRedisLimiterFailsClosedThenRecovers|TestWritePlatformError' ./internal/server ./pkg/agent/model ./internal/biz ./pkg/feishu ./internal/data ./internal/platform/ratelimit

backup-check:
	go test -count=1 ./internal/platform/backup ./deploy/backup

# 安全扫描：SAST、依赖/密钥/IaC/镜像扫描、API Fuzz 与模块完整性。
test-security:
	./scripts/security_scan.sh

test-coverage:
	mkdir -p coverage
	go test -coverprofile=coverage/biz.out ./internal/biz
	go test -coverprofile=coverage/service.out ./internal/service
	go test -coverprofile=coverage/auth.out ./internal/platform/auth
	go test -coverprofile=coverage/agent.out ./pkg/agent/...
	go test -coverprofile=coverage/mcp.out ./pkg/mcp
	go test -coverprofile=coverage/einoext.out ./pkg/einoext
	go test -coverprofile=coverage/memory.out ./pkg/memory
	python3 scripts/check_coverage.py coverage/biz.out 80
	python3 scripts/check_coverage.py coverage/service.out 90
	python3 scripts/check_coverage.py coverage/auth.out 90
	python3 scripts/check_coverage.py coverage/agent.out 80
	python3 scripts/check_coverage.py coverage/mcp.out 80
	python3 scripts/check_coverage.py coverage/einoext.out 80
	python3 scripts/check_coverage.py coverage/memory.out 80

test-quality:
	go test -count=1 ./tests/quality
	go test -count=1 -run 'TestPlan|TestSupervisor' ./internal/biz

test-web: web-install
	npm --prefix web test

web-install:
	npm --prefix web ci

web-build: web-install
	npm --prefix web run build
	rm -rf internal/server/static/dist
	mkdir -p internal/server/static/dist
	cp -R web/dist/. internal/server/static/dist/

build: web-build
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/harness-agent ./cmd/server
	CGO_ENABLED=0 go build -trimpath -o bin/harness-migrate ./cmd/migrate
	CGO_ENABLED=0 go build -trimpath -o bin/harness-retention ./cmd/retention
	CGO_ENABLED=0 go build -trimpath -o bin/harness-admin ./cmd/admin
	CGO_ENABLED=0 go build -trimpath -o bin/harness-reencrypt ./cmd/reencrypt
	CGO_ENABLED=0 go build -trimpath -o bin/harness-backup ./cmd/backup
	CGO_ENABLED=0 go build -trimpath -o bin/harness-loadgen ./cmd/loadgen

docker-up:
	docker compose up --build --wait

docker-stop:
	docker compose down

smoke:
	curl --fail --silent --show-error http://localhost:8080/health/ready
	curl --fail --silent --show-error http://localhost:8080/api/v1/health
	curl --fail --silent --show-error http://localhost:8080/api/v1/sse/probe
	curl --fail --silent --show-error http://localhost:8080/ > /dev/null
	nc -z localhost 9190
