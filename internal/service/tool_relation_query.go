package service

import (
	"WeaveKnow/pkg/llm"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RelationQueryTool 实现关系查询工具。
type RelationQueryTool struct{}

func NewRelationQueryTool() *RelationQueryTool {
	return &RelationQueryTool{}
}

func (t *RelationQueryTool) GetDefinition() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "relation_query",
			Description: "查询实体之间的关系，如人物与组织的关系、产品与技术的关系、概念之间的依赖关系等",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"entity1": map[string]interface{}{
						"type":        "string",
						"description": "第一个实体名称",
					},
					"entity2": map[string]interface{}{
						"type":        "string",
						"description": "第二个实体名称（可选，不提供则查询 entity1 的所有关系）",
					},
					"relation_types": map[string]interface{}{
						"type":        "array",
						"description": "关系类型，如 works_for, uses, depends_on, related_to",
						"items": map[string]interface{}{
							"type": "string",
						},
					},
				},
				"required": []string{"entity1"},
			},
		},
	}
}

func (t *RelationQueryTool) EstimateConfidence(query string, context []llm.Message) float64 {
	q := strings.ToLower(query)

	highConfKeywords := []string{
		"关系", "关联", "联系", "相关",
		"之间", "与", "和",
		"属于", "隶属", "依赖", "使用",
		"有什么关系", "如何关联", "怎么联系",
	}

	for _, kw := range highConfKeywords {
		if strings.Contains(q, kw) {
			return 0.85
		}
	}

	// 如果包含"A 和 B"的模式
	if strings.Contains(q, "和") || strings.Contains(q, "与") {
		return 0.7
	}

	return 0.3
}

type relationQueryArgs struct {
	Entity1       string   `json:"entity1"`
	Entity2       string   `json:"entity2"`
	RelationTypes []string `json:"relation_types"`
}

func (t *RelationQueryTool) Execute(ctx context.Context, args json.RawMessage, deps *ToolDependencies) (*ToolResult, error) {
	var params relationQueryArgs
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if strings.TrimSpace(params.Entity1) == "" {
		return nil, fmt.Errorf("entity1 cannot be empty")
	}

	// 构建查询语句
	var searchQuery string
	if params.Entity2 != "" {
		searchQuery = fmt.Sprintf("%s %s 关系", params.Entity1, params.Entity2)
	} else {
		searchQuery = params.Entity1
	}

	// 检索相关文档
	results, err := deps.SearchService.HybridSearch(ctx, searchQuery, 8, deps.User)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		return &ToolResult{
			Success:     true,
			Content:     `{"ok": true, "relations": [], "total": 0}`,
			DisplayName: "关系查询",
			Message:     "未找到相关关系信息",
		}, nil
	}

	// 构建文档内容
	var contentBuilder strings.Builder
	for i, r := range results {
		contentBuilder.WriteString(fmt.Sprintf("\n[文档 %d: %s]\n%s\n", i+1, r.FileName, r.TextContent))
	}

	// 构建关系提取提示词
	relationPrompt := buildRelationPrompt(params, contentBuilder.String())

	messages := []llm.Message{
		{Role: "system", Content: "你是专业的关系提取助手，擅长从文本中识别实体之间的关系。"},
		{Role: "user", Content: relationPrompt},
	}

	response, err := deps.LLMClient.ChatWithTools(ctx, messages, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("relation extraction failed: %w", err)
	}

	// 解析关系结果
	relationContent := strings.TrimSpace(response.Content)
	relationContent = strings.TrimPrefix(relationContent, "```json")
	relationContent = strings.TrimPrefix(relationContent, "```")
	relationContent = strings.TrimSuffix(relationContent, "```")
	relationContent = strings.TrimSpace(relationContent)

	// 验证 JSON 格式
	var relationResult map[string]interface{}
	if err := json.Unmarshal([]byte(relationContent), &relationResult); err != nil {
		// 如果解析失败，构造一个简单的结果
		relationResult = map[string]interface{}{
			"ok":        true,
			"relations": []interface{}{},
			"total":     0,
			"note":      "关系提取格式解析失败",
		}
		marshaled, _ := json.Marshal(relationResult)
		relationContent = string(marshaled)
	} else {
		// 添加元数据
		if relations, ok := relationResult["relations"].([]interface{}); ok {
			relationResult["total"] = len(relations)
		}
		relationResult["ok"] = true
		marshaled, _ := json.Marshal(relationResult)
		relationContent = string(marshaled)
	}

	relationCount := 0
	if relations, ok := relationResult["relations"].([]interface{}); ok {
		relationCount = len(relations)
	}

	return &ToolResult{
		Success:     true,
		Content:     relationContent,
		Sources:     buildSourceItems(results),
		DisplayName: "关系查询",
		Message:     fmt.Sprintf("已找到 %d 个关系", relationCount),
		Metadata: map[string]interface{}{
			"entity1":        params.Entity1,
			"entity2":        params.Entity2,
			"relation_count": relationCount,
			"document_count": len(results),
		},
	}, nil
}

func buildRelationPrompt(params relationQueryArgs, content string) string {
	if params.Entity2 != "" {
		return fmt.Sprintf(`请从以下文档中提取 "%s" 和 "%s" 之间的关系，返回 JSON 格式。

文档内容：
%s

返回格式：
{
  "relations": [
    {
      "entity1": "%s",
      "entity2": "%s",
      "relation_type": "关系类型",
      "description": "关系描述",
      "evidence": "支持证据（原文片段）"
    }
  ]
}

只返回 JSON，不要其他解释。`, params.Entity1, params.Entity2, content, params.Entity1, params.Entity2)
	}

	return fmt.Sprintf(`请从以下文档中提取与 "%s" 相关的所有关系，返回 JSON 格式。

文档内容：
%s

返回格式：
{
  "relations": [
    {
      "entity1": "%s",
      "entity2": "相关实体名称",
      "relation_type": "关系类型",
      "description": "关系描述",
      "evidence": "支持证据（原文片段）"
    }
  ]
}

只返回 JSON，不要其他解释。`, params.Entity1, content, params.Entity1)
}
