package service

import (
	"WeaveKnow/internal/model"
	"WeaveKnow/pkg/llm"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// KnowledgeSearchTool 实现知识库检索工具。
type KnowledgeSearchTool struct {
	defaultTopK int
}

// NewKnowledgeSearchTool 创建知识库检索工具。
func NewKnowledgeSearchTool(defaultTopK int) *KnowledgeSearchTool {
	if defaultTopK <= 0 {
		defaultTopK = 5
	}
	return &KnowledgeSearchTool{defaultTopK: defaultTopK}
}

func (t *KnowledgeSearchTool) GetDefinition() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "knowledge_search",
			Description: "在企业知识库中检索与问题相关的文档片段，支持语义检索和关键词匹配",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "用于检索的关键词或问题，支持自然语言描述",
					},
					"top_k": map[string]interface{}{
						"type":        "integer",
						"description": "返回文档条数，建议 1~20，默认 5",
						"default":     t.defaultTopK,
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

func (t *KnowledgeSearchTool) EstimateConfidence(query string, context []llm.Message) float64 {
	q := strings.ToLower(query)

	// 高置信度关键词
	highConfKeywords := []string{
		"查找", "搜索", "检索", "找到", "有没有", "是否存在",
		"文档", "资料", "信息", "内容", "知识库",
		"什么是", "如何", "怎么", "为什么",
	}

	for _, kw := range highConfKeywords {
		if strings.Contains(q, kw) {
			return 0.95
		}
	}

	// 中等置信度：问句形式
	if strings.HasSuffix(q, "?") || strings.HasSuffix(q, "？") {
		return 0.8
	}

	// 默认中等置信度（大部分问题都需要检索）
	return 0.7
}

type knowledgeSearchArgs struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func (t *KnowledgeSearchTool) Execute(ctx context.Context, args json.RawMessage, deps *ToolDependencies) (*ToolResult, error) {
	var params knowledgeSearchArgs
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if strings.TrimSpace(params.Query) == "" {
		return nil, fmt.Errorf("query cannot be empty")
	}

	if params.TopK <= 0 || params.TopK > 20 {
		params.TopK = t.defaultTopK
	}

	// 执行检索
	results, err := deps.SearchService.HybridSearch(ctx, params.Query, params.TopK, deps.User)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	// 格式化结果
	content := formatSearchResults(results)
	sources := buildSourceItems(results)

	message := fmt.Sprintf("已检索到 %d 条相关内容", len(results))
	if len(results) == 0 {
		message = "未检索到相关内容"
	}

	return &ToolResult{
		Success:     true,
		Content:     content,
		Sources:     sources,
		DisplayName: "知识库检索",
		Message:     message,
		Metadata: map[string]interface{}{
			"query":        params.Query,
			"top_k":        params.TopK,
			"result_count": len(results),
		},
	}, nil
}

func formatSearchResults(results []model.SearchResponseDTO) string {
	if len(results) == 0 {
		return `{"ok": true, "results": [], "total": 0}`
	}

	type item struct {
		Index   int     `json:"index"`
		File    string  `json:"file_name"`
		Score   float64 `json:"score"`
		Snippet string  `json:"snippet"`
	}

	items := make([]item, len(results))
	for i, r := range results {
		items[i] = item{
			Index:   i + 1,
			File:    r.FileName,
			Score:   r.Score,
			Snippet: strings.TrimSpace(r.TextContent),
		}
	}

	payload := map[string]interface{}{
		"ok":      true,
		"total":   len(results),
		"results": items,
	}

	b, _ := json.Marshal(payload)
	return string(b)
}
