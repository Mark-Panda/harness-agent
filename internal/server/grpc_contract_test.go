package server

import (
	"context"
	"net"
	"testing"
	"time"

	harnessv1 "github.com/Mark-Panda/harness-agent/api/harness/v1"
	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/Mark-Panda/harness-agent/internal/service"
	"github.com/Mark-Panda/harness-agent/internal/service/mocks"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// grpcContractAuth 是可控的鉴权桩：仅接受 valid-token。
type grpcContractAuth struct {
	principal biz.Principal
}

func (a *grpcContractAuth) Register(context.Context, string, string) (service.AuthResult, error) {
	return service.AuthResult{}, nil
}
func (a *grpcContractAuth) Login(context.Context, string, string) (service.AuthResult, error) {
	return service.AuthResult{}, nil
}
func (a *grpcContractAuth) Refresh(context.Context, string) (service.AuthResult, error) {
	return service.AuthResult{}, nil
}
func (a *grpcContractAuth) Authenticate(_ context.Context, token string) (biz.Principal, error) {
	if token != "valid-token" {
		return biz.Principal{}, service.ErrInvalidCredentials
	}
	return a.principal, nil
}
func (a *grpcContractAuth) Logout(context.Context, string) error { return nil }
func (a *grpcContractAuth) LogoutAll(context.Context, uuid.UUID) error {
	return nil
}
func (a *grpcContractAuth) ChangePassword(context.Context, uuid.UUID, string, string) error {
	return nil
}
func (a *grpcContractAuth) SetUserStatus(context.Context, biz.Principal, uuid.UUID, biz.UserStatus, string) error {
	return nil
}
func (a *grpcContractAuth) IssuePasswordReset(context.Context, biz.Principal, uuid.UUID) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (a *grpcContractAuth) ResetPassword(context.Context, string, string) error { return nil }
func (a *grpcContractAuth) CreateUser(context.Context, biz.Principal, string, string) (service.PublicUser, error) {
	return service.PublicUser{}, nil
}
func (a *grpcContractAuth) ListUsers(context.Context, biz.Principal, biz.UserFilter, int, string) (biz.Page[service.AdminUser], error) {
	return biz.Page[service.AdminUser]{}, nil
}
func (a *grpcContractAuth) SetUserRole(context.Context, biz.Principal, uuid.UUID, biz.Role, string) error {
	return nil
}
func (a *grpcContractAuth) FeishuLoginEnabled() bool { return false }
func (a *grpcContractAuth) FeishuAuthorizeURL(string) (string, bool) {
	return "", false
}
func (a *grpcContractAuth) LoginWithFeishu(context.Context, string) (service.AuthResult, error) {
	return service.AuthResult{}, nil
}

// TestGRPCAuthenticationAndSessionContract 验证 gRPC 鉴权拦截器与请求身份注入。
func TestGRPCAuthenticationAndSessionContract(t *testing.T) {
	ctrl := gomock.NewController(t)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	sessions, _ := biz.NewSessionService(sessionRepo, biz.QuotaLimits{})
	platform, _ := service.NewPlatformService(
		&biz.ProviderService{}, &biz.ModelService{}, &biz.PolicyService{},
		&biz.SkillService{}, &biz.MCPService{}, &biz.AgentService{}, sessions, &biz.RunService{}, &biz.IdempotencyService{},
	)
	platformServer := &platformGRPCServer{platform: platform}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(authUnaryInterceptor(&grpcContractAuth{principal: principal})))
	harnessv1.RegisterConversationServiceServer(server, platformServer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := harnessv1.NewConversationServiceClient(connection)

	// 未携带 Token 时必须返回 Unauthenticated，且不会触达业务层。
	_, err = client.CreateSession(context.Background(), &harnessv1.CreateSessionRequest{Title: "denied"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated call error = %v", err)
	}
	invalidContext := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer invalid")
	_, err = client.CreateSession(invalidContext, &harnessv1.CreateSessionRequest{Title: "denied"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalid token error = %v", err)
	}

	sessionRepo.EXPECT().CountSessions(gomock.Any(), principal.UserID).Return(int64(0), nil)
	sessionRepo.EXPECT().CreateSession(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, session biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			if session.UserID != principal.UserID || session.Status != biz.SessionActive {
				t.Fatalf("gRPC session used wrong identity: %#v", session)
			}
			return session, nil
		},
	)
	authContext := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer valid-token")
	response, err := client.CreateSession(authContext, &harnessv1.CreateSessionRequest{Title: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetSession().GetId() == "" || response.GetSession().GetStatus() != string(biz.SessionActive) {
		t.Fatalf("unexpected gRPC session response: %#v", response.GetSession())
	}

	// 非法分页参数必须在传输层直接拒绝。
	_, err = client.ListSessions(authContext, &harnessv1.ListSessionsRequest{Limit: 101})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid limit error = %v", err)
	}
}

// TestGRPCUpdateSessionMemoryPolicyContract 验证记忆策略 RPC 的身份注入与出参。
func TestGRPCUpdateSessionMemoryPolicyContract(t *testing.T) {
	ctrl := gomock.NewController(t)
	sessionRepo := mocks.NewMockSessionRepository(ctrl)
	principal := biz.Principal{UserID: uuid.New(), Role: biz.RoleUser}
	sessionID := uuid.New()
	sessions, _ := biz.NewSessionService(sessionRepo, biz.QuotaLimits{})
	platform, _ := service.NewPlatformService(
		&biz.ProviderService{}, &biz.ModelService{}, &biz.PolicyService{},
		&biz.SkillService{}, &biz.MCPService{}, &biz.AgentService{}, sessions, &biz.RunService{}, &biz.IdempotencyService{},
	)
	platformServer := &platformGRPCServer{platform: platform}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(authUnaryInterceptor(&grpcContractAuth{principal: principal})))
	harnessv1.RegisterConversationServiceServer(server, platformServer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := harnessv1.NewConversationServiceClient(connection)

	// 只允许当前用户的会话变更策略：仓储必须收到 Token 解析出的用户 ID。
	sessionRepo.EXPECT().GetSession(gomock.Any(), principal.UserID, sessionID).Return(biz.Session{
		ID: sessionID, UserID: principal.UserID, Status: biz.SessionActive,
		MemoryPolicy: biz.SessionMemoryPrivate, Version: 3,
	}, nil)
	sessionRepo.EXPECT().UpdateSessionMemoryPolicy(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, session biz.Session, _ biz.OutboxEvent) (biz.Session, error) {
			if session.UserID != principal.UserID || session.MemoryPolicy != biz.SessionMemoryDisabled || session.Version != 4 {
				t.Fatalf("gRPC memory policy used wrong mutation: %#v", session)
			}
			return session, nil
		},
	)
	authContext := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer valid-token")
	response, err := client.UpdateSessionMemoryPolicy(authContext, &harnessv1.UpdateSessionMemoryPolicyRequest{
		Id: sessionID.String(), MemoryPolicy: string(biz.SessionMemoryDisabled),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetSession().GetMemoryPolicy() != string(biz.SessionMemoryDisabled) || response.GetSession().GetVersion() != 4 {
		t.Fatalf("unexpected memory policy response: %#v", response.GetSession())
	}

	// 非法策略在领域层校验阶段直接被拒绝，不会触达仓储。
	_, err = client.UpdateSessionMemoryPolicy(authContext, &harnessv1.UpdateSessionMemoryPolicyRequest{
		Id: sessionID.String(), MemoryPolicy: "shared",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid memory policy error = %v", err)
	}
}
