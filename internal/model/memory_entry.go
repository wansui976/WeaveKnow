package model

import "time"

// MemoryEntry 表示一条结构化记忆（按 category 分桶）。
type MemoryEntry struct {
	ID          uint      `gorm:"primaryKey;autoIncrement;column:id" json:"id"`
	UserID      uint      `gorm:"not null;index:idx_user_workspace_cat,priority:1;column:user_id" json:"userId"`
	Workspace   string    `gorm:"type:varchar(128);not null;index:idx_user_workspace_cat,priority:2;column:workspace" json:"workspace"`
	Category    string    `gorm:"type:varchar(64);not null;index:idx_user_workspace_cat,priority:3;column:category" json:"category"`
	Content     string    `gorm:"type:text;not null;column:content" json:"content"`
	Keywords    string    `gorm:"type:varchar(512);column:keywords" json:"keywords"`
	Confidence  float64   `gorm:"type:decimal(5,4);default:0.8;column:confidence" json:"confidence"`
	Source      string    `gorm:"type:varchar(64);default:'manual';column:source" json:"source"`
	ContentHash string    `gorm:"type:char(32);not null;index:idx_memory_hash;column:content_hash" json:"-"`
	CreatedAt   time.Time `gorm:"autoCreateTime;column:created_at" json:"createdAt"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime;column:updated_at" json:"updatedAt"`
}

func (MemoryEntry) TableName() string {
	return "memory_entries"
}
