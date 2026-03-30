package service

import (
	"pai-smart-go/internal/model"
	"strings"
	"testing"
)

func TestBuildRetrievalQueryForFollowup(t *testing.T) {
	svc := &chatService{}
	history := []model.ChatMessage{
		{Role: "user", Content: "RRF 是什么"},
		{Role: "assistant", Content: "RRF 是一种融合检索结果的排序方法。"},
	}

	got := svc.buildRetrievalQuery("那它的优缺点呢？", history)
	if got == "那它的优缺点呢？" {
		t.Fatalf("expected fused retrieval query, got original query")
	}
	if !strings.Contains(got, "相关上下文") {
		t.Fatalf("expected fused query to include context, got: %s", got)
	}
}

func TestBuildRetrievalQueryForStandalone(t *testing.T) {
	svc := &chatService{}
	history := []model.ChatMessage{
		{Role: "user", Content: "上一个问题"},
		{Role: "assistant", Content: "上一个回答"},
	}

	query := "什么是向量数据库"
	got := svc.buildRetrievalQuery(query, history)
	if got != query {
		t.Fatalf("expected standalone query unchanged, got: %s", got)
	}
}
