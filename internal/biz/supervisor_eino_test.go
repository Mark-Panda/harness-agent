package biz

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/Mark-Panda/harness-agent/pkg/agent/react"
	"github.com/google/uuid"
)

// einoSupervisorFixture 构造可调用 executeSupervisor 的 Supervisor Worker。
func einoSupervisorFixture(t *testing.T, handler func(body string, w http.ResponseWriter)) (*ExecutionWorker, *memorySupervisorRepo, *memorySupervisorSessions, []model.Candidate) {
	t.Helper()
	worker, repo, sessions, candidates, _ := supervisorFixture(t, handler)
	return worker, repo, sessions, candidates
}

// TestExecuteSupervisorEinoSuccess 验证 Eino 图能生成委派、并发执行全部 Worker 并汇总。
func TestExecuteSupervisorEinoSuccess(t *testing.T) {
	delegation := `{"workers":[{"name":"A","task":"任务A","tools":[]},{"name":"B","task":"任务B","tools":[]}]}`
	worker, repo, sessions, candidates := einoSupervisorFixture(t, func(body string, w http.ResponseWriter) {
		switch {
		case strings.Contains(body, "请输出 Worker 委派 JSON"):
			supervisorReply(t, w, delegation, http.StatusOK)
		case strings.Contains(body, "请汇总以下 Worker 结果"):
			supervisorReply(t, w, "汇总结果：A 与 B 均完成", http.StatusOK)
		default:
			supervisorReply(t, w, "Worker 结果", http.StatusOK)
		}
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	result, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1},
		candidates, nil, []react.Message{{Role: react.RoleUser, Content: "请分别处理两个任务"}}, emptyToolRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "汇总结果") {
		t.Fatalf("result content = %q", result.Content)
	}
	tasks, err := repo.ListTasks(context.Background(), run.UserID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Status != SupervisorTaskCompleted || tasks[1].Status != SupervisorTaskCompleted {
		t.Fatalf("tasks = %#v", tasks)
	}
	if len(sessions.sessions) != 2 {
		t.Fatalf("child sessions = %d", len(sessions.sessions))
	}
}

// TestExecuteSupervisorEinoNoPendingDirectAggregate 验证没有待执行任务时跳过 Worker 节点直接汇总。
func TestExecuteSupervisorEinoNoPendingDirectAggregate(t *testing.T) {
	worker, repo, _, candidates := einoSupervisorFixture(t, func(body string, w http.ResponseWriter) {
		if strings.Contains(body, "请汇总以下 Worker 结果") {
			supervisorReply(t, w, "恢复汇总", http.StatusOK)
			return
		}
		supervisorReply(t, w, "unused", http.StatusOK)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	task := SupervisorTask{
		ID: uuid.New(), UserID: run.UserID, ParentRunID: run.ID, ChildSessionID: uuid.New(),
		WorkerName: "A", WorkerIndex: 1, Task: "任务A", Status: SupervisorTaskCompleted,
		Result: map[string]any{"content": "已完成结果"}, MaxAttempts: 1, Depth: 1,
	}
	repo.tasks[task.ID] = task
	result, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1},
		candidates, nil, []react.Message{{Role: react.RoleUser, Content: "任务"}}, emptyToolRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "恢复汇总" {
		t.Fatalf("result content = %q", result.Content)
	}
}

// TestExecuteSupervisorEinoAllFailed 验证全部 Worker 失败时 Eino 图返回统一失败错误。
func TestExecuteSupervisorEinoAllFailed(t *testing.T) {
	delegation := `{"workers":[{"name":"A","task":"FAIL","tools":[]}]}`
	worker, _, _, candidates := einoSupervisorFixture(t, func(body string, w http.ResponseWriter) {
		if strings.Contains(body, "请输出 Worker 委派 JSON") {
			supervisorReply(t, w, delegation, http.StatusOK)
			return
		}
		supervisorReply(t, w, "error", http.StatusInternalServerError)
	})
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	if _, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1},
		candidates, nil, []react.Message{{Role: react.RoleUser, Content: "处理"}}, emptyToolRegistry{}); !errors.Is(err, ErrSupervisorAllFailed) {
		t.Fatalf("all failed error = %v", err)
	}
}

// TestExecuteSupervisorEinoBudgetPartial 验证预算耗尽但已有 Worker 产出时降级为部分结果。
func TestExecuteSupervisorEinoBudgetPartial(t *testing.T) {
	delegation := `{"workers":[{"name":"A","task":"任务A","tools":[]}]}`
	worker, _, _, candidates := einoSupervisorFixture(t, func(body string, w http.ResponseWriter) {
		switch {
		case strings.Contains(body, "请输出 Worker 委派 JSON"):
			supervisorReply(t, w, delegation, http.StatusOK)
		default:
			supervisorReply(t, w, "结果", http.StatusOK)
		}
	})
	// 规划与 Worker 各消耗 8 Token；上限允许规划完成，但在 Worker 后触发部分结果降级。
	worker.config.SupervisorMaxTokens = 9
	run := Run{ID: uuid.New(), UserID: uuid.New(), SessionID: uuid.New(), Mode: AgentModeSupervisor}
	result, err := worker.executeSupervisor(context.Background(), run, RunFence{Owner: "supervisor-worker", Token: 1},
		candidates, nil, []react.Message{{Role: react.RoleUser, Content: "处理"}}, emptyToolRegistry{})
	if err != nil {
		t.Fatalf("budget degradation error = %v", err)
	}
	if !strings.Contains(result.Content, "结果") {
		t.Fatalf("partial result content = %q", result.Content)
	}
}
