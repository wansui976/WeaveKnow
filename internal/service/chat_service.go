// Package service 包含了应用的业务逻辑层。
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/llm"
	"pai-smart-go/pkg/log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// 提取为包级常量，避免硬编码散落在业务逻辑中。
const (
	defaultRefStart     = "<<REF>>"
	defaultRefEnd       = "<<END>>"
	defaultNoResultText = "（本轮无检索结果）"
	// maxSnippetLen 与 Processor 的 chunkSize 对齐，尽量不截断完整语义块。
	maxSnippetLen = 1000
	// sources 事件里展示的片段长度（更短，便于前端列表展示）。
	sourceSnippetLen = 220
	// 多轮问题融合默认使用最近 3 轮（user+assistant）对话片段。
	fusionMaxHistoryTurns = 3
	// 单条历史消息参与融合时的长度上限，避免检索 query 过长。
	fusionLineMaxRunes = 120
	// 融合后的检索 query 长度上限，控制 embedding 与 ES 检索开销。
	fusionQueryMaxRunes = 480
)

// ChatService 定义了聊天操作的接口。
type ChatService interface {
	StreamResponse(ctx context.Context, query string, user *model.User, ws llm.MessageWriter, shouldStop func() bool) error
}

type chatService struct {
	searchService    SearchService
	memoryService    MemoryService
	metricsService   MetricsService
	llmClient        llm.Client
	conversationRepo repository.ConversationRepository
	agentService     AgentService

	// 在构造时一次性加载 prompt 模板，避免每次请求都从 config 读取。
	promptRules string
	// noResultText 在检索无结果时写入 REF 区域，提示模型当前轮无外部文档上下文。
	noResultText string
	// refStart / refEnd 用于包裹“检索参考片段”边界，方便模型识别可引用范围。
	refStart string
	refEnd   string

	// 默认生成参数在构造时计算一次并缓存，运行时只读不写，无需加锁。
	defaultGenParams *llm.GenerationParams
}

// NewChatService 创建一个新的 ChatService 实例。
func NewChatService(
	searchService SearchService,
	memoryService MemoryService,
	metricsService MetricsService,
	llmClient llm.Client,
	conversationRepo repository.ConversationRepository,
	agentService AgentService,
) ChatService {
	s := &chatService{
		searchService:    searchService,
		memoryService:    memoryService,
		metricsService:   metricsService,
		llmClient:        llmClient,
		conversationRepo: conversationRepo,
		agentService:     agentService,
	}

	// 初始化 prompt 模板（优先 AI 配置，回退 LLM 配置，最后用默认值）。
	s.promptRules = firstNonEmpty(config.Conf.AI.Prompt.Rules, config.Conf.LLM.Prompt.Rules)
	s.noResultText = firstNonEmpty(config.Conf.AI.Prompt.NoResultText, config.Conf.LLM.Prompt.NoResultText, defaultNoResultText)
	s.refStart = firstNonEmpty(config.Conf.AI.Prompt.RefStart, config.Conf.LLM.Prompt.RefStart, defaultRefStart)
	s.refEnd = firstNonEmpty(config.Conf.AI.Prompt.RefEnd, config.Conf.LLM.Prompt.RefEnd, defaultRefEnd)

	s.defaultGenParams = buildGenerationParams()

	return s
}

