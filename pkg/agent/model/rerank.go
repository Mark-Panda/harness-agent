package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// RerankRequest 是一次 rerank 调用入参（Cohere/Jina 兼容格式）。
type RerankRequest struct {
	// Model 供应商侧 rerank 模型名。
	Model string
	// Query 查询文本。
	Query string
	// Documents 候选文档，单次最多 256 条。
	Documents []string
	// TopN 期望返回的结果数；零表示返回全部候选。
	TopN int
}

// RerankHit 是单条候选的重排结果。
type RerankHit struct {
	// Index 对应 Documents 的下标。
	Index int
	// Score 相关性分数，通常位于 [0,1]。
	Score float64
}

// RerankResponse 是重排结果，只保留公开分数与用量。
type RerankResponse struct {
	// Results 供应商返回的重排结果，可能少于候选数。
	Results []RerankHit
	// InputTokens 输入 Token 数。
	InputTokens int
	// ProviderRequestID 供应商请求 ID，用于排障。
	ProviderRequestID string
}

// Rerank 执行一次非流式 rerank 调用，校验下标与分数范围。
func (c *Client) Rerank(ctx context.Context, request RerankRequest) (RerankResponse, error) {
	if request.Model == "" {
		return RerankResponse{}, fmt.Errorf("%w: rerank model name is required", ErrPermanent)
	}
	if request.Query == "" {
		return RerankResponse{}, fmt.Errorf("%w: rerank query is required", ErrPermanent)
	}
	if len(request.Documents) == 0 || len(request.Documents) > 256 {
		return RerankResponse{}, fmt.Errorf("%w: rerank documents must contain between 1 and 256 texts", ErrPermanent)
	}
	for _, document := range request.Documents {
		if len(document) > 32*1024 {
			return RerankResponse{}, fmt.Errorf("%w: rerank document exceeds 32KB", ErrPermanent)
		}
	}
	topN := request.TopN
	if topN <= 0 || topN > len(request.Documents) {
		topN = len(request.Documents)
	}
	body := map[string]any{"model": request.Model, "query": request.Query, "documents": request.Documents, "top_n": topN}
	encoded, err := json.Marshal(body)
	if err != nil {
		return RerankResponse{}, fmt.Errorf("%w: encode rerank request: %v", ErrPermanent, err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/rerank", bytes.NewReader(encoded))
	if err != nil {
		return RerankResponse{}, fmt.Errorf("%w: build rerank request: %v", ErrPermanent, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)

	response, err := c.http.Do(httpRequest)
	if err != nil {
		// 上下文取消属于控制流错误，必须原样上抛。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RerankResponse{}, ctxErr
		}
		return RerankResponse{}, fmt.Errorf("%w: rerank transport error: %w", ErrRetryable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return RerankResponse{}, &ResponseError{
			StatusCode: response.StatusCode, RetryAfter: RetryAfter(response.Header.Get("Retry-After"), 0),
			Class: ErrRateLimited, Message: "rate limited",
		}
	}
	if response.StatusCode >= 500 {
		return RerankResponse{}, &ResponseError{StatusCode: response.StatusCode, Class: ErrRetryable, Message: "provider server error"}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return RerankResponse{}, &ResponseError{
			StatusCode: response.StatusCode, Class: ErrAuthentication, Message: c.redactedError(response.Body),
		}
	}
	if response.StatusCode != http.StatusOK {
		return RerankResponse{}, &ResponseError{
			StatusCode: response.StatusCode, Class: ErrPermanent, Message: c.redactedError(response.Body),
		}
	}

	var payload struct {
		ID      string `json:"id"`
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
			Score          float64 `json:"score"`
		} `json:"results"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&payload); err != nil {
		return RerankResponse{}, fmt.Errorf("%w: decode rerank response: %v", ErrRetryable, err)
	}
	if len(payload.Results) == 0 {
		return RerankResponse{}, fmt.Errorf("%w: provider returned no rerank results", ErrRetryable)
	}
	hits := make([]RerankHit, 0, len(payload.Results))
	seen := make(map[int]struct{}, len(payload.Results))
	for _, item := range payload.Results {
		if item.Index < 0 || item.Index >= len(request.Documents) {
			return RerankResponse{}, fmt.Errorf("%w: rerank index %d is out of range", ErrRetryable, item.Index)
		}
		if _, exists := seen[item.Index]; exists {
			return RerankResponse{}, fmt.Errorf("%w: duplicated rerank index %d", ErrRetryable, item.Index)
		}
		seen[item.Index] = struct{}{}
		score := item.RelevanceScore
		if score == 0 {
			// Jina/TEI 兼容实现使用 score 字段。
			score = item.Score
		}
		hits = append(hits, RerankHit{Index: item.Index, Score: score})
	}
	inputTokens := payload.Usage.PromptTokens
	if inputTokens == 0 {
		inputTokens = payload.Usage.TotalTokens
	}
	return RerankResponse{Results: hits, InputTokens: inputTokens, ProviderRequestID: payload.ID}, nil
}
