package service

import (
	"context"
	"pai-smart-go/internal/config"
	"pai-smart-go/internal/model"
	"testing"
)

type fakeMemoryRepo struct {
	lastUpsert *model.MemoryEntry
}

func (f *fakeMemoryRepo) UpsertByHash(ctx context.Context, entry *model.MemoryEntry) error {
	f.lastUpsert = entry
	return nil
}

func (f *fakeMemoryRepo) Search(ctx context.Context, userID uint, workspace string, categories []string, query string, limit int) ([]model.MemoryEntry, error) {
	return []model.MemoryEntry{
		{Category: "preferences", Content: "偏好 Go 代码风格"},
		{Category: "project", Content: "本项目使用 Gin + ES"},
	}, nil
}

func (f *fakeMemoryRepo) ListByCategory(ctx context.Context, userID uint, workspace string, category string, limit int) ([]model.MemoryEntry, error) {
	return nil, nil
}

func (f *fakeMemoryRepo) BoostConfidence(ctx context.Context, ids []uint, delta float64) error {
	return nil
}

func (f *fakeMemoryRepo) CleanupLowValue(ctx context.Context, olderThanDays int, minConfidence float64) (int64, error) {
	return 0, nil
}

func TestMemoryUpsertValidateCategory(t *testing.T) {
	svc := NewMemoryService(&fakeMemoryRepo{}, config.MemoryConfig{})
	_, err := svc.Upsert(context.Background(), 1, UpsertMemoryInput{
		Workspace: "default",
		Category:  "unknown",
		Content:   "test",
	})
	if err == nil {
		t.Fatalf("expected unsupported category error")
	}
}

func TestMemoryUpsertNormalizeFields(t *testing.T) {
	repo := &fakeMemoryRepo{}
	svc := NewMemoryService(repo, config.MemoryConfig{})

	_, err := svc.Upsert(context.Background(), 1, UpsertMemoryInput{
		Workspace:  "",
		Category:   "Preferences",
		Content:    "  用户偏好中文回复  ",
		Keywords:   []string{"中文", "中文", "回复"},
		Confidence: 9,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.lastUpsert == nil {
		t.Fatalf("expected upsert called")
	}
	if repo.lastUpsert.Workspace != "default" {
		t.Fatalf("expected default workspace, got %s", repo.lastUpsert.Workspace)
	}
	if repo.lastUpsert.Category != "preferences" {
		t.Fatalf("expected category normalized, got %s", repo.lastUpsert.Category)
	}
	if repo.lastUpsert.Confidence != 0.8 {
		t.Fatalf("expected confidence fallback 0.8, got %v", repo.lastUpsert.Confidence)
	}
}

func TestMemoryBuildContext(t *testing.T) {
	svc := NewMemoryService(&fakeMemoryRepo{}, config.MemoryConfig{})
	text := svc.BuildContext([]model.MemoryEntry{
		{Category: "preferences", Content: "偏好中文"},
		{Category: "project", Content: "项目使用 ES"},
	})
	if text == "" {
		t.Fatalf("expected non-empty memory context")
	}
}
