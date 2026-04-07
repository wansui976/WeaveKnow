// Package service 包含了应用的业务逻辑层。
package service

import (
	"WeaveKnow/internal/config"
	"WeaveKnow/internal/model"
	"WeaveKnow/internal/repository"
	"WeaveKnow/pkg/llm"
	"WeaveKnow/pkg/log"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	// maxAllowedAgentIterations 防止模型陷入多轮工具调用死循环。
	maxAllowedAgentIterations = 6
	// 工具层检索条数上下限，避免过小导致信息不足、过大导致上下文膨胀。
	minToolTopK = 1
	maxToolTopK = 20
)

// AgentService 定义 Agent 模式执行接口。
type AgentService interface {
	Run(ctx context.Context, query string, user *model.User, ws llm.MessageWriter, shouldStop func() bool) error
}

// AgentOptions 控制 Agent 循环与工具调用行为。
type AgentOptions struct {
	// MaxIterations 控制单次问答中"规划-调用工具"循环次数。
	MaxIterations int
	// DefaultTopK 是工具调用未显式给出 top_k 时的默认值。
	DefaultTopK int
	// ToolTimeout 限制单次工具调用时长，避免慢检索阻塞整体响应。
	ToolTimeout time.Duration
	// ToolContextBudgetTokens 限制单次工具结果注入的预算（近似 token 数）。
	ToolContextBudgetTokens int
	// HistoryMaxTokens 限制历史消息注入的 token 预算，0 使用默认值 3000。
	HistoryMaxTokens int
}

type agentService struct {
	// 依赖服务
	searchService    SearchService
	llmClient        llm.Client
	conversationRepo repository.ConversationRepository
	metricsService   MetricsService
	memoryService    MemoryService

	// 运行参数（构造时归一化，运行时只读）
	maxIterations     int
	defaultTopK       int
	toolTimeout       time.Duration
	toolContextBudget int
	historyMaxTokens  int
	defaultGenParams  *llm.GenerationParams
	tools             []llm.Tool
}

type toolExecutionMeta struct {
	Tool        string
	DisplayName string
	Query       string
	TopK        int
	Success     bool
	ResultCount int
	TotalCount  int
	Sources     []sourceItem
	Message     string
}

type toolCallPreview struct {
	Tool        string
	DisplayName string
	Query       string
	TopK        int
	Message     string
}

// NewAgentService 创建最小可用 Agent 服务。
func NewAgentService(
	searchService SearchService,
	llmClient llm.Client,
	conversationRepo repository.ConversationRepository,
	metricsService MetricsService,
	memoryService MemoryService,
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

	historyMaxTokens := opts.HistoryMaxTokens
	if historyMaxTokens <= 0 {
		historyMaxTokens = 3000
	}

	return &agentService{
		searchService:     searchService,
		llmClient:         llmClient,
		conversationRepo:  conversationRepo,
		metricsService:    metricsService,
		memoryService:     memoryService,
		maxIterations:     maxIterations,
		defaultTopK:       defaultTopK,
		toolTimeout:       toolTimeout,
		toolContextBudget: toolContextBudget,
		historyMaxTokens:  historyMaxTokens,
		defaultGenParams:  buildGenerationParams(),
		tools:             buildDefaultAgentTools(defaultTopK),
	}
}

