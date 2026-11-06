package data_test

import (
	"context"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/data"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TestRunEventBusDeliversAcrossInstances 验证通知经 Redis 跨实例扇出，且只命中对应 Run。
func TestRunEventBusDeliversAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	publisherClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	subscriberClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = publisherClient.Close()
		_ = subscriberClient.Close()
	})
	publisher := data.NewRunEventBus(publisherClient)
	subscriber := data.NewRunEventBus(subscriberClient)
	t.Cleanup(publisher.Close)
	t.Cleanup(subscriber.Close)

	runID := uuid.New()
	channel, unsubscribe := subscriber.Subscribe(runID)
	defer unsubscribe()
	otherChannel, unsubscribeOther := subscriber.Subscribe(uuid.New())
	defer unsubscribeOther()

	ctx := context.Background()
	// 订阅确认在独立协程完成，重试发布直到收到通知，避免与订阅建立竞争。
	deadline := time.Now().Add(3 * time.Second)
	for {
		publisher.Publish(ctx, runID)
		select {
		case <-channel:
			goto received
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("cross-instance notification was not delivered")
		}
	}
received:
	select {
	case <-otherChannel:
		t.Fatal("notification leaked to another run")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRunEventBusSubscribeUnsubscribe 验证退订后不再收到通知，且 nil 总线安全降级。
func TestRunEventBusSubscribeUnsubscribe(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	bus := data.NewRunEventBus(client)
	t.Cleanup(bus.Close)

	runID := uuid.New()
	channel, unsubscribe := bus.Subscribe(runID)
	bus.Publish(context.Background(), runID)
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatal("local notification was not delivered")
	}
	unsubscribe()
	bus.Publish(context.Background(), runID)
	select {
	case <-channel:
		t.Fatal("unsubscribed channel must stop receiving notifications")
	case <-time.After(100 * time.Millisecond):
	}

	// 未注入 Redis 时返回 nil 总线，方法调用必须安全。
	var nilBus *data.RunEventBus
	nilBus.Publish(context.Background(), runID)
	nilBus.Close()
}
