package biz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// feishuBindingSessionStub 按用户校验会话归属，用于模拟所有者与非所有者两种结果。
type feishuBindingSessionStub struct {
	biz.SessionRepository
	// session 会话归属校验通过时返回的会话。
	session biz.Session
	// err 会话归属校验失败错误。
	err error
	// calls 查询次数。
	calls int
	// getUserID 最近一次查询使用的用户 ID。
	getUserID uuid.UUID
}

// GetSession 记录查询身份并返回预设会话或错误。
func (s *feishuBindingSessionStub) GetSession(_ context.Context, userID, _ uuid.UUID) (biz.Session, error) {
	s.calls++
	s.getUserID = userID
	if s.err != nil {
		return biz.Session{}, s.err
	}
	return s.session, nil
}

// feishuBindingConversationStub 记录绑定生命周期入参并返回预设受影响绑定数。
type feishuBindingConversationStub struct {
	biz.FeishuConversationRepository
	// released 受影响绑定数。
	released int
	// restored 恢复影响的绑定数。
	restored int
	// err 生命周期操作失败错误。
	err error
	// calls 状态发布调用次数。
	calls int
	// userID 状态发布使用的用户 ID。
	userID uuid.UUID
	// sessionID 状态发布使用的会话 ID。
	sessionID uuid.UUID
	// status 目标绑定状态。
	status string
	// event 状态发布审计事件。
	event biz.OutboxEvent
	// restoreCalls 恢复调用次数。
	restoreCalls int
	// restoreUserID 恢复使用的用户 ID。
	restoreUserID uuid.UUID
	// restoreSessionID 恢复使用的会话 ID。
	restoreSessionID uuid.UUID
	// restoreEvent 恢复审计事件。
	restoreEvent biz.OutboxEvent
}

// ReleaseConversation 记录解绑参数并按预设返回结果。
func (s *feishuBindingConversationStub) ReleaseConversation(_ context.Context, userID, sessionID uuid.UUID, status string, event biz.OutboxEvent) (int, error) {
	s.calls++
	s.userID = userID
	s.sessionID = sessionID
	s.status = status
	s.event = event
	if s.err != nil {
		return 0, s.err
	}
	return s.released, nil
}

// RestoreConversation 记录恢复参数并按预设返回结果。
func (s *feishuBindingConversationStub) RestoreConversation(_ context.Context, userID, sessionID uuid.UUID, event biz.OutboxEvent) (int, error) {
	s.restoreCalls++
	s.restoreUserID = userID
	s.restoreSessionID = sessionID
	s.restoreEvent = event
	if s.err != nil {
		return 0, s.err
	}
	return s.restored, nil
}

// PromoteConversationThread 在绑定生命周期测试中不做实际升级，返回 0。
func (s *feishuBindingConversationStub) PromoteConversationThread(_ context.Context, _, _ uuid.UUID, _, _ string) (int, error) {
	return 0, nil
}

// TestNewFeishuBindingServiceRequiresDependencies 验证依赖缺失时拒绝构造服务。
func TestNewFeishuBindingServiceRequiresDependencies(t *testing.T) {
	if _, err := biz.NewFeishuBindingService(nil, &feishuBindingSessionStub{}); err == nil {
		t.Fatal("missing conversation repository was accepted")
	}
	if _, err := biz.NewFeishuBindingService(&feishuBindingConversationStub{}, nil); err == nil {
		t.Fatal("missing session repository was accepted")
	}
}

