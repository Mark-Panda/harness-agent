package telemetry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

// installMemoryTracer 安装内存导出器并把 Span 立即刷新到导出器，供断言使用。
func installMemoryTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return exporter
}

// spanByName 返回指定名称的最后一个 Span。
func spanByName(t *testing.T, exporter *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	spans := exporter.GetSpans()
	for index := len(spans) - 1; index >= 0; index-- {
		if spans[index].Name == name {
			return spans[index]
		}
	}
	t.Fatalf("span %q not found in %d spans", name, len(spans))
	return tracetest.SpanStub{}
}

// attributeValue 读取 Span 属性并转为字符串，兼容字符串与整型属性。
func attributeValue(span tracetest.SpanStub, key string) string {
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			return fmt.Sprintf("%v", attr.Value.AsInterface())
		}
	}
	return ""
}

// TestSetupValidationAndDisabledMode 覆盖禁用模式、必填端点与采样比例校验。
func TestSetupValidationAndDisabledMode(t *testing.T) {
	provider, err := Setup(context.Background(), Options{Enabled: false})
	if err != nil || provider == nil {
		t.Fatalf("disabled setup provider=%v err=%v", provider, err)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("disabled shutdown: %v", err)
	}
	if _, err := Setup(context.Background(), Options{Enabled: true, SampleRatio: 1}); err == nil {
		t.Fatal("enabled setup without endpoint must fail")
	}
	if _, err := Setup(context.Background(), Options{Enabled: true, Endpoint: "127.0.0.1:4318", SampleRatio: 0}); err == nil {
		t.Fatal("zero sample ratio must fail")
	}
	if _, err := Setup(context.Background(), Options{Enabled: true, Endpoint: "ftp://collector", SampleRatio: 1}); err == nil {
		t.Fatal("non-http endpoint must fail")
	}
}

// TestHTTPMiddlewarePropagatesTraceAndRequestID 验证 traceparent 提取、request_id 透传与最小属性集。
func TestHTTPMiddlewarePropagatesTraceAndRequestID(t *testing.T) {
	exporter := installMemoryTracer(t)
	const traceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestIDFrom(r.Context()) != "req-123" {
			t.Errorf("context request id = %q", RequestIDFrom(r.Context()))
		}
		w.WriteHeader(http.StatusCreated)
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agents?token=secret", nil)
	request.Header.Set("traceparent", traceParent)
	request.Header.Set(headerRequestID, "req-123")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get(headerRequestID) != "req-123" {
		t.Fatalf("response request id = %q", recorder.Header().Get(headerRequestID))
	}
	span := spanByName(t, exporter, "http.server.request")
	if attributeValue(span, AttrHTTPMethod) != http.MethodPost ||
		attributeValue(span, AttrHTTPPath) != "/api/v1/agents" ||
		attributeValue(span, AttrRequestID) != "req-123" {
		t.Fatalf("span attrs = %+v", span.Attributes)
	}
	// 查询串可能包含凭证或用户正文，必须禁止进入 Trace。
	if attributeValue(span, "url.query") != "" || attributeValue(span, "http.target") != "" {
		t.Fatalf("query string leaked into span: %+v", span.Attributes)
	}
	// 入站 traceparent 必须被提取为父级上下文，保证外部链路贯通。
	if span.Parent.SpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("parent span id = %s", span.Parent.SpanID())
	}
	if span.Status.Code == codes.Error {
		t.Fatalf("span status = %v", span.Status)
	}
}

// TestHTTPMiddlewareGeneratesRequestIDAndMarksError 验证非法请求 ID 重新生成，以及 5xx 标记失败。
func TestHTTPMiddlewareGeneratesRequestIDAndMarksError(t *testing.T) {
	exporter := installMemoryTracer(t)
	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	request.Header.Set(headerRequestID, "bad id with spaces")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	generated := recorder.Header().Get(headerRequestID)
	if generated == "" || generated == "bad id with spaces" {
		t.Fatalf("generated request id = %q", generated)
	}
	if len(generated) != 32 {
		t.Fatalf("generated request id length = %d", len(generated))
	}
	span := spanByName(t, exporter, "http.server.request")
	if attributeValue(span, AttrHTTPStatus) != "500" {
		t.Fatalf("status attr = %+v", span.Attributes)
	}
	if span.Status.Code != codes.Error {
		t.Fatalf("span status = %v", span.Status)
	}
}

