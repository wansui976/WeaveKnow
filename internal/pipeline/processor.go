// Package pipeline 定义了文件处理的核心流程。
package pipeline

import (
	"WeaveKnow/internal/config"
	"WeaveKnow/internal/model"
	"WeaveKnow/internal/repository"
	"WeaveKnow/pkg/embedding"
	"WeaveKnow/pkg/es"
	"WeaveKnow/pkg/log"
	"WeaveKnow/pkg/storage"
	"WeaveKnow/pkg/tasks"
	"WeaveKnow/pkg/tika"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/minio/minio-go/v7"
)

// Processor 封装了文件处理的所有依赖和逻辑。
// 它的职责是把“已合并文件”从对象存储一路处理到“可检索向量文档”：
// MinIO 文件 -> Tika 抽取文本 -> 分块 -> 入库 -> 向量化 -> 写入 Elasticsearch。
type Processor struct {
	// tikaClient 负责解析不同文件格式（pdf/docx/xlsx/pptx/...）并提取纯文本。
	tikaClient *tika.Client
	// embeddingClient 负责把文本分块转换为向量表示。
	embeddingClient embedding.Client
	// esCfg 指定 ES 索引名等配置。
	esCfg config.ElasticsearchConfig
	// minioCfg 指定对象存储 bucket 等配置。
	minioCfg config.MinIOConfig
	// pipelineCfg 指定分块与并发相关配置。
	pipelineCfg config.PipelineConfig
	// embeddingCfg 用于记录向量模型版本等信息，便于后续检索和排障。
	embeddingCfg config.EmbeddingConfig
	// uploadRepo 负责更新上传主记录（状态等）。
	uploadRepo repository.UploadRepository
	// docVectorRepo 负责分块文本在数据库中的持久化。
	docVectorRepo repository.DocumentVectorRepository
}

// NewProcessor 创建一个新的 Processor 实例（依赖注入）。
// 所有依赖由 main.go 装配后传入，方便替换实现与单元测试。
func NewProcessor(
	tikaClient *tika.Client,
	embeddingClient embedding.Client,
	esCfg config.ElasticsearchConfig,
	minioCfg config.MinIOConfig,
	pipelineCfg config.PipelineConfig,
	embeddingCfg config.EmbeddingConfig,
	uploadRepo repository.UploadRepository,
	docVectorRepo repository.DocumentVectorRepository,
) *Processor {
	return &Processor{
		tikaClient:      tikaClient,
		embeddingClient: embeddingClient,
		esCfg:           esCfg,
		minioCfg:        minioCfg,
		pipelineCfg:     pipelineCfg,
		embeddingCfg:    embeddingCfg,
		uploadRepo:      uploadRepo,
		docVectorRepo:   docVectorRepo,
	}
}

