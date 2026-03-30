package service

import (
	"sync/atomic"
	"time"
)

// RAGMetricsSnapshot 用于对外展示核心质量/稳定性指标。
type RAGMetricsSnapshot struct {
	// QueryRewrite*：查询改写（例如同义扩展、重写）的触发与命中情况。
	QueryRewriteTotal uint64  `json:"queryRewriteTotal"`
	QueryRewriteHit   uint64  `json:"queryRewriteHit"`
	QueryRewriteRate  float64 `json:"queryRewriteRate"`

	// Fusion*：多轮问题融合（结合历史上下文构造检索 query）的触发与命中情况。
	FusionTotal uint64  `json:"fusionTotal"`
	FusionHit   uint64  `json:"fusionHit"`
	FusionRate  float64 `json:"fusionRate"`

	// ToolCall*：Agent 工具调用总量与失败率。
	ToolCallTotal    uint64  `json:"toolCallTotal"`
	ToolCallFail     uint64  `json:"toolCallFail"`
	ToolCallFailRate float64 `json:"toolCallFailRate"`

	// Memory*：结构化记忆检索次数与命中率。
	MemoryQueryTotal uint64  `json:"memoryQueryTotal"`
	MemoryHit        uint64  `json:"memoryHit"`
	MemoryHitRate    float64 `json:"memoryHitRate"`

	// UpdatedAt 快照生成时间（Unix 毫秒时间戳）。
	UpdatedAt int64 `json:"updatedAt"`
}

// MetricsService 记录并读取 RAG/Agent 关键指标。
type MetricsService interface {
	// RecordQueryRewrite 记录一次查询改写流程是否命中。
	RecordQueryRewrite(hit bool)
	// RecordFusion 记录一次多轮融合流程是否生效/命中。
	RecordFusion(hit bool)
	// RecordToolCall 记录一次工具调用结果；success=false 记为失败。
	RecordToolCall(success bool)
	// RecordMemoryHit 记录一次记忆检索是否命中。
	RecordMemoryHit(hit bool)
	// Snapshot 读取当前指标快照（无锁原子读）。
	Snapshot() RAGMetricsSnapshot
}

// metricsService 使用 atomic 计数器实现高并发下的低开销指标统计。
type metricsService struct {
	// 查询改写统计
	queryRewriteTotal atomic.Uint64
	queryRewriteHit   atomic.Uint64

	// 多轮融合统计
	fusionTotal atomic.Uint64
	fusionHit   atomic.Uint64

	// 工具调用统计
	toolCallTotal atomic.Uint64
	toolCallFail  atomic.Uint64

	// 记忆命中统计
	memoryQueryTotal atomic.Uint64
	memoryHit        atomic.Uint64
}

// NewMetricsService 创建 MetricsService 默认实现。
func NewMetricsService() MetricsService {
	return &metricsService{}
}

// RecordQueryRewrite 记录查询改写次数与命中次数。
func (m *metricsService) RecordQueryRewrite(hit bool) {
	m.queryRewriteTotal.Add(1)
	if hit {
		m.queryRewriteHit.Add(1)
	}
}

// RecordFusion 记录多轮融合次数与命中次数。
func (m *metricsService) RecordFusion(hit bool) {
	m.fusionTotal.Add(1)
	if hit {
		m.fusionHit.Add(1)
	}
}

// RecordToolCall 记录工具调用总次数与失败次数。
func (m *metricsService) RecordToolCall(success bool) {
	m.toolCallTotal.Add(1)
	if !success {
		m.toolCallFail.Add(1)
	}
}

// RecordMemoryHit 记录记忆检索次数与命中次数。
func (m *metricsService) RecordMemoryHit(hit bool) {
	m.memoryQueryTotal.Add(1)
	if hit {
		m.memoryHit.Add(1)
	}
}

// Snapshot 读取当前原子计数，并计算各类比率，返回只读快照。
func (m *metricsService) Snapshot() RAGMetricsSnapshot {
	// 先读取原子计数，避免在构造快照时重复 Load。
	qTotal := m.queryRewriteTotal.Load()
	qHit := m.queryRewriteHit.Load()
	fTotal := m.fusionTotal.Load()
	fHit := m.fusionHit.Load()
	tTotal := m.toolCallTotal.Load()
	tFail := m.toolCallFail.Load()
	mTotal := m.memoryQueryTotal.Load()
	mHit := m.memoryHit.Load()

	return RAGMetricsSnapshot{
		QueryRewriteTotal: qTotal,
		QueryRewriteHit:   qHit,
		QueryRewriteRate:  ratio(qHit, qTotal),

		FusionTotal: fTotal,
		FusionHit:   fHit,
		FusionRate:  ratio(fHit, fTotal),

		ToolCallTotal:    tTotal,
		ToolCallFail:     tFail,
		ToolCallFailRate: ratio(tFail, tTotal),

		MemoryQueryTotal: mTotal,
		MemoryHit:        mHit,
		MemoryHitRate:    ratio(mHit, mTotal),
		UpdatedAt:        time.Now().UnixMilli(),
	}
}

// ratio 计算命中率（hit/total）；当 total=0 时返回 0，避免除零。
func ratio(hit, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(hit) / float64(total)
}
