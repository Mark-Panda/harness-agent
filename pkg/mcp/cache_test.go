package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countListCalls 统计 tools/list 次数，用于验证缓存命中不重复发现。
func countListCalls(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	return mockRPCServer(t, func(method string, _ json.RawMessage, _ *http.Request, _ http.ResponseWriter) (any, bool) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		case "tools/list":
			atomic.AddInt32(calls, 1)
			return map[string]any{"tools": []map[string]any{{"name": "search", "description": "search"}}}, true
		}
		return nil, false
	})
}

// TestClientCacheReusesClientAndTools 验证缓存命中复用客户端与工具清单，失效后重新发现。
func TestClientCacheReusesClientAndTools(t *testing.T) {
	var listCalls int32
	server := countListCalls(t, &listCalls)
	cache := NewClientCache(time.Minute)
	key := CacheKey("cache-key", nil, false, time.Second)
	build := func(context.Context) (*Client, error) {
		return NewClient(server.URL, "", "", 5*time.Second, WithHTTPClient(server.Client()))
	}

	first, tools, err := cache.Get(context.Background(), key, build)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Fatalf("first discovery tools/list calls = %d", got)
	}

	second, cachedTools, err := cache.Get(context.Background(), key, build)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("cache hit must reuse the same client")
	}
	if len(cachedTools) != 1 || cachedTools[0].Name != "search" {
		t.Fatalf("unexpected cached tools: %#v", cachedTools)
	}
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Fatalf("cache hit must not repeat tools/list, calls = %d", got)
	}

	cache.Invalidate(key)
	if _, _, err := cache.Get(context.Background(), key, build); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&listCalls); got != 2 {
		t.Fatalf("invalidate must trigger rediscovery, calls = %d", got)
	}
}

// TestClientCacheSingleFlight 验证同一 key 的并发未命中只触发一次远端发现。
func TestClientCacheSingleFlight(t *testing.T) {
	release := make(chan struct{})
	var listCalls int32
	server := mockRPCServer(t, func(method string, _ json.RawMessage, _ *http.Request, _ http.ResponseWriter) (any, bool) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2025-06-18"}, true
		case "notifications/initialized":
			return nil, false
		case "tools/list":
			atomic.AddInt32(&listCalls, 1)
			<-release
			return map[string]any{"tools": []map[string]any{{"name": "search"}}}, true
		}
		return nil, false
	})
	cache := NewClientCache(time.Minute)
	key := "single-flight"
	build := func(context.Context) (*Client, error) {
		return NewClient(server.URL, "", "", 5*time.Second, WithHTTPClient(server.Client()))
	}

	var waiters sync.WaitGroup
	waiters.Add(1)
	go func() {
		defer waiters.Done()
		if _, _, err := cache.Get(context.Background(), key, build); err != nil {
			t.Errorf("first get failed: %v", err)
		}
	}()
	// 等待首个发现进入 tools/list，再启动并发等待者。
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&listCalls) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first discovery did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 4; i++ {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			if _, _, err := cache.Get(context.Background(), key, build); err != nil {
				t.Errorf("concurrent get failed: %v", err)
			}
		}()
	}
	close(release)
	waiters.Wait()
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Fatalf("single flight must discover once, calls = %d", got)
	}
}

// TestCacheKeySeparatesAuth 验证不同鉴权头生成不同缓存键。
func TestCacheKeySeparatesAuth(t *testing.T) {
	headersA := http.Header{"Authorization": []string{"Bearer a"}}
	headersB := http.Header{"Authorization": []string{"Bearer b"}}
	if CacheKey("https://example.com/mcp", headersA, false, time.Second) ==
		CacheKey("https://example.com/mcp", headersB, false, time.Second) {
		t.Fatal("different credentials must not share a cache entry")
	}
	if CacheKey("https://example.com/mcp", headersA, false, time.Second) !=
		CacheKey("https://example.com/mcp", headersA, false, time.Second) {
		t.Fatal("cache key must be stable")
	}
	// 头顺序不同不应影响键。
	reordered := http.Header{}
	reordered.Set("X-B", "2")
	reordered.Set("X-A", "1")
	same := http.Header{}
	same.Set("X-A", "1")
	same.Set("X-B", "2")
	if CacheKey("https://example.com/mcp", reordered, false, time.Second) !=
		CacheKey("https://example.com/mcp", same, false, time.Second) {
		t.Fatal("header order must not change the cache key")
	}
}
