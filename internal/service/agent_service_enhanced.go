package service

import (
	"WeaveKnow/internal/model"
	"WeaveKnow/internal/repository"
	"WeaveKnow/pkg/llm"
	"WeaveKnow/pkg/log"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// EnhancedAgentService 增强版 Agent 服务，支持多工具和并行执行。
type EnhancedAgentService struct {
	searchService    SearchService
	llmClient        llm.Client
	conversationRepo repository.ConversationRepository
	metricsService   MetricsService
	toolRegistry     *ToolRegistry

	maxIterations     int
	defaultTopK       int
	toolTimeout       time.Duration
	toolContextBudget int
	defaultGenParams  *llm.GenerationParams
	enableParallel    bool
}

// NewEnhancedAgentService 创建增强版 Agent 服务。
func NewEnhancedAgentService(
	searchService SearchService,
	llmClient llm.Client,
	conversationRepo repository.ConversationRepository,
	metricsService MetricsService,
	opts AgentOptions,
) AgentService {
	maxIterations := opts.MaxIterations
	if maxIterations <= 0 {
		maxIterations = 4
	}
	if maxIterations > maxAllowedAgentIterations {
		maxIterations = maxAllowedAgentIterations
	}

	defaultTopK := opts.DefaultTopK
	if defaultTopK < minToolTopK || defaultTopK > maxToolTopK {
		defaultTopK = 5
	}

	toolTimeout := opts.ToolTimeout
	if toolTimeout <= 0 {
		toolTimeout = 8 * time.Second
	}

	toolContextBudget := opts.ToolContextBudgetTokens
	if toolContextBudget <= 0 {
		toolContextBudget = 1200
	}

	// 创建工具注册表并注册所有工具
	registry := NewToolRegistry()
	registry.Register("knowledge_search", NewKnowledgeSearchTool(defaultTopK))
	registry.Register("document_summary", NewDocumentSummaryTool())
	registry.Register("entity_extraction", NewEntityExtractionTool())
	registry.Register("relation_query", NewRelationQueryTool())

	return &EnhancedAgentService{
		searchService:     searchService,
		llmClient:         llmClient,
		conversationRepo:  conversationRepo,
		metricsService:    metricsService,
		toolRegistry:      registry,
		maxIterations:     maxIterations,
		defaultTopK:       defaultTopK,
		toolTimeout:       toolTimeout,
		toolContextBudget: toolContextBudget,
		defaultGenParams:  buildGenerationParams(),
		enableParallel:    true, // 默认启用并行执行
	}
}

func (s *EnhancedAgentService) Run(ctx context.Context, query string, user *model.User, ws llm.MessageWriter, shouldStop func() bool) error {
	if shouldStop != nil && shouldStop() {
		return nil
	}

	if err := sendProgressEvent(ws, "planning", "正在分析问题..."); err != nil {
		log.Warnf("[EnhancedAgentService] 发送 planning 进度失败: %v", err)
	}

	// 1. 加载历史对话
	history, err := s.loadHistory(ctx, user.ID)
	if err != nil {
		log.Errorf("[EnhancedAgentService] 加载历史失败: %v", err)
		history = []model.ChatMessage{}
	}

	// 2. 组装 messages
	messages := make([]llm.Message, 0, len(history)+2+s.maxIterations*4)
	messages = append(messages, llm.Message{Role: "system", Content: s.buildEnhancedSystemPrompt()})
	for _, h := range history {
		messages = append(messages, llm.Message{Role: h.Role, Content: h.Content})
	}
	messages = append(messages, llm.Message{Role: "user", Content: query})

	// 3. 获取所有工具，让 AI 自主决策使用哪些
	llmMessages := convertToLLMMessages(messages)
	allTools := s.toolRegistry.GetAllDefinitions()

	log.Infof("[EnhancedAgentService] 提供 %d 个工具供 AI 选择", len(allTools))

	// 4. Agent 循环
	for i := 0; i < s.maxIterations; i++ {
		if shouldStop != nil && shouldStop() {
			return nil
		}

		if i > 0 {
			if err := sendProgressEvent(ws, "planning", "正在整理检索结果..."); err != nil {
				log.Warnf("[EnhancedAgentService] 发送二次 planning 进度失败: %v", err)
			}
		}

		result, err := s.llmClient.ChatWithTools(ctx, llmMessages, allTools, s.defaultGenParams)
		if err != nil {
			return fmt.Errorf("agent planning failed: %w", err)
		}

		if len(result.ToolCalls) == 0 {
			break
		}

		// 添加 assistant 消息
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   "",
			ToolCalls: result.ToolCalls,
		})
		llmMessages = append(llmMessages, llm.Message{
			Role:      "assistant",
			Content:   "",
			ToolCalls: result.ToolCalls,
		})

		// 5. 执行工具调用（支持并行）
		toolResults := s.executeToolCalls(ctx, result.ToolCalls, user, ws)

		// 6. 将工具结果添加到上下文
		for _, call := range result.ToolCalls {
			toolResult, ok := toolResults[call.ID]
			if !ok {
				continue
			}

			toolMsg := llm.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    toolResult.Content,
			}
			messages = append(messages, toolMsg)
			llmMessages = append(llmMessages, toolMsg)
		}
	}

	// 7. 最终流式回答
	messages = append(messages, llm.Message{
		Role: "user",
		Content: strings.TrimSpace(`
现在只能直接输出给用户看的最终答案。

严格禁止输出：
- <think> 或任何思考内容
- tool_call / invoke / parameter 等工具标签
- "让我重新检索"等过程性描述

如果证据不足，直接回答"知识库中暂无相关信息"。
`),
	})
	llmMessages = append(llmMessages, llm.Message{
		Role: "user",
		Content: strings.TrimSpace(`
现在只能直接输出给用户看的最终答案。

严格禁止输出：
- <think> 或任何思考内容
- tool_call / invoke / parameter 等工具标签
- "让我重新检索"等过程性描述

如果证据不足，直接回答"知识库中暂无相关信息"。
`),
	})

	if err := sendProgressEvent(ws, "answering", "正在生成答案..."); err != nil {
		log.Warnf("[EnhancedAgentService] 发送 answering 进度失败: %v", err)
	}

	interceptor := &wsWriterInterceptor{
		conn:       ws,
		shouldStop: shouldStop,
	}

	if err := s.llmClient.StreamChatMessages(ctx, llmMessages, s.defaultGenParams, interceptor); err != nil {
		return err
	}

	if err := sendCompletion(ws); err != nil {
		log.Warnf("[EnhancedAgentService] 发送 completion 通知失败: %v", err)
	}

	// 8. 持久化对话
	answer := interceptor.Answer()
	if answer != "" {
		if err := s.addMessageToConversation(context.Background(), user.ID, query, answer); err != nil {
			log.Errorf("[EnhancedAgentService] 保存对话历史失败: %v", err)
		}
	}

	return nil
}