// Process 是文件处理的主函数。
// 输入：一个文件处理任务（包含 fileMD5、fileName、用户与权限信息）。
// 输出：处理成功返回 nil；任一阶段失败返回 error（由上层决定是否重试）。
//
// 处理步骤：
// 1) 从 MinIO 下载 merged 文件；
// 2) 用 Tika 抽取文本；
// 3) 按窗口切块；
// 4) 分块先落库（document_vectors）；
// 5) 逐块向量化并写入 ES。
func (p *Processor) Process(ctx context.Context, task tasks.FileProcessingTask) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	log.Infof("[Processor] 开始处理文件, FileMD5: %s, FileName: %s, UserID: %d", task.FileMD5, task.FileName, task.UserID)

	// 1. 从 MinIO 下载文件
	// 对象路径规则：merged/{fileName}
	// 说明：这里依赖上传合并流程将文件放到 merged 目录。
	objectName := fmt.Sprintf("merged/%s", task.FileName)
	log.Infof("[Processor] 步骤1: 从MinIO下载文件, Bucket: %s, Object: %s", p.minioCfg.BucketName, objectName)
	object, err := storage.MinioClient.GetObject(ctx, p.minioCfg.BucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		log.Errorf("[Processor] 从MinIO下载文件失败, Object: %s, Error: %v", objectName, err)
		return fmt.Errorf("从 MinIO 下载文件失败: %w", err)
	}
	defer object.Close()

	// 将对象流完整读入内存缓冲区：
	// - 便于先校验文件大小（空文件快速失败）；
	// - 便于后续重复读取（Tika 需要 io.Reader）。
	// 注意：大文件会占用较多内存，生产环境可考虑流式/分段处理优化。
	buf := new(bytes.Buffer)
	size, err := buf.ReadFrom(object)
	if err != nil {
		log.Errorf("[Processor] 从MinIO对象流中读取内容到缓冲区失败, Error: %v", err)
		return fmt.Errorf("读取MinIO对象流失败: %w", err)
	}
	log.Infof("[Processor] 步骤1: 文件下载成功, 从MinIO流中读取到的文件大小为: %d字节", size)
	if size == 0 {
		// 空文件无可提取内容，直接终止处理。
		log.Warnf("[Processor] 文件 '%s' 内容为空, 处理中止", task.FileName)
		return errors.New("文件内容为空")
	}

	// 2. 使用 Tika 提取文本（使用缓冲区中的数据）
	// fileName 会用于辅助 Tika 识别文件类型。
	log.Info("[Processor] 步骤2: 使用Tika提取文本内容")
	textContent, err := p.tikaClient.ExtractText(buf, task.FileName)
	if err != nil {
		log.Errorf("[Processor] 使用Tika提取文本失败, FileName: %s, Error: %v", task.FileName, err)
		return fmt.Errorf("使用 Tika 提取文本失败: %w", err)
	}
	if textContent == "" {
		// 解析成功但文本为空：通常代表文件内容确实为空或格式无法有效抽取。
		log.Warnf("[Processor] Tika提取的文本内容为空, 处理中止, FileName: %s", task.FileName)
		return errors.New("提取的文本内容为空")
	}
	log.Infof("[Processor] 步骤2: 文本提取成功, 内容长度: %d 字符", utf8.RuneCountInString(textContent))

	// 3. 文本切块
	chunkSize, chunkOverlap := p.resolvedChunkConfig()
	log.Infof("[Processor] 步骤3: 进行文本分块, chunkSize=%d, chunkOverlap=%d", chunkSize, chunkOverlap)
	chunks := splitTextRecursive(textContent, chunkSize, chunkOverlap)
	log.Infof("[Processor] 步骤3: 文本分块完成, 共生成 %d 个分块", len(chunks))
	if len(chunks) == 0 {
		log.Warnf("[Processor] 未生成任何文本分块, 处理中止, FileName: %s", task.FileName)
		return errors.New("未生成任何文本分块")
	}

	// 阶段一：将分块文本和元数据存入数据库
	// 这样做的好处：
	// - 分块文本可追踪、可审计；
	// - 向量化失败时可基于数据库重试，不必重新走 Tika。
	log.Info("[Processor] 阶段一: 开始将分块文本存入数据库")
	// 幂等处理：先按 fileMD5 清理旧分块，避免重复消费造成累计膨胀。
	if err := p.docVectorRepo.DeleteByFileMD5(task.FileMD5); err != nil {
		log.Errorf("[Processor] 清理 document_vectors 旧记录失败 (file_md5=%s): %v", task.FileMD5, err)
		return fmt.Errorf("清理旧分块记录失败: %w", err)
	}
	dbVectors := make([]*model.DocumentVector, 0, len(chunks))
	for i, chunk := range chunks {
		dbVectors = append(dbVectors, &model.DocumentVector{
			FileMD5:     task.FileMD5,
			ChunkID:     i,
			TextContent: chunk,
			UserID:      task.UserID,
			OrgTag:      task.OrgTag,
			IsPublic:    task.IsPublic,
		})
	}
	if err := p.docVectorRepo.BatchCreate(dbVectors); err != nil {
		log.Errorf("[Processor] 阶段一: 批量保存文本分块到数据库失败, Error: %v", err)
		return fmt.Errorf("批量保存文本分块失败: %w", err)
	}
	log.Infof("[Processor] 阶段一: 成功将 %d 个分块存入数据库", len(dbVectors))

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// 4. 向量化（批量或受控并发）+ ES Bulk 索引
	log.Info("[Processor] 步骤4: 开始向量化与批量索引")
	esDocs, err := p.embedVectors(ctx, dbVectors)
	if err != nil {
		return err
	}
	if err := es.BulkIndexDocuments(ctx, p.esCfg.IndexName, esDocs); err != nil {
		log.Errorf("[Processor] ES bulk 索引失败: %v", err)
		return fmt.Errorf("ES bulk 索引失败: %w", err)
	}
	log.Infof("[Processor] 步骤4: 向量化与批量索引完成, 共 %d 条", len(esDocs))

	log.Infof("[Processor] 文件处理成功完成, FileMD5: %s", task.FileMD5)
	return nil
}

const (
	defaultChunkSize               = 1000
	defaultChunkOverlap            = 100
	defaultEmbeddingMaxConcurrency = 5
)

type batchEmbeddingClient interface {
	CreateEmbeddings(ctx context.Context, texts []string) ([][]float32, error)
}

