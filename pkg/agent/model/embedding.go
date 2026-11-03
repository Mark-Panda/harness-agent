package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// EmbeddingRequest 是一次 OpenAI-compatible Embeddings 调用入参。
type EmbeddingRequest struct {
	// Model 供应商侧 embedding 模型名。
	Model string
	// Inputs 待向量化文本，单次最多 64 条。
	Inputs []string
	// Dimensions 期望维度；零表示由供应商返回，非零时必须与返回值一致。
	Dimensions int
}

// EmbeddingResponse 是向量化结果，只保留公开向量与用量。
type EmbeddingResponse struct {
	// Embeddings 与 Inputs 顺序一致的向量列表。
	Embeddings [][]float32
	// InputTokens 输入 Token 数。
	InputTokens int
	// ProviderRequestID 供应商请求 ID，用于排障。
	ProviderRequestID string
}

// Embed 执行一次非流式 Embeddings 调用，严格校验条数与维度。
func (c *Client) Embed(ctx context.Context, request EmbeddingRequest) (EmbeddingResponse, error) {
	if request.Model == "" {
		return EmbeddingResponse{}, fmt.Errorf("%w: embedding model name is required", ErrPermanent)
	}
	if len(request.Inputs) == 0 || len(request.Inputs) > 64 {
		return EmbeddingResponse{}, fmt.Errorf("%w: embedding inputs must contain between 1 and 64 texts", ErrPermanent)
	}
	for _, input := range request.Inputs {
		if len(input) > 32*1024 {
			return EmbeddingResponse{}, fmt.Errorf("%w: embedding input exceeds 32KB", ErrPermanent)
		}
	}
	body := map[string]any{"model": request.Model, "input": request.Inputs}
	if request.Dimensions > 0 {
		body["dimensions"] = request.Dimensions
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("%w: encode embedding request: %v", ErrPermanent, err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(encoded))
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("%w: build embedding request: %v", ErrPermanent, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)

	response, err := c.http.Do(httpRequest)
	if err != nil {
		// 上下文取消属于控制流错误，必须原样上抛。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return EmbeddingResponse{}, ctxErr
		}
		return EmbeddingResponse{}, fmt.Errorf("%w: embedding transport error: %w", ErrRetryable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return EmbeddingResponse{}, &ResponseError{
			StatusCode: response.StatusCode, RetryAfter: RetryAfter(response.Header.Get("Retry-After"), 0),
			Class: ErrRateLimited, Message: "rate limited",
		}
	}
	if response.StatusCode >= 500 {
		return EmbeddingResponse{}, &ResponseError{StatusCode: response.StatusCode, Class: ErrRetryable, Message: "provider server error"}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return EmbeddingResponse{}, &ResponseError{
			StatusCode: response.StatusCode, Class: ErrAuthentication, Message: c.redactedError(response.Body),
		}
	}
	if response.StatusCode != http.StatusOK {
		return EmbeddingResponse{}, &ResponseError{
			StatusCode: response.StatusCode, Class: ErrPermanent, Message: c.redactedError(response.Body),
		}
	}

	var payload struct {
		ID   string `json:"id"`
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&payload); err != nil {
		return EmbeddingResponse{}, fmt.Errorf("%w: decode embedding response: %v", ErrRetryable, err)
	}
	if len(payload.Data) != len(request.Inputs) {
		return EmbeddingResponse{}, fmt.Errorf("%w: embedding count %d does not match input count %d", ErrRetryable, len(payload.Data), len(request.Inputs))
	}
	sort.SliceStable(payload.Data, func(i, j int) bool { return payload.Data[i].Index < payload.Data[j].Index })
	embeddings := make([][]float32, 0, len(payload.Data))
	for position, item := range payload.Data {
		if item.Index != position {
			return EmbeddingResponse{}, fmt.Errorf("%w: embedding index is out of order", ErrRetryable)
		}
		if len(item.Embedding) == 0 {
			return EmbeddingResponse{}, fmt.Errorf("%w: provider returned an empty embedding", ErrRetryable)
		}
		if request.Dimensions > 0 && len(item.Embedding) != request.Dimensions {
			return EmbeddingResponse{}, fmt.Errorf("%w: embedding dimension %d does not match %d", ErrPermanent, len(item.Embedding), request.Dimensions)
		}
		embeddings = append(embeddings, item.Embedding)
	}
	inputTokens := payload.Usage.PromptTokens
	if inputTokens == 0 {
		inputTokens = payload.Usage.TotalTokens
	}
	return EmbeddingResponse{Embeddings: embeddings, InputTokens: inputTokens, ProviderRequestID: payload.ID}, nil
}
