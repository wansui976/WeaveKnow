package service

import (
	"pai-smart-go/internal/model"
	"strings"
)

const (
	chatHeartbeatPing = "__chat_ping__"
	chatHeartbeatPong = "__chat_pong__"
)

func filterConversationHistoryForDisplay(history []model.ChatMessage) []model.ChatMessage {
	if len(history) == 0 {
		return history
	}

	filtered := make([]model.ChatMessage, 0, len(history))
	skipNextAssistantHeartbeat := false

	for _, msg := range history {
		content := strings.TrimSpace(msg.Content)

		if msg.Role == "user" && content == chatHeartbeatPing {
			skipNextAssistantHeartbeat = true
			continue
		}

		if msg.Role == "assistant" && isHeartbeatAssistantMessage(content) {
			if skipNextAssistantHeartbeat {
				skipNextAssistantHeartbeat = false
			}
			continue
		}

		skipNextAssistantHeartbeat = false
		filtered = append(filtered, msg)
	}

	return filtered
}

func isHeartbeatAssistantMessage(content string) bool {
	if content == "" {
		return false
	}
	if content == chatHeartbeatPong {
		return true
	}
	if strings.Contains(content, "pong ✓") && strings.Contains(content, "系统连接正常") {
		return true
	}
	return false
}
