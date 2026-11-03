package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrIdempotencyConflict 表示同一幂等键对应了不同请求，或首个请求仍在处理中。
var ErrIdempotencyConflict = errors.New("idempotency key conflict")

// IdempotencyRecord 是一次幂等请求的持久化状态。
type IdempotencyRecord struct {
	// UserID 幂等键归属用户，隔离维度之一。
	UserID uuid.UUID
	// Key 客户端提供的幂等键，按用户唯一。
	Key string
	// RequestHash 请求指纹，用于识别同键不同体。
	RequestHash []byte
	// Status 状态：in_progress/completed/failed。
	Status string
	// ResponseJSON 已完成请求的响应信封（含状态码与响应体）。
	ResponseJSON []byte
	// ResourceType 资源类型，便于审计追溯。
	ResourceType string
	// ResourceID 资源标识，便于审计追溯。
	ResourceID string
	// ExpiresAt 幂等记录过期时间。
	ExpiresAt time.Time
}

// IdempotencyOutcome 描述 Begin 的结果：是否回放以及原始响应。
type IdempotencyOutcome struct {
	// Replay 为真时表示命中已完成记录，应直接返回 Status/Body。
	Replay bool
	// Status 原始 HTTP 状态码。
	Status int
	// Body 原始响应体。
	Body []byte
}

// IdempotencyRepository 定义幂等记录的持久化契约。
type IdempotencyRepository interface {
	// Begin 以原子方式占位：新键插入 in_progress，旧键校验指纹并返回回放或冲突。
	Begin(ctx context.Context, userID uuid.UUID, key string, requestHash []byte, resourceType string, expiresAt time.Time) (IdempotencyOutcome, error)
	// Complete 保存成功响应，供后续同键请求回放。
	Complete(ctx context.Context, userID uuid.UUID, key string, envelope []byte, resourceID string) error
	// Fail 标记失败，允许客户端使用同一幂等键重试。
	Fail(ctx context.Context, userID uuid.UUID, key string) error
}

// IdempotencyService 封装幂等协议：指纹校验、结果回放与失败释放。
type IdempotencyService struct {
	repo IdempotencyRepository
}

// NewIdempotencyService 构造幂等服务。
func NewIdempotencyService(repo IdempotencyRepository) (*IdempotencyService, error) {
	if repo == nil {
		return nil, invalid("idempotency repository is required")
	}
	return &IdempotencyService{repo: repo}, nil
}

// Begin 校验幂等键与请求指纹，返回是否需要回放。
func (s *IdempotencyService) Begin(ctx context.Context, userID uuid.UUID, key, resourceType string, requestHash []byte) (IdempotencyOutcome, error) {
	if userID == uuid.Nil {
		return IdempotencyOutcome{}, ErrForbidden
	}
	if len(key) < 1 || len(key) > 128 {
		return IdempotencyOutcome{}, invalid("idempotency key must contain between 1 and 128 bytes")
	}
	if len(requestHash) != 32 {
		return IdempotencyOutcome{}, invalid("idempotency request hash must be 32 bytes")
	}
	if resourceType == "" || len(resourceType) > 64 {
		return IdempotencyOutcome{}, invalid("idempotency resource type is required")
	}
	return s.repo.Begin(ctx, userID, key, requestHash, resourceType, time.Now().UTC().Add(24*time.Hour))
}

// Complete 保存成功响应。
func (s *IdempotencyService) Complete(ctx context.Context, userID uuid.UUID, key string, status int, body []byte, resourceID string) error {
	if status < 200 || status >= 300 {
		return invalid("idempotency completion requires a 2xx response")
	}
	envelope, err := json.Marshal(map[string]any{"status": status, "body": json.RawMessage(body)})
	if err != nil {
		return err
	}
	return s.repo.Complete(ctx, userID, key, envelope, resourceID)
}

// Fail 释放幂等键，允许重试。
func (s *IdempotencyService) Fail(ctx context.Context, userID uuid.UUID, key string) error {
	return s.repo.Fail(ctx, userID, key)
}

// ReplayOutcome 解析已保存的响应信封，返回原始状态码与响应体。
func ReplayOutcome(stored []byte) (IdempotencyOutcome, error) {
	var envelope struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	if len(stored) == 0 || json.Unmarshal(stored, &envelope) != nil || envelope.Status < 200 || envelope.Status >= 300 {
		return IdempotencyOutcome{}, fmt.Errorf("%w: stored response is invalid", ErrIdempotencyConflict)
	}
	return IdempotencyOutcome{Replay: true, Status: envelope.Status, Body: envelope.Body}, nil
}
