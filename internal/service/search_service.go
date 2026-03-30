// Package service 提供了搜索相关的业务逻辑。
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/embedding"
	"pai-smart-go/pkg/llm"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/reranker"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elastic/go-elasticsearch/v8"
)

// 包级变量，避免每次调用 normalizeQuery 时重复编译正则
var (
	reKeep  = regexp.MustCompile(`[^\p{Han}a-z0-9\s]+`)
	reSpace = regexp.MustCompile(`\s+`)
)

// SearchService 接口定义了搜索操作。
type SearchService interface {
	HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error)
}

type searchService struct {
	embeddingClient embedding.Client
	llmClient       llm.Client
	rerankerClient  reranker.Client
	esClient        *elasticsearch.Client
	userService     UserService
	uploadRepo      repository.UploadRepository
	metricsService  MetricsService
	searchCfg       config.SearchConfig
	indexName       string
}

// NewSearchService 创建一个新的 SearchService 实例。
func NewSearchService(
	embeddingClient embedding.Client,
	llmClient llm.Client,
	rerankerClient reranker.Client,
	esClient *elasticsearch.Client,
	userService UserService,
	uploadRepo repository.UploadRepository,
	metricsService MetricsService,
	searchCfg config.SearchConfig,
	indexName string,
) SearchService {
	if strings.TrimSpace(indexName) == "" {
		indexName = "knowledge_base"
	}
	return &searchService{
		embeddingClient: embeddingClient,
		llmClient:       llmClient,
		rerankerClient:  rerankerClient,
		esClient:        esClient,
		userService:     userService,
		uploadRepo:      uploadRepo,
		metricsService:  metricsService,
		searchCfg:       searchCfg,
		indexName:       indexName,
	}
}

