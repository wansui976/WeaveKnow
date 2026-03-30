// Package llm provides a client for interacting with Large Language Models.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"pai-smart-go/internal/config"
	"strings"

	"github.com/gorilla/websocket"
)

// MessageWriter defines an interface for writing WebSocket messages.
// This allows both a standard websocket.Conn and our interceptor to be used.
type MessageWriter interface {
	WriteMessage(messageType int, data []byte) error
}

// Tool 描述一个可被模型调用的函数工具（OpenAI/Qwen 兼容格式）。
type Tool struct {
	Type     string       `json:"type"` // 固定为 "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction 描述工具函数签名。
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters"` // JSON Schema
}

// ToolCall 表示模型请求调用某个工具。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON string
	} `json:"function"`
}

// ChatResult 封装非流式对话响应（支持 tool_calls）。
type ChatResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
}

// Client defines the interface for an LLM client.
type Client interface {
	// ChatWithTools 调用非流式聊天接口，让模型自主决定是否触发工具调用。
	ChatWithTools(ctx context.Context, messages []Message, tools []Tool, gen *GenerationParams) (*ChatResult, error)
	// StreamChatMessages 以 role-based 消息与可选生成参数调用聊天接口，并将流式分块写入 writer。
	StreamChatMessages(ctx context.Context, messages []Message, gen *GenerationParams, writer MessageWriter) error
	// 为兼容旧调用，保留 StreamChat：由内部包装为 messages 调用。
	StreamChat(ctx context.Context, prompt string, writer MessageWriter) error
}

type deepseekClient struct {
	// cfg 保存 LLM 提供方地址、模型名、鉴权等静态配置。
	cfg config.LLMConfig
	// client 负责实际发起 HTTP 请求，便于后续统一设置超时/Transport。
	client *http.Client
}

// NewClient creates a new LLM client based on the provider in the config.
func NewClient(cfg config.LLMConfig) Client {
	return &deepseekClient{
		cfg:    cfg,
		client: &http.Client{},
	}
}

// Message 表示一条对话消息（与 OpenAI/DeepSeek chat 格式对齐）。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// chatRequest 是请求 /chat/completions 的最小字段集合。
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
}

// chatResponse 仅保留当前流式解析所需字段（delta.content）。
type chatResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// GenerationParams 控制生成行为。
// 使用指针是为了区分“未设置”和“显式设置为 0”两种语义。
type GenerationParams struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
}

// StreamChat calls the DeepSeek API for chat completions and streams the response.
func (c *deepseekClient) StreamChat(ctx context.Context, prompt string, writer MessageWriter) error {
	// 兼容旧接口：仅发送一条 user 消息，不带生成参数
	return c.StreamChatMessages(ctx, []Message{{Role: "user", Content: prompt}}, nil, writer)
}

// ChatWithTools 调用非流式接口，返回 tool_calls 或普通文本。
// 该实现优先兼容 OpenAI/Qwen 响应格式，并兼容解析 Claude 原生 tool_use 结构。
func (c *deepseekClient) ChatWithTools(ctx context.Context, messages []Message, tools []Tool, gen *GenerationParams) (*ChatResult, error) {
	reqBody := chatRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Tools:    tools,
		Stream:   false,
	}
	c.applyGenerationParams(&reqBody, gen)

	rawBody, err := c.doChatRequest(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	result, err := parseChatResult(rawBody)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *deepseekClient) StreamChatMessages(ctx context.Context, messages []Message, gen *GenerationParams, writer MessageWriter) error {
	// 1) 组装基础请求体：固定使用流式返回。
	reqBody := chatRequest{
		Model:    c.cfg.Model,
		Messages: messages,
		Stream:   true,
	}
	c.applyGenerationParams(&reqBody, gen)

	// 2) 序列化为 JSON 并创建 HTTP 请求。
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return fmt.Errorf("failed to create chat request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	// 3) 发起请求并校验状态码。
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call chat api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("chat api returned non-200 status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	// 4) 按 SSE 协议逐行读取 data: 事件，并把增量内容透传到 WebSocket。
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to read from stream: %w", err)
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if strings.TrimSpace(data) == "[DONE]" {
				break
			}

			// 某些行可能是空行或非 JSON 事件，解析失败时直接跳过。
			var chunk chatResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			if len(chunk.Choices) > 0 {
				content := chunk.Choices[0].Delta.Content
				if err := writer.WriteMessage(websocket.TextMessage, []byte(content)); err != nil {
					return fmt.Errorf("failed to write message to websocket: %w", err)
				}
			}
		}
	}
	return nil
}

