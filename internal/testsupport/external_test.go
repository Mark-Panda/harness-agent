package testsupport

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func TestFakeExternalServerInjectsFailuresAndRecovery(t *testing.T) {
	fake := NewFakeExternalServer(t)
	const path = "/v1/chat/completions"
	fake.Enqueue(path, ExternalResponse{Status: http.StatusServiceUnavailable, Body: `{"error":"unavailable"}`})
	fake.Enqueue(path, ExternalResponse{Status: http.StatusOK, Body: `{"id":"chat-1"}`, Delay: time.Millisecond})

	for index, expected := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		response, err := http.Get(fake.URL(path))
		if err != nil {
			t.Fatalf("request %d: %v", index, err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatalf("read response %d: %v", index, err)
		}
		if response.StatusCode != expected || len(body) == 0 {
			t.Fatalf("request %d returned %d %s", index, response.StatusCode, body)
		}
	}
	if got := fake.RequestCount(path); got != 2 {
		t.Fatalf("RequestCount() = %d, want 2", got)
	}
}
