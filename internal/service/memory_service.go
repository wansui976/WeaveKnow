package service

import (
	"WeaveKnow/internal/config"
	"WeaveKnow/internal/model"
	"WeaveKnow/internal/repository"
	"WeaveKnow/pkg/embedding"
	"WeaveKnow/pkg/es"
	"WeaveKnow/pkg/log"
	"context"
	"crypto/md5"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	// allowedMemoryCategories 定义系统允许写入/查询的记忆分类白名单。
	// 说明：
	// - preferences: 用户偏好（语言、输出风格等）
	// - project: 当前项目相关事实
	// - entities: 人名/系统名/业务对象等实体信息
	// - workflow: 步骤、流程、操作习惯
	// - notes: 通用备注
	allowedMemoryCategories = map[string]struct{}{
		"preferences": {},
		"project":     {},
		"entities":    {},
		"workflow":    {},
		"notes":       {},
	}
	// reWorkspace 约束 workspace 仅允许安全字符，长度 1~128。
	// 允许字符：字母、数字、下划线、连字符、点号、冒号。
	reWorkspace = regexp.MustCompile(`^[a-zA-Z0-9_\-.:]{1,128}$`)
)

// UpsertMemoryInput 定义新增/更新记忆所需参数。
type UpsertMemoryInput struct {
	// Workspace 逻辑工作区；为空时会回退为 default。
	Workspace string
	// Category 记忆分类，必须在 allowedMemoryCategories 白名单内。
	Category string
	// Content 记忆正文，不能为空。
	Content string
	// Keywords 关键词列表，服务层会做去重、规整并限制数量。
	Keywords []string
	// Confidence 置信度，合法区间 (0,1]；不合法时回退到 0.8。
	Confidence float64
	// Source 记录来源，如 manual/agent；为空时默认 manual。
	Source string
}

// SearchMemoryInput 定义记忆检索参数。
type SearchMemoryInput struct {
	// Workspace 检索所属工作区；为空时回退为 default。
	Workspace string
	// Categories 分类过滤器；非法分类会被自动忽略。
	Categories []string
	// Query 检索文本；会进行轻量规整 compactMemoryQuery。
	Query string
	// Limit 返回条数（服务层会收敛到 1~20，默认 5）。
	Limit int
}

// MemoryService 提供“记忆”模块的核心能力。
type MemoryService interface {
	// Upsert 按内容哈希幂等写入（存在则更新，不存在则新增）。
	Upsert(ctx context.Context, userID uint, input UpsertMemoryInput) (*model.MemoryEntry, error)
	// Search 在指定 workspace + categories 中检索用户记忆。
	Search(ctx context.Context, userID uint, input SearchMemoryInput) ([]model.MemoryEntry, error)
	// ListByCategory 列出某工作区某分类下的记忆条目。
	ListByCategory(ctx context.Context, userID uint, workspace string, category string, limit int) ([]model.MemoryEntry, error)
	// BuildContext 将记忆条目组装为可直接注入 Prompt 的文本块。
	BuildContext(entries []model.MemoryEntry) string
	// CleanupLowValue 清理低价值/过期记忆。
	CleanupLowValue(ctx context.Context) (int64, error)
}

// memoryService 是 MemoryService 的默认实现。
type memoryService struct {
	repo            repository.MemoryRepository
	cfg             config.MemoryConfig
	embeddingClient embedding.Client
}

// NewMemoryService 创建 MemoryService。
func NewMemoryService(repo repository.MemoryRepository, cfg config.MemoryConfig, embeddingClient embedding.Client) MemoryService {
	return &memoryService{repo: repo, cfg: cfg, embeddingClient: embeddingClient}
}

