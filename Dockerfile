# syntax=docker/dockerfile:1

FROM node:24.12.0-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25.11-alpine AS server-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web/dist ./internal/server/static/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/harness-agent ./cmd/server

FROM alpine:3.22
RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -S -g 10001 harness \
    && adduser -S -D -H -u 10001 -G harness harness
WORKDIR /app
COPY --from=server-builder /out/harness-agent /app/harness-agent
COPY configs /app/configs
RUN chown -R harness:harness /app
USER harness
ENV APP_ENV=production TZ=Asia/Shanghai
EXPOSE 8080
ENTRYPOINT ["/app/harness-agent"]
