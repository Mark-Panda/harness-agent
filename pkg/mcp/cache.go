package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 默认缓存时长与条目上限；工具清单变化低频，60 秒足以吸收同批次重复发现。
const (
	defaultClientCacheTTL        = 60 * time.Second
	defaultClientCacheMaxEntries = 256
)

// ClientCache 缓存 MCP 客户端与工具清单，避免每个 Run 都重新握手与 tools/list。
// 同一 key 的并发发现只执行一次（单飞），后到请求等待首个结果，避免重复打远端服务。
type ClientCache struct {
	// ttl 缓存有效期，从发现成功开始计算。
	ttl time.Duration
	// maxEntries 条目上限，超出时机会式清理过期条目与最早过期条目。
	maxEntries int
	// mu 保护 entries，网络往返期间不持锁。
	mu sync.Mutex
	// entries key 到缓存条目。
	entries map[string]*clientCacheEntry
}

// clientCacheEntry 是单个缓存条目；ready 非空表示发现进行中。
type clientCacheEntry struct {
	// client 已初始化的客户端。
	client *Client
	// tools 发现到的工具清单。
	tools []Tool
	// expires 过期时间。
	expires time.Time
	// ready 发现完成后关闭，等待者据此醒来重试读取。
	ready chan struct{}
}

// NewClientCache 构造客户端缓存；非正 TTL 使用默认值。
func NewClientCache(ttl time.Duration) *ClientCache {
	if ttl <= 0 {
		ttl = defaultClientCacheTTL
	}
	return &ClientCache{ttl: ttl, maxEntries: defaultClientCacheMaxEntries, entries: map[string]*clientCacheEntry{}}
}

// CacheKey 生成稳定的缓存键；鉴权头按名称排序后哈希，避免凭证原文进入内存键。
func CacheKey(endpoint string, headers http.Header, allowPrivate bool, timeout time.Duration) string {
	digest := sha256.Sum256([]byte(canonicalHeaders(headers)))
	return strings.Join([]string{
		strings.TrimSpace(endpoint),
		hex.EncodeToString(digest[:]),
		strconv.FormatBool(allowPrivate),
		timeout.String(),
	}, "\x00")
}

// canonicalHeaders 把请求头规整为可比较字符串，名称与值均按稳定顺序排列。
func canonicalHeaders(headers http.Header) string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		builder.WriteString(strings.ToLower(name))
		builder.WriteString(":")
		builder.WriteString(strings.Join(headers[name], ","))
		builder.WriteString("\n")
	}
	return builder.String()
}

// Get 返回缓存命中的客户端与工具清单；未命中时调用 build 构造客户端并执行工具发现。
// 同一 key 并发未命中会合并为一次远端发现，其余调用等待结果。
func (c *ClientCache) Get(ctx context.Context, key string, build func(context.Context) (*Client, error)) (*Client, []Tool, error) {
	if c == nil {
		client, err := build(ctx)
		if err != nil {
			return nil, nil, err
		}
		tools, err := client.ListTools(ctx)
		if err != nil {
			return nil, nil, err
		}
		return client, tools, nil
	}
	for {
		now := time.Now()
		c.mu.Lock()
		if entry := c.entries[key]; entry != nil {
			if entry.ready == nil {
				if now.Before(entry.expires) {
					client, tools := entry.client, cloneTools(entry.tools)
					c.mu.Unlock()
					return client, tools, nil
				}
				// 过期条目直接移除，由当前调用重新发现。
				delete(c.entries, key)
			} else {
				// 已有并发发现进行中，等待其完成后重新读取缓存。
				ready := entry.ready
				c.mu.Unlock()
				select {
				case <-ready:
					continue
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			}
		}
		// 成为本次发现方：先占位，阻止其他协程重复请求远端。
		entry := &clientCacheEntry{ready: make(chan struct{})}
		c.entries[key] = entry
		c.evictLocked()
		c.mu.Unlock()

		client, err := build(ctx)
		var tools []Tool
		if err == nil {
			tools, err = client.ListTools(ctx)
		}

		c.mu.Lock()
		ready := entry.ready
		if err != nil {
			// 发现失败不缓存，下一次调用重新尝试。
			delete(c.entries, key)
		} else {
			entry.client, entry.tools, entry.expires = client, tools, time.Now().Add(c.ttl)
			entry.ready = nil
		}
		close(ready)
		c.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		return client, cloneTools(tools), nil
	}
}

// Invalidate 主动失效指定键，供管理界面修改 MCP 配置后立即重建连接。
func (c *ClientCache) Invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// evictLocked 在持有锁时清理超限条目；优先清理已过期条目，再清理最早过期条目。
func (c *ClientCache) evictLocked() {
	if len(c.entries) <= c.maxEntries {
		return
	}
	now := time.Now()
	for key, entry := range c.entries {
		if entry.ready == nil && !now.Before(entry.expires) {
			delete(c.entries, key)
		}
	}
	for len(c.entries) > c.maxEntries {
		var oldestKey string
		var oldest time.Time
		for key, entry := range c.entries {
			if entry.ready != nil {
				continue
			}
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = key, entry.expires
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}

// cloneTools 复制工具清单切片，避免调用方修改缓存内容。
func cloneTools(tools []Tool) []Tool {
	if tools == nil {
		return nil
	}
	return append([]Tool(nil), tools...)
}
