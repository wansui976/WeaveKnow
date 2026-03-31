// Package es 提供了与 Elasticsearch 交互的客户端功能。
package es

import (
	"WeaveKnow/internal/config"
	"WeaveKnow/internal/model"
	"WeaveKnow/pkg/log"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
)

var ESClient *elasticsearch.Client

// InitES 初始化 Elasticsearch 客户端
func InitES(esCfg config.ElasticsearchConfig) error {
	cfg := elasticsearch.Config{
		Addresses: []string{esCfg.Addresses},
		Username:  esCfg.Username,
		Password:  esCfg.Password,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	client, err := elasticsearch.NewClient(cfg)
	if err != nil {
		return err
	}
	ESClient = client
	return createIndexIfNotExists(esCfg.IndexName)
}

// createIndexIfNotExists 检查索引是否存在，如果不存在则创建它
func createIndexIfNotExists(indexName string) error {
	res, err := ESClient.Indices.Exists([]string{indexName})
	if err != nil {
		log.Errorf("检查索引是否存在时出错: %v", err)
		return err
	}
	// 如果 res.StatusCode 是 200，说明索引已存在
	if !res.IsError() && res.StatusCode == http.StatusOK {
		log.Infof("索引 '%s' 已存在", indexName)
		return nil
	}
	// 如果 res.StatusCode 是 404，说明索引不存在，需要创建
	if res.StatusCode != http.StatusNotFound {
		log.Errorf("检查索引 '%s' 是否存在时收到意外的状态码: %d", indexName, res.StatusCode)
		return fmt.Errorf("检查索引是否存在时收到意外的状态码: %d", res.StatusCode)
	}

	// 优先尝试 IK 中文分词器；若当前 ES 未安装 IK 插件，则回退到标准分词器，
	// 以保证本地 Docker 环境可以先正常启动。
	ikMapping := `{
		"mappings": {
			"properties": {
				"vector_id": { "type": "keyword" },
				"file_md5": { "type": "keyword" },
				"chunk_id": { "type": "integer" },
				"text_content": { 
					"type": "text",
					"analyzer": "ik_max_word",
					"search_analyzer": "ik_smart" 
				},
				"vector": {
					"type": "dense_vector",
					"dims": 2048,
					"index": true,
					"similarity": "cosine"
				},
				"model_version": { "type": "keyword" },
				"user_id": { "type": "long" },
				"org_tag": { "type": "keyword" },
				"is_public": { "type": "boolean" }
			}
		}
	}`
	standardMapping := `{
		"mappings": {
			"properties": {
				"vector_id": { "type": "keyword" },
				"file_md5": { "type": "keyword" },
				"chunk_id": { "type": "integer" },
				"text_content": {
					"type": "text"
				},
				"vector": {
					"type": "dense_vector",
					"dims": 2048,
					"index": true,
					"similarity": "cosine"
				},
				"model_version": { "type": "keyword" },
				"user_id": { "type": "long" },
				"org_tag": { "type": "keyword" },
				"is_public": { "type": "boolean" }
			}
		}
	}`

	res, err = ESClient.Indices.Create(
		indexName,
		ESClient.Indices.Create.WithBody(strings.NewReader(ikMapping)),
	)

	if err != nil {
		log.Errorf("创建索引 '%s' 失败: %v", indexName, err)
		return err
	}
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		bodyStr := string(body)

		if strings.Contains(bodyStr, "analyzer [ik_smart] has not been configured") ||
			strings.Contains(bodyStr, "analyzer [ik_max_word] has not been configured") {
			log.Warnf("Elasticsearch 未安装 IK 分词器，索引 '%s' 将回退为标准分词映射", indexName)
			res, err = ESClient.Indices.Create(
				indexName,
				ESClient.Indices.Create.WithBody(strings.NewReader(standardMapping)),
			)
			if err != nil {
				log.Errorf("使用标准分词器创建索引 '%s' 失败: %v", indexName, err)
				return err
			}
			if res.IsError() {
				log.Errorf("使用标准分词器创建索引 '%s' 时 Elasticsearch 返回错误: %s", indexName, res.String())
				return errors.New("使用标准分词器创建索引时 Elasticsearch 返回错误")
			}
			log.Infof("索引 '%s' 已使用标准分词映射创建成功", indexName)
			return nil
		}

		log.Errorf("创建索引 '%s' 时 Elasticsearch 返回错误: status=%s body=%s", indexName, res.Status(), bodyStr)
		return errors.New("创建索引时 Elasticsearch 返回错误")
	}

	log.Infof("索引 '%s' 创建成功", indexName)
	return nil
}

// IndexDocument 将单个文档向量索引到 Elasticsearch。
func IndexDocument(ctx context.Context, indexName string, doc model.EsDocument) error {
	docBytes, err := json.Marshal(doc)
	if err != nil {
		return err
	}

	req := esapi.IndexRequest{
		Index:      indexName,
		DocumentID: doc.VectorID,
		Body:       bytes.NewReader(docBytes),
		Refresh:    "true",
	}

	res, err := req.Do(ctx, ESClient)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.IsError() {
		log.Errorf("索引文档到 Elasticsearch 出错: %s", res.String())
		return errors.New("failed to index document")
	}

	return nil
}

// BulkIndexDocuments 使用 Elasticsearch _bulk API 批量写入向量文档。
func BulkIndexDocuments(ctx context.Context, indexName string, docs []model.EsDocument) error {
	if len(docs) == 0 {
		return nil
	}

	var body bytes.Buffer
	for _, doc := range docs {
		meta := map[string]map[string]string{
			"index": {
				"_index": indexName,
				"_id":    doc.VectorID,
			},
		}
		metaBytes, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("marshal bulk meta failed: %w", err)
		}
		docBytes, err := json.Marshal(doc)
		if err != nil {
			return fmt.Errorf("marshal bulk document failed: %w", err)
		}
		body.Write(metaBytes)
		body.WriteByte('\n')
		body.Write(docBytes)
		body.WriteByte('\n')
	}

	req := esapi.BulkRequest{
		Index:   indexName,
		Body:    &body,
		Refresh: "true",
	}

	res, err := req.Do(ctx, ESClient)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return fmt.Errorf("bulk request failed: status=%s body=%s", res.Status(), string(respBody))
	}

	var bulkResp struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&bulkResp); err != nil {
		return fmt.Errorf("decode bulk response failed: %w", err)
	}
	if !bulkResp.Errors {
		return nil
	}

	failed := 0
	for _, item := range bulkResp.Items {
		for _, result := range item {
			if result.Status >= 300 {
				failed++
			}
		}
	}
	return fmt.Errorf("bulk index contains failed items: failed=%d total=%d", failed, len(bulkResp.Items))
}
