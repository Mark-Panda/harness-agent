package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

// metadataCarrier 把 gRPC metadata 适配为 OpenTelemetry TextMapCarrier，仅用于传播上下文。
type metadataCarrier metadata.MD

// Get 读取 metadata 中指定键的第一个值。
func (c metadataCarrier) Get(key string) string {
	values := metadata.MD(c).Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// Set 写入 metadata 键值，供需要回写传播字段的场景使用。
func (c metadataCarrier) Set(key, value string) {
	metadata.MD(c).Set(key, value)
}

// Keys 列出全部 metadata 键。
func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}

// UnaryServerInterceptor 为 gRPC 一元请求建立服务端 Span，并从 metadata 提取 traceparent 与 request_id。
// 只记录方法级元数据与状态码，不记录请求/响应消息内容。
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if incoming, ok := metadata.FromIncomingContext(ctx); ok {
			ctx = otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(incoming))
		}
		service, method := splitFullMethod(info.FullMethod)
		attrs := []attribute.KeyValue{
			attribute.String(AttrRPCSystem, "grpc"),
			attribute.String(AttrRPCService, service),
			attribute.String(AttrRPCMethod, method),
		}
		if requestID := normalizeRequestID(metadataCarrierValue(ctx, "x-request-id")); requestID != "" {
			ctx = WithRequestID(ctx, requestID)
			attrs = append(attrs, attribute.String(AttrRequestID, requestID))
		}
		ctx, span := StartServer(ctx, "grpc.server.request", attrs...)
		defer span.End()

		response, err := handler(ctx, request)
		code := grpcstatus.Code(err)
		span.SetAttributes(attribute.Int(AttrGRPCStatusCode, int(code)))
		if err != nil {
			Fail(span, code.String())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		return response, err
	}
}

// metadataCarrierValue 读取入站 metadata 的单个值。
func metadataCarrierValue(ctx context.Context, key string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// splitFullMethod 拆分 /package.Service/Method 形式的全限定方法名。
func splitFullMethod(fullMethod string) (string, string) {
	trimmed := strings.TrimPrefix(fullMethod, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return trimmed, ""
	}
	return parts[0], parts[1]
}
