// Package service 包含了应用的业务逻辑层。
package service

import (
	"WeaveKnow/internal/model"
	"WeaveKnow/internal/repository"
	"context"
)

// ConversationService 定义了对话业务逻辑的接口。
type ConversationService interface {
	GetConversationHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error)
	AddMessageToConversation(ctx context.Context, userID uint, message model.ChatMessage) error
}

type conversationService struct {
	repo repository.ConversationRepository
}

// NewConversationService 创建一个新的 ConversationService。
func NewConversationService(repo repository.ConversationRepository) ConversationService {
	return &conversationService{repo: repo}
}

// GetConversationHistory 获取用户当前会话的完整消息历史。
func (s *conversationService) GetConversationHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error) {
	conversationID, err := s.repo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return nil, err
	}
	history, err := s.repo.GetConversationHistory(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	return filterConversationHistoryForDisplay(history), nil
}

// AddMessageToConversation 将一条消息添加到用户的对话历史中。
func (s *conversationService) AddMessageToConversation(ctx context.Context, userID uint, message model.ChatMessage) error {
	conversationID, err := s.repo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return err
	}
	history, err := s.repo.GetConversationHistory(ctx, conversationID)
	if err != nil {
		return err
	}
	history = append(history, message)
	return s.repo.UpdateConversationHistory(ctx, conversationID, history)
}
