package feishu

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestLongConnectionReconcilesBotsAndReportsHealth 验证新增、删除、凭据轮换与健康状态恢复。
func TestLongConnectionReconcilesBotsAndReportsHealth(t *testing.T) {
	aV1 := LongConnectionBot{ID: "bot-a", AppID: "cli-a", AppSecret: "secret-a-1"}
	bV1 := LongConnectionBot{ID: "bot-b", AppID: "cli-b", AppSecret: "secret-b-1"}
	bV2 := LongConnectionBot{ID: "bot-b", AppID: "cli-b", AppSecret: "secret-b-2"}
	var configMu sync.Mutex
	desired := []LongConnectionBot{aV1}
	var loadErr error
	connection, err := NewLongConnection(LongConnectionConfig{
		Bots: desired,
		LoadBots: func(context.Context) ([]LongConnectionBot, error) {
			configMu.Lock()
			defer configMu.Unlock()
			return append([]LongConnectionBot(nil), desired...), loadErr
		},
		RefreshInterval: 10 * time.Millisecond,
		Handler:         func(context.Context, string, string, []byte) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 16)
	cancelled := make(chan string, 16)
	key := func(bot LongConnectionBot) string { return bot.ID + ":" + bot.AppSecret }
	connection.worker = func(ctx context.Context, bot LongConnectionBot) {
		started <- key(bot)
		<-ctx.Done()
		cancelled <- key(bot)
	}
	setDesired := func(bots ...LongConnectionBot) {
		configMu.Lock()
		desired = append([]LongConnectionBot(nil), bots...)
		loadErr = nil
		configMu.Unlock()
	}
	assertEvent := func(channel <-chan string, want string) {
		t.Helper()
		select {
		case got := <-channel:
			if got != want {
				t.Fatalf("event = %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	waitHealth := func(wantError bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := connection.Health()
			if (err != nil) == wantError {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("health error = %v, wantError = %t", err, wantError)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		connection.Start(ctx)
		close(done)
	}()
	assertEvent(started, key(aV1))

	// 新增机器人不影响已有连接。
	setDesired(aV1, bV1)
	assertEvent(started, key(bV1))
	// 凭据轮换只重建对应机器人。
	setDesired(aV1, bV2)
	assertEvent(cancelled, key(bV1))
	assertEvent(started, key(bV2))
	// 停用或删除机器人会取消对应连接。
	setDesired(bV2)
	assertEvent(cancelled, key(aV1))

	connection.setBotError(bV2, errors.New("connection failed"))
	waitHealth(true)
	connection.setBotError(bV2, nil)
	waitHealth(false)
	configMu.Lock()
	loadErr = errors.New("database unavailable")
	configMu.Unlock()
	waitHealth(true)
	setDesired(bV2)
	waitHealth(false)

	cancel()
	assertEvent(cancelled, key(bV2))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("long connection did not stop")
	}
}