func (c *deepseekClient) applyGenerationParams(reqBody *chatRequest, gen *GenerationParams) {
	if gen != nil {
		reqBody.Temperature = gen.Temperature
		reqBody.TopP = gen.TopP
		reqBody.MaxTokens = gen.MaxTokens
		return
	}

	if c.cfg.Generation.Temperature != 0 {
		t := c.cfg.Generation.Temperature
		reqBody.Temperature = &t
	}
	if c.cfg.Generation.TopP != 0 {
		p := c.cfg.Generation.TopP
		reqBody.TopP = &p
	}
	if c.cfg.Generation.MaxTokens != 0 {
		m := c.cfg.Generation.MaxTokens
		reqBody.MaxTokens = &m
	}
}

func (c *deepseekClient) doChatRequest(ctx context.Context, reqBody chatRequest) ([]byte, error) {
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call chat api: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chat api returned non-200 status: %s, body: %s", resp.Status, string(body))
	}
	return body, nil
}

func parseChatResult(body []byte) (*ChatResult, error) {
	if result, ok := parseOpenAIOrQwenChatResult(body); ok {
		return result, nil
	}
	if result, ok := parseClaudeChatResult(body); ok {
		return result, nil
	}
	return nil, fmt.Errorf("failed to parse tool response, unsupported schema: %s", truncateBodyForLog(body))
}

func parseOpenAIOrQwenChatResult(body []byte) (*ChatResult, bool) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content   interface{} `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string      `json:"name"`
						Arguments interface{} `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Choices) == 0 {
		return nil, false
	}

	choice := resp.Choices[0]
	result := &ChatResult{
		Content:      normalizeModelContent(choice.Message.Content),
		FinishReason: choice.FinishReason,
	}
	result.ToolCalls = normalizeToolCalls(choice.Message.ToolCalls)
	return result, true
}

func parseClaudeChatResult(body []byte) (*ChatResult, bool) {
	var resp struct {
		Content []struct {
			Type  string      `json:"type"`
			Text  string      `json:"text"`
			ID    string      `json:"id"`
			Name  string      `json:"name"`
			Input interface{} `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false
	}
	if len(resp.Content) == 0 && resp.StopReason == "" {
		return nil, false
	}

	result := &ChatResult{FinishReason: resp.StopReason}
	if result.FinishReason == "tool_use" {
		result.FinishReason = "tool_calls"
	}
	var textParts []string
	for _, part := range resp.Content {
		switch part.Type {
		case "text":
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		case "tool_use":
			var toolCall ToolCall
			toolCall.ID = part.ID
			toolCall.Type = "function"
			toolCall.Function.Name = part.Name
			toolCall.Function.Arguments = normalizeArguments(part.Input)
			result.ToolCalls = append(result.ToolCalls, toolCall)
		}
	}
	result.Content = strings.TrimSpace(strings.Join(textParts, "\n"))
	return result, true
}

func normalizeToolCalls(raw []struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string      `json:"name"`
		Arguments interface{} `json:"arguments"`
	} `json:"function"`
}) []ToolCall {
	if len(raw) == 0 {
		return nil
	}
	calls := make([]ToolCall, 0, len(raw))
	for _, item := range raw {
		call := ToolCall{
			ID:   item.ID,
			Type: item.Type,
		}
		if call.Type == "" {
			call.Type = "function"
		}
		call.Function.Name = item.Function.Name
		call.Function.Arguments = normalizeArguments(item.Function.Arguments)
		calls = append(calls, call)
	}
	return calls
}

func normalizeArguments(args interface{}) string {
	switch v := args.(type) {
	case string:
		return v
	case nil:
		return "{}"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}

func normalizeModelContent(raw interface{}) string {
	switch v := raw.(type) {
	case string:
		return v
	case nil:
		return ""
	case []interface{}:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok && text != "" {
					parts = append(parts, text)
					continue
				}
				if text, ok := m["content"].(string); ok && text != "" {
					parts = append(parts, text)
					continue
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func truncateBodyForLog(body []byte) string {
	const max = 400
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "...(truncated)"
}
