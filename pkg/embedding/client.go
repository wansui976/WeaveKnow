// Package embedding provides a client for interacting with embedding models.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"pai-smart-go/internal/config"
	"pai-smart-go/pkg/log"
)

// Client defines the interface for an embedding client.
type Client interface {
	CreateEmbedding(ctx context.Context, text string) ([]float32, error)
}

type openAICompatibleClient struct {
	// cfg 保存 Embedding 提供方的模型、地址、鉴权与维度配置。
	cfg config.EmbeddingConfig
	// client 负责发起 HTTP 请求，便于统一设置超时/Transport。
	client *http.Client
}

// NewClient creates a new embedding client based on the provider in the config.
func NewClient(cfg config.EmbeddingConfig) Client {
	return &openAICompatibleClient{
		cfg:    cfg,
		client: &http.Client{},
	}
}

// embeddingRequest 是对 OpenAI-compatible /embeddings 请求体的最小封装。
type embeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

// embeddingResponse 仅保留当前业务需要的响应字段（data[].embedding）。
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// CreateEmbedding calls the OpenAI-compatible API to get the vector for a given text.
func (c *openAICompatibleClient) CreateEmbedding(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.CreateEmbeddings(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, fmt.Errorf("received empty embedding from api")
	}
	return vectors[0], nil
}

// CreateEmbeddings 批量调用 OpenAI-compatible /embeddings 接口。
// 该方法未加入 Client 接口，可通过类型断言按需启用批量向量化。
func (c *openAICompatibleClient) CreateEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	log.Infof("[EmbeddingClient] 开始调用 Embedding API, model: %s, batch_size: %d", c.cfg.Model, len(texts))

	reqBody := embeddingRequest{
		Model:      c.cfg.Model,
		Input:      texts,
		Dimensions: c.cfg.Dimensions,
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.BaseURL+"/embeddings", bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create embedding request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.client.Do(req)
	if err != nil {
		log.Errorf("[EmbeddingClient] 调用 Embedding API 失败, error: %v", err)
		return nil, fmt.Errorf("failed to call embedding api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Errorf("[EmbeddingClient] Embedding API 返回非 200 状态码: %s", resp.Status)
		return nil, fmt.Errorf("embedding api returned non-200 status: %s", resp.Status)
	}

	var embeddingResp embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embeddingResp); err != nil {
		log.Errorf("[EmbeddingClient] 解析 Embedding API 响应失败, error: %v", err)
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}

	if len(embeddingResp.Data) == 0 {
		log.Warnf("[EmbeddingClient] Embedding API 返回了空的向量数据")
		return nil, fmt.Errorf("received empty embedding from api")
	}

	vectors := make([][]float32, 0, len(embeddingResp.Data))
	for _, item := range embeddingResp.Data {
		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("received empty embedding from api")
		}
		vectors = append(vectors, item.Embedding)
	}

	log.Infof("[EmbeddingClient] 成功从 Embedding API 获取向量, 批量条数: %d, 维度: %d", len(vectors), len(vectors[0]))
	return vectors, nil
}
