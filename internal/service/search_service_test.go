package service

import (
	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/reranker"
	"testing"
)

func TestSanitizeRewrittenQuery(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		maxLength int
		want      string
	}{
		{
			name:      "strip_prefix_and_newline",
			raw:       "改写后的查询：\n上次合同中的违约责任条款",
			maxLength: 64,
			want:      "上次合同中的违约责任条款",
		},
		{
			name:      "trim_quotes",
			raw:       "\"query: SDK 升级说明\"",
			maxLength: 64,
			want:      "SDK 升级说明",
		},
		{
			name:      "truncate_by_runes",
			raw:       "这是一个非常非常长的检索查询字符串",
			maxLength: 6,
			want:      "这是一个非常",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeRewrittenQuery(tt.raw, tt.maxLength)
			if got != tt.want {
				t.Fatalf("sanitizeRewrittenQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRerankSearchResults(t *testing.T) {
	docs := []model.SearchResponseDTO{
		{TextContent: "RRF 是一种融合算法", Score: 0.2},
		{TextContent: "这里不相关", Score: 0.1},
	}
	got := rerankSearchResults("RRF 是什么", docs, 1)
	if len(got) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(got))
	}
	if got[0].TextContent != "RRF 是一种融合算法" {
		t.Fatalf("expected RRF related doc ranked first")
	}
}

func TestApplyExternalRerankResult(t *testing.T) {
	docs := []model.SearchResponseDTO{
		{FileMD5: "a", TextContent: "doc-a", Score: 0.2},
		{FileMD5: "b", TextContent: "doc-b", Score: 0.3},
		{FileMD5: "c", TextContent: "doc-c", Score: 0.4},
	}
	results := []reranker.Result{
		{ID: 2, Score: 0.95},
		{ID: 0, Score: 0.82},
	}
	got := applyExternalRerankResult(docs, results, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(got))
	}
	if got[0].FileMD5 != "c" || got[1].FileMD5 != "a" {
		t.Fatalf("unexpected rerank order: %+v", got)
	}
}