// StreamResponse 协调 RAG 流程并流式传输 LLM 响应。
func (s *chatService) StreamResponse(ctx context.Context, query string, user *model.User, ws llm.MessageWriter, shouldStop func() bool) error {
	if config.Conf.AI.Agent.Enabled && s.agentService != nil {
		return s.agentService.Run(ctx, query, user, ws, shouldStop)
	}

	if err := sendProgressEvent(ws, "planning", "正在分析问题..."); err != nil {
		log.Warnf("[ChatService] 发送 planning 进度失败: %v", err)
	}

	// 1. 先加载历史，再进行“多轮问题融合”后检索。
	history, err := s.loadHistory(ctx, user.ID)
	if err != nil {
		log.Errorf("[ChatService] 加载对话历史失败: %v", err)
		history = []model.ChatMessage{}
	}

	retrievalQuery := s.buildRetrievalQuery(query, history)
	if retrievalQuery != query {
		log.Infof("[ChatService] 检索问题融合生效: '%s' -> '%s'", query, retrievalQuery)
	}
	if s.metricsService != nil {
		s.metricsService.RecordFusion(retrievalQuery != query)
	}

	if err := sendProgressEvent(ws, "retrieving", "正在检索知识库..."); err != nil {
		log.Warnf("[ChatService] 发送 retrieving 进度失败: %v", err)
	}

	results, err := s.searchService.HybridSearch(ctx, retrievalQuery, 10, user)
	if err != nil {
		return fmt.Errorf("failed to retrieve context: %w", err)
	}

	// 多轮融合无命中时，降级回原问题再查一次。
	if len(results) == 0 && retrievalQuery != query {
		log.Infof("[ChatService] 融合问题无命中，回退原问题重试: '%s'", query)
		results, err = s.searchService.HybridSearch(ctx, query, 10, user)
		if err != nil {
			return fmt.Errorf("failed to retrieve context by fallback query: %w", err)
		}
	}

	// 空结果时发送友好提示，不静默继续（用户可感知到无文档支撑）。
	if len(results) == 0 {
		log.Warnf("[ChatService] HybridSearch 返回空结果, query: %s, user: %s", query, user.Username)
		if sendErr := sendWarningChunk(ws, "未找到相关文档内容，将基于模型自身知识回答。"); sendErr != nil {
			log.Warnf("[ChatService] 发送空结果提示失败: %v", sendErr)
		}
	} else {
		// 检索命中时先推送结构化 sources，前端可直接渲染“可点击来源”。
		if sendErr := sendSources(ws, buildSourceItems(results)); sendErr != nil {
			log.Warnf("[ChatService] 发送 sources 失败: %v", sendErr)
		}
	}

	// 2. 构建上下文、system 消息与完整对话输入。
	contextText := s.buildContextText(results)
	memoryText := s.buildMemoryContext(ctx, user.ID, retrievalQuery)
	systemMsg := s.buildSystemMessage(contextText, memoryText)
	messages := s.composeMessages(systemMsg, history, query)

	if err := sendProgressEvent(ws, "answering", "正在生成答案..."); err != nil {
		log.Warnf("[ChatService] 发送 answering 进度失败: %v", err)
	}

	// 3. 初始化线程安全的答案收集器与 WebSocket 拦截器。
	// 用 sync.Mutex 保护 bytes.Buffer，防止 LLM 客户端内部并发写入时的数据竞争。
	interceptor := &wsWriterInterceptor{
		conn:       ws,
		shouldStop: shouldStop,
	}

	// 4. 调用 LLM 客户端流式输出。
	llmMsgs := make([]llm.Message, 0, len(messages))
	for _, m := range messages {
		llmMsgs = append(llmMsgs, llm.Message{Role: m.Role, Content: m.Content})
	}
	if err := s.llmClient.StreamChatMessages(ctx, llmMsgs, s.defaultGenParams, interceptor); err != nil {
		return err
	}

	// 5. 发送完成通知。
	if sendErr := sendCompletion(ws); sendErr != nil {
		// sendCompletion 的错误不再被静默忽略。
		log.Warnf("[ChatService] 发送 completion 通知失败: %v", sendErr)
	}

	// 6. 后台保存对话（使用 Background ctx，避免请求取消后历史丢失）。
	fullAnswer := interceptor.Answer()
	if len(fullAnswer) > 0 {
		if saveErr := s.addMessageToConversation(context.Background(), user.ID, query, fullAnswer); saveErr != nil {
			log.Errorf("[ChatService] 保存对话历史失败: %v", saveErr)
		}
		s.asyncUpdateMemory(user.ID, query, fullAnswer)
	}

	return nil
}

// buildContextText 将搜索结果拼装为 LLM 可读的参考文本。
func (s *chatService) buildContextText(searchResults []model.SearchResponseDTO) string {
	if len(searchResults) == 0 {
		return ""
	}
	var buf strings.Builder
	for i, r := range searchResults {
		snippet := truncateAtSentence(r.TextContent, maxSnippetLen)
		fileLabel := r.FileName
		if fileLabel == "" {
			fileLabel = "unknown"
		}
		buf.WriteString(fmt.Sprintf("[%d] (%s) %s\n", i+1, fileLabel, snippet))
	}
	return buf.String()
}

