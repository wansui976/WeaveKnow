// Package config 负责加载和管理应用程序的配置。
package config

import (
	"fmt"

	"github.com/spf13/viper"
)

// 全局配置变量，存储从配置文件加载的所有设置。
var Conf Config

// Config 是整个应用程序的配置结构体，与 config.yaml 文件结构对应。
type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	Database      DatabaseConfig      `mapstructure:"database"`
	JWT           JWTConfig           `mapstructure:"jwt"`
	Log           LogConfig           `mapstructure:"log"`
	Kafka         KafkaConfig         `mapstructure:"kafka"`
	Tika          TikaConfig          `mapstructure:"tika"`
	Elasticsearch ElasticsearchConfig `mapstructure:"elasticsearch"`
	Search        SearchConfig        `mapstructure:"search"`
	Memory        MemoryConfig        `mapstructure:"memory"`
	MinIO         MinIOConfig         `mapstructure:"minio"`
	Pipeline      PipelineConfig      `mapstructure:"pipeline"`
	Embedding     EmbeddingConfig     `mapstructure:"embedding"`
	LLM           LLMConfig           `mapstructure:"llm"`
	AI            AIConfig            `mapstructure:"ai"`
}

// ServerConfig 存储服务器相关的配置。
type ServerConfig struct {
	Port string `mapstructure:"port"`
	Mode string `mapstructure:"mode"`
}

// DatabaseConfig 存储所有数据库连接的配置。
type DatabaseConfig struct {
	MySQL MySQLConfig `mapstructure:"mysql"`
	Redis RedisConfig `mapstructure:"redis"`
}

// MySQLConfig 存储 MySQL 数据库的配置。
type MySQLConfig struct {
	DSN string `mapstructure:"dsn"`
}

// RedisConfig 存储 Redis 的配置。
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// JWTConfig 存储 JWT 相关的配置。
type JWTConfig struct {
	Secret                 string `mapstructure:"secret"`
	AccessTokenExpireHours int    `mapstructure:"access_token_expire_hours"`
	RefreshTokenExpireDays int    `mapstructure:"refresh_token_expire_days"`
}

// LogConfig 存储日志相关的配置。
type LogConfig struct {
	Level      string `mapstructure:"level"`
	Format     string `mapstructure:"format"`
	OutputPath string `mapstructure:"output_path"`
}

// KafkaConfig 存储 Kafka 相关的配置。
type KafkaConfig struct {
	Brokers string `mapstructure:"brokers"`
	Topic   string `mapstructure:"topic"`
}

// TikaConfig 存储 Tika 服务器相关的配置。
type TikaConfig struct {
	ServerURL string `mapstructure:"server_url"`
}

// ElasticsearchConfig 存储 Elasticsearch 相关的配置。
type ElasticsearchConfig struct {
	Addresses string `mapstructure:"addresses"`
	Username  string `mapstructure:"username"`
	Password  string `mapstructure:"password"`
	IndexName string `mapstructure:"index_name"`
}

// SearchConfig 存储检索阶段增强相关配置。
type SearchConfig struct {
	QueryRewriteEnabled          bool   `mapstructure:"query_rewrite_enabled"`
	QueryRewriteTimeoutS         int    `mapstructure:"query_rewrite_timeout_seconds"`
	QueryRewriteMaxLength        int    `mapstructure:"query_rewrite_max_length"`
	EmbeddingCacheEnabled        bool   `mapstructure:"embedding_cache_enabled"`
	EmbeddingCacheTTLSeconds     int    `mapstructure:"embedding_cache_ttl_seconds"`
	ResultCacheEnabled           bool   `mapstructure:"result_cache_enabled"`
	ResultCacheTTLSeconds        int    `mapstructure:"result_cache_ttl_seconds"`
	ResultCacheSimilarityEnabled bool   `mapstructure:"result_cache_similarity_enabled"`
	RerankEnabled                bool   `mapstructure:"rerank_enabled"`
	RerankCandidateK             int    `mapstructure:"rerank_candidate_k"`
	ExternalRerankerEnabled      bool   `mapstructure:"external_reranker_enabled"`
	ExternalRerankerURL          string `mapstructure:"external_reranker_url"`
	ExternalRerankerTimeoutS     int    `mapstructure:"external_reranker_timeout_seconds"`
	ExternalRerankerAPIKey       string `mapstructure:"external_reranker_api_key"`
	ExternalRerankerModel        string `mapstructure:"external_reranker_model"`
}