// HybridSearch 执行"两阶段混合检索"（RRF 版本）：
//  1. kNN + BM25 两路独立召回，由 Elasticsearch RRF 机制融合排名；
//  2. 权限过滤（本人 / 公共 / 同组织）分别下沉到每路检索，确保 kNN 阶段不越权；
//  3. 首次无命中时，用核心短语（phrase）降级重试一次；
//  4. 最终补齐文件名，组装响应 DTO 返回给上层。
func (s *searchService) HybridSearch(ctx context.Context, query string, topK int, user *model.User) ([]model.SearchResponseDTO, error) {
	log.Infof("[SearchService] 开始执行混合搜索, query: '%s', topK: %d, user: %s", query, topK, user.Username)

	trimmedQuery := strings.TrimSpace(query)
	if trimmedQuery == "" {
		return []model.SearchResponseDTO{}, nil
	}

	rewrittenQuery := s.rewriteQueryIfEnabled(ctx, trimmedQuery)
	if rewrittenQuery != trimmedQuery {
		log.Infof("[SearchService] Query Rewriting 生效: '%s' -> '%s'", trimmedQuery, rewrittenQuery)
	}
	if s.searchCfg.QueryRewriteEnabled && s.metricsService != nil {
		s.metricsService.RecordQueryRewrite(rewrittenQuery != trimmedQuery)
	}

	// 1) 计算用户"有效组织标签"（包含父级标签）。
	log.Info("[SearchService] 步骤1: 获取用户有效组织标签")
	userEffectiveTags, err := s.userService.GetUserEffectiveOrgTags(user)
	if err != nil {
		log.Errorf("[SearchService] 获取用户有效组织标签失败: %v", err)
		// 容错：标签获取失败不中断主流程，仍有 user_id / is_public 两条权限路径。
		userEffectiveTags = []string{}
	}
	log.Infof("[SearchService] 获取到 %d 个有效组织标签: %v", len(userEffectiveTags), userEffectiveTags)

	// 2) 轻量归一化：
	//    - normalized：去除口语化词汇后的干净查询词，用于 BM25 must 匹配；
	//    - phrase：从 normalized 中进一步提取的核心短语，用于 match_phrase 兜底及降级重试。
	normalized, phrase := normalizeQuery(rewrittenQuery)
	if normalized != rewrittenQuery {
		log.Infof("[SearchService] 规范化查询: '%s' -> normalized='%s', phrase='%s'", rewrittenQuery, normalized, phrase)
	}

	// 3) 使用改写后的 query（非 normalized）生成查询向量，最大保留语义信息。
	log.Info("[SearchService] 步骤2: 开始向量化查询")
	queryVector, err := s.embeddingClient.CreateEmbedding(ctx, rewrittenQuery)
	if err != nil {
		log.Errorf("[SearchService] 向量化查询失败: %v", err)
		return nil, fmt.Errorf("failed to create query embedding: %w", err)
	}
	log.Infof("[SearchService] 步骤2: 向量化成功, 向量维度: %d", len(queryVector))

	// 4) 在 ES 8.10 兼容模式下执行“两路召回 + Go 内存 RRF 融合”。
	//    不使用较新版本才支持的 retriever/rrf DSL，避免 8.10.4 报错。
	log.Info("[SearchService] 步骤3: 执行双路召回并在本地做 RRF 融合")
	recallTopK := s.resolveRecallTopK(topK)
	results, err := s.execHybridSearch(ctx, queryVector, normalized, phrase, recallTopK, user, userEffectiveTags)
	if err != nil {
		return nil, err
	}

	// 5) 降级重试：首次无命中且存在可用短语时，用 phrase 替换 normalized 再查一次。
	//    场景：口语化问句 normalized 结果较长，must operator=or 仍匹配不到文档；
	//    用更短的核心 phrase 提升召回率。
	if len(results) == 0 && phrase != "" && phrase != normalized {
		log.Infof("[SearchService] 首次无命中，使用核心短语重试: '%s'", phrase)
		results, err = s.execHybridSearch(ctx, queryVector, phrase, phrase, recallTopK, user, userEffectiveTags)
		if err != nil {
			// 重试失败记录 Warn，不中断主流程，返回空结果。
			log.Warnf("[SearchService] 降级重试失败: %v", err)
			return []model.SearchResponseDTO{}, nil
		}
		log.Infof("[SearchService] 重试后命中 %d 条", len(results))
	}

	if len(results) == 0 {
		return []model.SearchResponseDTO{}, nil
	}

	// 6) 批量补齐文件名（ES 命中里只有 file_md5，需回表查询）。
	log.Info("[SearchService] 步骤6: 批量获取文件名")
	fileNameMap, err := s.fetchFileNames(results)
	if err != nil {
		return nil, err
	}

	// 7) 组装响应 DTO。
	log.Info("[SearchService] 步骤7: 组装最终响应 DTO")
	// 预分配切片，避免多次扩容。
	dtos := make([]model.SearchResponseDTO, 0, len(results))
	for _, hit := range results {
		fileName := fileNameMap[hit.Source.FileMD5]
		if fileName == "" {
			log.Warnf("[SearchService] 未找到 FileMD5 '%s' 对应的文件名, 使用 '未知文件'", hit.Source.FileMD5)
			fileName = "未知文件"
		}
		dtos = append(dtos, model.SearchResponseDTO{
			FileMD5:     hit.Source.FileMD5,
			FileName:    fileName,
			ChunkID:     hit.Source.ChunkID,
			TextContent: hit.Source.TextContent,
			Score:       hit.Score,
			UserID:      strconv.FormatUint(uint64(hit.Source.UserID), 10),
			OrgTag:      hit.Source.OrgTag,
			IsPublic:    hit.Source.IsPublic,
		})
	}

	if s.searchCfg.RerankEnabled {
		dtos = s.rerankWithFallback(ctx, rewrittenQuery, dtos, topK)
	} else if len(dtos) > topK && topK > 0 {
		dtos = dtos[:topK]
	}

	log.Infof("[SearchService] 混合搜索完毕, 返回 %d 条结果, query: '%s'", len(dtos), rewrittenQuery)
	return dtos, nil
}

func (s *searchService) resolveRecallTopK(topK int) int {
	if topK <= 0 {
		topK = 10
	}
	if !s.searchCfg.RerankEnabled {
		return topK
	}
	candidate := s.searchCfg.RerankCandidateK
	if candidate <= 0 {
		candidate = topK * 3
	}
	if candidate < topK {
		candidate = topK
	}
	// 给 ES 召回一个合理上限，避免过大开销。
	if candidate > 80 {
		candidate = 80
	}
	return candidate
}