// buildSystemMessage 根据上下文文本和预加载的模板构建 system prompt。
func (s *chatService) buildSystemMessage(contextText string, memoryText string) string {
	var sys strings.Builder
	if s.promptRules != "" {
		sys.WriteString(s.promptRules)
		sys.WriteString("\n\n")
	}
	if memoryText != "" {
		sys.WriteString(memoryText)
		sys.WriteString("\n\n")
	}
	sys.WriteString(s.refStart)
	sys.WriteString("\n")
	if contextText != "" {
		sys.WriteString(contextText)
	} else {
		sys.WriteString(s.noResultText)
		sys.WriteString("\n")
	}
	sys.WriteString(s.refEnd)
	sys.WriteString("\n\n回答时若引用参考片段，请使用 [1][2] 这样的编号标注；编号必须与参考片段序号一致。")
	return sys.String()
}

func (s *chatService) buildMemoryContext(ctx context.Context, userID uint, retrievalQuery string) string {
	if s.memoryService == nil {
		return ""
	}

	memories, err := s.memoryService.Search(ctx, userID, SearchMemoryInput{
		Workspace:  "default",
		Categories: []string{"preferences", "project", "entities"},
		Query:      retrievalQuery,
		Limit:      5,
	})
	if err != nil {
		log.Warnf("[ChatService] 检索结构化记忆失败: %v", err)
		if s.metricsService != nil {
			s.metricsService.RecordMemoryHit(false)
		}
		return ""
	}
	if s.metricsService != nil {
		s.metricsService.RecordMemoryHit(len(memories) > 0)
	}
	return s.memoryService.BuildContext(memories)
}

// asyncUpdateMemory 在主回答完成后异步提炼结构化记忆并落库。
//
// 设计目的：
// - 不阻塞用户主链路（WebSocket 流式回答已结束后再后台执行）；
// - 将本轮问答中的可复用事实沉淀为 memory，供后续对话检索增强。
//
// 处理流程：
// 1) 读取并校正超时/最大条数配置；
// 2) 调用 LLM 按固定 JSON 结构提炼记忆候选；
// 3) 解析与清洗输出，按上限截断；
// 4) 逐条 Upsert 到默认 workspace。
func (s *chatService) asyncUpdateMemory(userID uint, question string, answer string) {
	// 依赖未注入时直接跳过，保证调用安全。
	if s.memoryService == nil || s.llmClient == nil {
		return
	}

	// 后台任务超时（秒）：避免提炼任务长期占用资源。
	timeoutS := config.Conf.Memory.AsyncUpdateTimeoutS
	if timeoutS <= 0 {
		timeoutS = 8
	}
	// 单轮最多写入条目数，做上下限保护，避免模型输出过多污染记忆。
	maxEntries := config.Conf.Memory.AsyncUpdateMaxEntries
	if maxEntries <= 0 {
		maxEntries = 5
	}
	if maxEntries > 8 {
		maxEntries = 8
	}

	// 使用 goroutine 异步执行，避免阻塞当前请求生命周期。
	go func() {
		// 使用独立 Background 上下文，防止上游请求取消后任务被连带取消。
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutS)*time.Second)
		defer cancel()

		// 让模型以“纯 JSON 数组”输出，降低解析复杂度与歧义。
		messages := []llm.Message{
			{
				Role: "system",
				Content: "你是记忆提炼器。请从问答中提炼可复用事实，返回 JSON 数组。" +
					"每个元素包含 category/content/keywords/confidence。" +
					"category 只能是 preferences/project/entities。" +
					"不要返回解释，不要 markdown。",
			},
			{
				Role:    "user",
				Content: fmt.Sprintf("问题：%s\n回答：%s\n最多返回 %d 条。", question, answer, maxEntries),
			},
		}

		// 这里不需要 tools 与 generation params，走一次普通 chat 即可。
		resp, err := s.llmClient.ChatWithTools(ctx, messages, nil, nil)
		if err != nil {
			log.Warnf("[ChatService] 异步记忆提炼失败: %v", err)
			return
		}

		// 解析模型输出并做字段级清洗（见 parse/sanitize 逻辑）。
		items := parseMemoryExtraction(resp.Content)
		if len(items) == 0 {
			return
		}
		// 防止极端输出过多条目，截断到配置上限。
		if len(items) > maxEntries {
			items = items[:maxEntries]
		}

		// 逐条幂等写入：内层 Upsert 会做 category 校验、hash 去重与默认值修正。
		for _, item := range items {
			_, err := s.memoryService.Upsert(ctx, userID, UpsertMemoryInput{
				Workspace:  "default",
				Category:   item.Category,
				Content:    item.Content,
				Keywords:   item.Keywords,
				Confidence: item.Confidence,
				Source:     "llm",
			})
			if err != nil {
				log.Warnf("[ChatService] 写入结构化记忆失败: %v", err)
			}
		}
	}()
}

