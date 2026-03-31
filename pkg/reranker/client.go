package reranker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	// Model 仅在 Jina 接口下生效；为空时会使用默认值。
	Model string
}

type httpClient struct {
	endpoint string
	apiKey   string
	model    string
	isJina   bool
	client   *http.Client
}

func NewClient(opts Options) Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	endpoint := strings.TrimSpace(opts.Endpoint)
	isJina := isJinaEndpoint(endpoint)

	apiKey := strings.TrimSpace(opts.APIKey)
	if apiKey == "" && isJina {
		apiKey = strings.TrimSpace(os.Getenv("JINA_API_KEY"))
	}

	model := strings.TrimSpace(opts.Model)
	if model == "" && isJina {
		model = strings.TrimSpace(os.Getenv("JINA_RERANK_MODEL"))
	}
	if model == "" && isJina {
		// 多语种默认模型，中文可直接使用。
		model = "jina-reranker-v2-base-multilingual"
	}

	return &httpClient{
		endpoint: endpoint,
		apiKey:   apiKey,
		model:    model,
		isJina:   isJina,
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

	if c.isJina {
		return c.rerankJina(ctx, query, docs, topK)
	}

	return c.rerankGeneric(ctx, query, docs, topK)
}

func (c *httpClient) rerankGeneric(ctx context.Context, query string, docs []Document, topK int) ([]Result, error) {
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

func (c *httpClient) rerankJina(ctx context.Context, query string, docs []Document, topK int) ([]Result, error) {
	// Jina 文档可直接传字符串数组；返回结果中的 index 就是输入顺序位置。
	docTexts := make([]string, 0, len(docs))
	for _, d := range docs {
		docTexts = append(docTexts, d.Text)
	}

	reqBody := map[string]interface{}{
		"model":     c.model,
		"query":     query,
		"documents": docTexts,
		"top_n":     topK,
		// 仅需排序结果，减少响应体大小。
		"return_documents": false,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal jina rerank request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create jina rerank request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call jina rerank service failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jina rerank returned status=%s, body=%s", resp.Status, string(raw))
	}

	var parsed struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode jina rerank response failed: %w", err)
	}

	out := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		out = append(out, Result{
			ID:    r.Index,
			Score: r.RelevanceScore,
		})
	}
	return out, nil
}

func isJinaEndpoint(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return strings.Contains(strings.ToLower(endpoint), "jina.ai")
	}

	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "jina.ai") {
		return true
	}
	return strings.Contains(strings.ToLower(u.Path), "/v1/rerank")
}
