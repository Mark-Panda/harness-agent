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

// LongConnectionLoader 读取当前应启用的机器人凭据，供运行期周期性协调。
type LongConnectionLoader func(ctx context.Context) ([]LongConnectionBot, error)

// LongConnectionConfig 是长连接运行配置。
type LongConnectionConfig struct {
	// Bots 启动时需要建立长连接的机器人。
	Bots []LongConnectionBot
	// LoadBots 非空时周期性加载最新机器人，处理新增、停用、删除与凭据轮换。
	LoadBots LongConnectionLoader
	// RefreshInterval 动态配置刷新间隔，默认 10 秒。
	RefreshInterval time.Duration
	// Handler 处理收到的原始事件。
	Handler LongConnectionHandler
	// Logger 运行日志；为空时使用默认 logger。
	Logger *slog.Logger
}

// longConnectionBotState 记录单条连接的运行句柄与最近一次错误。
type longConnectionBotState struct {
	bot     LongConnectionBot
	cancel  context.CancelFunc
	lastErr error
}

// LongConnection 为每个机器人维持一条飞书 WebSocket 长连接，并支持运行期协调。
type LongConnection struct {
	initialBots     []LongConnectionBot
	loadBots        LongConnectionLoader
	refreshInterval time.Duration
	handler         LongConnectionHandler
	logger          *slog.Logger
	// worker 可在包内测试中替换，避免建立真实飞书连接。
	worker func(context.Context, LongConnectionBot)

	mu          sync.Mutex
	bots        map[string]*longConnectionBotState
	lastLoadErr error
	workers     sync.WaitGroup
}

// NewLongConnection 构造长连接运行器；没有机器人且未配置加载器时 Start 立即返回。
func NewLongConnection(config LongConnectionConfig) (*LongConnection, error) {
	if config.Handler == nil {
		return nil, fmt.Errorf("feishu long connection handler is required")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	refreshInterval := config.RefreshInterval
	if refreshInterval <= 0 {
		refreshInterval = 10 * time.Second
	}
	return &LongConnection{
		initialBots: sanitizeLongConnectionBots(config.Bots), loadBots: config.LoadBots,
		refreshInterval: refreshInterval, handler: config.Handler, logger: logger,
		bots: make(map[string]*longConnectionBotState),
	}, nil
}

// Start 运行连接协调器，直到上下文取消；加载器非空时即使没有初始机器人也持续等待配置。
func (l *LongConnection) Start(ctx context.Context) {
	if len(l.initialBots) == 0 && l.loadBots == nil {
		l.logger.Warn("feishu long connection has no enabled bots")
		return
	}
	l.reconcile(ctx, l.initialBots)
	if l.loadBots == nil {
		<-ctx.Done()
		l.stopAll()
		l.wait()
		return
	}
	ticker := time.NewTicker(l.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.stopAll()
			l.wait()
			return
		case <-ticker.C:
			bots, err := l.loadBots(ctx)
			if err != nil {
				if ctx.Err() == nil {
					l.mu.Lock()
					l.lastLoadErr = err
					l.mu.Unlock()
					l.logger.Warn("refresh feishu long connection bots failed", "error", err)
				}
				continue
			}
			l.mu.Lock()
			l.lastLoadErr = nil
			l.mu.Unlock()
			l.reconcile(ctx, bots)
		}
	}
}

// sanitizeLongConnectionBots 过滤无效凭据并按机器人 ID 去重，最后一次配置生效。
func sanitizeLongConnectionBots(bots []LongConnectionBot) []LongConnectionBot {
	byID := make(map[string]LongConnectionBot, len(bots))
	for _, bot := range bots {
		if strings.TrimSpace(bot.ID) == "" || strings.TrimSpace(bot.AppID) == "" || strings.TrimSpace(bot.AppSecret) == "" {
			continue
		}
		byID[bot.ID] = bot
	}
	result := make([]LongConnectionBot, 0, len(byID))
	for _, bot := range byID {
		result = append(result, bot)
	}
	return result
}

// reconcile 只启动新增或凭据变化的连接，只停止删除或变化的连接，未变化的连接保持不动。
func (l *LongConnection) reconcile(ctx context.Context, bots []LongConnectionBot) {
	desiredList := sanitizeLongConnectionBots(bots)
	desired := make(map[string]LongConnectionBot, len(desiredList))
	for _, bot := range desiredList {
		desired[bot.ID] = bot
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, state := range l.bots {
		bot, keep := desired[id]
		if keep && state.bot == bot {
			continue
		}
		state.cancel()
		delete(l.bots, id)
	}
	for id, bot := range desired {
		if _, exists := l.bots[id]; exists {
			continue
		}
		workerContext, cancel := context.WithCancel(ctx)
		state := &longConnectionBotState{bot: bot, cancel: cancel}
		l.bots[id] = state
		worker := l.worker
		if worker == nil {
			worker = l.runBot
		}
		l.workers.Add(1)
		go func(bot LongConnectionBot, workerContext context.Context) {
			defer l.workers.Done()
			worker(workerContext, bot)
		}(bot, workerContext)
	}
}

// stopAll 取消全部连接；实际退出由 wait 等待。
func (l *LongConnection) stopAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, state := range l.bots {
		state.cancel()
	}
}

// wait 等待所有已启动的连接退出。
func (l *LongConnection) wait() {
	l.workers.Wait()
}

// setBotError 更新仍然活跃的连接的最近错误，已删除连接的旧 goroutine 不得污染新状态。
func (l *LongConnection) setBotError(bot LongConnectionBot, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if state, exists := l.bots[bot.ID]; exists && state.bot == bot {
		state.lastErr = err
	}
}

// Health 返回配置刷新或长连接重试状态；没有启用机器人属于正常空配置。
func (l *LongConnection) Health() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastLoadErr != nil {
		return fmt.Errorf("feishu long connection configuration refresh failed")
	}
	for id, state := range l.bots {
		if state.lastErr != nil {
			return fmt.Errorf("feishu long connection bot %s is unavailable", id)
		}
	}
	return nil
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
		l.setBotError(bot, nil)
		err := client.Start(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("connection stopped unexpectedly")
		}
		l.setBotError(bot, err)
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
