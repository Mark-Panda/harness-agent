package biz

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recordingJobQueue 记录 Claim 与 ClaimLane 的调用，用于验证池化认领分流。
type recordingJobQueue struct {
	claimedLane *JobClaim
	claimedBase bool
}

func (q *recordingJobQueue) EnqueueRun(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	return nil
}

func (q *recordingJobQueue) Claim(context.Context, string, string, time.Duration, time.Time) (Job, bool, error) {
	q.claimedBase = true
	return Job{}, false, nil
}

func (q *recordingJobQueue) Renew(context.Context, uuid.UUID, string, int64, time.Duration, time.Time) error {
	return nil
}

func (q *recordingJobQueue) Release(context.Context, uuid.UUID, string, int64, time.Time, time.Time) error {
	return nil
}

func (q *recordingJobQueue) Complete(context.Context, uuid.UUID, string, int64) error { return nil }

func (q *recordingJobQueue) Fail(context.Context, uuid.UUID, string, int64, time.Time, string, time.Time) error {
	return nil
}

func (q *recordingJobQueue) ClaimLane(_ context.Context, claim JobClaim) (Job, bool, error) {
	copied := claim
	q.claimedLane = &copied
	return Job{}, false, nil
}

// TestRunLaneMapping 校验三种 Agent 模式映射到稳定 lane。
func TestRunLaneMapping(t *testing.T) {
	cases := map[AgentMode]string{
		AgentModeReact:       JobLaneReact,
		AgentModePlan:        JobLanePlan,
		AgentModeSupervisor:  JobLaneSupervisor,
		AgentMode("unknown"): "",
	}
	for mode, expected := range cases {
		if actual := RunLane(mode); actual != expected {
			t.Fatalf("RunLane(%q) = %q, want %q", mode, actual, expected)
		}
	}
}

// TestClaimNextJobUsesLaneQueue 校验配置 lane 时优先走池化认领。
func TestClaimNextJobUsesLaneQueue(t *testing.T) {
	queue := &recordingJobQueue{}
	claim := JobClaim{
		WorkerID: "worker-react", JobType: "agent.run",
		Lanes: []string{JobLaneReact}, Lease: time.Minute, Now: time.Now().UTC(),
	}
	if _, _, err := ClaimNextJob(context.Background(), queue, claim); err != nil {
		t.Fatal(err)
	}
	if queue.claimedBase {
		t.Fatal("lane-aware queue should not fall back to Claim")
	}
	if queue.claimedLane == nil || len(queue.claimedLane.Lanes) != 1 || queue.claimedLane.Lanes[0] != JobLaneReact {
		t.Fatalf("unexpected lane claim: %#v", queue.claimedLane)
	}
}

// TestClaimNextJobFallsBackWithoutLane 校验单池模式仍使用原有 Claim。
func TestClaimNextJobFallsBackWithoutLane(t *testing.T) {
	queue := &recordingJobQueue{}
	claim := JobClaim{WorkerID: "worker-default", JobType: "agent.run", Lease: time.Minute}
	if _, _, err := ClaimNextJob(context.Background(), queue, claim); err != nil {
		t.Fatal(err)
	}
	if !queue.claimedBase {
		t.Fatal("expected fallback Claim when lanes are empty")
	}
}
