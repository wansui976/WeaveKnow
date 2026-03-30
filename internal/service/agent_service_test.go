package service

import (
	"context"
	"encoding/json"
	"errors"
	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/llm"
	"strings"
	"testing"
	"time"
)

type fakeSearchService struct {
	lastTopK     int
	deadlineSeen bool
}

func (f *fakeSearchService) HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
	f.lastTopK = topK
	_, f.deadlineSeen = ctx.Deadline()
	return []model.SearchResponseDTO{
		{FileMD5: "m1", FileName: "a.txt", ChunkID: 1, TextContent: "hello", Score: 0.8},
	}, nil
}

type fakeLLMClient struct{}

func (f *fakeLLMClient) ChatWithTools(ctx context.Context, messages []llm.Message, tools []llm.Tool, gen *llm.GenerationParams) (*llm.ChatResult, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeLLMClient) StreamChatMessages(ctx context.Context, messages []llm.Message, gen *llm.GenerationParams, writer llm.MessageWriter) error {
	return errors.New("not implemented")
}

func (f *fakeLLMClient) StreamChat(ctx context.Context, prompt string, writer llm.MessageWriter) error {
	return errors.New("not implemented")
}

type fakeConversationRepo struct{}

func (f *fakeConversationRepo) GetOrCreateConversationID(ctx context.Context, userID uint) (string, error) {
	return "conv-1", nil
}

func (f *fakeConversationRepo) GetConversationHistory(ctx context.Context, conversationID string) ([]model.ChatMessage, error) {
	return nil, nil
}

func (f *fakeConversationRepo) UpdateConversationHistory(ctx context.Context, conversationID string, messages []model.ChatMessage) error {
	return nil
}

func (f *fakeConversationRepo) GetAllUserConversationMappings(ctx context.Context) (map[uint]string, error) {
	return nil, nil
}

func TestNewAgentServiceCapsGuardrails(t *testing.T) {
	svc := NewAgentService(
		&fakeSearchService{},
		&fakeLLMClient{},
		&fakeConversationRepo{},
		NewMetricsService(),
		AgentOptions{
			MaxIterations: 99,
			DefaultTopK:   999,
			ToolTimeout:   0,
		},
	)

	impl := svc.(*agentService)
	if impl.maxIterations != maxAllowedAgentIterations {
		t.Fatalf("expected maxIterations=%d, got=%d", maxAllowedAgentIterations, impl.maxIterations)
	}
	if impl.defaultTopK != 5 {
		t.Fatalf("expected defaultTopK fallback=5, got=%d", impl.defaultTopK)
	}
	if impl.toolTimeout <= 0 {
		t.Fatalf("expected positive toolTimeout")
	}
}

func TestExecuteToolClampsTopKAndAddsTimeout(t *testing.T) {
	search := &fakeSearchService{}
	svc := &agentService{
		searchService: search,
		defaultTopK:   5,
		toolTimeout:   2 * time.Second,
	}

	call := llm.ToolCall{}
	call.Function.Name = "knowledge_search"
	call.Function.Arguments = `{"query":"RRF 是什么","top_k":999}`

	_, err := svc.executeTool(context.Background(), &model.User{ID: 1}, call, nil)
	if err != nil {
		t.Fatalf("executeTool returned error: %v", err)
	}
	if search.lastTopK != 5 {
		t.Fatalf("expected top_k to be clamped to 5, got=%d", search.lastTopK)
	}
	if !search.deadlineSeen {
		t.Fatalf("expected executeTool to set context deadline via WithTimeout")
	}
}

func TestExecuteToolInvalidJSONReturnsError(t *testing.T) {
	svc := &agentService{
		searchService: &fakeSearchService{},
		defaultTopK:   5,
		toolTimeout:   2 * time.Second,
	}

	call := llm.ToolCall{}
	call.Function.Name = "knowledge_search"
	call.Function.Arguments = `{"query":`

	_, err := svc.executeTool(context.Background(), &model.User{ID: 1}, call, nil)
	if err == nil {
		t.Fatalf("expected JSON parse error")
	}
}

func TestBuildToolErrorResult(t *testing.T) {
	got := buildToolErrorResult("knowledge_search", errors.New("tool arguments parse failed: bad json"))
	if !strings.Contains(got, `"ok":false`) {
		t.Fatalf("expected error payload with ok=false, got=%s", got)
	}
	if !strings.Contains(got, "INVALID_TOOL_ARGUMENTS") {
		t.Fatalf("expected INVALID_TOOL_ARGUMENTS code, got=%s", got)
	}
}

func TestFormatKnowledgeSearchResultWithBudget(t *testing.T) {
	svc := &agentService{
		toolContextBudget: 160,
	}
	longText := strings.Repeat("这是很长的文本内容用于测试预算裁剪。", 40)
	results := []model.SearchResponseDTO{
		{FileMD5: "a", FileName: "a.txt", ChunkID: 1, TextContent: longText, Score: 0.9},
		{FileMD5: "b", FileName: "b.txt", ChunkID: 2, TextContent: longText, Score: 0.8},
		{FileMD5: "c", FileName: "c.txt", ChunkID: 3, TextContent: longText, Score: 0.7},
	}

	raw := svc.formatKnowledgeSearchResult(results, nil)
	var payload struct {
		OK       bool `json:"ok"`
		Selected int  `json:"selected"`
		Total    int  `json:"total"`
		Results  []struct {
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}
	if !payload.OK {
		t.Fatalf("expected ok payload")
	}
	if payload.Total != len(results) {
		t.Fatalf("expected total=%d, got=%d", len(results), payload.Total)
	}
	if payload.Selected == 0 {
		t.Fatalf("expected at least one selected result")
	}
	if payload.Selected > len(results) {
		t.Fatalf("selected overflow")
	}
}

func TestResolveToolContextBudgetTokensByContextWindow(t *testing.T) {
	oldLLM := config.Conf.LLM
	defer func() {
		config.Conf.LLM = oldLLM
	}()
	config.Conf.LLM.ContextWindow = 1000
	config.Conf.LLM.Model = "deepseek-chat"

	maxOut := 120
	svc := &agentService{
		toolContextBudget: 220,
		defaultGenParams: &llm.GenerationParams{
			MaxTokens: &maxOut,
		},
	}
	msgs := []llm.Message{
		{Role: "system", Content: strings.Repeat("a", 800)},
		{Role: "user", Content: "hello"},
	}
	budget := svc.resolveToolContextBudgetTokens(msgs)
	if budget <= 0 {
		t.Fatalf("expected positive budget")
	}
	if budget > 220 {
		t.Fatalf("expected budget <= toolContextBudget, got %d", budget)
	}
}
