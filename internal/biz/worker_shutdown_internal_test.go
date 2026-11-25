package biz

import (
	"context"
	"testing"
	"time"
)

// TestDetachedWriteContextIgnoresCancelAndKeepsDeadline 验证停机写上下文脱离取消但仍受截止时间约束。
func TestDetachedWriteContextIgnoresCancelAndKeepsDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	parent, cancelParent := context.WithCancel(context.Background())
	parent, cancelDeadline := context.WithDeadline(parent, deadline)
	defer cancelDeadline()

	writeCtx, cancel := detachedWriteContext(parent)
	defer cancel()
	cancelParent()

	if writeCtx.Err() != nil {
		t.Fatalf("detached context must ignore parent cancellation: %v", writeCtx.Err())
	}
	got, ok := writeCtx.Deadline()
	if !ok || !got.Equal(deadline) {
		t.Fatalf("deadline = %v, %v; want %v", got, ok, deadline)
	}
}

// TestDetachedWriteContextWithoutDeadline 验证普通执行路径没有截止时间时行为不变。
func TestDetachedWriteContextWithoutDeadline(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	writeCtx, cancel := detachedWriteContext(parent)
	defer cancel()
	cancelParent()

	if writeCtx.Err() != nil {
		t.Fatalf("detached context must ignore parent cancellation: %v", writeCtx.Err())
	}
	if _, ok := writeCtx.Deadline(); ok {
		t.Fatal("detached context must not invent a deadline")
	}
}

// TestReleaseWriteContextIsBounded 验证退队写即使在父上下文取消后仍有独立超时。
func TestReleaseWriteContextIsBounded(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	writeCtx, cancel := releaseWriteContext(parent)
	defer cancel()
	if writeCtx.Err() != nil {
		t.Fatalf("release context must ignore parent cancellation: %v", writeCtx.Err())
	}
	deadline, ok := writeCtx.Deadline()
	if !ok {
		t.Fatal("release context must have a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > jobReleaseTimeout+time.Second {
		t.Fatalf("release deadline remaining = %v, want within %v", remaining, jobReleaseTimeout)
	}
}