// executeToolCalls 执行工具调用，支持并行和串行模式。
func (s *EnhancedAgentService) executeToolCalls(
	ctx context.Context,
	calls []llm.ToolCall,
	user *model.User,
	ws llm.MessageWriter,
) map[string]*ToolResult {
	deps := &ToolDependencies{
		SearchService: s.searchService,
		LLMClient:     s.llmClient,
		User:          user,
		Timeout:       s.toolTimeout,
	}

	// 判断是否可以并行执行
	canParallel := s.enableParallel && len(calls) > 1 && s.areToolCallsIndependent(calls)

	if canParallel {
		log.Infof("[EnhancedAgentService] 并行执行 %d 个工具调用", len(calls))
		return s.executeParallel(ctx, calls, deps, ws)
	}

	log.Infof("[EnhancedAgentService] 串行执行 %d 个工具调用", len(calls))
	return s.executeSequential(ctx, calls, deps, ws)
}

// executeParallel 并行执行多个工具调用。
func (s *EnhancedAgentService) executeParallel(
	ctx context.Context,
	calls []llm.ToolCall,
	deps *ToolDependencies,
	ws llm.MessageWriter,
) map[string]*ToolResult {
	results := make(map[string]*ToolResult)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, call := range calls {
		wg.Add(1)
		go func(c llm.ToolCall) {
			defer wg.Done()

			// 发送工具调用事件
			s.sendToolCallEvent(ws, c)

			// 执行工具
			executor, ok := s.toolRegistry.Get(c.Function.Name)
			if !ok {
				mu.Lock()
				results[c.ID] = &ToolResult{
					Success: false,
					Content: fmt.Sprintf(`{"ok": false, "error": "unknown tool: %s"}`, c.Function.Name),
				}
				mu.Unlock()
				return
			}

			result, err := executor.Execute(ctx, json.RawMessage(c.Function.Arguments), deps)
			if err != nil {
				log.Warnf("[EnhancedAgentService] 工具执行失败: tool=%s, err=%v", c.Function.Name, err)
				result = &ToolResult{
					Success: false,
					Content: fmt.Sprintf(`{"ok": false, "error": "%s"}`, err.Error()),
				}
			}

			// 发送工具结果事件
			s.sendToolResultEvent(ws, c, result)

			mu.Lock()
			results[c.ID] = result
			mu.Unlock()
		}(call)
	}

	wg.Wait()
	return results
}

// executeSequential 串行执行工具调用。
func (s *EnhancedAgentService) executeSequential(
	ctx context.Context,
	calls []llm.ToolCall,
	deps *ToolDependencies,
	ws llm.MessageWriter,
) map[string]*ToolResult {
	results := make(map[string]*ToolResult)

	for _, call := range calls {
		s.sendToolCallEvent(ws, call)

		executor, ok := s.toolRegistry.Get(call.Function.Name)
		if !ok {
			results[call.ID] = &ToolResult{
				Success: false,
				Content: fmt.Sprintf(`{"ok": false, "error": "unknown tool: %s"}`, call.Function.Name),
			}
			continue
		}

		result, err := executor.Execute(ctx, json.RawMessage(call.Function.Arguments), deps)
		if err != nil {
			log.Warnf("[EnhancedAgentService] 工具执行失败: tool=%s, err=%v", call.Function.Name, err)
			result = &ToolResult{
				Success: false,
				Content: fmt.Sprintf(`{"ok": false, "error": "%s"}`, err.Error()),
			}
		}

		s.sendToolResultEvent(ws, call, result)
		results[call.ID] = result
	}

	return results
}

