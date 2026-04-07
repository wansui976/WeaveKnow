package service

import (
	"WeaveKnow/internal/model"
	"WeaveKnow/pkg/llm"
	"context"
	"encoding/json"
	"fmt"
	"sort"
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

	// 格式化结果，使用 token 预算裁剪 snippet，防止工具结果撑爆上下文窗口。
	content := formatSearchResultsWithBudget(results, deps.BudgetTokens)
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

// formatSearchResultsWithBudget 将检索结果序列化为 JSON，按 token 预算裁剪 snippet。
// 与 agentService.formatKnowledgeSearchResult 逻辑一致：按 score 降序分配预算，
// 每条 snippet 通过 trimTextByTokenBudget 在句子边界截断。
// budgetTokens <= 0 时退化为不限制（使用完整 TextContent）。
func formatSearchResultsWithBudget(results []model.SearchResponseDTO, budgetTokens int) string {
	type item struct {
		Index   int     `json:"index"`
		FileMD5 string  `json:"file_md5"`
		File    string  `json:"file_name"`
		ChunkID int     `json:"chunk_id"`
		Score   float64 `json:"score"`
		Snippet string  `json:"snippet"`
	}
	type payload struct {
		OK           bool   `json:"ok"`
		BudgetTokens int    `json:"budget_tokens"`
		Selected     int    `json:"selected"`
		Total        int    `json:"total"`
		Results      []item `json:"results"`
	}

	if len(results) == 0 {
		b, _ := json.Marshal(payload{OK: true, BudgetTokens: budgetTokens, Results: []item{}})
		return string(b)
	}

	// 无预算限制时直接全量返回，无需排序和裁剪。
	if budgetTokens <= 0 {
		items := make([]item, len(results))
		for i, r := range results {
			items[i] = item{
				Index:   i + 1,
				FileMD5: r.FileMD5,
				File:    r.FileName,
				ChunkID: r.ChunkID,
				Score:   r.Score,
				Snippet: strings.TrimSpace(r.TextContent),
			}
		}
		b, _ := json.Marshal(payload{OK: true, BudgetTokens: 0, Selected: len(items), Total: len(results), Results: items})
		return string(b)
	}

	// 按 score 降序排序，优先给高价值结果分配预算。
	sorted := make([]model.SearchResponseDTO, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Score > sorted[j].Score
	})

	const (
		baseOverheadTokens = 80
		docOverheadTokens  = 35
		minSnippetTokens   = 40
		maxSnippetTokens   = 220
	)
	usedTokens := baseOverheadTokens
	selected := make([]item, 0, len(sorted))
	for i, r := range sorted {
		remainingBudget := budgetTokens - usedTokens
		if remainingBudget <= docOverheadTokens+minSnippetTokens {
			break
		}
		remainingDocs := len(sorted) - i
		targetSnippetTokens := (remainingBudget/remainingDocs) - docOverheadTokens
		if targetSnippetTokens < minSnippetTokens {
			targetSnippetTokens = minSnippetTokens
		}
		if targetSnippetTokens > maxSnippetTokens {
			targetSnippetTokens = maxSnippetTokens
		}
		snippet := trimTextByTokenBudget(strings.TrimSpace(r.TextContent), targetSnippetTokens)
		if snippet == "" {
			continue
		}
		usedTokens += docOverheadTokens + estimateApproxTokens(snippet)
		selected = append(selected, item{
			Index:   i + 1,
			FileMD5: r.FileMD5,
			File:    r.FileName,
			ChunkID: r.ChunkID,
			Score:   r.Score,
			Snippet: snippet,
		})
	}

	b, _ := json.Marshal(payload{OK: true, BudgetTokens: budgetTokens, Selected: len(selected), Total: len(results), Results: selected})
	return string(b)
}