// buildRetrievalQuery 基于最近对话做“多轮问题融合”，用于检索阶段（不会替换用户原始提问）。
func (s *chatService) buildRetrievalQuery(query string, history []model.ChatMessage) string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" || len(history) == 0 {
		return trimmed
	}
	if !isLikelyFollowupQuery(trimmed) {
		return trimmed
	}

	lines := collectRecentHistoryLines(history, fusionMaxHistoryTurns)
	if len(lines) == 0 {
		return trimmed
	}

	var fused strings.Builder
	fused.WriteString("当前问题：")
	fused.WriteString(trimmed)
	fused.WriteString("\n相关上下文：\n")
	for _, line := range lines {
		fused.WriteString(line)
		fused.WriteByte('\n')
	}

	merged := strings.TrimSpace(fused.String())
	if utf8.RuneCountInString(merged) > fusionQueryMaxRunes {
		merged = truncateRunesWithEllipsis(merged, fusionQueryMaxRunes)
	}
	return merged
}

func isLikelyFollowupQuery(query string) bool {
	q := strings.TrimSpace(strings.ToLower(query))
	if q == "" {
		return false
	}

	indicators := []string{
		"它", "这个", "那个", "这些", "那些", "其", "该",
		"上次", "前面", "前文", "刚才", "上述", "之前",
		"那它", "然后", "继续", "再", "优缺点", "区别",
	}
	for _, k := range indicators {
		if strings.Contains(q, k) {
			return true
		}
	}

	if strings.HasPrefix(q, "那") || strings.HasPrefix(q, "这") {
		return true
	}
	if utf8.RuneCountInString(q) <= 8 {
		if strings.HasSuffix(q, "吗") || strings.HasSuffix(q, "呢") ||
			strings.HasSuffix(q, "?") || strings.HasSuffix(q, "？") {
			return true
		}
	}
	return false
}

// collectRecentHistoryLines 提取最近 maxTurns 轮 user/assistant 历史，并按时间正序返回。
// 返回行格式为“用户: ...”或“助手: ...”，用于构建检索融合上下文。
func collectRecentHistoryLines(history []model.ChatMessage, maxTurns int) []string {
	// 至少保留 1 轮，避免传入非法值导致完全不取历史。
	if maxTurns <= 0 {
		maxTurns = 1
	}
	// 一轮最多两条（user + assistant）。
	maxMessages := maxTurns * 2
	// 先倒序收集（从最新往前），最后再翻转回正序。
	reversed := make([]string, 0, maxMessages)

	for i := len(history) - 1; i >= 0 && len(reversed) < maxMessages; i-- {
		m := history[i]
		// 仅保留问答角色，忽略 system/tool 等消息。
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		// 控制单行长度，避免融合 query 过长。
		content = truncateRunesWithEllipsis(content, fusionLineMaxRunes)

		role := "用户"
		if m.Role == "assistant" {
			role = "助手"
		}
		reversed = append(reversed, fmt.Sprintf("%s: %s", role, content))
	}

	if len(reversed) == 0 {
		return nil
	}
	// reversed 当前为“新 -> 旧”，翻转为“旧 -> 新”后返回。
	lines := make([]string, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		lines = append(lines, reversed[i])
	}
	return lines
}

