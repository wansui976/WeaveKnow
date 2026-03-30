package reranker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Document 是重排输入文档。
type Document struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
}

// Result 是重排输出分数。
type Result struct {
	ID    int     `json:"id"`
	Score float64 `json:"score"`
}

// Client 定义外部重排服务客户端接口。
type Client interface {
	Rerank(ctx context.Context, query string, docs []Document, topK int) ([]Result, error)
}

type Options struct {
	Endpoint string
	Timeout  time.Duration
	APIKey   string
}

type httpClient struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewClient(opts Options) Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &httpClient{
		endpoint: strings.TrimSpace(opts.Endpoint),
		apiKey:   strings.TrimSpace(opts.APIKey),
		client:   &http.Client{Timeout: timeout},
	}
}

func (c *httpClient) Rerank(ctx context.Context, query string, docs []Document, topK int) ([]Result, error) {
	if c.endpoint == "" {
		return nil, fmt.Errorf("reranker endpoint is empty")
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}
	if len(docs) == 0 {
		return []Result{}, nil
	}
	if topK <= 0 || topK > len(docs) {
		topK = len(docs)
	}

	reqBody := map[string]interface{}{
		"query":     query,
		"documents": docs,
		"top_k":     topK,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal rerank request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create rerank request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call rerank service failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rerank service returned status=%s, body=%s", resp.Status, string(raw))
	}

	// 兼容两种格式：{results:[{id,score}]} / {data:[{id,score}]}
	var parsed struct {
		Results []Result `json:"results"`
		Data    []Result `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode rerank response failed: %w", err)
	}
	if len(parsed.Results) > 0 {
		return parsed.Results, nil
	}
	return parsed.Data, nil
}