// areToolCallsIndependent 判断工具调用是否相互独立（可并行）。
func (s *EnhancedAgentService) areToolCallsIndependent(calls []llm.ToolCall) bool {
	// 简单策略：如果所有工具都是只读操作，则可以并行
	// 更复杂的策略可以分析工具之间的依赖关系
	for _, call := range calls {
		// 目前所有工具都是只读的，可以并行
		if call.Function.Name == "" {
			return false
		}
	}
	return true
}

func (s *EnhancedAgentService) sendToolCallEvent(ws llm.MessageWriter, call llm.ToolCall) {
	executor, ok := s.toolRegistry.Get(call.Function.Name)
	if !ok {
		return
	}

	var args map[string]interface{}
	json.Unmarshal([]byte(call.Function.Arguments), &args)

	query := ""
	if q, ok := args["query"].(string); ok {
		query = q
	}

	event := toolCallEvent{
		Tool:        call.Function.Name,
		DisplayName: call.Function.Name,
		Message:     fmt.Sprintf("正在调用工具：%s", call.Function.Name),
		Query:       query,
	}

	// 获取更友好的显示名称
	def := executor.GetDefinition()
	if strings.Contains(def.Function.Description, "知识库") {
		event.DisplayName = "知识库检索"
	} else if strings.Contains(def.Function.Description, "摘要") {
		event.DisplayName = "文档摘要"
	} else if strings.Contains(def.Function.Description, "实体") {
		event.DisplayName = "实体提取"
	} else if strings.Contains(def.Function.Description, "关系") {
		event.DisplayName = "关系查询"
	}

	if err := sendToolCallEvent(ws, event); err != nil {
		log.Warnf("[EnhancedAgentService] 发送 tool_call 事件失败: %v", err)
	}
}

func (s *EnhancedAgentService) sendToolResultEvent(ws llm.MessageWriter, call llm.ToolCall, result *ToolResult) {
	if result.Sources != nil && len(result.Sources) > 0 {
		if err := sendSources(ws, result.Sources); err != nil {
			log.Warnf("[EnhancedAgentService] 发送 sources 失败: %v", err)
		}
	}

	event := toolResultEvent{
		Tool:        call.Function.Name,
		DisplayName: result.DisplayName,
		Message:     result.Message,
		Success:     result.Success,
	}

	if result.Metadata != nil {
		if count, ok := result.Metadata["result_count"].(int); ok {
			event.ResultCount = count
		}
		if total, ok := result.Metadata["total"].(int); ok {
			event.TotalCount = total
		}
	}

	if err := sendToolResultEvent(ws, event); err != nil {
		log.Warnf("[EnhancedAgentService] 发送 tool_result 事件失败: %v", err)
	}
}

func (s *EnhancedAgentService) loadHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error) {
	convID, err := s.conversationRepo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.conversationRepo.GetConversationHistory(ctx, convID)
}

func (s *EnhancedAgentService) addMessageToConversation(ctx context.Context, userID uint, question, answer string) error {
	conversationID, err := s.conversationRepo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get or create conversation ID: %w", err)
	}

	history, err := s.conversationRepo.GetConversationHistory(ctx, conversationID)
	if err != nil {
		return fmt.Errorf("failed to get conversation history: %w", err)
	}

	now := time.Now()
	history = append(history,
		model.ChatMessage{Role: "user", Content: question, Timestamp: now},
		model.ChatMessage{Role: "assistant", Content: answer, Timestamp: now},
	)
	return s.conversationRepo.UpdateConversationHistory(ctx, conversationID, history)
}

func (s *EnhancedAgentService) buildEnhancedSystemPrompt() string {
	return strings.TrimSpace(`
你是 WeaveKnow 知识库智能体。你拥有多种工具来帮助用户解答问题。

可用工具：
1. knowledge_search - 在知识库中检索相关文档
2. document_summary - 生成文档摘要和要点
3. entity_extraction - 提取关键实体（人名、组织、产品、概念等）
4. relation_query - 查询实体之间的关系

工作流程：
1. 分析用户问题，判断需要使用哪些工具
2. 可以并行调用多个独立的工具以提高效率
3. 基于工具返回的结果进行推理和回答
4. 如果信息不足，可以变换关键词多次检索

关键约束：
- 必须基于工具返回的事实进行回答，禁止凭空捏造
- 在最终回答时使用 [1][2] 等编号标注引用来源
- 如果尝试了所有相关查询仍无可用证据，直接回答"知识库中暂无相关信息"
- 不要输出内部思考过程、工具调用标签或调试信息
- 最终回答必须直接面向用户，清晰简洁
`)
}

func convertToLLMMessages(messages []llm.Message) []llm.Message {
	return messages
}