func (s *agentService) Run(ctx context.Context, query string, user *model.User, ws llm.MessageWriter, shouldStop func() bool) error {
	// 0. 在真正处理前先检查是否被上层中断。
	if shouldStop != nil && shouldStop() {
		return nil
	}

	if err := sendProgressEvent(ws, "planning", "正在分析问题..."); err != nil {
		log.Warnf("[AgentService] 发送 planning 进度失败: %v", err)
	}

	// 1. 加载历史对话，失败时降级为空历史，保证当前轮可继续执行。
	history, err := s.loadHistory(ctx, user.ID)
	if err != nil {
		log.Errorf("[AgentService] 加载历史失败: %v", err)
		history = []model.ChatMessage{}
	}
	// 按 token 预算截断历史，防止超出上下文窗口。
	history = truncateHistoryMessages(history, s.historyMaxTokens)

	// 1.5 检索用户记忆，注入 system prompt。
	memoryText := s.buildMemoryContext(ctx, user.ID, query)

	// 2. 组装 messages：system + history + 当前用户问题。
	messages := make([]llm.Message, 0, len(history)+2+s.maxIterations*2)
	messages = append(messages, llm.Message{Role: "system", Content: s.buildAgentSystemPrompt(memoryText)})
	for _, h := range history {
		messages = append(messages, llm.Message{Role: h.Role, Content: h.Content})
	}
	messages = append(messages, llm.Message{Role: "user", Content: query})

	// 4. Agent 循环：模型规划 -> 调用工具 -> 回填工具输出。
	for i := 0; i < s.maxIterations; i++ {
		if shouldStop != nil && shouldStop() {
			return nil
		}

		if i > 0 {
			if err := sendProgressEvent(ws, "planning", "正在整理检索结果..."); err != nil {
				log.Warnf("[AgentService] 发送二次 planning 进度失败: %v", err)
			}
		}

		result, err := s.llmClient.ChatWithTools(ctx, messages, s.tools, s.defaultGenParams)
		if err != nil {
			return fmt.Errorf("agent planning failed: %w", err)
		}
		if len(result.ToolCalls) == 0 {
			// 没有工具调用时，说明模型已完成规划，可进入最终回答阶段。
			break
		}

		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   "",
			ToolCalls: result.ToolCalls,
		})

		for _, call := range result.ToolCalls {
			preview := s.previewToolCall(call)
			if err := sendToolCallEvent(ws, toolCallEvent{
				Tool:        preview.Tool,
				DisplayName: preview.DisplayName,
				Message:     preview.Message,
				Query:       preview.Query,
				TopK:        preview.TopK,
			}); err != nil {
				log.Warnf("[AgentService] 发送 tool_call 事件失败: %v", err)
			}
			if preview.Tool == "knowledge_search" {
				if err := sendProgressEvent(ws, "retrieving", "正在检索知识库..."); err != nil {
					log.Warnf("[AgentService] 发送 retrieving 进度失败: %v", err)
				}
			}

			toolOutput, meta, execErr := s.executeTool(ctx, user, call, messages)
			if execErr != nil {
				log.Warnf("[AgentService] 工具调用失败: tool=%s, err=%v", call.Function.Name, execErr)
				if err := sendToolResultEvent(ws, toolResultEvent{
					Tool:        preview.Tool,
					DisplayName: preview.DisplayName,
					Message:     fmt.Sprintf("%s失败，请稍后重试", preview.DisplayName),
					Success:     false,
				}); err != nil {
					log.Warnf("[AgentService] 发送 tool_result 失败事件失败: %v", err)
				}
				toolOutput = buildToolErrorResult(call.Function.Name, execErr)
			} else if meta != nil {
				if len(meta.Sources) > 0 {
					if err := sendSources(ws, meta.Sources); err != nil {
						log.Warnf("[AgentService] 发送 sources 失败: %v", err)
					}
				}
				if err := sendToolResultEvent(ws, toolResultEvent{
					Tool:        meta.Tool,
					DisplayName: meta.DisplayName,
					Message:     meta.Message,
					Success:     meta.Success,
					ResultCount: meta.ResultCount,
					TotalCount:  meta.TotalCount,
				}); err != nil {
					log.Warnf("[AgentService] 发送 tool_result 事件失败: %v", err)
				}
			}
			// tool 角色消息按 OpenAI tool-calling 规范回填到上下文。
			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    toolOutput,
			})
		}
	}

	// 5. 最终流式回答阶段：基于完整上下文（含工具结果）输出答案。
	// 不再注入额外 user 消息，约束已内置于 system prompt；
	// 同时不传 tools 参数，防止模型在最终阶段再次发起工具调用。
	if err := sendProgressEvent(ws, "answering", "正在生成答案..."); err != nil {
		log.Warnf("[AgentService] 发送 answering 进度失败: %v", err)
	}

	interceptor := &wsWriterInterceptor{
		conn:       ws,
		shouldStop: shouldStop,
	}

	if err := s.llmClient.StreamChatMessages(ctx, messages, s.defaultGenParams, interceptor); err != nil {
		return err
	}
	if err := sendCompletion(ws); err != nil {
		log.Warnf("[AgentService] 发送 completion 通知失败: %v", err)
	}

	// 6. 持久化本轮对话（后台上下文，避免请求取消导致历史丢失）。
	answer := interceptor.Answer()
	if answer != "" {
		if err := s.addMessageToConversation(context.Background(), user.ID, query, answer); err != nil {
			log.Errorf("[AgentService] 保存对话历史失败: %v", err)
		}
	}
	return nil
}

