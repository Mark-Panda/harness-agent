//go:build integration

package data

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// approvalIntegrationRun 创建审批测试所需的会话与 Run，返回 Run ID。
func approvalIntegrationRun(t *testing.T, ctx context.Context, sessions *SessionRepository, runs *RunRepository, userID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	sessionID, runID := uuid.New(), uuid.New()
	if _, err := sessions.CreateSession(ctx, biz.Session{
		ID: sessionID, UserID: userID, Title: "approval", Source: biz.SessionSourceWeb,
		Status: biz.SessionActive, Version: 1,
	}, testOutboxEvent(userID, "session.created")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Minute).UTC()
	if _, err := runs.CreateRun(ctx, biz.Run{
		ID: runID, SessionID: sessionID, UserID: userID, Mode: biz.AgentModeReact, Status: biz.RunQueued,
		ConfigSnapshot:      map[string]any{"agent": map[string]any{"name": "审批测试"}},
		ModelPolicySnapshot: map[string]any{}, DeadlineAt: &deadline,
	}); err != nil {
		t.Fatal(err)
	}
	return sessionID, runID
}

// TestToolApprovalRepositoryLifecycle 验证审批幂等创建、决策、执行与投递的真实数据库语义。
func TestToolApprovalRepositoryLifecycle(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	userA, err := users.CreateUser(ctx, biz.User{
		Username: "approval_a_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	userB, err := users.CreateUser(ctx, biz.User{
		Username: "approval_b_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id IN (?, ?)", userA.ID, userB.ID).Error }()

	sessions := NewSessionRepository(appDB)
	runs := NewRunRepository(appDB)
	approvals := NewToolApprovalRepository(appDB)
	_, runID := approvalIntegrationRun(t, ctx, sessions, runs, userA.ID)

	created, err := approvals.CreateApproval(ctx, biz.ToolApproval{
		RunID: runID, UserID: userA.ID, ToolCallID: "call-1", ToolName: "mcp_admin_delete",
		Arguments: json.RawMessage(`{"id":"x"}`), RiskLevel: "high", Status: biz.ApprovalPending,
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 同一 Run 与 tool_call_id 重复创建必须返回既有记录，不产生第二条审批。
	again, err := approvals.CreateApproval(ctx, biz.ToolApproval{
		RunID: runID, UserID: userA.ID, ToolCallID: "call-1", ToolName: "mcp_admin_delete",
		Arguments: json.RawMessage(`{}`), RiskLevel: "high", IdempotencyKey: "idem-other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != created.ID || again.IdempotencyKey != "idem-1" {
		t.Fatalf("idempotent create = %#v, want %s", again, created.ID)
	}
	list, err := approvals.ListApprovals(ctx, userA.ID, runID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListApprovals() = %#v, %v", list, err)
	}
	if pending, err := approvals.CountPendingApprovals(ctx, userA.ID, runID); err != nil || pending != 1 {
		t.Fatalf("pending count = %d, %v", pending, err)
	}
	// 其他用户受 RLS 约束，无法读取审批。
	if _, err := approvals.GetApproval(ctx, userB.ID, created.ID); !errors.Is(err, biz.ErrResourceNotFound) {
		t.Fatalf("cross-user approval read error = %v", err)
	}

	approved, err := approvals.DecideApproval(ctx, userA.ID, created.ID, biz.ApprovalDecisionApprove, userA.ID, time.Now().UTC())
	if err != nil || approved.Status != biz.ApprovalApproved || approved.DecidedAt == nil {
		t.Fatalf("DecideApproval() = %#v, %v", approved, err)
	}
	// 同一决策重复提交幂等返回。
	if repeated, err := approvals.DecideApproval(ctx, userA.ID, created.ID, biz.ApprovalDecisionApprove, userA.ID, time.Now().UTC()); err != nil || repeated.Status != biz.ApprovalApproved {
		t.Fatalf("repeated decision = %#v, %v", repeated, err)
	}
	// 已批准的审批不能被反向拒绝。
	if _, err := approvals.DecideApproval(ctx, userA.ID, created.ID, biz.ApprovalDecisionDeny, userA.ID, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("reverse decision error = %v", err)
	}
	executing, err := approvals.ClaimApprovalExecution(ctx, userA.ID, created.ID, time.Now().UTC())
	if err != nil || executing.Status != biz.ApprovalExecuting {
		t.Fatalf("ClaimApprovalExecution() = %#v, %v", executing, err)
	}
	// 已进入执行态后再次认领必须冲突，避免重复副作用。
	if _, err := approvals.ClaimApprovalExecution(ctx, userA.ID, created.ID, time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("second claim error = %v", err)
	}
	executed, err := approvals.FinishApprovalExecution(ctx, userA.ID, created.ID, biz.ApprovalExecuted,
		json.RawMessage(`{"ok":true}`), "", "", time.Now().UTC())
	if err != nil || executed.Status != biz.ApprovalExecuted || len(executed.ResultJSON) == 0 {
		t.Fatalf("FinishApprovalExecution() = %#v, %v", executed, err)
	}
	if _, err := approvals.FinishApprovalExecution(ctx, userA.ID, created.ID, biz.ApprovalFailed,
		nil, "x", "y", time.Now().UTC()); !errors.Is(err, biz.ErrResourceConflict) {
		t.Fatalf("reverse finish error = %v", err)
	}
	delivered, err := approvals.MarkApprovalDelivered(ctx, userA.ID, created.ID, time.Now().UTC())
	if err != nil || delivered.DeliveredAt == nil {
		t.Fatalf("MarkApprovalDelivered() = %#v, %v", delivered, err)
	}
	if repeated, err := approvals.MarkApprovalDelivered(ctx, userA.ID, created.ID, time.Now().UTC()); err != nil || repeated.DeliveredAt == nil {
		t.Fatalf("repeated delivery = %#v, %v", repeated, err)
	}
	var rowCount int64
	if err := adminDB.Raw("SELECT COUNT(*) FROM tool_approvals WHERE run_id = ?", runID).Scan(&rowCount).Error; err != nil || rowCount != 1 {
		t.Fatalf("approval rows = %d, %v", rowCount, err)
	}
}

// TestRunApprovalStateTransitions 验证 Run 的真实暂停、恢复与人工对账状态及事件。
func TestRunApprovalStateTransitions(t *testing.T) {
	if os.Getenv("REQUIRE_DATABASE") != "true" {
		t.Skip("requires PostgreSQL")
	}
	adminDB := openIntegrationDatabase(t, integrationDSN("TEST_ADMIN_DATABASE_URL", "postgres://harness:harness@localhost:5432/harness?sslmode=disable"))
	appDB := openIntegrationDatabase(t, integrationDSN("TEST_APP_DATABASE_URL", "postgres://harness_app:harness@localhost:5432/harness?sslmode=disable"))
	authDB := openIntegrationDatabase(t, integrationDSN("TEST_AUTH_DATABASE_URL", "postgres://harness_auth:harness@localhost:5432/harness?sslmode=disable"))
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	users := NewUserRepository(appDB, authDB)
	user, err := users.CreateUser(ctx, biz.User{
		Username: "approval_run_" + suffix, PasswordHash: "hash", Role: biz.RoleUser,
		Status: biz.UserStatusActive, CredentialVersion: 1,
	}, testOutboxEvent(uuid.Nil, "test.register"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminDB.Exec("DELETE FROM users WHERE id = ?", user.ID).Error }()
	sessions := NewSessionRepository(appDB)
	runs := NewRunRepository(appDB)
	_, runID := approvalIntegrationRun(t, ctx, sessions, runs, user.ID)

	claimed, fence, err := runs.ClaimRun(ctx, user.ID, runID, "approval-worker", time.Minute, time.Now().UTC())
	if err != nil || claimed.Status != biz.RunRunning {
		t.Fatalf("ClaimRun() = %#v, %#v, %v", claimed, fence, err)
	}
	paused, err := runs.PauseRun(ctx, user.ID, runID, fence, time.Now().UTC())
	if err != nil || paused.Status != biz.RunWaitingApproval {
		t.Fatalf("PauseRun() = %#v, %v", paused, err)
	}
	// 重复暂停幂等成功且不重复写事件。
	if _, err := runs.PauseRun(ctx, user.ID, runID, fence, time.Now().UTC()); err != nil {
		t.Fatalf("repeated PauseRun() error = %v", err)
	}
	resumed, err := runs.ResumeRun(ctx, user.ID, runID, time.Now().UTC())
	if err != nil || resumed.Status != biz.RunRunning {
		t.Fatalf("ResumeRun() = %#v, %v", resumed, err)
	}
	if resumed.DeadlineAt == nil || !resumed.DeadlineAt.After(*claimed.DeadlineAt) {
		t.Fatalf("deadline was not extended: %v -> %v", claimed.DeadlineAt, resumed.DeadlineAt)
	}
	// 恢复后租约已清空，可再次认领并进入人工对账。
	_, secondFence, err := runs.ClaimRun(ctx, user.ID, runID, "approval-worker", time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatalf("second ClaimRun() error = %v", err)
	}
	reconciled, err := runs.MarkRunReconciliationRequired(ctx, user.ID, runID, secondFence, "tool_execution_unknown", "副作用未知", time.Now().UTC())
	if err != nil || reconciled.Status != biz.RunReconciliationRequired {
		t.Fatalf("MarkRunReconciliationRequired() = %#v, %v", reconciled, err)
	}
	events, err := runs.ListEvents(ctx, user.ID, runID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, event := range events {
		types[event.EventType]++
	}
	if types["run.paused"] != 1 || types["run.resumed"] != 1 || types["run.reconciliation_required"] != 1 {
		t.Fatalf("unexpected run events: %#v", types)
	}
}
