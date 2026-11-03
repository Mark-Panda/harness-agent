package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"

	"sort"
	"strings"
	"sync"
	"time"
)

// main 是 API 与 SSE 负载生成器入口。
func main() {
	base := flag.String("base", "http://localhost:8080", "API base URL")
	token := flag.String("token", "", "existing bearer token; empty registers a load user")
	paths := flag.String("paths", "/api/v1/sessions,/api/v1/agents", "comma separated read paths")
	concurrency := flag.Int("concurrency", 50, "concurrent API workers")
	duration := flag.Duration("duration", 60*time.Second, "load duration")
	rps := flag.Int("rps", 0, "target requests per second; 0 uses closed-loop workers")
	sseConnections := flag.Int("sse", 0, "number of SSE probe connections to hold")
	sseDuration := flag.Duration("sse-duration", 30*time.Second, "SSE hold duration")
	jsonOutput := flag.Bool("json", false, "emit JSON summary only")
	flag.Parse()

	accessToken := *token
	if accessToken == "" {
		registered, err := registerUser(*base)
		if err != nil {
			log.Fatalf("register load user: %v", err)
		}
		accessToken = registered
	}
	if *sseConnections > 0 {
		runSSE(*base, *sseConnections, *sseDuration, *jsonOutput)
		return
	}
	summary, err := runAPI(*base, accessToken, strings.Split(*paths, ","), *concurrency, *duration, *rps)
	if err != nil {
		log.Fatal(err)
	}
	if *jsonOutput {
		encoded, _ := json.Marshal(summary)
		fmt.Println(string(encoded))
		return
	}
	fmt.Printf("请求=%d 错误=%d RPS=%.1f p50=%.1fms p95=%.1fms p99=%.1fms\n",
		summary.Total, summary.Errors, summary.RPS, summary.P50MS, summary.P95MS, summary.P99MS)
}

// loadSummary 是压测结果摘要。
type loadSummary struct {
	Total   int64            `json:"total"`
	Errors  int64            `json:"errors"`
	RPS     float64          `json:"rps"`
	P50MS   float64          `json:"p50_ms"`
	P95MS   float64          `json:"p95_ms"`
	P99MS   float64          `json:"p99_ms"`
	Status  map[string]int64 `json:"status"`
	Elapsed float64          `json:"elapsed_seconds"`
}

// registerUser 注册一个随机负载用户并返回 Bearer Token。
func registerUser(base string) (string, error) {
	username := fmt.Sprintf("load_%d", time.Now().UnixNano())
	password := "LoadTest-0123456789"
	payload := map[string]string{"username": username, "password": password, "transport": "bearer"}
	encoded, _ := json.Marshal(payload)
	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/api/v1/auth/register", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return "", fmt.Errorf("register status %d: %s", response.StatusCode, raw)
	}
	var result struct {
		AccessToken string `json:"access_token"`
		Tokens      struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Tokens.AccessToken != "" {
		return result.Tokens.AccessToken, nil
	}
	if result.AccessToken != "" {
		return result.AccessToken, nil
	}
	return "", fmt.Errorf("register response missing access token")
}

// runAPI 以固定 RPS 或闭环并发执行读请求并统计延迟分位数。
func runAPI(base, token string, paths []string, concurrency int, duration time.Duration, rps int) (loadSummary, error) {
	if concurrency <= 0 || len(paths) == 0 {
		return loadSummary{}, fmt.Errorf("concurrency and paths are required")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var tokens chan struct{}
	if rps > 0 {
		tokens = make(chan struct{}, rps)
		go func() {
			ticker := time.NewTicker(time.Second / time.Duration(rps))
			defer ticker.Stop()
			for range ticker.C {
				select {
				case tokens <- struct{}{}:
				default:
				}
			}
		}()
	}
	var (
		mu       sync.Mutex
		latency  []float64
		statuses = map[string]int64{}
		errorsN  int64
		wg       sync.WaitGroup
	)
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	started := time.Now()
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for index := 0; ; index++ {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if tokens != nil {
					select {
					case <-ctx.Done():
						return
					case <-tokens:
					}
				}
				path := paths[(worker+index)%len(paths)]
				requestStart := time.Now()
				request, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
				request.Header.Set("Authorization", "Bearer "+token)
				response, err := client.Do(request)
				elapsed := float64(time.Since(requestStart).Microseconds()) / 1000
				mu.Lock()
				latency = append(latency, elapsed)
				if err != nil {
					errorsN++
					statuses["error"]++
				} else {
					statuses[fmt.Sprintf("%d", response.StatusCode)]++
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
					_ = response.Body.Close()
					if response.StatusCode >= 400 {
						errorsN++
					}
				}
				mu.Unlock()
			}
		}(worker)
	}
	wg.Wait()
	elapsed := time.Since(started)
	mu.Lock()
	defer mu.Unlock()
	sort.Float64s(latency)
	return loadSummary{
		Total: int64(len(latency)), Errors: errorsN,
		RPS: float64(len(latency)) / elapsed.Seconds(), Status: statuses,
		P50MS: percentile(latency, 0.50), P95MS: percentile(latency, 0.95), P99MS: percentile(latency, 0.99),
		Elapsed: elapsed.Seconds(),
	}, nil
}

// percentile 返回升序样本的分位数；空样本返回 0。
func percentile(sorted []float64, ratio float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(ratio*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

// runSSE 保持多个 SSE 连接并刻意慢速读取，验证慢消费者不会阻塞服务端。
func runSSE(base string, connections int, duration time.Duration, jsonOutput bool) {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var wg sync.WaitGroup
	var completed, errorsN int64
	var mu sync.Mutex
	for index := 0; index < connections; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/v1/sse/probe", nil)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				mu.Lock()
				errorsN++
				mu.Unlock()
				return
			}
			defer response.Body.Close()
			scanner := bufio.NewScanner(response.Body)
			for scanner.Scan() {
				// 模拟慢消费者：每条事件后休眠，服务端必须按写超时主动断开。
				time.Sleep(50 * time.Millisecond)
			}
			mu.Lock()
			if scanner.Err() != nil && ctx.Err() == nil {
				errorsN++
			}
			completed++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if jsonOutput {
		encoded, _ := json.Marshal(map[string]any{"sse_connections": connections, "completed": completed, "errors": errorsN})
		fmt.Println(string(encoded))
		return
	}
	fmt.Printf("SSE 连接=%d 完成=%d 错误=%d 时长=%s\n", connections, completed, errorsN, duration)
}