// Upsert 对单条记忆做标准化、校验与幂等写入。
//
// 处理流程：
// 1) 归一化并校验 workspace/category；
// 2) 规整 content/keywords/confidence/source；
// 3) 使用 workspace|category|content 计算 MD5 作为幂等键；
// 4) 调用仓储层 UpsertByHash 落库。
func (s *memoryService) Upsert(ctx context.Context, userID uint, input UpsertMemoryInput) (*model.MemoryEntry, error) {
	// workspace 为空时回退 default，并进行格式校验。
	workspace := normalizeWorkspace(input.Workspace)
	if err := validateWorkspace(workspace); err != nil {
		return nil, err
	}

	// category 必须在白名单中。
	category, err := normalizeCategory(input.Category)
	if err != nil {
		return nil, err
	}

	// content 是记忆核心内容，不能为空。
	content := strings.TrimSpace(input.Content)
	if content == "" {
		return nil, fmt.Errorf("content cannot be empty")
	}

	// keywords 去重、转小写并限制数量。
	keywords := normalizeKeywords(input.Keywords, 10)

	// 置信度超出范围时回退默认值，避免脏数据。
	confidence := input.Confidence
	if confidence <= 0 || confidence > 1 {
		confidence = 0.8
	}

	// source 为空时标记为 manual，便于后续审计来源。
	source := strings.TrimSpace(input.Source)
	if source == "" {
		source = "manual"
	}

	// 用“工作区 + 分类 + 内容”计算哈希，实现语义相同内容的幂等写入。
	hash := md5.Sum([]byte(workspace + "|" + category + "|" + content))
	entry := &model.MemoryEntry{
		UserID:      userID,
		Workspace:   workspace,
		Category:    category,
		Content:     content,
		Keywords:    strings.Join(keywords, ","),
		Confidence:  confidence,
		Source:      source,
		ContentHash: fmt.Sprintf("%x", hash[:]),
	}
	if err := s.repo.UpsertByHash(ctx, entry); err != nil {
		return nil, err
	}

	if s.embeddingClient != nil {
		vec, err := s.embeddingClient.CreateEmbedding(ctx, content)
		if err != nil {
			log.Warnf("[MemoryService] 生成记忆向量失败: %v", err)
		} else {
			entry.ContentVec = vec
			if updateErr := s.repo.UpdateContentVector(ctx, entry.ID, vec); updateErr != nil {
				log.Warnf("[MemoryService] 更新记忆向量到数据库失败: %v", updateErr)
			}
			memDoc := model.MemoryEsDocument{
				MemoryID:   entry.ID,
				UserID:     entry.UserID,
				Workspace:  entry.Workspace,
				Category:   entry.Category,
				Content:    entry.Content,
				Keywords:   entry.Keywords,
				Confidence: entry.Confidence,
				Vector:     vec,
				UpdatedAt:  entry.UpdatedAt.Unix(),
			}
			if indexErr := es.IndexMemoryVector(ctx, memDoc); indexErr != nil {
				log.Warnf("[MemoryService] 索引记忆向量到ES失败: %v", indexErr)
			}
		}
	}

	return entry, nil
}

// Search 按用户维度检索记忆，并做输入清洗与限流控制。
// 采用混合检索策略：向量 kNN 检索 + 关键词 BM25 检索，使用 RRF 融合。
func (s *memoryService) Search(ctx context.Context, userID uint, input SearchMemoryInput) ([]model.MemoryEntry, error) {
	workspace := normalizeWorkspace(input.Workspace)
	if err := validateWorkspace(workspace); err != nil {
		return nil, err
	}

	categories := make([]string, 0, len(input.Categories))
	for _, c := range input.Categories {
		nc, err := normalizeCategory(c)
		if err != nil {
			continue
		}
		categories = append(categories, nc)
	}

	query := strings.TrimSpace(input.Query)
	query = compactMemoryQuery(query)

	limit := input.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	fetchLimit := limit * 4
	if fetchLimit < 20 {
		fetchLimit = 20
	}
	if fetchLimit > 80 {
		fetchLimit = 80
	}

	if s.embeddingClient != nil && query != "" {
		return s.hybridSearch(ctx, userID, workspace, categories, query, limit, fetchLimit)
	}

	candidates, err := s.repo.Search(ctx, userID, workspace, categories, query, fetchLimit)
	if err != nil {
		return nil, err
	}

	ranked := s.applyDecayAndRank(candidates, limit)
	s.updateHitBoost(ranked)
	return ranked, nil
}

