package data

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// 分页默认值与上限，防止单次查询无界返回。
const (
	// DefaultPageLimit 未指定 limit 时的默认页大小。
	DefaultPageLimit = 20
	// MaxPageLimit 单页最大条数。
	MaxPageLimit = 100
)

// Cursor 是游标分页位置，编码为 Base64URL JSON。
type Cursor struct {
	// CreatedAt 上一页最后一条的创建时间。
	CreatedAt time.Time `json:"created_at"`
	// ID 上一页最后一条的 ID，用于同时间戳内稳定排序。
	ID uuid.UUID `json:"id"`
}

// Page 是通用分页结果。
type Page[T any] struct {
	// Items 当前页数据。
	Items []T `json:"items"`
	// NextCursor 下一页游标，为空表示已到末页。
	NextCursor string `json:"next_cursor,omitempty"`
}

// NormalizeLimit 把页大小收敛到 [1, MaxPageLimit]，未指定时使用默认值。
func NormalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultPageLimit
	}
	if limit > MaxPageLimit {
		return MaxPageLimit
	}
	return limit
}

// EncodeCursor 编码游标，缺少时间或 ID 时拒绝，避免产生无效翻页位置。
func EncodeCursor(cursor Cursor) (string, error) {
	if cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return "", errors.New("cursor timestamp and id are required")
	}
	value, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// DecodeCursor 解码游标；空串表示从头开始，非法内容返回错误。
func DecodeCursor(encoded string) (Cursor, error) {
	if encoded == "" {
		return Cursor{}, nil
	}
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Cursor{}, errors.New("invalid cursor encoding")
	}
	var cursor Cursor
	if err := json.Unmarshal(value, &cursor); err != nil {
		return Cursor{}, errors.New("invalid cursor payload")
	}
	if cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return Cursor{}, errors.New("invalid cursor values")
	}
	return cursor, nil
}
