# Agent 工具系统使用指南

## 概述

WeaveKnow 的增强 Agent 系统提供了多种智能工具，支持并行执行和置信度评分，大幅提升问答效率和准确性。

## 可用工具

### 1. 知识库检索 (knowledge_search)
在企业知识库中检索与问题相关的文档片段。

**适用场景：**
- 查找特定信息
- 搜索文档内容
- 回答"什么是"、"如何"等问题

**参数：**
- `query` (必需): 检索关键词或问题
- `top_k` (可选): 返回文档数量，默认 5

**示例：**
```json
{
  "query": "如何配置数据库连接",
  "top_k": 10
}
```

### 2. 文档摘要 (document_summary)
对检索到的文档生成结构化摘要。

**适用场景：**
- 需要快速了解文档核心内容
- 提取关键信息和要点
- 长文档的概括总结

**参数：**
- `query` (必需): 用于检索文档的关键词
- `max_docs` (可选): 要摘要的文档数量，默认 3
- `summary_type` (可选): 摘要类型
  - `brief`: 简要摘要（2-3句话）
  - `detailed`: 详细摘要
  - `bullet_points`: 要点列表

**示例：**
```json
{
  "query": "微服务架构设计",
  "max_docs": 5,
  "summary_type": "bullet_points"
}
```

### 3. 实体提取 (entity_extraction)
从文档中提取关键实体。

**适用场景：**
- 识别人名、组织、产品、技术术语
- 分析文档中涉及的关键概念
- 构建知识图谱

**参数：**
- `query` (必需): 用于检索文档的关键词
- `entity_types` (可选): 实体类型列表
  - `person`: 人名
  - `organization`: 组织/公司
  - `product`: 产品
  - `concept`: 概念
  - `technology`: 技术
- `max_entities` (可选): 最多提取数量，默认 10

**示例：**
```json
{
  "query": "云计算平台",
  "entity_types": ["organization", "product", "technology"],
  "max_entities": 15
}
```

### 4. 关系查询 (relation_query)
查询实体之间的关系。

**适用场景：**
- 了解人物与组织的关系
- 分析产品与技术的依赖关系
- 探索概念之间的联系

**参数：**
- `entity1` (必需): 第一个实体名称
- `entity2` (可选): 第二个实体名称
- `relation_types` (可选): 关系类型列表

**示例：**
```json
{
  "entity1": "Docker",
  "entity2": "Kubernetes",
  "relation_types": ["uses", "depends_on", "related_to"]
}
```

## 核心特性

### 1. 智能工具选择（置信度评分）

系统会根据用户问题自动评估每个工具的适用性，只向 LLM 提供最相关的工具。

**评分机制：**
- 0.9-1.0: 高度相关，强烈推荐
- 0.7-0.9: 相关，推荐使用
- 0.5-0.7: 中等相关，可选
- 0.3-0.5: 低相关，不推荐
- 0.0-0.3: 不相关，过滤掉

**示例：**
```
用户问题: "总结一下微服务架构的核心要点"

工具置信度评分：
- document_summary: 0.95 ✓ (包含"总结"关键词)
- knowledge_search: 0.80 ✓ (需要先检索)
- entity_extraction: 0.40 ✗ (不太相关)
- relation_query: 0.30 ✗ (不相关)

最终选择: document_summary, knowledge_search
```

### 2. 并行工具调用

当多个工具调用相互独立时，系统会自动并行执行，大幅提升响应速度。

**并行条件：**
- 工具调用之间没有数据依赖
- 所有工具都是只读操作
- 启用了并行模式（默认启用）

**性能对比：**
```
串行执行: Tool1(2s) -> Tool2(2s) -> Tool3(2s) = 6秒
并行执行: Tool1(2s) | Tool2(2s) | Tool3(2s) = 2秒
```

**示例场景：**
```
用户: "分析 Docker 和 Kubernetes 的关系，并提取相关技术实体"

Agent 决策:
1. relation_query(Docker, Kubernetes) - 并行
2. entity_extraction(Docker Kubernetes) - 并行

执行时间: max(2s, 2s) = 2秒（而非 4秒）
```

### 3. 动态上下文预算

系统会根据对话长度和模型上下文窗口，动态调整工具结果的注入量。

**预算计算：**
```
可用预算 = 模型上下文窗口 - 已用token - 预留输出 - 安全边际
```

**自适应策略：**
- 对话初期：更多工具结果
- 对话后期：精简工具结果，保留最相关内容
- 长文档：自动截断，保留关键片段

## 使用示例

