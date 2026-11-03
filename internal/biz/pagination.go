package biz

import (
	"time"

	"github.com/google/uuid"
)

// Cursor 是游标分页位置，按创建时间与 ID 降序稳定翻页。
type Cursor struct {
	// CreatedAt 上一页最后一条的创建时间。
	CreatedAt time.Time
	// ID 上一页最后一条的 ID，用于同一时间戳内去重。
	ID uuid.UUID
}

// Page 是通用分页结果。
type Page[T any] struct {
	// Items 当前页数据。
	Items []T
	// NextCursor 下一页游标，为空表示没有更多数据。
	NextCursor string
}