func (s *memoryService) hybridSearch(ctx context.Context, userID uint, workspace string, categories []string, query string, limit int, fetchLimit int) ([]model.MemoryEntry, error) {
	queryVector, err := s.embeddingClient.CreateEmbedding(ctx, query)
	if err != nil {
		log.Warnf("[MemoryService] 生成查询向量失败，回退到关键词检索: %v", err)
		candidates, err := s.repo.Search(ctx, userID, workspace, categories, query, fetchLimit)
		if err != nil {
			return nil, err
		}
		ranked := s.applyDecayAndRank(candidates, limit)
		s.updateHitBoost(ranked)
		return ranked, nil
	}

	knnResults, err := es.SearchMemoryVectors(ctx, queryVector, userID, workspace, categories, fetchLimit)
	if err != nil {
		log.Warnf("[MemoryService] 向量检索失败，回退到关键词检索: %v", err)
		candidates, err := s.repo.Search(ctx, userID, workspace, categories, query, fetchLimit)
		if err != nil {
			return nil, err
		}
		ranked := s.applyDecayAndRank(candidates, limit)
		s.updateHitBoost(ranked)
		return ranked, nil
	}

	bm25Candidates, err := s.repo.Search(ctx, userID, workspace, categories, query, fetchLimit)
	if err != nil {
		log.Warnf("[MemoryService] 关键词检索失败: %v", err)
		return s.knnResultsToMemoryEntries(ctx, knnResults, limit)
	}

	fused := s.fuseResults(knnResults, bm25Candidates, limit)
	ranked := s.applyDecayAndRank(fused, limit)
	s.updateHitBoost(ranked)
	return ranked, nil
}

func (s *memoryService) knnResultsToMemoryEntries(ctx context.Context, knnResults []es.MemorySearchResult, limit int) ([]model.MemoryEntry, error) {
	if len(knnResults) == 0 {
		return []model.MemoryEntry{}, nil
	}
	ids := make([]uint, 0, len(knnResults))
	for _, r := range knnResults {
		ids = append(ids, r.MemoryID)
	}
	entries, err := s.repo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	entryMap := make(map[uint]model.MemoryEntry, len(entries))
	for _, e := range entries {
		entryMap[e.ID] = e
	}
	result := make([]model.MemoryEntry, 0, len(knnResults))
	for _, r := range knnResults {
		if e, ok := entryMap[r.MemoryID]; ok {
			result = append(result, e)
			if len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

const rrfConstant = 60

func (s *memoryService) fuseResults(knnResults []es.MemorySearchResult, bm25Candidates []model.MemoryEntry, limit int) []model.MemoryEntry {
	type scoredEntry struct {
		entry model.MemoryEntry
		score float64
	}

	knnRank := make(map[uint]int)
	for i, r := range knnResults {
		knnRank[r.MemoryID] = i + 1
	}

	bm25Rank := make(map[uint]int)
	for i, e := range bm25Candidates {
		bm25Rank[e.ID] = i + 1
	}

	bm25Map := make(map[uint]model.MemoryEntry)
	for _, e := range bm25Candidates {
		bm25Map[e.ID] = e
	}

	allIDs := make(map[uint]bool)
	for _, r := range knnResults {
		allIDs[r.MemoryID] = true
	}
	for _, e := range bm25Candidates {
		allIDs[e.ID] = true
	}

	scored := make([]scoredEntry, 0, len(allIDs))
	for id := range allIDs {
		entry, ok := bm25Map[id]
		if !ok {
			continue
		}
		knnPos := knnRank[id]
		bm25Pos := bm25Rank[id]

		var rrfScore float64
		if knnPos > 0 {
			rrfScore += 1.0 / (rrfConstant + float64(knnPos))
		}
		if bm25Pos > 0 {
			rrfScore += 1.0 / (rrfConstant + float64(bm25Pos))
		}

		scored = append(scored, scoredEntry{entry: entry, score: rrfScore})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	result := make([]model.MemoryEntry, 0, limit)
	for _, se := range scored {
		result = append(result, se.entry)
		if len(result) >= limit {
			break
		}
	}
	return result
}

func (s *memoryService) updateHitBoost(ranked []model.MemoryEntry) {
	if len(ranked) == 0 {
		return
	}
	ids := make([]uint, 0, len(ranked))
	for _, item := range ranked {
		ids = append(ids, item.ID)
	}
	boost := s.cfg.HitBoost
	if boost == 0 {
		boost = 0.02
	}
	go func(ids []uint, delta float64) {
		if err := s.repo.BoostConfidence(context.Background(), ids, delta); err != nil {
			log.Warnf("[MemoryService] 提升命中记忆置信度失败: %v", err)
		}
	}(ids, boost)
}

// ListByCategory 列举指定用户在某 workspace/category 下的记忆。
func (s *memoryService) ListByCategory(ctx context.Context, userID uint, workspace string, category string, limit int) ([]model.MemoryEntry, error) {
	workspace = normalizeWorkspace(workspace)
	if err := validateWorkspace(workspace); err != nil {
		return nil, err
	}
	nc, err := normalizeCategory(category)
	if err != nil {
		return nil, err
	}
	return s.repo.ListByCategory(ctx, userID, workspace, nc, limit)
}

// BuildContext 将检索出的记忆拼装成可读上下文，供上层注入模型 Prompt。
func (s *memoryService) BuildContext(entries []model.MemoryEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Memory Context\n")
	for i, item := range entries {
		b.WriteString(fmt.Sprintf("[%d][%s] %s\n", i+1, item.Category, item.Content))
	}
	return strings.TrimSpace(b.String())
}

func (s *memoryService) CleanupLowValue(ctx context.Context) (int64, error) {
	days := s.cfg.CleanupOlderThanDays
	if days <= 0 {
		days = 90
	}
	minConfidence := s.cfg.CleanupMinConfidence
	if minConfidence <= 0 {
		minConfidence = 0.2
	}
	return s.repo.CleanupLowValue(ctx, days, minConfidence)
}

func (s *memoryService) applyDecayAndRank(items []model.MemoryEntry, limit int) []model.MemoryEntry {
	if len(items) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 5
	}
	halfLifeHours := s.cfg.DecayHalfLifeHours
	if halfLifeHours <= 0 {
		halfLifeHours = 336 // 14 天
	}
	minEffective := s.cfg.MinEffectiveScore
	if minEffective <= 0 {
		minEffective = 0.25
	}

	type scoredItem struct {
		entry model.MemoryEntry
		score float64
	}
	scored := make([]scoredItem, 0, len(items))
	now := time.Now()
	for _, item := range items {
		ageHours := now.Sub(item.UpdatedAt).Hours()
		if ageHours < 0 {
			ageHours = 0
		}
		decay := math.Pow(0.5, ageHours/float64(halfLifeHours))
		effective := item.Confidence * decay
		if effective < minEffective {
			continue
		}
		scored = append(scored, scoredItem{entry: item, score: effective})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]model.MemoryEntry, 0, len(scored))
	for _, it := range scored {
		out = append(out, it.entry)
	}
	return out
}

// normalizeCategory 将分类统一为小写并校验白名单。
func normalizeCategory(category string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(category))
	if c == "" {
		return "", fmt.Errorf("category cannot be empty")
	}
	if _, ok := allowedMemoryCategories[c]; !ok {
		return "", fmt.Errorf("unsupported category: %s", category)
	}
	return c, nil
}

// normalizeWorkspace 处理 workspace 默认值逻辑。
func normalizeWorkspace(workspace string) string {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		return "default"
	}
	return ws
}

