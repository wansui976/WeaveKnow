// Package service 包含了应用的业务逻辑层。
package service

import (
	"WeaveKnow/internal/model"
	"WeaveKnow/pkg/llm"
	"WeaveKnow/pkg/log"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ToolRegistry 管理所有可用的 Agent 工具。
type ToolRegistry struct {
	tools map[string]ToolExecutor
	mu    sync.RWMutex
}

// ToolExecutor 定义工具执行接口。
type ToolExecutor interface {
	// Execute 执行工具并返回结果。
	Execute(ctx context.Context, args json.RawMessage, deps *ToolDependencies) (*ToolResult, error)
	// GetDefinition 返回工具的 LLM 可见定义。
	GetDefinition() llm.Tool
	// EstimateConfidence 评估工具对当前查询的适用性（0-1）。
	EstimateConfidence(query string, context []llm.Message) float64
}

// ToolDependencies 封装工具执行所需的依赖。
type ToolDependencies struct {
	SearchService SearchService
	LLMClient     llm.Client
	User          *model.User
	Timeout       time.Duration
}

// ToolResult 封装工具执行结果。
type ToolResult struct {
	Success     bool
	Content     string
	Metadata    map[string]interface{}
	Sources     []sourceItem
	DisplayName string
	Message     string
}

// NewToolRegistry 创建工具注册表。
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]ToolExecutor),
	}
}

// Register 注册一个工具。
func (r *ToolRegistry) Register(name string, executor ToolExecutor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[name] = executor
}

// Get 获取指定工具。
func (r *ToolRegistry) Get(name string) (ToolExecutor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	executor, ok := r.tools[name]
	return executor, ok
}

// GetAllDefinitions 返回所有工具的定义。
func (r *ToolRegistry) GetAllDefinitions() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	defs := make([]llm.Tool, 0, len(r.tools))
	for _, executor := range r.tools {
		defs = append(defs, executor.GetDefinition())
	}
	return defs
}

// SelectTools 根据查询和上下文智能选择最相关的工具。
func (r *ToolRegistry) SelectTools(query string, context []llm.Message, maxTools int) []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type scoredTool struct {
		tool       llm.Tool
		confidence float64
	}

	scored := make([]scoredTool, 0, len(r.tools))
	for _, executor := range r.tools {
		confidence := executor.EstimateConfidence(query, context)
		if confidence > 0.3 { // 只保留置信度 > 0.3 的工具
			scored = append(scored, scoredTool{
				tool:       executor.GetDefinition(),
				confidence: confidence,
			})
		}
	}

	// 按置信度降序排序
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].confidence > scored[j].confidence
	})

	// 返回前 N 个工具
	limit := maxTools
	if limit <= 0 || limit > len(scored) {
		limit = len(scored)
	}

	result := make([]llm.Tool, limit)
	for i := 0; i < limit; i++ {
		result[i] = scored[i].tool
	}
	return result
}

// ExecuteParallel 并行执行多个工具调用。
func (r *ToolRegistry) ExecuteParallel(ctx context.Context, calls []llm.ToolCall, deps *ToolDependencies) map[string]*ToolResult {
	results := make(map[string]*ToolResult)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, call := range calls {
		wg.Add(1)
		go func(c llm.ToolCall) {
			defer wg.Done()

			executor, ok := r.Get(c.Function.Name)
			if !ok {
				mu.Lock()
				results[c.ID] = &ToolResult{
					Success: false,
					Content: fmt.Sprintf("unknown tool: %s", c.Function.Name),
				}
				mu.Unlock()
				return
			}

			result, err := executor.Execute(ctx, json.RawMessage(c.Function.Arguments), deps)
			if err != nil {
				log.Warnf("[ToolRegistry] 工具执行失败: tool=%s, err=%v", c.Function.Name, err)
				result = &ToolResult{
					Success: false,
					Content: fmt.Sprintf("tool execution failed: %v", err),
				}
			}

			mu.Lock()
			results[c.ID] = result
			mu.Unlock()
		}(call)
	}

	wg.Wait()
	return results
}