// MemoryConfig 存储记忆检索/衰减/清理相关配置。
type MemoryConfig struct {
	DecayHalfLifeHours    int     `mapstructure:"decay_half_life_hours"`
	HitBoost              float64 `mapstructure:"hit_boost"`
	MinEffectiveScore     float64 `mapstructure:"min_effective_score"`
	CleanupEnabled        bool    `mapstructure:"cleanup_enabled"`
	CleanupIntervalHours  int     `mapstructure:"cleanup_interval_hours"`
	CleanupOlderThanDays  int     `mapstructure:"cleanup_older_than_days"`
	CleanupMinConfidence  float64 `mapstructure:"cleanup_min_confidence"`
	AsyncUpdateTimeoutS   int     `mapstructure:"async_update_timeout_seconds"`
	AsyncUpdateMaxEntries int     `mapstructure:"async_update_max_entries"`
}

// MinIOConfig 存储 MinIO 对象存储的配置。
type MinIOConfig struct {
	Endpoint        string `mapstructure:"endpoint"`
	AccessKeyID     string `mapstructure:"access_key_id"`
	SecretAccessKey string `mapstructure:"secret_access_key"`
	UseSSL          bool   `mapstructure:"use_ssl"`
	BucketName      string `mapstructure:"bucket_name"`
}

// PipelineConfig 存储文档处理流水线相关配置。
type PipelineConfig struct {
	ChunkSize               int `mapstructure:"chunk_size"`
	ChunkOverlap            int `mapstructure:"chunk_overlap"`
	EmbeddingMaxConcurrency int `mapstructure:"embedding_max_concurrency"`
}

// EmbeddingConfig 存储 Embedding 模型相关的配置。
type EmbeddingConfig struct {
	APIKey     string `mapstructure:"api_key"`
	BaseURL    string `mapstructure:"base_url"`
	Model      string `mapstructure:"model"`
	Dimensions int    `mapstructure:"dimensions"`
}

// LLMConfig 存储大语言模型相关的配置。
type LLMConfig struct {
	APIKey        string              `mapstructure:"api_key"`
	BaseURL       string              `mapstructure:"base_url"`
	Model         string              `mapstructure:"model"`
	ContextWindow int                 `mapstructure:"context_window"`
	Generation    LLMGenerationConfig `mapstructure:"generation"`
	Prompt        LLMPromptConfig     `mapstructure:"prompt"`
}

// LLMGenerationConfig 配置生成相关参数（可选）。
type LLMGenerationConfig struct {
	Temperature float64 `mapstructure:"temperature"`
	TopP        float64 `mapstructure:"top_p"`
	MaxTokens   int     `mapstructure:"max_tokens"`
}

// LLMPromptConfig 配置系统提示与上下文包裹格式（可选）。
type LLMPromptConfig struct {
	Rules        string `mapstructure:"rules"`
	RefStart     string `mapstructure:"ref_start"`
	RefEnd       string `mapstructure:"ref_end"`
	NoResultText string `mapstructure:"no_result_text"`
}

// AIConfig 对齐 Java 的 ai.prompt/ai.generation（连字符键）
type AIConfig struct {
	Generation AIGenerationConfig `mapstructure:"generation"`
	Prompt     AIPromptConfig     `mapstructure:"prompt"`
	Agent      AgentConfig        `mapstructure:"agent"`
}

type AIGenerationConfig struct {
	Temperature float64 `mapstructure:"temperature"`
	TopP        float64 `mapstructure:"top-p"`
	MaxTokens   int     `mapstructure:"max-tokens"`
}

type AIPromptConfig struct {
	Rules        string `mapstructure:"rules"`
	RefStart     string `mapstructure:"ref-start"`
	RefEnd       string `mapstructure:"ref-end"`
	NoResultText string `mapstructure:"no-result-text"`
}

// AgentConfig 配置 Agent 模式开关与工具调用循环参数。
type AgentConfig struct {
	Enabled                 bool `mapstructure:"enabled"`
	MaxIterations           int  `mapstructure:"max_iterations"`
	DefaultTopK             int  `mapstructure:"default_top_k"`
	ToolTimeoutS            int  `mapstructure:"tool_timeout_seconds"`
	ToolContextBudgetTokens int  `mapstructure:"tool_context_budget_tokens"`
}

// Init 初始化配置加载，从指定的路径读取 YAML 文件并解析到 Conf 变量中。
func Init(configPath string) {
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")

	if err := viper.ReadInConfig(); err != nil {
		panic(fmt.Errorf("读取配置文件失败: %w", err))
	}

	if err := viper.Unmarshal(&Conf); err != nil {
		panic(fmt.Errorf("无法将配置解析到结构体中: %w", err))
	}
}