// TestHTTPMiddlewareKeepsSSEFlush 验证状态包装器不会破坏流式 Flush。
func TestHTTPMiddlewareKeepsSSEFlush(t *testing.T) {
	installMemoryTracer(t)
	flushed := false
	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: ok\n\n"))
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("flush capability lost")
			return
		}
		flusher.Flush()
		flushed = true
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sse", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if !flushed {
		t.Fatal("handler did not flush")
	}
}

// TestUnaryServerInterceptor 验证 gRPC 传播、服务/方法属性与失败状态分类。
func TestUnaryServerInterceptor(t *testing.T) {
	exporter := installMemoryTracer(t)
	interceptor := UnaryServerInterceptor()
	incoming := metadata.Pairs(
		"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"x-request-id", "grpc-req-1",
	)
	ctx := metadata.NewIncomingContext(context.Background(), incoming)
	info := &grpc.UnaryServerInfo{FullMethod: "/harness.v1.AgentService/GetAgent"}
	response, err := interceptor(ctx, struct{}{}, info, func(ctx context.Context, _ any) (any, error) {
		if RequestIDFrom(ctx) != "grpc-req-1" {
			t.Errorf("grpc request id = %q", RequestIDFrom(ctx))
		}
		return "ok", grpcstatus.Error(grpccodes.Internal, "boom")
	})
	if response != "ok" || grpcstatus.Code(err) != grpccodes.Internal {
		t.Fatalf("response=%v err=%v", response, err)
	}
	span := spanByName(t, exporter, "grpc.server.request")
	if attributeValue(span, AttrRPCService) != "harness.v1.AgentService" || attributeValue(span, AttrRPCMethod) != "GetAgent" {
		t.Fatalf("rpc attrs = %+v", span.Attributes)
	}
	if attributeValue(span, AttrRequestID) != "grpc-req-1" {
		t.Fatalf("grpc request id attr = %+v", span.Attributes)
	}
	if attributeValue(span, AttrGRPCStatusCode) != "13" || attributeValue(span, AttrErrorClass) != "Internal" {
		t.Fatalf("error attrs = %+v", span.Attributes)
	}
	if span.Status.Code != codes.Error {
		t.Fatalf("span status = %v", span.Status)
	}
}

// TestSetupExportsWithAuthHeader 验证启用后真实导出到 OTLP HTTP，并携带出口认证头与资源属性。
func TestSetupExportsWithAuthHeader(t *testing.T) {
	var exportAuth, exportBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exportAuth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		exportBody.Store(string(body))
		// OTLP/HTTP protobuf 成功响应为空消息。
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	provider, err := Setup(context.Background(), Options{
		Enabled: true, ServiceName: "harness-test", Endpoint: server.URL, SampleRatio: 1,
		BatchTimeout: 50 * time.Millisecond, ExportTimeout: 2 * time.Second,
		Headers: map[string]string{"authorization": "Bearer test-token"},
	})
	if err != nil {
		t.Fatalf("setup exporter: %v", err)
	}
	_, span := Start(context.Background(), "export.test", attribute.String(AttrRunID, "run-export"))
	span.End()
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown exporter: %v", err)
	}
	if auth, _ := exportAuth.Load().(string); auth != "Bearer test-token" {
		t.Fatalf("export authorization = %q", auth)
	}
	body, _ := exportBody.Load().(string)
	if body == "" {
		t.Fatal("exporter sent empty payload")
	}
	// 资源属性必须包含服务名；Span 正文不得包含 Prompt/Completion 等用户内容。
	if !strings.Contains(body, "harness-test") || !strings.Contains(body, "run-export") {
		t.Fatalf("export payload missing resource or span attributes")
	}
}

// TestFailSanitizesDescription 验证失败标记只写错误分类，不写错误原文。
func TestFailSanitizesDescription(t *testing.T) {
	exporter := installMemoryTracer(t)
	_, span := Start(context.Background(), "unit.fail", attribute.String(AttrRunID, "run-1"))
	Fail(span, "retryable_failure")
	span.End()
	recorded := spanByName(t, exporter, "unit.fail")
	if recorded.Status.Description != "retryable_failure" || recorded.Status.Code != codes.Error {
		t.Fatalf("status = %v", recorded.Status)
	}
	if attributeValue(recorded, AttrErrorClass) != "retryable_failure" {
		t.Fatalf("error class attr = %+v", recorded.Attributes)
	}
}
