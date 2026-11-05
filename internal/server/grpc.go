package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	harnessv1 "github.com/Mark-Panda/harness-agent/api/harness/v1"
	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// GRPCServer 是 gRPC 传输层包装，实现 Kratos transport.Server 的 Start/Stop 契约。
type GRPCServer struct {
	addr     string
	server   *grpc.Server
	listener net.Listener
}

// NewGRPCServer 注册全部平台 gRPC 服务，并挂载 Bearer 鉴权一元拦截器。
func NewGRPCServer(addr string, auth AuthApplication, platform *service.PlatformService) (*GRPCServer, error) {
	if strings.TrimSpace(addr) == "" || auth == nil || platform == nil {
		return nil, errors.New("gRPC address, auth and platform services are required")
	}
	platformServer := &platformGRPCServer{platform: platform}
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnaryInterceptor(auth)))
	harnessv1.RegisterProviderServiceServer(server, platformServer)
	harnessv1.RegisterModelCatalogServiceServer(server, platformServer)
	harnessv1.RegisterModelPolicyServiceServer(server, platformServer)
	harnessv1.RegisterAgentServiceServer(server, platformServer)
	harnessv1.RegisterConversationServiceServer(server, platformServer)
	return &GRPCServer{addr: addr, server: server}, nil
}

// Start 监听配置地址并阻塞提供 gRPC 服务。
func (s *GRPCServer) Start(context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen gRPC %s: %w", s.addr, err)
	}
	s.listener = listener
	if err := s.server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// Stop 优雅停止 gRPC 服务并关闭监听。
func (s *GRPCServer) Stop(context.Context) error {
	s.server.GracefulStop()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	return nil
}

// authUnaryInterceptor 从 metadata 读取 Bearer Token 并注入 Principal，缺失或无效时返回 Unauthenticated。
func authUnaryInterceptor(auth AuthApplication) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "unauthenticated")
		}
		values := md.Get("authorization")
		if len(values) == 0 {
			return nil, status.Error(codes.Unauthenticated, "unauthenticated")
		}
		parts := strings.SplitN(values[0], " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			return nil, status.Error(codes.Unauthenticated, "unauthenticated")
		}
		principal, err := auth.Authenticate(ctx, strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "unauthenticated")
		}
		return handler(biz.WithPrincipal(ctx, principal), request)
	}
}

// grpcPrincipal 读取拦截器注入的调用者身份，缺失时按未认证处理。
func grpcPrincipal(ctx context.Context) (biz.Principal, error) {
	principal, ok := biz.PrincipalFromContext(ctx)
	if !ok {
		return biz.Principal{}, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	return principal, nil
}

// grpcError 把领域错误映射为稳定 gRPC 状态码。
func grpcError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, biz.ErrForbidden):
		return status.Error(codes.PermissionDenied, "forbidden")
	case errors.Is(err, biz.ErrQuotaExceeded):
		return status.Error(codes.ResourceExhausted, "quota_exceeded")
	case errors.Is(err, biz.ErrResourceNotFound):
		return status.Error(codes.NotFound, "resource_not_found")
	case errors.Is(err, biz.ErrResourceConflict):
		return status.Error(codes.Aborted, "resource_conflict")
	case errors.Is(err, biz.ErrInvalidConfig):
		return status.Error(codes.InvalidArgument, "invalid_configuration")
	default:
		return status.Error(codes.Internal, "internal_error")
	}
}