// executeTool 根据工具名分发执行逻辑，并返回给模型可消费的字符串结果。
func (s *agentService) executeTool(ctx context.Context, user *model.User, call llm.ToolCall, messages []llm.Message) (string, *toolExecutionMeta, error) {
	switch call.Function.Name {
	case "knowledge_search":
		type toolArgs struct {
			Query string `json:"query"`
			TopK  int    `json:"top_k"`
		}
		var args toolArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			if s.metricsService != nil {
				s.metricsService.RecordToolCall(false)
			}
			return "", nil, fmt.Errorf("tool arguments parse failed: %w", err)
		}
		if strings.TrimSpace(args.Query) == "" {
			if s.metricsService != nil {
				s.metricsService.RecordToolCall(false)
			}
			return "", nil, fmt.Errorf("tool arguments invalid: query is empty")
		}
		if args.TopK < minToolTopK || args.TopK > maxToolTopK {
			args.TopK = s.defaultTopK
		}

		// 单次检索超时保护，避免工具调用长时间占用 agent 轮次。
		toolCtx, cancel := context.WithTimeout(ctx, s.toolTimeout)
		defer cancel()

		results, err := s.searchService.HybridSearch(toolCtx, args.Query, args.TopK, user)
		if err != nil {
			if s.metricsService != nil {
				s.metricsService.RecordToolCall(false)
			}
			return "", nil, fmt.Errorf("knowledge_search failed: %w", err)
		}
		if s.metricsService != nil {
			s.metricsService.RecordToolCall(true)
		}
		selectedCount := len(results)
		meta := &toolExecutionMeta{
			Tool:        "knowledge_search",
			DisplayName: "知识库检索",
			Query:       args.Query,
			TopK:        args.TopK,
			Success:     true,
			ResultCount: selectedCount,
			TotalCount:  len(results),
			Sources:     buildSourceItems(results),
			Message:     fmt.Sprintf("已检索到 %d 条相关内容", selectedCount),
		}
		if selectedCount == 0 {
			meta.Message = "未检索到相关内容"
		}
		return s.formatKnowledgeSearchResult(results, messages), meta, nil
	default:
		if s.metricsService != nil {
			s.metricsService.RecordToolCall(false)
		}
		return "", nil, fmt.Errorf("unsupported tool: %s", call.Function.Name)
	}
}

// formatKnowledgeSearchResult 将检索结果序列化为 JSON 字符串，供模型继续推理/引用。
// 支持动态预算：按 score 从高到低选取结果，并按 token 预算裁剪 snippet 长度和条数。
func (s *agentService) formatKnowledgeSearchResult(results []model.SearchResponseDTO, messages []llm.Message) string {
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
		Tool         string `json:"tool"`
		BudgetTokens int    `json:"budget_tokens"`
		Selected     int    `json:"selected"`
		Total        int    `json:"total"`
		Results      []item `json:"results"`
	}

	budget := s.resolveToolContextBudgetTokens(messages)
	if len(results) == 0 {
		b, _ := json.Marshal(payload{
			OK:           true,
			Tool:         "knowledge_search",
			BudgetTokens: budget,
			Selected:     0,
			Total:        0,
			Results:      []item{},
		})
		return string(b)
	}

	// 复制并按 score 降序排序，保证预算优先给更高价值结果。
	sorted := make([]model.SearchResponseDTO, 0, len(results))
	sorted = append(sorted, results...)
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
		remainingBudget := budget - usedTokens
		if remainingBudget <= docOverheadTokens+minSnippetTokens {
			break
		}

		remainingDocs := len(sorted) - i
		targetSnippetTokens := (remainingBudget / remainingDocs) - docOverheadTokens
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

	b, _ := json.Marshal(payload{
		OK:           true,
		Tool:         "knowledge_search",
		BudgetTokens: budget,
		Selected:     len(selected),
		Total:        len(results),
		Results:      selected,
	})
	return string(b)
}

