package repository

import (
	"context"
	"errors"
	"fmt"
	"pai-smart-go/internal/model"

	"gorm.io/gorm"
)

// MemoryRepository 提供结构化记忆的存取能力。
type MemoryRepository interface {
	UpsertByHash(ctx context.Context, entry *model.MemoryEntry) error
	Search(ctx context.Context, userID uint, workspace string, categories []string, query string, limit int) ([]model.MemoryEntry, error)
	ListByCategory(ctx context.Context, userID uint, workspace string, category string, limit int) ([]model.MemoryEntry, error)
	BoostConfidence(ctx context.Context, ids []uint, delta float64) error
	CleanupLowValue(ctx context.Context, olderThanDays int, minConfidence float64) (int64, error)
}

type memoryRepository struct {
	db *gorm.DB
}

func NewMemoryRepository(db *gorm.DB) MemoryRepository {
	return &memoryRepository{db: db}
}

func (r *memoryRepository) UpsertByHash(ctx context.Context, entry *model.MemoryEntry) error {
	if entry == nil {
		return fmt.Errorf("memory entry is nil")
	}

	var existing model.MemoryEntry
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND workspace = ? AND category = ? AND content_hash = ?",
			entry.UserID, entry.Workspace, entry.Category, entry.ContentHash).
		First(&existing).Error

	if err == nil {
		existing.Content = entry.Content
		existing.Keywords = entry.Keywords
		existing.Confidence = entry.Confidence
		existing.Source = entry.Source
		return r.db.WithContext(ctx).Save(&existing).Error
	}

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r.db.WithContext(ctx).Create(entry).Error
	}

	return err
}

func (r *memoryRepository) Search(ctx context.Context, userID uint, workspace string, categories []string, query string, limit int) ([]model.MemoryEntry, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	tx := r.db.WithContext(ctx).Model(&model.MemoryEntry{}).Where("user_id = ?", userID)

	// 检索时优先当前 workspace，同时允许命中 global 记忆。
	if workspace != "" {
		tx = tx.Where("(workspace = ? OR workspace = ?)", workspace, "global")
	}
	if len(categories) > 0 {
		tx = tx.Where("category IN ?", categories)
	}
	if query != "" {
		like := "%" + query + "%"
		tx = tx.Where("(content LIKE ? OR keywords LIKE ?)", like, like)
	}

	orderExpr := "confidence DESC, updated_at DESC"
	if workspace != "" {
		orderExpr = fmt.Sprintf("CASE WHEN workspace = '%s' THEN 0 ELSE 1 END, confidence DESC, updated_at DESC", workspace)
	}

	var items []model.MemoryEntry
	if err := tx.Order(orderExpr).Limit(limit).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *memoryRepository) ListByCategory(ctx context.Context, userID uint, workspace string, category string, limit int) ([]model.MemoryEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	tx := r.db.WithContext(ctx).Model(&model.MemoryEntry{}).
		Where("user_id = ? AND category = ?", userID, category)
	if workspace != "" {
		tx = tx.Where("workspace = ?", workspace)
	}

	var items []model.MemoryEntry
	if err := tx.Order("updated_at DESC").Limit(limit).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (r *memoryRepository) BoostConfidence(ctx context.Context, ids []uint, delta float64) error {
	if len(ids) == 0 || delta == 0 {
		return nil
	}
	return r.db.WithContext(ctx).
		Model(&model.MemoryEntry{}).
		Where("id IN ?", ids).
		Update("confidence", gorm.Expr("LEAST(1.0, GREATEST(0.0, confidence + ?))", delta)).
		Error
}

func (r *memoryRepository) CleanupLowValue(ctx context.Context, olderThanDays int, minConfidence float64) (int64, error) {
	if olderThanDays <= 0 {
		olderThanDays = 90
	}
	if minConfidence < 0 {
		minConfidence = 0
	}
	result := r.db.WithContext(ctx).
		Where("updated_at < DATE_SUB(NOW(), INTERVAL ? DAY) AND confidence < ?", olderThanDays, minConfidence).
		Delete(&model.MemoryEntry{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}
