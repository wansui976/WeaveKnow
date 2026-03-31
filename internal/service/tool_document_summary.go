package service

import (
	"WeaveKnow/pkg/llm"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DocumentSummaryTool 实现文档摘要工具。
type DocumentSummaryTool struct{}

func NewDocumentSummaryTool() *DocumentSummaryTool {
	return &DocumentSummaryTool{}
}

func (t *DocumentSummaryTool) GetDefinition() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "document_summary",
			Description: "对检索到的文档内容生成结构化摘要，提取关键信息和要点",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "用于检索文档的查询关键词",
					},
					"max_docs": map[string]interface{}{
						"type":        "integer",
						"description": "要摘要的文档数量，默认 3",
						"default":     3,
					},
					"summary_type": map[string]interface{}{
						"type":        "string",
						"description": "摘要类型：brief(简要), detailed(详细), bullet_points(要点)",
						"enum":        []string{"brief", "detailed", "bullet_points"},
						"default":     "brief",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

func (t *DocumentSummaryTool) EstimateConfidence(query string, context []llm.Message) float64 {
	q := strings.ToLower(query)

	highConfKeywords := []string{
		"总结", "摘要", "概括", "归纳", "梳理",
		"主要内容", "核心要点", "关键信息",
		"简述", "概述", "大致说说",
	}

	for _, kw := range highConfKeywords {
		if strings.Contains(q, kw) {
			return 0.9
		}
	}

	// 如果查询较长（>50字），可能需要摘要
	if len([]rune(q)) > 50 {
		return 0.6
	}

	return 0.4
}

type documentSummaryArgs struct {
	Query       string `json:"query"`
	MaxDocs     int    `json:"max_docs"`
	SummaryType string `json:"summary_type"`
}

func (t *DocumentSummaryTool) Execute(ctx context.Context, args json.RawMessage, deps *ToolDependencies) (*ToolResult, error) {
	var params documentSummaryArgs
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.MaxDocs <= 0 || params.MaxDocs > 10 {
		params.MaxDocs = 3
	}
	if params.SummaryType == "" {
		params.SummaryType = "brief"
	}

	// 先检索文档
	results, err := deps.SearchService.HybridSearch(ctx, params.Query, params.MaxDocs, deps.User)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		return &ToolResult{
			Success:     true,
			Content:     `{"ok": true, "summary": "未找到相关文档", "documents": 0}`,
			DisplayName: "文档摘要",
			Message:     "未找到相关文档",
		}, nil
	}

	// 构建摘要提示词
	var contentBuilder strings.Builder
	for i, r := range results {
		contentBuilder.WriteString(fmt.Sprintf("\n[文档 %d: %s]\n%s\n", i+1, r.FileName, r.TextContent))
	}

	summaryPrompt := buildSummaryPrompt(params.SummaryType, contentBuilder.String())

	// 调用 LLM 生成摘要
	messages := []llm.Message{
		{Role: "system", Content: "你是专业的文档摘要助手，擅长提取关键信息。"},
		{Role: "user", Content: summaryPrompt},
	}

	response, err := deps.LLMClient.ChatWithTools(ctx, messages, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("summary generation failed: %w", err)
	}

	summary := strings.TrimSpace(response.Content)

	result := map[string]interface{}{
		"ok":        true,
		"summary":   summary,
		"documents": len(results),
		"sources":   buildSourceItems(results),
	}

	content, _ := json.Marshal(result)

	return &ToolResult{
		Success:     true,
		Content:     string(content),
		Sources:     buildSourceItems(results),
		DisplayName: "文档摘要",
		Message:     fmt.Sprintf("已生成 %d 篇文档的摘要", len(results)),
		Metadata: map[string]interface{}{
			"query":        params.Query,
			"doc_count":    len(results),
			"summary_type": params.SummaryType,
		},
	}, nil
}

func buildSummaryPrompt(summaryType, content string) string {
	switch summaryType {
	case "detailed":
		return fmt.Sprintf("请对以下文档内容生成详细摘要，包含主要观点、关键数据和重要细节：\n%s", content)
	case "bullet_points":
		return fmt.Sprintf("请将以下文档内容提炼为要点列表（使用 - 开头）：\n%s", content)
	default: // brief
		return fmt.Sprintf("请用 2-3 句话简要概括以下文档的核心内容：\n%s", content)
	}
}