// loadHistory 获取（或创建）会话并加载历史消息。
func (s *agentService) loadHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error) {
	convID, err := s.conversationRepo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.conversationRepo.GetConversationHistory(ctx, convID)
}

// addMessageToConversation 将当前轮问答原子追加到对话历史。
func (s *agentService) addMessageToConversation(ctx context.Context, userID uint, question, answer string) error {
	conversationID, err := s.conversationRepo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get or create conversation ID: %w", err)
	}
	now := time.Now()
	return s.conversationRepo.AppendMessages(ctx, conversationID, []model.ChatMessage{
		{Role: "user", Content: question, Timestamp: now},
		{Role: "assistant", Content: answer, Timestamp: now},
	})
}

// buildAgentSystemPrompt 定义 Agent 行为边界，注入记忆上下文。
func (s *agentService) buildAgentSystemPrompt(memoryText string) string {
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(`
你是 WeaveKnow 知识库智能体。你的核心任务是通过检索外部知识库来解答用户问题。

为了保证回答的准确性，你必须遵循"先分析、再检索、后回答"的流程，但分析过程只允许在内部完成：
1. 先理解用户问题，判断是否需要调用工具。
2. 需要证据时，优先调用 knowledge_search 检索知识库。
3. 基于工具返回的事实继续分析，并给出最终答案。

【关键约束】
- 必须基于工具返回的事实进行回答，禁止凭空捏造。
- 如果单次检索信息不完整，允许你变换关键词进行多次检索。
- 在最终回答时，必须使用 [1][2] 等编号标注引用来源，编号需与工具返回的片段索引严格一致。
- 如果尝试了所有相关查询仍无可用证据，请直接回答"知识库中暂无相关信息"。
- 严格禁止输出 <think>、Thought、Action、Observation、tool_call 或任何内部调试标签。
- 严格禁止输出"让我重新检索""让我继续检索"等过程性描述。
- 最终回答必须直接面向用户，只输出结论、依据和必要的引用编号。`))
	if memoryText != "" {
		sb.WriteString("\n\n")
		sb.WriteString(memoryText)
	}
	return sb.String()
}

// buildMemoryContext 检索用户记忆并返回可注入 prompt 的文本。
func (s *agentService) buildMemoryContext(ctx context.Context, userID uint, query string) string {
	if s.memoryService == nil {
		return ""
	}
	memories, err := s.memoryService.Search(ctx, userID, SearchMemoryInput{
		Workspace:  "default",
		Categories: []string{"preferences", "project", "entities"},
		Query:      query,
		Limit:      5,
	})
	if err != nil {
		log.Warnf("[AgentService] 检索用户记忆失败: %v", err)
		return ""
	}
	return s.memoryService.BuildContext(memories)
}

