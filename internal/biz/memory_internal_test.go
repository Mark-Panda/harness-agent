package biz

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestSanitizeVersion 验证模型名被转换为安全的索引版本片段。
func TestSanitizeVersion(t *testing.T) {
	if got := sanitizeVersion("Text-Embedding/中文"); got == "" || strings.ContainsAny(got, "/中文") {
		t.Fatalf("sanitized = %q", got)
	}
	if got := sanitizeVersion(""); got != "embedding" {
		t.Fatalf("empty sanitized = %q", got)
	}
	long := strings.Repeat("a", 80)
	if got := sanitizeVersion(long); len(got) != 48 {
		t.Fatalf("long version length = %d", len(got))
	}
}

// TestMemoryMaintenanceStartStops 验证维护循环随上下文取消退出。
func TestMemoryMaintenanceStartStops(t *testing.T) {
	indexes := &internalIndexStub{}
	service, err := NewMemoryIndexService(indexes, &internalModelStub{}, &internalQueueStub{}, MemoryIndexServiceConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := NewMemoryMaintenance(service, MemoryMaintenanceConfig{Enabled: true, Interval: time.Millisecond}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	maintenance.Start(ctx)
	// 未启用时立即返回。
	disabled, err := NewMemoryMaintenance(service, MemoryMaintenanceConfig{}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	disabled.Start(context.Background())
}

// internalIndexStub 是最小索引仓储桩。
type internalIndexStub struct{}

// ListIndexes 返回空列表。
func (internalIndexStub) ListIndexes(context.Context) ([]EmbeddingIndex, error) { return nil, nil }

// GetIndex 返回未找到。
func (internalIndexStub) GetIndex(context.Context, string) (EmbeddingIndex, error) {
	return EmbeddingIndex{}, ErrResourceNotFound
}

// CreateIndex 返回输入索引。
func (internalIndexStub) CreateIndex(_ context.Context, index EmbeddingIndex) (EmbeddingIndex, error) {
	return index, nil
}

// ActivateIndex 返回激活索引。
func (internalIndexStub) ActivateIndex(_ context.Context, version string) (EmbeddingIndex, error) {
	return EmbeddingIndex{Version: version, Status: EmbeddingIndexActive}, nil
}

// SetDualWrite 返回 nil。
func (internalIndexStub) SetDualWrite(context.Context, string, bool) error { return nil }

// CountVectors 返回 0。
func (internalIndexStub) CountVectors(context.Context, string) (int64, error) { return 0, nil }

// CountEligibleMessages 返回 0。
func (internalIndexStub) CountEligibleMessages(context.Context) (int64, error) { return 0, nil }

// ListUnembeddedMessages 返回空列表。
func (internalIndexStub) ListUnembeddedMessages(context.Context, string, int) ([]Message, error) {
	return nil, nil
}

// internalModelStub 是 embedding 模型来源桩。
type internalModelStub struct{}

// GetEnabledEmbeddingModel 返回固定模型。
func (internalModelStub) GetEnabledEmbeddingModel(context.Context) (uuid.UUID, string, int, error) {
	return uuid.New(), "embedding", EmbeddingDimension, nil
}

// internalQueueStub 是任务入队桩。
type internalQueueStub struct{}

// EnqueueMemory 返回 nil。
func (internalQueueStub) EnqueueMemory(context.Context, MemoryTask, time.Time) error { return nil }
