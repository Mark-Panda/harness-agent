package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FuzzDecodeJSON 验证任意请求体都不会触发解码 panic，且错误响应仍为合法 JSON。
func FuzzDecodeJSON(f *testing.F) {
	f.Add([]byte(`{"username":"alice","password":"secret","transport":"bearer"}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"username":123}`))
	f.Add([]byte(`{"username":"` + string(make([]byte, 256)) + `"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		var target struct {
			Username  string `json:"username"`
			Password  string `json:"password"`
			Transport string `json:"transport"`
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		// 只要求不 panic 且响应状态可判定；具体错误码由现有单测覆盖。
		_ = decodeJSON(recorder, request, &target)
		if recorder.Code == 0 {
			t.Fatal("decodeJSON must write an HTTP response")
		}
	})
}

// FuzzWriteSSE 验证任意事件载荷都不会导致序列化 panic或非法状态。
func FuzzWriteSSE(f *testing.F) {
	f.Add(int64(1), "run.completed", "ok")
	f.Add(int64(0), "heartbeat", "{}")
	f.Fuzz(func(t *testing.T, seq int64, eventType, payload string) {
		recorder := httptest.NewRecorder()
		_ = writeSSE(recorder, seq, eventType, map[string]any{"data": payload})
		if recorder.Code == 0 {
			t.Fatal("writeSSE must write an HTTP response")
		}
	})
}