type sourceItem struct {
	Index        int     `json:"index"`
	FileMD5      string  `json:"fileMd5"`
	FileName     string  `json:"fileName"`
	ChunkID      int     `json:"chunkId"`
	Score        float64 `json:"score"`
	Snippet      string  `json:"snippet"`
	PreviewPath  string  `json:"previewPath"`
	DownloadPath string  `json:"downloadPath"`
}

func buildSourceItems(results []model.SearchResponseDTO) []sourceItem {
	if len(results) == 0 {
		return nil
	}

	items := make([]sourceItem, 0, len(results))
	for i, r := range results {
		fileName := r.FileName
		if fileName == "" {
			fileName = "unknown"
		}
		fileMD5Escaped := url.QueryEscape(r.FileMD5)
		items = append(items, sourceItem{
			Index:        i + 1,
			FileMD5:      r.FileMD5,
			FileName:     fileName,
			ChunkID:      r.ChunkID,
			Score:        r.Score,
			Snippet:      truncateAtSentence(r.TextContent, sourceSnippetLen),
			PreviewPath:  "/api/v1/documents/preview?fileMd5=" + fileMD5Escaped,
			DownloadPath: "/api/v1/documents/download?fileMd5=" + fileMD5Escaped,
		})
	}
	return items
}

// loadHistory 获取或创建对话 ID，并加载历史消息。
func (s *chatService) loadHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error) {
	convID, err := s.conversationRepo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.conversationRepo.GetConversationHistory(ctx, convID)
}

// composeMessages 将 system message、历史、用户输入拼接为完整消息列表。
func (s *chatService) composeMessages(systemMsg string, history []model.ChatMessage, userInput string) []model.ChatMessage {
	msgs := make([]model.ChatMessage, 0, len(history)+2)
	msgs = append(msgs, model.ChatMessage{Role: "system", Content: systemMsg})
	msgs = append(msgs, history...)
	msgs = append(msgs, model.ChatMessage{Role: "user", Content: userInput})
	return msgs
}

// addMessageToConversation 将本轮 Q&A 追加到对话历史。
//
// 🟠 Fix #5: 若底层 Repository 支持 AppendMessages，应优先使用 Append-only 接口
// 以避免"读-改-写"模式在并发下覆盖其他请求写入的消息。
// 当前实现保持与原接口兼容，但标注了改造方向。
func (s *chatService) addMessageToConversation(ctx context.Context, userID uint, question, answer string) error {
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

// ---------------------------------------------------------------------------
// wsWriterInterceptor
// ---------------------------------------------------------------------------

// wsWriterInterceptor 实现 llm.MessageWriter，同时捕获完整答案文本。
type wsWriterInterceptor struct {
	conn       llm.MessageWriter
	shouldStop func() bool

	//mu 保护 buf，防止 LLM 客户端内部并发调用 WriteMessage。
	mu  sync.Mutex
	buf bytes.Buffer

	// rawBuf 保存原始流，filteredBuf 保存过滤后的用户可见输出。
	rawBuf      bytes.Buffer
	filteredBuf bytes.Buffer
}

// WriteMessage 满足 llm.MessageWriter 接口。
// 每个 chunk 被包装为 {"type":"text","chunk":"..."} 后发送给前端。
func (w *wsWriterInterceptor) WriteMessage(messageType int, data []byte) error {
	if w.shouldStop != nil && w.shouldStop() {
		return nil
	}

	filtered := w.consumeAndFilter(data)
	if len(filtered) == 0 {
		return nil
	}

	// 使用预分配栈缓冲区序列化 chunk，避免每次 heap alloc 一个 map。
	// 手动拼接 JSON，规避 json.Marshal 的反射开销与 map 分配。
	var jsonBuf bytes.Buffer
	jsonBuf.Grow(len(filtered) + 26) // {"type":"text","chunk":"..."} 约 26 字节固定开销
	jsonBuf.WriteString(`{"type":"text","chunk":`)
	chunkJSON, _ := json.Marshal(string(filtered))
	jsonBuf.Write(chunkJSON)
	jsonBuf.WriteByte('}')

	return w.conn.WriteMessage(messageType, jsonBuf.Bytes())
}

// Answer 返回拦截到的完整答案文本（流式结束后调用）。
func (w *wsWriterInterceptor) Answer() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(stripResidualInternalOutput(w.filteredBuf.String()))
}

