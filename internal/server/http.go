package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/config"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/pkg/metrics"
	"github.com/Mark-Panda/harness-agent/pkg/telemetry"
	kratoshttp "github.com/go-kratos/kratos/v3/transport/http"
)

// APILimiters 是全局、IP 与用户三个维度的 API 限流器；为空表示关闭该维度。
type APILimiters struct {
	// Global 全实例维度。
	Global AuthRateLimiter
	// IP 单来源 IP 维度。
	IP AuthRateLimiter
	// User 单登录用户维度。
	User AuthRateLimiter
}

func NewHTTPServer(cfg config.ServerConfig, assets fs.FS, authService AuthApplication, limiter AuthRateLimiter, platform ...*service.PlatformService) (*kratoshttp.Server, error) {
	return NewHTTPServerWithLimits(cfg, assets, authService, limiter, APILimiters{}, platform...)
}

// NewHTTPServerWithLimits 构造带多维限流的 HTTP Server。
func NewHTTPServerWithLimits(cfg config.ServerConfig, assets fs.FS, authService AuthApplication, limiter AuthRateLimiter, apiLimits APILimiters, platform ...*service.PlatformService) (*kratoshttp.Server, error) {
	// SSE 容量与写入超时来自配置，避免慢消费者无限占用实例资源。
	SetSSELimits(cfg.SSE.MaxConnections, cfg.SSE.WriteTimeout.Duration)
	options := []kratoshttp.ServerOption{
		kratoshttp.Address(cfg.HTTP.Address),
		kratoshttp.Timeout(cfg.HTTP.Timeout.Duration),
	}
	if cfg.HTTP.TLS.Enabled {
		certificate, err := tls.LoadX509KeyPair(cfg.HTTP.TLS.CertFile, cfg.HTTP.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load HTTP TLS certificate: %w", err)
		}
		minVersion := uint16(tls.VersionTLS13)
		if cfg.HTTP.TLS.MinVersion == "1.2" {
			minVersion = tls.VersionTLS12
		}
		options = append(options, kratoshttp.TLSConfig(&tls.Config{
			Certificates: []tls.Certificate{certificate}, MinVersion: minVersion,
		}))
	}
	// 可信代理配置在启动时解析一次；非法 CIDR 直接拒绝启动，避免带病运行。
	ipResolver, err := newClientIPResolver(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	srv := kratoshttp.NewServer(options...)
	srv.HandlePrefix("/", newHandler(assets, authService, limiter, authCookiePolicy{Secure: cfg.CookieSecure}, apiLimits, ipResolver, platform...))
	return srv, nil
}

func NewHandler(assets fs.FS, authService AuthApplication, limiter AuthRateLimiter, platform ...*service.PlatformService) http.Handler {
	return newHandler(assets, authService, limiter, authCookiePolicy{}, APILimiters{}, nil, platform...)
}

// NewHandlerWithLimits 构造带多维限流的测试/嵌入 Handler。
func NewHandlerWithLimits(assets fs.FS, authService AuthApplication, limiter AuthRateLimiter, apiLimits APILimiters, platform ...*service.PlatformService) http.Handler {
	return newHandler(assets, authService, limiter, authCookiePolicy{}, apiLimits, nil, platform...)
}

// newHandler 装配完整 HTTP 路由；cookies 控制鉴权 Cookie 的安全属性，
// ipResolver 决定限流与审计使用的客户端 IP。
func newHandler(assets fs.FS, authService AuthApplication, limiter AuthRateLimiter, cookies authCookiePolicy, apiLimits APILimiters, ipResolver *clientIPResolver, platform ...*service.PlatformService) http.Handler {
	authHandler := &authHTTPHandler{service: authService, limiter: limiter, userLimiter: apiLimits.User, ipResolver: ipResolver, cookies: cookies}
	mux := http.NewServeMux()
	if len(platform) > 0 && platform[0] != nil {
		registerPlatformRoutes(mux, authHandler, platform[0])
	}
	if authService != nil {
		mux.HandleFunc("GET /api/v1/auth/csrf", authHandler.csrf)
		mux.HandleFunc("POST /api/v1/auth/register", authHandler.register)
		mux.HandleFunc("POST /api/v1/auth/login", authHandler.login)
		mux.HandleFunc("POST /api/v1/auth/refresh", authHandler.refresh)
		mux.HandleFunc("POST /api/v1/auth/logout", authHandler.logout)
		mux.HandleFunc("POST /api/v1/auth/logout-all", authHandler.requireAuth(authHandler.logoutAll))
		mux.HandleFunc("POST /api/v1/auth/password", authHandler.requireAuth(authHandler.changePassword))
		mux.HandleFunc("POST /api/v1/auth/password/reset", authHandler.resetPassword)
		// 网页飞书登录：state 防重放，回调成功后按浏览器 Cookie 模式签发会话。
		mux.HandleFunc("GET /api/v1/auth/feishu/status", authHandler.feishuStatus)
		mux.HandleFunc("GET /api/v1/auth/feishu/start", authHandler.feishuStart)
		mux.HandleFunc("GET /api/v1/auth/feishu/callback", authHandler.feishuCallback)
		mux.HandleFunc("POST /api/v1/admin/users/{id}/status", authHandler.requireAuth(authHandler.setUserStatus))
		mux.HandleFunc("POST /api/v1/admin/users/{id}/password-reset", authHandler.requireAuth(authHandler.issuePasswordReset))
		mux.HandleFunc("POST /api/v1/admin/users", authHandler.requireAuth(authHandler.createUser))
		mux.HandleFunc("GET /api/v1/admin/users", authHandler.requireAuth(authHandler.listUsers))
		mux.HandleFunc("POST /api/v1/admin/users/{id}/role", authHandler.requireAuth(authHandler.setUserRole))
	}
	mux.HandleFunc("GET /health/live", healthHandler("alive"))
	mux.HandleFunc("GET /health/ready", readinessHandler)
	mux.HandleFunc("GET /api/v1/health", healthHandler("ok"))
	mux.HandleFunc("GET /api/v1/sse/probe", sseProbeHandler)
	mux.HandleFunc("GET /metrics", metrics.Handler().ServeHTTP)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "api_not_found"})
	})
	mux.Handle("/", spaHandler(assets))
	apiHandler := apiRateLimitMiddleware(apiLimits, ipResolver, mux)
	// 指标中间件位于追踪内层：路由模板由 ServeMux 回填后作为低基数标签；追踪仍为每个请求建立 Span。
	return securityHeaders(cookies.Secure, telemetry.HTTPMiddleware(metrics.Middleware(apiHandler)))
}

