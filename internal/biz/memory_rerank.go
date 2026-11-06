package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mark-Panda/harness-agent/pkg/agent/model"
	"github.com/google/uuid"
)

// ErrRerankModelUnavailable 当前没有可用的 rerank 模型或凭证，检索应降级到向量分数。
var ErrRerankModelUnavailable = errors.New("rerank model is unavailable")

// RerankModelSource 返回当前可用的平台级 rerank 模型。
type RerankModelSource interface {
	// GetEnabledRerankModel 返回已启用供应商下的 rerank 模型；不存在时返回未找到。
	GetEnabledRerankModel(ctx context.Context) (providerID uuid.UUID, modelName string, err error)
}

// Reranker 对候选记忆重新打分，返回与 documents 顺序一致的分数。
type Reranker interface {
	// Rerank 返回 len(documents) 个分数；调用失败时由检索方降级。
	Rerank(ctx context.Context, query string, documents []string) ([]float64, error)
}

// LLMReranker 使用平台启用的 rerank 模型为候选记忆重新排序。
type LLMReranker struct {
	providers ProviderCredentialRepository
	models    RerankModelSource
	opener    CredentialOpener
	timeout   time.Duration
}

// NewLLMReranker 构造 rerank 生成器。
func NewLLMReranker(providers ProviderCredentialRepository, models RerankModelSource, opener CredentialOpener, timeout time.Duration) (*LLMReranker, error) {
	if providers == nil || models == nil || opener == nil {
		return nil, invalid("reranker requires provider credentials, model source and opener")
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &LLMReranker{providers: providers, models: models, opener: opener, timeout: timeout}, nil
}

// Rerank 调用启用的 rerank 模型并校验返回下标与文档数量。
func (r *LLMReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if len(documents) == 0 {
		return nil, fmt.Errorf("%w: rerank documents are empty", ErrInvalidConfig)
	}
	providerID, modelName, err := r.models.GetEnabledRerankModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRerankModelUnavailable, err)
	}
	provider, credential, err := r.providers.GetProviderCredential(ctx, providerID)
	if err != nil {
		return nil, fmt.Errorf("%w: read provider credential: %v", ErrRerankModelUnavailable, err)
	}
	if provider.Status != ProviderEnabled {
		return nil, fmt.Errorf("%w: provider is disabled", ErrRerankModelUnavailable)
	}
	plaintext, err := r.opener.Open(credential.KeyVersion, credential.Encrypted)
	if err != nil || len(plaintext) == 0 {
		return nil, fmt.Errorf("%w: decrypt provider credential", ErrRerankModelUnavailable)
	}
	client, err := model.NewClient(provider.BaseURL, string(plaintext), r.timeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRerankModelUnavailable, err)
	}
	response, err := client.Rerank(ctx, model.RerankRequest{
		Model: modelName, Query: query, Documents: documents, TopN: len(documents),
	})
	if err != nil {
		// 认证、参数与能力错误重试无意义，其余错误交给调用方降级。
		if errors.Is(err, model.ErrAuthentication) || errors.Is(err, model.ErrPermanent) || errors.Is(err, model.ErrCapability) {
			return nil, fmt.Errorf("%w: %v", ErrRerankModelUnavailable, err)
		}
		return nil, err
	}
	scores := make([]float64, len(documents))
	scored := 0
	for _, hit := range response.Results {
		if hit.Index < 0 || hit.Index >= len(scores) {
			return nil, fmt.Errorf("%w: rerank index %d is out of range", ErrRerankModelUnavailable, hit.Index)
		}
		scores[hit.Index] = hit.Score
		scored++
	}
	if scored == 0 {
		return nil, fmt.Errorf("%w: rerank returned no results", ErrRerankModelUnavailable)
	}
	return scores, nil
}
