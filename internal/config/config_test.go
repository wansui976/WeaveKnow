package config

import "testing"

func TestInitLoadsAgentConfig(t *testing.T) {
	Init("../../configs/config.yaml")
	if !Conf.AI.Agent.Enabled {
		t.Fatalf("expected ai.agent.enabled=true in config")
	}
	if Conf.AI.Agent.MaxIterations == 0 {
		t.Fatalf("expected ai.agent.max_iterations to be loaded")
	}
	if Conf.AI.Agent.ToolContextBudgetTokens == 0 {
		t.Fatalf("expected ai.agent.tool_context_budget_tokens to be loaded")
	}
	if Conf.Search.QueryRewriteTimeoutS != 3 {
		t.Fatalf("expected search.query_rewrite_timeout_seconds=3, got %d", Conf.Search.QueryRewriteTimeoutS)
	}
	if !Conf.Search.RerankEnabled {
		t.Fatalf("expected search.rerank_enabled=true")
	}
	if Conf.Memory.DecayHalfLifeHours == 0 {
		t.Fatalf("expected memory.decay_half_life_hours to be loaded")
	}
	if Conf.LLM.ContextWindow == 0 {
		t.Fatalf("expected llm.context_window to be loaded")
	}
}
