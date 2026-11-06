package data

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// runEventChannelPrefix 是 Run 事件通知频道前缀，频道名为前缀加 Run ID。
const runEventChannelPrefix = "harness:run-events:"

// runEventReconnectDelay Redis 订阅断开后的重连间隔。
const runEventReconnectDelay = time.Second

// RunEventBus 基于 Redis Pub/Sub 向本实例的 SSE 连接广播「Run 有新事件」通知。
// 订阅端按 Run ID 扇出到本地等待者；通知只承载信号，不承载事件内容，事件仍从 PostgreSQL 读取。
type RunEventBus struct {
	// client Redis 客户端，支持单机与集群。
	client redis.UniversalClient
	// mu 保护 subscribers。
	mu sync.Mutex
	// subscribers Run ID 到等待者集合。
	subscribers map[uuid.UUID]map[chan struct{}]struct{}
	// stop 关闭信号。
	stop chan struct{}
	// once 保证 Close 幂等。
	once sync.Once
}

// NewRunEventBus 构造事件总线并启动订阅协程；client 为空时返回 nil，调用方按无通知模式降级。
func NewRunEventBus(client redis.UniversalClient) *RunEventBus {
	if client == nil {
		return nil
	}
	bus := &RunEventBus{
		client:      client,
		subscribers: map[uuid.UUID]map[chan struct{}]struct{}{},
		stop:        make(chan struct{}),
	}
	go bus.subscribeLoop()
	return bus
}

// Publish 通知指定 Run 有新事件；先本地扇出保证同实例即时可见，再尽力广播到其他实例。
// 失败不返回错误：通知是加速手段，SSE 兜底轮询仍能发现漏掉的事件。
func (b *RunEventBus) Publish(ctx context.Context, runID uuid.UUID) {
	if b == nil || runID == uuid.Nil {
		return
	}
	b.fanout(runID)
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = b.client.Publish(pubCtx, runEventChannelPrefix+runID.String(), "1").Err()
}

// Subscribe 订阅指定 Run 的通知；返回信号通道与退订函数。
func (b *RunEventBus) Subscribe(runID uuid.UUID) (<-chan struct{}, func()) {
	channel := make(chan struct{}, 1)
	if b == nil {
		return channel, func() {}
	}
	b.mu.Lock()
	waiters := b.subscribers[runID]
	if waiters == nil {
		waiters = map[chan struct{}]struct{}{}
		b.subscribers[runID] = waiters
	}
	waiters[channel] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return channel, func() {
		once.Do(func() {
			b.mu.Lock()
			if current := b.subscribers[runID]; current != nil {
				delete(current, channel)
				if len(current) == 0 {
					delete(b.subscribers, runID)
				}
			}
			b.mu.Unlock()
		})
	}
}

// Close 停止订阅协程；重复调用安全。
func (b *RunEventBus) Close() {
	if b == nil {
		return
	}
	b.once.Do(func() { close(b.stop) })
}

// fanout 向本实例等待者发送非阻塞信号；通道缓冲为 1，重复通知会被合并。
func (b *RunEventBus) fanout(runID uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for channel := range b.subscribers[runID] {
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}

// subscribeLoop 维护 Redis 订阅；连接断开后按固定间隔重连，直到总线关闭。
func (b *RunEventBus) subscribeLoop() {
	for {
		select {
		case <-b.stop:
			return
		default:
		}
		pubsub := b.client.PSubscribe(context.Background(), runEventChannelPrefix+"*")
		// Receive 等待订阅确认，避免把确认前的事件当作已订阅。
		subscribeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := pubsub.Receive(subscribeCtx)
		cancel()
		if err != nil {
			_ = pubsub.Close()
			if !b.waitReconnect() {
				return
			}
			continue
		}
		channels := pubsub.Channel()
		for message := range channels {
			runID := strings.TrimPrefix(message.Channel, runEventChannelPrefix)
			if parsed, parseErr := uuid.Parse(runID); parseErr == nil {
				b.fanout(parsed)
			}
		}
		_ = pubsub.Close()
		if !b.waitReconnect() {
			return
		}
	}
}

// waitReconnect 等待重连间隔；总线已关闭时返回 false。
func (b *RunEventBus) waitReconnect() bool {
	timer := time.NewTimer(runEventReconnectDelay)
	defer timer.Stop()
	select {
	case <-b.stop:
		return false
	case <-timer.C:
		return true
	}
}
