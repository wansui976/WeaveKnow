package llm

import (
	"encoding/json"
	"testing"
)

func TestParseOpenAIOrQwenChatResult(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"message": {
				"content": "planning",
				"tool_calls": [{
					"id": "call_123",
					"type": "function",
					"function": {
						"name": "knowledge_search",
						"arguments": "{\"query\":\"合同条款\",\"top_k\":5}"
					}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)

	result, ok := parseOpenAIOrQwenChatResult(body)
	if !ok {
		t.Fatalf("expected parse success")
	}
	if result.FinishReason != "tool_calls" {
		t.Fatalf("unexpected finish reason: %s", result.FinishReason)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Function.Name != "knowledge_search" {
		t.Fatalf("unexpected tool calls: %+v", result.ToolCalls)
	}
}

func TestParseClaudeChatResult(t *testing.T) {
	body := []byte(`{
		"content": [
			{"type":"text","text":"好的，我先检索。"},
			{"type":"tool_use","id":"toolu_1","name":"knowledge_search","input":{"query":"合同违约责任","top_k":3}}
		],
		"stop_reason": "tool_use"
	}`)

	result, ok := parseClaudeChatResult(body)
	if !ok {
		t.Fatalf("expected parse success")
	}
	if result.FinishReason != "tool_calls" {
		t.Fatalf("unexpected finish reason: %s", result.FinishReason)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("unexpected tool call count: %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Function.Name != "knowledge_search" {
		t.Fatalf("unexpected tool name: %s", result.ToolCalls[0].Function.Name)
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(result.ToolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments should be valid json: %v", err)
	}
}