### 示例 1: 基础检索
```
用户: "如何配置 Redis 集群？"

Agent 执行流程:
1. 选择工具: knowledge_search (置信度 0.95)
2. 执行检索: query="Redis 集群配置", top_k=5
3. 返回结果: 5篇相关文档
4. 生成答案: 基于检索结果回答
```

### 示例 2: 摘要 + 检索
```
用户: "总结一下微服务架构的优缺点"

Agent 执行流程:
1. 选择工具: document_summary (0.90), knowledge_search (0.85)
2. 并行执行:
   - document_summary: query="微服务架构", summary_type="bullet_points"
   - knowledge_search: query="微服务优缺点", top_k=5
3. 整合结果: 结合摘要和检索内容
4. 生成答案: 结构化回答优缺点
```

### 示例 3: 实体 + 关系分析
```
用户: "Docker 和 Kubernetes 有什么关系？涉及哪些技术？"

Agent 执行流程:
1. 选择工具: relation_query (0.85), entity_extraction (0.80)
2. 并行执行:
   - relation_query: entity1="Docker", entity2="Kubernetes"
   - entity_extraction: query="Docker Kubernetes", entity_types=["technology"]
3. 整合结果: 关系图谱 + 技术列表
4. 生成答案: 详细说明关系和相关技术
```

### 示例 4: 复杂多轮查询
```
用户: "介绍一下 Kafka"
Agent: [检索并回答 Kafka 基础知识]

用户: "它和 RabbitMQ 有什么区别？"
Agent 执行流程:
1. 识别追问: 融合上下文 "Kafka 和 RabbitMQ 区别"
2. 选择工具: relation_query (0.85), knowledge_search (0.80)
3. 并行执行:
   - relation_query: entity1="Kafka", entity2="RabbitMQ"
   - knowledge_search: query="Kafka RabbitMQ 对比"
4. 生成答案: 详细对比分析
```

## 配置说明

### 启用增强 Agent

在 `config.yaml` 中配置：

```yaml
ai:
  agent:
    enabled: true
    max_iterations: 4
    default_top_k: 5
    tool_timeout: 8s
    tool_context_budget_tokens: 1200
    enable_parallel: true  # 启用并行工具调用
```

### 工具注册

在代码中注册自定义工具：

```go
registry := NewToolRegistry()

// 注册内置工具
registry.Register("knowledge_search", NewKnowledgeSearchTool(5))
registry.Register("document_summary", NewDocumentSummaryTool())
registry.Register("entity_extraction", NewEntityExtractionTool())
registry.Register("relation_query", NewRelationQueryTool())

// 注册自定义工具
registry.Register("custom_tool", NewCustomTool())
```

## 性能优化建议

### 1. 合理设置 top_k
- 简单问题: top_k=3-5
- 复杂问题: top_k=8-10
- 摘要任务: top_k=3-5（避免信息过载）

### 2. 选择合适的摘要类型
- 快速浏览: `brief`
- 详细分析: `detailed`
- 结构化展示: `bullet_points`

### 3. 限制实体提取数量
- 一般场景: max_entities=10
- 详细分析: max_entities=20-30
- 避免设置过大（影响性能）

### 4. 利用并行执行
- 独立查询可以并行
- 避免在循环中串行调用工具
- 让 Agent 自动决策并行策略

## 监控和调试

### 查看工具执行日志
```bash
# 查看工具调用记录
grep "EnhancedAgentService" logs/app.log

# 查看并行执行情况
grep "并行执行" logs/app.log

# 查看工具选择决策
grep "为查询选择了" logs/app.log
```

### 性能指标
- 工具调用成功率
- 平均执行时间
- 并行执行比例
- 置信度评分分布

## 常见问题

### Q: 如何添加自定义工具？
A: 实现 `ToolExecutor` 接口，然后在 `ToolRegistry` 中注册。

### Q: 并行执行会影响结果准确性吗？
A: 不会。系统只对独立的只读操作进行并行，不会影响结果。

### Q: 如何调整工具选择的置信度阈值？
A: 在 `ToolRegistry.SelectTools()` 中修改阈值（默认 0.3）。

### Q: 工具执行超时怎么办？
A: 调整 `tool_timeout` 配置，或优化工具实现。

## 最佳实践

1. **明确问题意图**: 清晰的问题能获得更好的工具选择
2. **合理使用摘要**: 长文档优先使用摘要工具
3. **组合使用工具**: 复杂问题可以组合多个工具
4. **关注性能**: 监控工具执行时间，及时优化
5. **迭代优化**: 根据用户反馈调整置信度评分逻辑

## 未来规划

- [ ] 支持更多工具类型（图表生成、代码分析等）
- [ ] 工具依赖关系自动推断
- [ ] 基于用户反馈的置信度学习
- [ ] 工具执行结果缓存
- [ ] 分布式工具执行
