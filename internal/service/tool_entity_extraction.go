package service

import (
	"WeaveKnow/pkg/llm"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// EntityExtractionTool 实现实体提取工具。
type EntityExtractionTool struct{}

func NewEntityExtractionTool() *EntityExtractionTool {
	return &EntityExtractionTool{}
}

func (t *EntityExtractionTool) GetDefinition() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "entity_extraction",
			Description: "从文档中提取关键实体，包括人名、组织、产品、概念、技术术语等",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "用于检索文档的查询关键词",
					},
					"entity_types": map[string]interface{}{
						"type":        "array",
						"description": "要提取的实体类型，如 person, organization, product, concept, technology",
						"items": map[string]interface{}{
							"type": "string",
						},
						"default": []string{"person", "organization", "product", "concept"},
					},
					"max_entities": map[string]interface{}{
						"type":        "integer",
						"description": "最多提取的实体数量，默认 10",
						"default":     10,
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

func (t *EntityExtractionTool) EstimateConfidence(query string, context []llm.Message) float64 {
	q := strings.ToLower(query)

	highConfKeywords := []string{
		"提取", "识别", "找出", "列出",
		"有哪些", "包含哪些", "涉及哪些",
		"人物", "组织", "公司", "产品", "技术",
		"实体", "关键词", "术语",
	}

	for _, kw := range highConfKeywords {
		if strings.Contains(q, kw) {
			return 0.85
		}
	}

	// 如果问题是"谁"、"什么公司"等，可能需要实体提取
	if strings.Contains(q, "谁") || strings.Contains(q, "什么公司") ||
		strings.Contains(q, "哪个组织") || strings.Contains(q, "什么产品") {
		return 0.75
	}

	return 0.3
}

type entityExtractionArgs struct {
	Query       string   `json:"query"`
	EntityTypes []string `json:"entity_types"`
	MaxEntities int      `json:"max_entities"`
}

func (t *EntityExtractionTool) Execute(ctx context.Context, args json.RawMessage, deps *ToolDependencies) (*ToolResult, error) {
	var params entityExtractionArgs
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.MaxEntities <= 0 || params.MaxEntities > 50 {
		params.MaxEntities = 10
	}
	if len(params.EntityTypes) == 0 {
		params.EntityTypes = []string{"person", "organization", "product", "concept"}
	}

	// 先检索相关文档
	results, err := deps.SearchService.HybridSearch(ctx, params.Query, 5, deps.User)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		return &ToolResult{
			Success:     true,
			Content:     `{"ok": true, "entities": [], "total": 0}`,
			DisplayName: "实体提取",
			Message:     "未找到相关文档",
		}, nil
	}

	// 构建文档内容
	var contentBuilder strings.Builder
	for i, r := range results {
		contentBuilder.WriteString(fmt.Sprintf("\n[文档 %d]\n%s\n", i+1, r.TextContent))
	}

	// 构建提取提示词
	extractionPrompt := fmt.Sprintf(`请从以下文档中提取关键实体，返回 JSON 格式。
实体类型：%s
最多提取：%d 个
文档内容：%s
返回格式：
{
  "entities": [
    {"name": "实体名称", "type": "实体类型", "context": "出现上下文"}
  ]
}

只返回 JSON，不要其他解释。`, strings.Join(params.EntityTypes, ", "), params.MaxEntities, contentBuilder.String())

	messages := []llm.Message{
		{Role: "system", Content: "你是专业的实体提取助手，擅长从文本中识别关键实体。"},
		{Role: "user", Content: extractionPrompt},
	}

	response, err := deps.LLMClient.ChatWithTools(ctx, messages, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("entity extraction failed: %w", err)
	}

	// 解析提取结果
	extractedContent := strings.TrimSpace(response.Content)
	extractedContent = strings.TrimPrefix(extractedContent, "```json")
	extractedContent = strings.TrimPrefix(extractedContent, "```")
	extractedContent = strings.TrimSuffix(extractedContent, "```")
	extractedContent = strings.TrimSpace(extractedContent)

	// 验证 JSON 格式
	var entityResult map[string]interface{}
	if err := json.Unmarshal([]byte(extractedContent), &entityResult); err != nil {
		// 如果解析失败，构造一个简单的结果
		entityResult = map[string]interface{}{
			"ok":       true,
			"entities": []interface{}{},
			"total":    0,
			"note":     "实体提取格式解析失败",
		}
		marshaled, _ := json.Marshal(entityResult)
		extractedContent = string(marshaled)
	} else {
		// 添加元数据
		if entities, ok := entityResult["entities"].([]interface{}); ok {
			entityResult["total"] = len(entities)
		}
		entityResult["ok"] = true
		marshaled, _ := json.Marshal(entityResult)
		extractedContent = string(marshaled)
	}

	entityCount := 0
	if entities, ok := entityResult["entities"].([]interface{}); ok {
		entityCount = len(entities)
	}

	return &ToolResult{
		Success:     true,
		Content:     extractedContent,
		Sources:     buildSourceItems(results),
		DisplayName: "实体提取",
		Message:     fmt.Sprintf("已提取 %d 个实体", entityCount),
		Metadata: map[string]interface{}{
			"query":          params.Query,
			"entity_types":   params.EntityTypes,
			"entity_count":   entityCount,
			"document_count": len(results),
		},
	}, nil
}