// TestFeishuBindingServiceUnbindOwnership 验证解绑仅限会话所有者，并按 user_id 与 session_id 限定。
func TestFeishuBindingServiceUnbindOwnership(t *testing.T) {
	ownerID, sessionID := uuid.New(), uuid.New()
	conversations := &feishuBindingConversationStub{released: 2}
	sessions := &feishuBindingSessionStub{session: biz.Session{ID: sessionID, UserID: ownerID, Status: biz.SessionActive}}
	service, err := biz.NewFeishuBindingService(conversations, sessions)
	if err != nil {
		t.Fatal(err)
	}
	owner := biz.Principal{UserID: ownerID, Role: biz.RoleUser}
	released, err := service.Unbind(context.Background(), owner, sessionID)
	if err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if released != 2 {
		t.Fatalf("released = %d", released)
	}
	if sessions.getUserID != ownerID || conversations.userID != ownerID || conversations.sessionID != sessionID {
		t.Fatalf("scope user=%s session=%s conversation user=%s session=%s", sessions.getUserID, sessionID, conversations.userID, conversations.sessionID)
	}
	if conversations.status != biz.FeishuBindingUnbound {
		t.Fatalf("release status = %q", conversations.status)
	}
	// 审计事件必须归属操作者并可追溯到会话。
	if conversations.event.UserID != ownerID || conversations.event.EventType != "feishu.binding_unbound" ||
		conversations.event.AggregateType != "session" || conversations.event.AggregateID != sessionID.String() {
		t.Fatalf("release event = %+v", conversations.event)
	}
	// 重复解绑幂等：仓储返回 0 时仍视为成功。
	conversations.released = 0
	if released, err := service.Unbind(context.Background(), owner, sessionID); err != nil || released != 0 {
		t.Fatalf("idempotent unbind released=%d err=%v", released, err)
	}
}

// TestFeishuBindingServiceRejectsNonOwner 验证非所有者（含管理员）与匿名调用均被拒绝。
func TestFeishuBindingServiceRejectsNonOwner(t *testing.T) {
	ownerID, otherID, sessionID := uuid.New(), uuid.New(), uuid.New()
	conversations := &feishuBindingConversationStub{released: 1}
	sessions := &feishuBindingSessionStub{err: biz.ErrResourceNotFound}
	service, err := biz.NewFeishuBindingService(conversations, sessions)
	if err != nil {
		t.Fatal(err)
	}
	// 非所有者：仓储按 user_id 未命中，必须返回 NotFound 且不得触达解绑。
	for _, principal := range []biz.Principal{
		{UserID: otherID, Role: biz.RoleUser},
		{UserID: otherID, Role: biz.RoleAdmin},
	} {
		if _, err := service.Unbind(context.Background(), principal, sessionID); !errors.Is(err, biz.ErrResourceNotFound) {
			t.Fatalf("role %s unbind err = %v", principal.Role, err)
		}
		if sessions.getUserID != otherID {
			t.Fatalf("role %s queried user = %s", principal.Role, sessions.getUserID)
		}
	}
	if conversations.calls != 0 {
		t.Fatalf("non-owner reached release %d times", conversations.calls)
	}
	// 匿名调用直接拒绝，不触达任何仓储。
	sessions.calls = 0
	if _, err := service.Unbind(context.Background(), biz.Principal{}, sessionID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous unbind err = %v", err)
	}
	if sessions.calls != 0 || conversations.calls != 0 {
		t.Fatalf("anonymous reached storage sessions=%d conversations=%d", sessions.calls, conversations.calls)
	}
	// 空会话 ID 属于非法入参，同样不得触达仓储。
	if _, err := service.Unbind(context.Background(), biz.Principal{UserID: ownerID, Role: biz.RoleUser}, uuid.Nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil session err = %v", err)
	}
	if sessions.calls != 0 {
		t.Fatalf("nil session reached storage %d times", sessions.calls)
	}
}

