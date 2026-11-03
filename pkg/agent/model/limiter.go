package model

import "context"

// ConcurrencyLimiter 是按作用域隔离的并发舱壁，用于限制全局、用户与供应商并发。
// 实现方通常使用 Redis；Acquire 阻塞等待，TryAcquire 立即返回是否获得槽位。
type ConcurrencyLimiter interface {
	// Acquire 获取一个并发槽位并返回释放函数；上下文取消时返回错误。
	Acquire(ctx context.Context, key string) (release func(), err error)
	// TryAcquire 非阻塞获取槽位；未获得时返回 false。
	TryAcquire(ctx context.Context, key string) (release func(), acquired bool, err error)
}