func (s *agentService) previewToolCall(call llm.ToolCall) toolCallPreview {
	preview := toolCallPreview{
		Tool:        call.Function.Name,
		DisplayName: call.Function.Name,
		Message:     fmt.Sprintf("正在调用工具：%s", call.Function.Name),
	}

	switch call.Function.Name {
	case "knowledge_search":
		preview.DisplayName = "知识库检索"
		preview.Message = "正在检索知识库..."

		var args struct {
			Query string `json:"query"`
			TopK  int    `json:"top_k"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err == nil {
			preview.Query = strings.TrimSpace(args.Query)
			if args.TopK >= minToolTopK && args.TopK <= maxToolTopK {
				preview.TopK = args.TopK
			} else {
				preview.TopK = s.defaultTopK
			}
			if preview.Query != "" {
				preview.Message = fmt.Sprintf("正在检索知识库：%s", preview.Query)
			}
		}
	}

	return preview
}

// resolveToolContextBudgetTokens 估算"本轮可注入工具结果"的 token 预算。
//
// 预算思路：
// 1) 以模型上下文窗为总量（contextWindow）；
// 2) 扣除当前 messages 已占用 token；
// 3) 预留模型输出 token（避免回答被截断）；
// 4) 额外预留安全边际（至少 256 或窗口 8%）；
// 5) 对结果做上下限约束，防止工具结果过少或过多。
func (s *agentService) resolveToolContextBudgetTokens(messages []llm.Message) int {
	// 获取模型上下文窗（配置优先，其次按模型名推断）。
	contextWindow := resolveModelContextWindow()
	// 估算当前对话已占用 token（近似值，用于预算控制）。
	usedTokens := estimateMessagesTokens(messages)
	// 为最终回答保留输出空间，避免工具注入过多挤占回答长度。
	reservedOutput := resolveReservedOutputTokens(contextWindow, s.defaultGenParams)
	// 安全边际：吸收估算误差、系统消息抖动与格式化开销。
	safetyMargin := int(math.Max(256, float64(contextWindow)*0.08))

	// 初始可用预算 = 总窗 - 已用 - 预留输出 - 安全边际。
	remaining := contextWindow - usedTokens - reservedOutput - safetyMargin
	// 下限保护：预算太小时仍保证最小可用注入能力。
	if remaining < 120 {
		remaining = 120
	}

	// toolContextBudget 作为上限开关，避免一次注入过多工具结果。
	if s.toolContextBudget > 0 && remaining > s.toolContextBudget {
		remaining = s.toolContextBudget
	}
	// 不允许超过上下文窗的 1/3，防止工具结果淹没主对话。
	maxByWindow := contextWindow / 3
	if maxByWindow < 120 {
		maxByWindow = 120
	}
	if remaining > maxByWindow {
		remaining = maxByWindow
	}
	// 返回最终预算（近似 token 数）。
	return remaining
}

func estimateApproxTokens(text string) int {
	if text == "" {
		return 0
	}
	var total float64
	for _, r := range text {
		if r <= 127 {
			total += 0.25
		} else {
			total += 1.0
		}
	}
	return int(math.Ceil(total))
}

func estimateMessagesTokens(messages []llm.Message) int {
	if len(messages) == 0 {
		return 0
	}
	total := 0
	for _, msg := range messages {
		total += 6 // role / separators / JSON framing 粗略开销
		total += estimateApproxTokens(msg.Role)
		total += estimateApproxTokens(msg.Content)
		total += estimateApproxTokens(msg.ToolCallID)
		for _, tc := range msg.ToolCalls {
			total += 10
			total += estimateApproxTokens(tc.ID)
			total += estimateApproxTokens(tc.Type)
			total += estimateApproxTokens(tc.Function.Name)
			total += estimateApproxTokens(tc.Function.Arguments)
		}
	}
	return total
}

func resolveReservedOutputTokens(contextWindow int, gen *llm.GenerationParams) int {
	if gen != nil && gen.MaxTokens != nil && *gen.MaxTokens > 0 {
		return *gen.MaxTokens
	}
	reserved := contextWindow / 6
	if reserved < 512 {
		reserved = 512
	}
	if reserved > 4096 {
		reserved = 4096
	}
	return reserved
}

func resolveModelContextWindow() int {
	if config.Conf.LLM.ContextWindow > 0 {
		return config.Conf.LLM.ContextWindow
	}
	model := strings.ToLower(strings.TrimSpace(config.Conf.LLM.Model))
	switch {
	case strings.Contains(model, "claude"):
		return 200000
	case strings.Contains(model, "gpt-4"), strings.Contains(model, "gpt-5"), strings.Contains(model, "qwen"):
		return 128000
	case strings.Contains(model, "deepseek"):
		return 64000
	case strings.Contains(model, "llama"):
		return 32000
	default:
		return 32000
	}
}

// trimTextByTokenBudget 按 token 预算截断文本，优先在句子边界截断以保持语义完整。
func trimTextByTokenBudget(text string, budgetTokens int) string {
	text = strings.TrimSpace(text)
	if text == "" || budgetTokens <= 0 {
		return ""
	}
	if estimateApproxTokens(text) <= budgetTokens {
		return text
	}

	// 先按字符硬截断到预算内。
	var b strings.Builder
	used := 0.0
	limit := float64(budgetTokens - 1) // 预留 "…" 预算
	if limit < 1 {
		limit = 1
	}
	for _, r := range text {
		cost := 1.0
		if r <= 127 {
			cost = 0.25
		}
		if used+cost > limit {
			break
		}
		used += cost
		b.WriteRune(r)
	}
	raw := strings.TrimSpace(b.String())
	if raw == "" {
		return ""
	}

	// 尝试在句子边界截断，优先中文句号，其次英文句点。
	sentencePuncs := []string{"。", "！", "？", ".", "!", "?", "；", ";"}
	bestIdx := -1
	for _, p := range sentencePuncs {
		if idx := strings.LastIndex(raw, p); idx > bestIdx {
			bestIdx = idx + len(p)
		}
	}
	// 仅当句边界截断保留了至少 40% 的内容时才采用，避免过度截短。
	if bestIdx > len([]rune(raw))*2/5 {
		return raw[:bestIdx] + "…"
	}
	return raw + "…"
}

func buildToolErrorResult(toolName string, err error) string {
	type toolErrorPayload struct {
		OK    bool   `json:"ok"`
		Tool  string `json:"tool"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}

	payload := toolErrorPayload{OK: false, Tool: toolName}
	payload.Error.Code = "TOOL_EXECUTION_FAILED"
	payload.Error.Message = err.Error()
	payload.Error.Hint = "请检查参数后重试，例如更换 query 或调小 top_k。"

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "parse"):
		payload.Error.Code = "INVALID_TOOL_ARGUMENTS"
		payload.Error.Hint = "arguments 必须是合法 JSON，如 {\"query\":\"...\",\"top_k\":5}。"
	case strings.Contains(msg, "query is empty"):
		payload.Error.Code = "INVALID_QUERY"
		payload.Error.Hint = "query 不能为空，可改为具体关键词再试。"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "deadline exceeded"):
		payload.Error.Code = "TOOL_TIMEOUT"
		payload.Error.Hint = "本次检索超时，请缩短 query 或降低 top_k 再试。"
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// truncateHistoryMessages 按近似 token 预算保留最近历史，成对保留 user/assistant 消息。
// 从最新消息向前累加，超出 budget 时停止，保证注入上下文不超出窗口限制。
func truncateHistoryMessages(history []model.ChatMessage, budgetTokens int) []model.ChatMessage {
	if len(history) == 0 || budgetTokens <= 0 {
		return history
	}
	used := 0
	cutIdx := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		cost := estimateApproxTokens(history[i].Content) + 6
		if used+cost > budgetTokens {
			// 确保成对截断（不能只保留 assistant 而没有对应 user）。
			cutIdx = i + 1
			// 如果截断点正好落在 assistant 消息上，则再往后移一条，保证首条是 user。
			if cutIdx < len(history) && history[cutIdx].Role == "assistant" {
				cutIdx++
			}
			break
		}
		used += cost
		cutIdx = i
	}
	return history[cutIdx:]
}

func buildDefaultAgentTools(defaultTopK int) []llm.Tool {
	return []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "knowledge_search",
				Description: "在企业知识库中检索与问题相关的文档片段",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "用于检索的关键词或问题",
						},
						"top_k": map[string]interface{}{
							"type":        "integer",
							"description": "返回文档条数，建议 1~20",
							"default":     defaultTopK,
						},
					},
					"required": []string{"query"},
				},
			},
		},
	}
}