func (p *Processor) resolvedChunkConfig() (int, int) {
	chunkSize := p.pipelineCfg.ChunkSize
	chunkOverlap := p.pipelineCfg.ChunkOverlap
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	if chunkOverlap < 0 {
		chunkOverlap = 0
	}
	if chunkOverlap >= chunkSize {
		chunkOverlap = chunkSize / 10
	}
	return chunkSize, chunkOverlap
}

func (p *Processor) resolvedEmbeddingMaxConcurrency() int {
	if p.pipelineCfg.EmbeddingMaxConcurrency <= 0 {
		return defaultEmbeddingMaxConcurrency
	}
	return p.pipelineCfg.EmbeddingMaxConcurrency
}

// embedVectors 将数据库中的文本分块转换为 ES 文档（含向量）。
//
// 执行策略：
// 1) 优先尝试“批量 embedding”能力（若客户端实现了 batchEmbeddingClient）；
// 2) 批量不可用/失败时，回退到“受控并发的单条 embedding”；
// 3) 并发模式下采用 fail-fast：首个错误会 cancel 其余 worker，避免无效消耗。
func (p *Processor) embedVectors(ctx context.Context, dbVectors []*model.DocumentVector) ([]model.EsDocument, error) {
	// 空输入快速返回，避免后续分配与协程调度开销。
	if len(dbVectors) == 0 {
		return []model.EsDocument{}, nil
	}

	// 如果 embedding 客户端支持批量接口，优先批量调用：
	// - 远端通常可做请求合并，吞吐更高；
	// - 网络往返次数更少，整体延迟更低。
	if client, ok := p.embeddingClient.(batchEmbeddingClient); ok {
		texts := make([]string, len(dbVectors))
		for i, dv := range dbVectors {
			texts[i] = dv.TextContent
		}
		vectors, err := client.CreateEmbeddings(ctx, texts)
		if err == nil && len(vectors) == len(dbVectors) {
			// 批量成功：按原顺序映射回 ES 文档，保证 chunk 顺序稳定。
			docs := make([]model.EsDocument, len(dbVectors))
			for i, dv := range dbVectors {
				docs[i] = buildESDocument(dv, vectors[i], p.embeddingCfg.Model)
			}
			return docs, nil
		}
		// 批量失败不直接终止：降级到并发单条模式，提高可用性。
		if err != nil {
			log.Warnf("[Processor] 批量 embedding 调用失败，回退并发单条模式: %v", err)
		} else {
			log.Warnf("[Processor] 批量 embedding 返回条数不匹配，回退并发单条模式: got=%d want=%d", len(vectors), len(dbVectors))
		}
	}

	// 回退方案：受控并发单条 embedding。
	maxConcurrency := p.resolvedEmbeddingMaxConcurrency()
	// 信号量：限制同时在飞请求数，防止打爆上游 embedding 服务。
	sem := make(chan struct{}, maxConcurrency)
	// 预分配结果切片，索引与 dbVectors 一一对应，便于并发写入固定位置。
	esDocs := make([]model.EsDocument, len(dbVectors))

	// workerCtx 用于 fail-fast 取消：任何 worker 出错后立即取消其他任务。
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	// mu 保护 firstErr，确保只记录首个错误且并发安全。
	var mu sync.Mutex
	var firstErr error

loop:
	for i, dv := range dbVectors {
		select {
		case <-workerCtx.Done():
			// 上下文已取消（外部取消或某个 worker 失败），停止继续派发新任务。
			break loop
		case sem <- struct{}{}:
			// 获取一个并发配额，进入 worker 执行。
		}

		wg.Add(1)
		go func(idx int, docVector *model.DocumentVector) {
			defer wg.Done()
			// 释放并发配额。
			defer func() { <-sem }()

			select {
			case <-workerCtx.Done():
				// 取消后尽快退出，减少无效请求。
				return
			default:
			}

			vector, err := p.embeddingClient.CreateEmbedding(workerCtx, docVector.TextContent)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					// 只保留首个错误作为最终返回；并触发全局取消。
					firstErr = fmt.Errorf("块 %d 向量化失败: %w", docVector.ChunkID, err)
					cancel()
				}
				mu.Unlock()
				return
			}

			// 写入固定下标，无需额外锁（每个 idx 只会被一个 goroutine 写入一次）。
			esDocs[idx] = buildESDocument(docVector, vector, p.embeddingCfg.Model)
			log.Debugf("[Processor] 向量化完成 ChunkID=%d, TextLen=%d", docVector.ChunkID, len(docVector.TextContent))
		}(i, dv)
	}

	// 等待全部已派发 worker 结束，避免协程泄漏。
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	// firstErr 为空但上下文被取消/超时时，返回上下文错误，便于上层区分。
	if err := workerCtx.Err(); err != nil {
		return nil, err
	}
	return esDocs, nil
}

