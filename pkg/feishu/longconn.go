// 飞书长连接：由服务主动与飞书建立 WebSocket，适合无法暴露公网回调的部署。
package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// LongConnectionBot 是一条长连接使用的机器人应用凭据。
type LongConnectionBot struct {
	// ID 系统内机器人 ID，用于事件入库与审计。
	ID string
	// AppID 飞书应用 App ID。
	AppID string
	// AppSecret 飞书应用 App Secret，仅保存在内存。
	AppSecret string
}

// LongConnectionHandler 接收长连接事件原始载荷。
type LongConnectionHandler func(ctx context.Context, botID, appID string, body []byte) error

// LongConnectionConfig 是长连接运行配置。
type LongConnectionConfig struct {
	// Bots 需要建立长连接的机器人。
	Bots []LongConnectionBot
	// Handler 处理收到的原始事件。
	Handler LongConnectionHandler
	// Logger 运行日志；为空时使用默认 logger。
	Logger *slog.Logger
}

// LongConnection 为每个机器人维持一条飞书 WebSocket 长连接。
type LongConnection struct {
	bots    []LongConnectionBot
	handler LongConnectionHandler
	logger  *slog.Logger
}

// NewLongConnection 构造长连接运行器；没有机器人时 Start 立即返回。
func NewLongConnection(config LongConnectionConfig) (*LongConnection, error) {
	if config.Handler == nil {
		return nil, fmt.Errorf("feishu long connection handler is required")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	bots := make([]LongConnectionBot, 0, len(config.Bots))
	for _, bot := range config.Bots {
		if strings.TrimSpace(bot.ID) == "" || strings.TrimSpace(bot.AppID) == "" || strings.TrimSpace(bot.AppSecret) == "" {
			continue
		}
		bots = append(bots, bot)
	}
	return &LongConnection{bots: bots, handler: config.Handler, logger: logger}, nil
}

// Start 并发运行全部机器人连接，直到上下文取消。
func (l *LongConnection) Start(ctx context.Context) {
	if len(l.bots) == 0 {
		l.logger.Warn("feishu long connection has no enabled bots")
		return
	}
	var workers sync.WaitGroup
	for _, bot := range l.bots {
		bot := bot
		workers.Add(1)
		go func() {
			defer workers.Done()
			l.runBot(ctx, bot)
		}()
	}
	workers.Wait()
}

// runBot 建立单条连接；连接异常退出后按退避重试，直到上下文取消。
func (l *LongConnection) runBot(ctx context.Context, bot LongConnectionBot) {
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		l.logger.Info("feishu long connection starting", "bot_id", bot.ID, "app_id", bot.AppID)
		eventDispatcher := larkdispatcher.NewEventDispatcher("", "")
		// 只订阅对话相关事件，其余事件由入库后的 Worker 统一忽略。
		for _, eventType := range []string{EventTypeMessageReceive, EventTypeMessageRecalled, EventTypeMessageUpdated} {
			eventType := eventType
			eventDispatcher.OnCustomizedEvent(eventType, func(ctx context.Context, request *larkevent.EventReq) error {
				return l.handler(ctx, bot.ID, bot.AppID, request.Body)
			})
		}
		client := larkws.NewClient(bot.AppID, bot.AppSecret,
			larkws.WithEventHandler(eventDispatcher),
			larkws.WithLogLevel(larkcore.LogLevelInfo),
		)
		err := client.Start(ctx)
		if ctx.Err() != nil {
			return
		}
		l.logger.Warn("feishu long connection stopped, retrying", "bot_id", bot.ID, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}