// apiRateLimitMiddleware 对 /api/v1 请求执行全局与来源 IP 维度限流；
// 用户维度由 requireAuth 在解析 Principal 后执行。
func apiRateLimitMiddleware(limits APILimiters, ipResolver *clientIPResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			if limits.Global != nil && !allowAPIRate(w, r, limits.Global, "api_global") {
				return
			}
			if limits.IP != nil && !allowAPIRate(w, r, limits.IP, "api_ip:"+ipResolver.resolve(r)) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// allowAPIRate 执行单维度限流并输出 429/503；失败关闭，避免依赖故障绕过保护。
func allowAPIRate(w http.ResponseWriter, r *http.Request, limiter AuthRateLimiter, key string) bool {
	result, err := limiter.Allow(r.Context(), key)
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "rate_limit_unavailable")
		return false
	}
	if !result.Allowed {
		seconds := int(result.RetryAfter.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
		writeErrorCode(w, http.StatusTooManyRequests, "rate_limited")
		return false
	}
	return true
}

// securityHeaders 为所有响应添加浏览器安全头；启用 Secure Cookie 时视为 HTTPS 部署并附加 HSTS。
func securityHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		header.Set("Cross-Origin-Resource-Policy", "same-origin")
		// 前端仅加载同源脚本与样式；禁止内联脚本、对象嵌入和跨站表单提交。
		header.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'")
		if hsts {
			header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func healthHandler(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
	}
}

func sseProbeHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	_, _ = fmt.Fprint(w, "event: heartbeat\ndata: {\"status\":\"ok\"}\n\n")
	flusher.Flush()
}

func spaHandler(assets fs.FS) http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}
		file, err := assets.Open(name)
		if err != nil {
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
		} else {
			_ = file.Close()
		}
		if name == "index.html" {
			content, err := fs.ReadFile(assets, "index.html")
			if err != nil {
				http.Error(w, "web assets unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(content))
			return
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = "/" + name
		files.ServeHTTP(w, clone)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