func (w *wsWriterInterceptor) consumeAndFilter(data []byte) []byte {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.rawBuf.Write(data)
	w.buf.Write(data)

	cleaned := stripInternalOutput(w.rawBuf.String())
	if cleaned == "" {
		w.filteredBuf.Reset()
		return nil
	}

	prev := w.filteredBuf.String()
	if strings.HasPrefix(cleaned, prev) {
		delta := cleaned[len(prev):]
		w.filteredBuf.Reset()
		w.filteredBuf.WriteString(cleaned)
		return []byte(delta)
	}

	w.filteredBuf.Reset()
	w.filteredBuf.WriteString(cleaned)
	return []byte(cleaned)
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

type progressEvent struct {
	Type      string `json:"type"`
	Stage     string `json:"stage"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

type toolCallEvent struct {
	Type        string `json:"type"`
	Tool        string `json:"tool"`
	DisplayName string `json:"displayName,omitempty"`
	Message     string `json:"message"`
	Query       string `json:"query,omitempty"`
	TopK        int    `json:"topK,omitempty"`
	Timestamp   int64  `json:"timestamp"`
}

type toolResultEvent struct {
	Type        string `json:"type"`
	Tool        string `json:"tool"`
	DisplayName string `json:"displayName,omitempty"`
	Message     string `json:"message"`
	Success     bool   `json:"success"`
	ResultCount int    `json:"resultCount,omitempty"`
	TotalCount  int    `json:"totalCount,omitempty"`
	Timestamp   int64  `json:"timestamp"`
}

func writeWSJSON(ws llm.MessageWriter, payload interface{}) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return ws.WriteMessage(websocket.TextMessage, b)
}

func sendProgressEvent(ws llm.MessageWriter, stage string, message string) error {
	return writeWSJSON(ws, progressEvent{
		Type:      "progress",
		Stage:     stage,
		Message:   message,
		Timestamp: time.Now().UnixMilli(),
	})
}

func sendToolCallEvent(ws llm.MessageWriter, event toolCallEvent) error {
	event.Type = "tool_call"
	event.Timestamp = time.Now().UnixMilli()
	return writeWSJSON(ws, event)
}

func sendToolResultEvent(ws llm.MessageWriter, event toolResultEvent) error {
	event.Type = "tool_result"
	event.Timestamp = time.Now().UnixMilli()
	return writeWSJSON(ws, event)
}

// sendCompletion 向前端发送流式结束通知。
func sendCompletion(ws llm.MessageWriter) error {
	return writeWSJSON(ws, map[string]interface{}{
		"type":      "completion",
		"status":    "finished",
		"message":   "响应已完成",
		"timestamp": time.Now().UnixMilli(),
		"date":      time.Now().Format("2006-01-02T15:04:05"),
	})
}

// sendWarningChunk 向前端发送一条警告分块（用于通知用户无检索结果等情况）。
func sendWarningChunk(ws llm.MessageWriter, msg string) error {
	payload := map[string]string{
		"type":  "warning",
		"chunk": msg,
	}
	return writeWSJSON(ws, payload)
}

// sendSources 向前端发送结构化来源列表，便于做“可点击来源”渲染。
func sendSources(ws llm.MessageWriter, sources []sourceItem) error {
	if len(sources) == 0 {
		return nil
	}
	payload := map[string]interface{}{
		"type":      "sources",
		"sources":   sources,
		"timestamp": time.Now().UnixMilli(),
	}
	return writeWSJSON(ws, payload)
}

// buildGenerationParams 从配置构建 LLM 生成参数，返回 nil 表示使用模型默认值。
// 此函数在 NewChatService 中调用一次，结果缓存在结构体字段中。
func buildGenerationParams() *llm.GenerationParams {
	var gp llm.GenerationParams
	if t := config.Conf.LLM.Generation.Temperature; t != 0 {
		v := t
		gp.Temperature = &v
	}
	if p := config.Conf.LLM.Generation.TopP; p != 0 {
		v := p
		gp.TopP = &v
	}
	if m := config.Conf.LLM.Generation.MaxTokens; m != 0 {
		v := m
		gp.MaxTokens = &v
	}
	if gp.Temperature == nil && gp.TopP == nil && gp.MaxTokens == nil {
		return nil
	}
	return &gp
}

func stripInternalOutput(s string) string {
	if s == "" {
		return ""
	}

	cleaned := s

	// 删除完整 think 块。
	for {
		start := strings.Index(strings.ToLower(cleaned), "<think>")
		if start < 0 {
			break
		}
		end := strings.Index(strings.ToLower(cleaned[start:]), "</think>")
		if end < 0 {
			cleaned = cleaned[:start]
			break
		}
		end += start + len("</think>")
		cleaned = cleaned[:start] + cleaned[end:]
	}

	cleaned = stripLinePrefixes(cleaned, []string{
		"minimax:tool_call",
		"<minimax:tool_call",
		"</minimax:tool_call>",
		"<invoke ",
		"</invoke>",
		"<parameter ",
		"</parameter>",
		"让我重新检索",
		"让我继续检索",
		"让我基于检索结果来回答用户的问题",
		"让我整理一下",
		"the user is asking",
		"i should",
		"let me",
	})

	return strings.TrimLeft(cleaned, "\n")
}

func stripResidualInternalOutput(s string) string {
	cleaned := stripInternalOutput(s)
	lines := strings.Split(cleaned, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" && len(out) == 0 {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func stripLinePrefixes(s string, prefixes []string) string {
	if s == "" {
		return ""
	}

	lines := strings.Split(s, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.ToLower(line))
		skip := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(trimmed, strings.ToLower(prefix)) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

// truncateAtSentence 在不超过 maxLen 字节的前提下，尽量按句子边界截断文本。
//
//	原来直接切 [:1000] 可能把中文句子切断，现改为按"。"或"."截断。
func truncateAtSentence(s string, maxLen int) string {
	// 1. 安全检查：如果字节长度本身就没超，直接返回
	if len(s) <= maxLen {
		return s
	}

	// 2. 寻找合法的 UTF-8 边界，避免硬截断产生乱码
	// 向上取最近的一个合法字符起始位
	validLen := 0
	for i := range s {
		if i > maxLen {
			break
		}
		validLen = i
	}
	sub := s[:validLen]

	// 3. 优先级匹配：中文句号 > 英文句点
	// 技巧：利用 strings.LastIndexAny 可以一次性查找多个标点
	punctuations := []string{"。", "！", "？", ".", "!", "?"}
	lastIdx := -1
	matchedPunc := ""

	for _, p := range punctuations {
		if idx := strings.LastIndex(sub, p); idx > lastIdx {
			lastIdx = idx
			matchedPunc = p
		}
	}

	if lastIdx > 0 {
		return s[:lastIdx+len(matchedPunc)] + "…"
	}

	// 4. 兜底：如果没有发现句号，返回安全截断后的内容
	return sub + "…"
}

func truncateRunesWithEllipsis(s string, maxRunes int) string {
	if maxRunes <= 0 || s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

type memoryExtractItem struct {
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Keywords   []string `json:"keywords"`
	Confidence float64  `json:"confidence"`
}

func parseMemoryExtraction(raw string) []memoryExtractItem {
	payload := strings.TrimSpace(raw)
	if payload == "" {
		return nil
	}
	payload = trimCodeFence(payload)

	var items []memoryExtractItem
	if err := json.Unmarshal([]byte(payload), &items); err == nil {
		return sanitizeMemoryExtractItems(items)
	}

	// 兜底：兼容模型输出 {"items":[...]}。
	var wrapped struct {
		Items []memoryExtractItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(payload), &wrapped); err == nil {
		return sanitizeMemoryExtractItems(wrapped.Items)
	}
	return nil
}

func sanitizeMemoryExtractItems(items []memoryExtractItem) []memoryExtractItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]memoryExtractItem, 0, len(items))
	for _, it := range items {
		it.Category = strings.TrimSpace(strings.ToLower(it.Category))
		it.Content = strings.TrimSpace(it.Content)
		if it.Category == "" || it.Content == "" {
			continue
		}
		if it.Confidence <= 0 || it.Confidence > 1 {
			it.Confidence = 0.8
		}
		out = append(out, it)
	}
	return out
}

func trimCodeFence(text string) string {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// firstNonEmpty 返回参数列表中第一个非空字符串，全空则返回空字符串。
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
