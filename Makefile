SHELL := /bin/sh

.PHONY: generate generate-check lint test-unit test-race test-integration test-contract test-web web-install web-build build docker-up docker-stop smoke

generate:
	buf generate

generate-check: generate
	git diff --exit-code -- api

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
	go test -tags=integration ./...

test-contract:
	buf build

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

docker-up:
	docker compose up --build --wait

docker-stop:
	docker compose down

smoke:
	curl --fail --silent --show-error http://localhost:8080/health/ready
	curl --fail --silent --show-error http://localhost:8080/api/v1/health
	curl --fail --silent --show-error http://localhost:8080/api/v1/sse/probe
	curl --fail --silent --show-error http://localhost:8080/ > /dev/null