// buildKNNQuery 构造 ES 8.10 兼容的 kNN 查询 DSL。
func buildKNNQuery(
	queryVector []float32,
	topK int,
	user *model.User,
	orgTags []string,
) map[string]interface{} {
	permFilter := buildPermissionFilter(user, orgTags)

	k := topK * 10
	if k < topK {
		k = topK
	}
	if k < 20 {
		k = 20
	}

	return map[string]interface{}{
		"knn": map[string]interface{}{
			"field":          "vector",
			"query_vector":   queryVector,
			"k":              k,
			"num_candidates": k,
			"filter":         permFilter,
		},
		"size": topK,
	}
}

// buildBM25Query 构造 ES 8.10 兼容的 BM25 查询 DSL。
func buildBM25Query(
	normalized string,
	phrase string,
	topK int,
	user *model.User,
	orgTags []string,
) map[string]interface{} {
	permFilter := buildPermissionFilter(user, orgTags)

	return map[string]interface{}{
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				// must：规范化问句文本匹配，抑制纯向量噪声命中。
				"must": map[string]interface{}{
					"match": map[string]interface{}{
						"text_content": normalized,
					},
				},
				// filter：权限过滤，不参与打分。
				"filter": permFilter,
				// should：精确短语兜底，命中后获得更高 BM25 分。
				"should": buildPhraseShould(phrase),
			},
		},
		"size": topK,
	}
}

// buildPermissionFilter 构造权限过滤子句（不参与打分）。
// 满足以下任一条件即可通过：
//   - user_id 匹配当前用户（私有文档）
//   - is_public = true（公共文档）
//   - org_tag 属于用户有效组织集合（组织内共享）
func buildPermissionFilter(user *model.User, orgTags []string) map[string]interface{} {
	return map[string]interface{}{
		"bool": map[string]interface{}{
			"should": []map[string]interface{}{
				{"term": map[string]interface{}{"user_id": user.ID}},
				{"term": map[string]interface{}{"is_public": true}},
				{"terms": map[string]interface{}{"org_tag": orgTags}},
			},
			"minimum_should_match": 1,
		},
	}
}

// esHit 是 ES 命中结果的内部表示，仅包含业务关心的字段。
type esHit struct {
	Source model.EsDocument
	Score  float64
}

const defaultRRFConstant = 60.0

// execHybridSearch 兼容 ES 8.10：分别执行 kNN 与 BM25 检索，然后在 Go 内存中用 RRF 融合。
func (s *searchService) execHybridSearch(
	ctx context.Context,
	queryVector []float32,
	normalized string,
	phrase string,
	topK int,
	user *model.User,
	orgTags []string,
) ([]esHit, error) {
	knnQuery := buildKNNQuery(queryVector, topK, user, orgTags)
	knnHits, err := s.execSearch(ctx, knnQuery)
	if err != nil {
		return nil, err
	}

	bm25Query := buildBM25Query(normalized, phrase, topK, user, orgTags)
	bm25Hits, err := s.execSearch(ctx, bm25Query)
	if err != nil {
		return nil, err
	}

	return fuseRRFHits(knnHits, bm25Hits, topK, defaultRRFConstant), nil
}