// validateWorkspace 校验 workspace 格式合法性。
// 约定：
// - "global" 是保留的跨空间标识，允许直接通过；
// - 其他 workspace 必须满足 reWorkspace 正则。
func validateWorkspace(workspace string) error {
	if workspace == "" {
		return fmt.Errorf("workspace cannot be empty")
	}
	if workspace == "global" {
		return nil
	}
	if !reWorkspace.MatchString(workspace) {
		return fmt.Errorf("invalid workspace")
	}
	return nil
}

// normalizeKeywords 对关键词进行标准化：
// - 去空白
// - 小写归一
// - 去重
// - 最多保留 maxCount 个
func normalizeKeywords(keywords []string, maxCount int) []string {
	if maxCount <= 0 {
		maxCount = 10
	}
	seen := make(map[string]struct{}, len(keywords))
	out := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		n := strings.ToLower(strings.TrimSpace(kw))
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
		if len(out) >= maxCount {
			break
		}
	}
	return out
}

// compactMemoryQuery 对多行 query 做“轻量压缩”：
// - 统一换行符；
// - 去掉“当前问题：/相关上下文：”等模板前缀；
// - 取前两段有效内容拼接，降低检索噪声。
func compactMemoryQuery(query string) string {
	q := strings.TrimSpace(query)
	if q == "" {
		return q
	}
	q = strings.ReplaceAll(q, "\r\n", "\n")
	lines := strings.Split(q, "\n")
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "当前问题：")
		line = strings.TrimPrefix(line, "相关上下文：")
		if line == "" {
			continue
		}
		parts = append(parts, line)
		if len(parts) >= 2 {
			break
		}
	}
	if len(parts) == 0 {
		return q
	}
	return strings.Join(parts, " ")
}
