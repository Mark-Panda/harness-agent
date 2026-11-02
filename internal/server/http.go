package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/platform/config"
	kratoshttp "github.com/go-kratos/kratos/v3/transport/http"
)

func NewHTTPServer(cfg config.ServerConfig, assets fs.FS) *kratoshttp.Server {
	srv := kratoshttp.NewServer(
		kratoshttp.Address(cfg.HTTP.Address),
		kratoshttp.Timeout(cfg.HTTP.Timeout.Duration),
	)
	srv.HandlePrefix("/", NewHandler(assets))
	return srv
}

func NewHandler(assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", healthHandler("alive"))
	mux.HandleFunc("GET /health/ready", healthHandler("ready"))
	mux.HandleFunc("GET /api/v1/health", healthHandler("ok"))
	mux.HandleFunc("GET /api/v1/sse/probe", sseProbeHandler)
	mux.HandleFunc("GET /metrics", metricsHandler)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "api_not_found"})
	})
	mux.Handle("/", spaHandler(assets))
	return mux
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

func metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprint(w, "harness_agent_up 1\n")
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