// execSearch 序列化 DSL、发送请求并解析命中列表。
func (s *searchService) execSearch(ctx context.Context, esQuery map[string]interface{}) ([]esHit, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(esQuery); err != nil {
		return nil, fmt.Errorf("failed to encode es query: %w", err)
	}
	// 🟡 Fix #5: 向量数组体积极大（1536 维 × float32），仅在 Debug 级别输出，
	//            避免生产环境每次请求打印几十 KB 日志。
	//log.Debugf("[SearchService] ES 查询语句: %s", buf.String())

	res, err := s.esClient.Search(
		s.esClient.Search.WithContext(ctx),
		s.esClient.Search.WithIndex(s.indexName),
		s.esClient.Search.WithBody(&buf),
		s.esClient.Search.WithTrackTotalHits(true),
	)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch search failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		bodyBytes, _ := io.ReadAll(res.Body)
		log.Errorf("[SearchService] Elasticsearch 返回错误, status: %s, body: %s", res.Status(), string(bodyBytes))
		return nil, fmt.Errorf("elasticsearch returned an error: %s", res.Status())
	}

	var esResponse struct {
		Hits struct {
			Hits []struct {
				Source model.EsDocument `json:"_source"`
				Score  float64          `json:"_score"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(res.Body).Decode(&esResponse); err != nil {
		return nil, fmt.Errorf("failed to decode es response: %w", err)
	}

	hits := make([]esHit, 0, len(esResponse.Hits.Hits))
	for _, h := range esResponse.Hits.Hits {
		hits = append(hits, esHit{Source: h.Source, Score: h.Score})
	}
	return hits, nil
}

// fuseRRFHits 使用 Reciprocal Rank Fusion 融合两路结果。
// score = Σ 1 / (k + rank)
func fuseRRFHits(knnHits, bm25Hits []esHit, topK int, rankConstant float64) []esHit {
	if topK <= 0 {
		topK = len(knnHits) + len(bm25Hits)
	}

	type fused struct {
		hit       esHit
		rrfScore  float64
		bestScore float64
	}

	keyOf := func(hit esHit) string {
		return hit.Source.VectorID
	}

	fusedMap := make(map[string]*fused, len(knnHits)+len(bm25Hits))

	merge := func(hits []esHit) {
		for idx, hit := range hits {
			key := keyOf(hit)
			item, ok := fusedMap[key]
			if !ok {
				item = &fused{hit: hit, bestScore: hit.Score}
				fusedMap[key] = item
			}
			rank := float64(idx + 1)
			item.rrfScore += 1.0 / (rankConstant + rank)
			if hit.Score > item.bestScore {
				item.bestScore = hit.Score
				item.hit = hit
			}
		}
	}

	merge(knnHits)
	merge(bm25Hits)

	fusedHits := make([]fused, 0, len(fusedMap))
	for _, item := range fusedMap {
		item.hit.Score = item.rrfScore
		fusedHits = append(fusedHits, *item)
	}

	sort.SliceStable(fusedHits, func(i, j int) bool {
		if math.Abs(fusedHits[i].rrfScore-fusedHits[j].rrfScore) > 1e-12 {
			return fusedHits[i].rrfScore > fusedHits[j].rrfScore
		}
		return fusedHits[i].bestScore > fusedHits[j].bestScore
	})

	if len(fusedHits) > topK {
		fusedHits = fusedHits[:topK]
	}

	out := make([]esHit, 0, len(fusedHits))
	for _, item := range fusedHits {
		out = append(out, item.hit)
	}
	return out
}

// fetchFileNames 根据命中列表中的 FileMD5 批量回表查询文件名。
func (s *searchService) fetchFileNames(hits []esHit) (map[string]string, error) {
	// 去重，减少 IN 查询长度。
	seen := make(map[string]struct{}, len(hits))
	md5List := make([]string, 0, len(hits))
	for _, h := range hits {
		if _, ok := seen[h.Source.FileMD5]; !ok {
			seen[h.Source.FileMD5] = struct{}{}
			md5List = append(md5List, h.Source.FileMD5)
		}
	}

	fileInfos, err := s.uploadRepo.FindBatchByMD5s(md5List)
	if err != nil {
		return nil, fmt.Errorf("批量查询文件信息失败: %w", err)
	}

	nameMap := make(map[string]string, len(fileInfos))
	for _, info := range fileInfos {
		nameMap[info.FileMD5] = info.FileName
	}
	log.Infof("[SearchService] 批量获取文件名成功, 共 %d 条", len(nameMap))
	return nameMap, nil
}

// normalizeQuery 对用户查询做轻量去噪与短语提取。
//
// 返回值：
//   - normalized：去除口语化词汇后的查询词，用于 BM25 must 匹配与降级重试；
//   - phrase：在 normalized 基础上进一步压缩的核心短语，用于 match_phrase 兜底。
//     当前实现：取 normalized 中最长的连续中文词块作为 phrase，
//     若无中文则退化为 normalized 本身。

func normalizeQuery(q string) (normalized, phrase string) {
	if q == "" {
		return q, ""
	}
	lower := strings.ToLower(q)

	// 去除常见口语/功能词。
	stopPhrases := []string{
		"是谁", "是什么", "是啥", "请问", "怎么", "如何",
		"告诉我", "严格", "按照", "不要补充", "的区别", "区别",
		"吗", "呢", "？", "?",
	}
	for _, sp := range stopPhrases {
		lower = strings.ReplaceAll(lower, sp, " ")
	}

	// 仅保留中文、英文、数字与空白。
	kept := reKeep.ReplaceAllString(lower, " ")
	kept = strings.TrimSpace(reSpace.ReplaceAllString(kept, " "))
	if kept == "" {
		return q, ""
	}
	normalized = kept

	// 提取最长连续中文词块作为 phrase（不含英文/数字片段）。
	// 若 normalized 本身全为英文/数字，则 phrase = normalized。
	reChinese := regexp.MustCompile(`\p{Han}+`)
	chineseBlocks := reChinese.FindAllString(normalized, -1)
	if len(chineseBlocks) == 0 {
		phrase = normalized
		return
	}
	longest := ""
	for _, block := range chineseBlocks {
		if len([]rune(block)) > len([]rune(longest)) {
			longest = block
		}
	}
	phrase = longest
	return
}

// buildPhraseShould 构建 match_phrase should 子句（带 boost），phrase 为空则返回 nil。
func buildPhraseShould(phrase string) interface{} {
	if phrase == "" {
		return nil
	}
	return []map[string]interface{}{
		{
			"match_phrase": map[string]interface{}{
				"text_content": map[string]interface{}{
					"query": phrase,
					"boost": 3.0,
				},
			},
		},
	}
}

func (s *searchService) rerankWithFallback(ctx context.Context, query string, docs []model.SearchResponseDTO, topK int) []model.SearchResponseDTO {
	if s.rerankerClient != nil && s.searchCfg.ExternalRerankerEnabled {
		ranked, err := s.rerankByExternal(ctx, query, docs, topK)
		if err == nil && len(ranked) > 0 {
			return ranked
		}
		if err != nil {
			log.Warnf("[SearchService] 外部 Reranker 失败，回退本地重排: %v", err)
		}
	}
	return rerankSearchResults(query, docs, topK)
}

// rerankByExternal 使用外部 Reranker 服务对候选文档进行二次排序。
//
// 主要流程：
// 1) 校验依赖与输入参数（client/topK/docs）；
// 2) 创建带超时的子上下文，避免远端服务拖慢主链路；
// 3) 将候选文档转换为 reranker.Document（保留原索引 ID 以便映射回原结果）；
// 4) 调用远端重排并按返回顺序组装最终结果。
func (s *searchService) rerankByExternal(ctx context.Context, query string, docs []model.SearchResponseDTO, topK int) ([]model.SearchResponseDTO, error) {
	// 未配置外部重排客户端时，直接返回错误，由上层决定降级策略。
	if s.rerankerClient == nil {
		return nil, fmt.Errorf("reranker client is nil")
	}
	// 空候选直接返回空列表，避免无意义远程调用。
	if len(docs) == 0 {
		return []model.SearchResponseDTO{}, nil
	}
	// topK 归一化：非法或越界时回退为全部候选。
	if topK <= 0 || topK > len(docs) {
		topK = len(docs)
	}

	// 外部服务调用超时配置（秒），默认 5 秒。
	timeoutS := s.searchCfg.ExternalRerankerTimeoutS
	if timeoutS <= 0 {
		timeoutS = 5
	}
	// 子上下文隔离超时，避免影响上层 ctx 生命周期。
	rCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS)*time.Second)
	defer cancel()

	// 组装远端输入：ID 保存原 docs 下标，便于回填映射。
	input := make([]reranker.Document, 0, len(docs))
	for i, d := range docs {
		text := strings.TrimSpace(d.TextContent)
		// 防止单文档过长撑爆远端服务输入。
		if len(text) > 1500 {
			text = text[:1500]
		}
		input = append(input, reranker.Document{ID: i, Text: text})
	}

	// 执行远端重排；失败由调用方决定是否降级到本地重排。
	results, err := s.rerankerClient.Rerank(rCtx, query, input, topK)
	if err != nil {
		return nil, err
	}
	// 将重排结果映射回原始文档结构，并裁剪到 topK。
	return applyExternalRerankResult(docs, results, topK), nil
}

func applyExternalRerankResult(docs []model.SearchResponseDTO, results []reranker.Result, topK int) []model.SearchResponseDTO {
	if len(docs) == 0 {
		return docs
	}
	if topK <= 0 || topK > len(docs) {
		topK = len(docs)
	}
	if len(results) == 0 {
		if len(docs) > topK {
			return docs[:topK]
		}
		return docs
	}

	used := make(map[int]struct{}, len(results))
	ranked := make([]model.SearchResponseDTO, 0, topK)
	for _, r := range results {
		if len(ranked) >= topK {
			break
		}
		if r.ID < 0 || r.ID >= len(docs) {
			continue
		}
		if _, ok := used[r.ID]; ok {
			continue
		}
		used[r.ID] = struct{}{}

		doc := docs[r.ID]
		doc.Score = r.Score
		ranked = append(ranked, doc)
	}

	// 远端返回不足 topK 时，回补原始结果，保证条数稳定。
	if len(ranked) < topK {
		for i := 0; i < len(docs) && len(ranked) < topK; i++ {
			if _, ok := used[i]; ok {
				continue
			}
			ranked = append(ranked, docs[i])
		}
	}
	return ranked
}

// rerankSearchResults 对初召回结果做轻量重排，融合三类信号：
// 1) 语义分（semantic）：来自 ES/RRF 的原始分，经 logistic 归一化；
// 2) 词项重叠（overlap）：query token 与文档文本命中比例；
// 3) 短语命中（phraseBoost）：核心短语完整出现时给予额外加分。
//
// 最终分计算：final = semantic*0.6 + overlap*0.3 + phraseBoost*0.1
// 并按最终分降序返回前 topK 条。
func rerankSearchResults(query string, docs []model.SearchResponseDTO, topK int) []model.SearchResponseDTO {
	// 空输入直接返回，避免后续不必要计算。
	if len(docs) == 0 {
		return docs
	}
	// topK 兜底：<=0 视为“返回全部”；>len(docs) 时截到上限。
	if topK <= 0 {
		topK = len(docs)
	}
	if topK > len(docs) {
		topK = len(docs)
	}

	normalized, phrase := normalizeQuery(query)
	tokens := splitQueryTokens(normalized)

	// 内部结构：保存原文档与重排分，避免覆盖原切片。
	type scored struct {
		doc   model.SearchResponseDTO
		score float64
	}
	scoredDocs := make([]scored, 0, len(docs))

	// 先找分数边界
	minScore, maxScore := docs[0].Score, docs[0].Score
	for _, d := range docs {
		if d.Score < minScore {
			minScore = d.Score
		}
		if d.Score > maxScore {
			maxScore = d.Score
		}
	}

	scoreRange := maxScore - minScore
	for _, d := range docs {

		// min-max 归一化，保留相对差距
		var semantic float64
		if scoreRange > 1e-9 {
			semantic = (d.Score - minScore) / scoreRange
		} else {
			semantic = 1.0 // 所有分数相同，全部给满分
		}

		text := strings.ToLower(d.TextContent)
		// overlap：token 命中比例，范围 [0,1]。
		overlap := tokenOverlapScore(tokens, text)
		phraseBoost := 0.0
		// phraseBoost：完整短语命中时置为 1，未命中为 0。
		if phrase != "" && strings.Contains(text, strings.ToLower(phrase)) {
			phraseBoost = 1.0
		}

		// 综合分：语义优先，其次词项覆盖，最后短语精确命中加成。
		finalScore := semantic*0.6 + overlap*0.3 + phraseBoost*0.1
		scoredDocs = append(scoredDocs, scored{doc: d, score: finalScore})
	}

	// 稳定排序：分数相同保持原相对顺序，减少结果抖动。
	sort.SliceStable(scoredDocs, func(i, j int) bool {
		return scoredDocs[i].score > scoredDocs[j].score
	})

	// 截取前 topK，并将返回分替换为综合分（便于前端观测重排效果）。
	out := make([]model.SearchResponseDTO, 0, topK)
	for i := 0; i < topK; i++ {
		item := scoredDocs[i].doc
		item.Score = scoredDocs[i].score // 返回重排后的综合分，便于前端观察。
		out = append(out, item)
	}
	return out
}

func splitQueryTokens(q string) []string {
	if q == "" {
		return nil
	}
	parts := strings.Fields(q)
	tokens := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		tokens = append(tokens, strings.ToLower(p))
	}
	return tokens
}

func tokenOverlapScore(tokens []string, text string) float64 {
	if len(tokens) == 0 || text == "" {
		return 0
	}
	hit := 0
	for _, tk := range tokens {
		if strings.Contains(text, tk) {
			hit++
		}
	}
	return float64(hit) / float64(len(tokens))
}

func normalizeSemanticScore(score float64) float64 {
	// logistic 压缩到 0~1，兼容 BM25/RRF 不同尺度。
	return 1.0 / (1.0 + math.Exp(-score))
}

// rewriteQueryIfEnabled 在 HybridSearch 前执行可选的 Query Rewriting。
// 失败时回退到原始 query，保证主流程稳定。
func (s *searchService) rewriteQueryIfEnabled(ctx context.Context, query string) string {
	if !s.searchCfg.QueryRewriteEnabled || s.llmClient == nil {
		return query
	}

	timeoutSeconds := s.searchCfg.QueryRewriteTimeoutS
	if timeoutSeconds <= 0 {
		timeoutSeconds = 3
	}
	maxLength := s.searchCfg.QueryRewriteMaxLength
	if maxLength <= 0 {
		maxLength = 128
	}

	rewriteCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	messages := []llm.Message{
		{
			Role: "system",
			Content: fmt.Sprintf(
				"你是企业知识库检索查询改写器。请把用户问题改写成更适合向量+关键词混合检索的单行查询。\n"+
					"要求：保留实体与关键约束；消解口语指代；不要回答问题；不要解释；最多 %d 个字。",
				maxLength,
			),
		},
		{Role: "user", Content: query},
	}

	resp, err := s.llmClient.ChatWithTools(rewriteCtx, messages, nil, nil)
	if err != nil {
		log.Warnf("[SearchService] Query Rewriting 失败，回退原 query: %v", err)
		return query
	}

	rewritten := sanitizeRewrittenQuery(resp.Content, maxLength)
	if rewritten == "" {
		log.Warnf("[SearchService] Query Rewriting 返回空内容，回退原 query")
		return query
	}
	return rewritten
}

func sanitizeRewrittenQuery(raw string, maxLength int) string {
	if maxLength <= 0 {
		maxLength = 128
	}

	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return ""
	}

	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")

	lines := strings.Split(cleaned, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.Trim(line, "`\"'“”‘’ ")
		line = stripRewritePrefix(line)
		line = strings.Trim(line, "`\"'“”‘’ ")
		if line != "" {
			cleaned = line
			break
		}
	}

	if utf8.RuneCountInString(cleaned) > maxLength {
		cleaned = truncateRunes(cleaned, maxLength)
	}
	return strings.TrimSpace(cleaned)
}

func stripRewritePrefix(line string) string {
	prefixes := []string{
		"改写后的查询：", "改写后的查询:",
		"检索查询：", "检索查询:",
		"query:", "Query:",
		"rewritten query:", "Rewritten Query:",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return line
}

func truncateRunes(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:maxRunes])
}

/*
分数归一化 (Score Normalization)：k-NN 的余弦相似度分数与 BM25 的 TF-IDF 分数处于完全不同的数值量级（BM25 没有上限）。直接按 0.2 和 1.0 结合有时会导致 BM25 分数彻底淹没 k-NN 分数。现代 ES 版本建议采用 RRF (Reciprocal Rank Fusion) 排名融合机制来替代暴力的权重相加。*/
