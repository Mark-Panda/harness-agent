package biz_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
)

// fakeEmbeddingProviderReader 是供应商元数据端口的测试替身。
type fakeEmbeddingProviderReader struct {
	provider biz.Provider
	err      error
}

// GetProvider 返回预置供应商。
func (f fakeEmbeddingProviderReader) GetProvider(context.Context, uuid.UUID) (biz.Provider, error) {
	return f.provider, f.err
}

// TestVectorStatusHealthy 验证模型与 active 索引一致时判定为生效。
func TestVectorStatusHealthy(t *testing.T) {
	providerID := uuid.New()
	version := "text-embedding-v1-1536-1"
	indexes := &fakeMemoryIndexes{
		indexes: []biz.EmbeddingIndex{{ProviderID: providerID, Model: "text-embedding-v1", Dimension: biz.EmbeddingDimension, Version: version, Status: biz.EmbeddingIndexActive}},
		vectors: map[string]int64{version: 42},
	}
	service, err := biz.NewVectorStatusService(
		&fakeEmbeddingModelSource{providerID: providerID, model: "text-embedding-v1", dimension: biz.EmbeddingDimension},
		indexes, fakeEmbeddingProviderReader{provider: biz.Provider{ID: providerID, Name: "洋葱"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Healthy || status.VectorCount != 42 || status.Warning != "" {
		t.Fatalf("status = %+v", status)
	}
	if status.ProviderName != "洋葱" || status.IndexStatus != "active" {
		t.Fatalf("status = %+v", status)
	}
}

// TestVectorStatusMissingModel 验证未配置 embedding 模型时给出配置提醒。
func TestVectorStatusMissingModel(t *testing.T) {
	service, err := biz.NewVectorStatusService(&fakeEmbeddingModelSource{err: biz.ErrResourceNotFound}, &fakeMemoryIndexes{}, fakeEmbeddingProviderReader{}, true)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Healthy || status.ModelConfigured || !strings.Contains(status.Warning, "embedding") {
		t.Fatalf("status = %+v", status)
	}
}

// TestVectorStatusDisabled 验证配置关闭时即使模型齐备仍提示未生效。
func TestVectorStatusDisabled(t *testing.T) {
	providerID := uuid.New()
	service, err := biz.NewVectorStatusService(
		&fakeEmbeddingModelSource{providerID: providerID, model: "text-embedding-v1", dimension: biz.EmbeddingDimension},
		&fakeMemoryIndexes{}, fakeEmbeddingProviderReader{}, false)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Healthy || status.VectorEnabled || !strings.Contains(status.Warning, "memory.vector.enabled") {
		t.Fatalf("status = %+v", status)
	}
}

// TestVectorStatusModelMismatch 验证生效索引与已配置模型不一致时提示重建中。
func TestVectorStatusModelMismatch(t *testing.T) {
	providerID := uuid.New()
	service, err := biz.NewVectorStatusService(
		&fakeEmbeddingModelSource{providerID: providerID, model: "text-embedding-v2", dimension: biz.EmbeddingDimension},
		&fakeMemoryIndexes{indexes: []biz.EmbeddingIndex{{ProviderID: providerID, Model: "text-embedding-v1", Version: "v1", Status: biz.EmbeddingIndexActive}}},
		fakeEmbeddingProviderReader{}, true)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Healthy || !strings.Contains(status.Warning, "重建") {
		t.Fatalf("status = %+v", status)
	}
}

// TestNewVectorStatusServiceRequiresDependencies 验证缺失依赖时构造失败。
func TestNewVectorStatusServiceRequiresDependencies(t *testing.T) {
	if _, err := biz.NewVectorStatusService(nil, nil, nil, true); !errors.Is(err, biz.ErrInvalidConfig) {
		t.Fatalf("constructor error = %v", err)
	}
}