// TestFeishuBindingServiceReleaseFailure 验证解绑失败向上返回错误。
func TestFeishuBindingServiceReleaseFailure(t *testing.T) {
	ownerID, sessionID := uuid.New(), uuid.New()
	releaseErr := errors.New("database unavailable")
	conversations := &feishuBindingConversationStub{err: releaseErr}
	sessions := &feishuBindingSessionStub{session: biz.Session{ID: sessionID, UserID: ownerID, Status: biz.SessionActive}}
	service, err := biz.NewFeishuBindingService(conversations, sessions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Unbind(context.Background(), biz.Principal{UserID: ownerID, Role: biz.RoleUser}, sessionID); !errors.Is(err, releaseErr) {
		t.Fatalf("release failure err = %v", err)
	}
}

// TestFeishuBindingServiceCloseAndArchive 验证关闭与归档写入目标状态并产生对应审计事件。
func TestFeishuBindingServiceCloseAndArchive(t *testing.T) {
	ownerID, sessionID := uuid.New(), uuid.New()
	owner := biz.Principal{UserID: ownerID, Role: biz.RoleUser}
	for _, testCase := range []struct {
		name      string
		status    string
		eventType string
		change    func(*biz.FeishuBindingService) (int, error)
	}{
		{
			name: "close", status: biz.FeishuBindingClosed, eventType: "feishu.binding_closed",
			change: func(service *biz.FeishuBindingService) (int, error) {
				return service.Close(context.Background(), owner, sessionID)
			},
		},
		{
			name: "archive", status: biz.FeishuBindingArchived, eventType: "feishu.binding_archived",
			change: func(service *biz.FeishuBindingService) (int, error) {
				return service.Archive(context.Background(), owner, sessionID)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			conversations := &feishuBindingConversationStub{released: 1}
			sessions := &feishuBindingSessionStub{session: biz.Session{ID: sessionID, UserID: ownerID, Status: biz.SessionActive}}
			service, err := biz.NewFeishuBindingService(conversations, sessions)
			if err != nil {
				t.Fatal(err)
			}
			affected, err := testCase.change(service)
			if err != nil || affected != 1 {
				t.Fatalf("affected=%d err=%v", affected, err)
			}
			// 状态与审计事件必须成对出现，便于外部追溯绑定生命周期。
			if conversations.status != testCase.status || conversations.event.EventType != testCase.eventType {
				t.Fatalf("status=%q event=%q", conversations.status, conversations.event.EventType)
			}
			if conversations.event.UserID != ownerID || conversations.event.AggregateID != sessionID.String() {
				t.Fatalf("audit event = %+v", conversations.event)
			}
		})
	}
}

// TestFeishuBindingServiceRestore 验证恢复仅限会话所有者，并产生恢复审计事件。
func TestFeishuBindingServiceRestore(t *testing.T) {
	ownerID, otherID, sessionID := uuid.New(), uuid.New(), uuid.New()
	conversations := &feishuBindingConversationStub{restored: 1}
	sessions := &feishuBindingSessionStub{session: biz.Session{ID: sessionID, UserID: ownerID, Status: biz.SessionActive}}
	service, err := biz.NewFeishuBindingService(conversations, sessions)
	if err != nil {
		t.Fatal(err)
	}
	owner := biz.Principal{UserID: ownerID, Role: biz.RoleUser}
	restored, err := service.Restore(context.Background(), owner, sessionID)
	if err != nil || restored != 1 {
		t.Fatalf("restore affected=%d err=%v", restored, err)
	}
	if conversations.restoreUserID != ownerID || conversations.restoreSessionID != sessionID {
		t.Fatalf("restore scope user=%s session=%s", conversations.restoreUserID, conversations.restoreSessionID)
	}
	if conversations.restoreEvent.EventType != "feishu.binding_restored" ||
		conversations.restoreEvent.UserID != ownerID || conversations.restoreEvent.AggregateID != sessionID.String() {
		t.Fatalf("restore event = %+v", conversations.restoreEvent)
	}
	// 重复恢复幂等：仓储返回 0 时仍视为成功。
	conversations.restored = 0
	if restored, err := service.Restore(context.Background(), owner, sessionID); err != nil || restored != 0 {
		t.Fatalf("idempotent restore affected=%d err=%v", restored, err)
	}
	// 非所有者即使猜到会话 ID 也会得到 NotFound，且不得触达恢复。
	conversations.restoreCalls = 0
	sessions.err = biz.ErrResourceNotFound
	if _, err := service.Restore(context.Background(), biz.Principal{UserID: otherID, Role: biz.RoleAdmin}, sessionID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("non-owner restore err = %v", err)
	}
	if conversations.restoreCalls != 0 {
		t.Fatalf("non-owner reached restore %d times", conversations.restoreCalls)
	}
	// 匿名调用与空会话 ID 均直接拒绝。
	sessions.err = nil
	if _, err := service.Restore(context.Background(), biz.Principal{}, sessionID); !errors.Is(err, biz.ErrForbidden) {
		t.Fatalf("anonymous restore err = %v", err)
	}
	if _, err := service.Restore(context.Background(), owner, uuid.Nil); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("nil session restore err = %v", err)
	}
}
