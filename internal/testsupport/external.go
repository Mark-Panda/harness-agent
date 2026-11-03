package testsupport

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type ExternalResponse struct {
	Status  int
	Body    string
	Delay   time.Duration
	Headers http.Header
}

type FakeExternalServer struct {
	server    *httptest.Server
	mu        sync.Mutex
	responses map[string][]ExternalResponse
	requests  map[string]int
}

func NewFakeExternalServer(t testing.TB) *FakeExternalServer {
	t.Helper()
	fake := &FakeExternalServer{
		responses: make(map[string][]ExternalResponse),
		requests:  make(map[string]int),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *FakeExternalServer) Enqueue(path string, response ExternalResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if response.Status == 0 {
		response.Status = http.StatusOK
	}
	f.responses[path] = append(f.responses[path], response)
}

func (f *FakeExternalServer) URL(path string) string {
	return f.server.URL + path
}

func (f *FakeExternalServer) RequestCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

func (f *FakeExternalServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests[r.URL.Path]++
	responses := f.responses[r.URL.Path]
	if len(responses) == 0 {
		f.mu.Unlock()
		http.Error(w, "no scripted response", http.StatusInternalServerError)
		return
	}
	response := responses[0]
	if len(responses) > 1 {
		f.responses[r.URL.Path] = responses[1:]
	}
	f.mu.Unlock()

	if response.Delay > 0 {
		timer := time.NewTimer(response.Delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	for key, values := range response.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(response.Status)
	_, _ = w.Write([]byte(response.Body))
}