func buildESDocument(docVector *model.DocumentVector, vector []float32, modelVersion string) model.EsDocument {
	return model.EsDocument{
		VectorID:     fmt.Sprintf("%s_%d", docVector.FileMD5, docVector.ChunkID),
		FileMD5:      docVector.FileMD5,
		ChunkID:      docVector.ChunkID,
		TextContent:  docVector.TextContent,
		Vector:       vector,
		ModelVersion: modelVersion,
		UserID:       docVector.UserID,
		OrgTag:       docVector.OrgTag,
		IsPublic:     docVector.IsPublic,
	}
}

// splitTextRecursive 递归字符文本分割器（推荐版）
func splitTextRecursive(text string, chunkSize, chunkOverlap int) []string {
	if chunkSize <= chunkOverlap || chunkSize <= 0 {
		chunkSize = defaultChunkSize
		chunkOverlap = defaultChunkOverlap
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// 分隔符优先级（从大到小）：段落 > 句子 > 词语 > 字符
	separators := []string{
		"\n\n",                       // 段落
		"\n",                         // 换行
		"。", "！", "？", ".", "!", "?", // 中英文句子
		"；", ";",
		"，", ",", " ", // 词级
	}

	chunks := recursiveSplit(text, separators, chunkSize)
	return mergeWithOverlap(chunks, chunkSize, chunkOverlap)
}

// 核心递归函数
func recursiveSplit(text string, separators []string, chunkSize int) []string {
	// 1. 如果文本已足够短，直接返回
	if utf8.RuneCountInString(text) <= chunkSize {
		return []string{text}
	}

	// 2. 尝试用当前优先级的分隔符切分
	for i, sep := range separators {
		if sep == "" {
			continue
		}
		splits := strings.Split(text, sep)
		if len(splits) <= 1 {
			continue // 该分隔符没起作用，尝试下一个
		}

		// 3. 对每个子片段递归处理（不在递归层做 overlap，统一在最外层处理）
		chunks := make([]string, 0, len(splits))
		var current strings.Builder
		currentLen := 0

		for _, s := range splits {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if sep != " " && sep != "\n" && sep != "\n\n" {
				s += sep // 保留分隔符（标点）
			}

			sLen := utf8.RuneCountInString(s)

			// 单块本身超长 → 递归或暴力切
			if sLen > chunkSize {
				if current.Len() > 0 {
					chunks = append(chunks, current.String())
					current.Reset()
					currentLen = 0
				}
				nextSeparators := separators[i+1:]
				var subChunks []string
				if len(nextSeparators) == 0 {
					subChunks = simpleRuneSplit(s, chunkSize)
				} else {
					subChunks = recursiveSplit(s, nextSeparators, chunkSize) // 降低分隔符优先级递归
				}
				chunks = append(chunks, subChunks...)
				continue
			}

			// 加入当前块会超长 → 结算
			if currentLen+sLen > chunkSize && currentLen > 0 {
				chunks = append(chunks, current.String())
				current.Reset()
				currentLen = 0

			}

			current.WriteString(s)
			currentLen += sLen
		}

		if current.Len() > 0 {
			chunks = append(chunks, current.String())
		}

		if len(chunks) > 0 {
			return chunks
		}
	}

	// 4. 所有分隔符都无效 → 退化为字符级切分
	return simpleRuneSplit(text, chunkSize)
}

// mergeWithOverlap 为相邻 chunk 添加重叠内容（保持语义连贯）
func mergeWithOverlap(chunks []string, chunkSize, chunkOverlap int) []string {
	if len(chunks) <= 1 || chunkOverlap <= 0 {
		return chunks
	}

	result := make([]string, 0, len(chunks))
	for i := 0; i < len(chunks); i++ {
		chunk := chunks[i]
		if i > 0 {
			// 从前一个 chunk 末尾取 overlap 长度的内容拼到当前 chunk 开头
			prev := chunks[i-1]
			overlapRunes := []rune(prev)
			if len(overlapRunes) > chunkOverlap {
				overlapRunes = overlapRunes[len(overlapRunes)-chunkOverlap:]
			}
			currentRunes := []rune(chunk)
			maxPrefix := chunkSize - len(currentRunes)
			if maxPrefix < 0 {
				maxPrefix = 0
			}
			if len(overlapRunes) > maxPrefix {
				overlapRunes = overlapRunes[len(overlapRunes)-maxPrefix:]
			}
			chunk = string(overlapRunes) + chunk
		}
		result = append(result, strings.TrimSpace(chunk))
	}
	return result
}

func simpleRuneSplit(text string, chunkSize int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	capacity := (len(runes) + chunkSize - 1) / chunkSize
	chunks := make([]string, 0, capacity)

	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}
