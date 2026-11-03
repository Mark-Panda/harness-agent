package data

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// Data 层通用错误，供上层统一映射。
var (
	// ErrNotFound 记录不存在。
	ErrNotFound = errors.New("data not found")
	// ErrConflict 唯一约束冲突。
	ErrConflict = errors.New("data conflict")
	// ErrTimeout 上下文超时或取消。
	ErrTimeout = errors.New("data operation timed out")
	// ErrUnavailable 数据存储不可用。
	ErrUnavailable = errors.New("data store unavailable")
)

// MapError 把 GORM 与上下文错误映射为数据层稳定错误。
func MapError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return ErrNotFound
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return ErrConflict
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return ErrTimeout
	default:
		return err
	}
}
