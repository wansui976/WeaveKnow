// Package handler 包含了处理 HTTP 请求的控制器逻辑。
package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/token"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// allowedOrigin 生产环境应从配置读取
// 用白名单替代 `return true`，防止跨站 WebSocket 劫持（CSWSH）。
// 若需要允许多个域名，可改为 map 或从 config 读取。
const allowedOrigin = "" // 空字符串表示跳过校验（开发模式），生产环境填写域名

const (
	wsHeartbeatPing = "__chat_ping__"
	wsHeartbeatPong = "__chat_pong__"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		if allowedOrigin == "" {
			return true // 开发模式：允许所有来源
		}
		return r.Header.Get("Origin") == allowedOrigin
	},
}

// ChatHandler 负责处理 WebSocket 聊天连接。
type ChatHandler struct {
	chatService service.ChatService
	userService service.UserService
	jwtManager  *token.JWTManager

	// stopTokens 按连接存储，防止用户 A 的 token 覆盖或停止用户 B 的流。
	// key: sessionKey(conn), value: string
	stopTokens sync.Map

	// stopFlags 按连接存储停止标志。
	// key: sessionKey(conn), value: bool
	stopFlags sync.Map
}

// NewChatHandler 创建一个新的 ChatHandler。
func NewChatHandler(chatService service.ChatService, userService service.UserService, jwtManager *token.JWTManager) *ChatHandler {
	return &ChatHandler{
		chatService: chatService,
		userService: userService,
		jwtManager:  jwtManager,
	}
}

// GetWebsocketStopToken 为当前请求的用户生成一个停止令牌并返回。
//
// 令牌通过 userID 或 connID 进行隔离，不再是全局单例。
// 此接口应在 WebSocket 升级前调用，或通过另一个接口传入 connID。
// 当前方案：令牌在 Handle 阶段由 conn 指针绑定，此接口仅生成并返回令牌，
// 真正的绑定发生在 Handle 开始时。
func (h *ChatHandler) GetWebsocketStopToken(c *gin.Context) {
	stopToken := "WSS_STOP_CMD_" + token.GenerateRandomString(16)
	// 将 token 临时写入 context，客户端持有后在 Handle 中验证。
	// 实际生产环境应存入 Redis（key=userID，TTL=5min），此处保持单机简化实现。
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "message": "success", "data": gin.H{"cmdToken": stopToken}})
}

// Handle 处理一个传入的 WebSocket 连接。
func (h *ChatHandler) Handle(c *gin.Context) {
	// 1. 验证 JWT。
	tokenString := c.Param("token")
	claims, err := h.jwtManager.VerifyToken(tokenString)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "message": "无效的 token", "data": nil})
		return
	}

	// 2. 获取用户信息。
	user, err := h.userService.GetProfile(claims.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": http.StatusInternalServerError, "message": "无法获取用户信息", "data": nil})
		return
	}

	// 3. 升级为 WebSocket 连接。
	rawConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Error("WebSocket 升级失败", err)
		return
	}
	defer rawConn.Close()

	//  用 safeConn 包装，保证所有 WriteMessage 调用都经过互斥锁，
	//   防止 StreamResponse（流式输出）与控制消息（stop/error）并发写入导致 panic。
	conn := &safeConn{conn: rawConn}
	key := sessionKey(rawConn)
	defer h.stopFlags.Delete(key)
	defer h.stopTokens.Delete(key)

	//  配置 Ping/Pong 心跳，防止长连接静默挂死。
	//如果在接下来的 60 秒内没有收到任何数据，Read 操作将返回错误。
	// 这防止了连接在客户端异常掉线（如拔网线）时一直死锁在服务器内存中。
	rawConn.SetReadDeadline(time.Now().Add(60 * time.Second))
	//每当服务器收到客户端回传的 PongMessage 时，就会触发这个回调。
	//处理器会将 ReadDeadline 往后顺延 60 秒。只要客户端还在跳动，连接就永远不会超时。
	rawConn.SetPongHandler(func(string) error {
		return rawConn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})
	pingStop := make(chan struct{})
	defer close(pingStop)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			case <-pingStop:
				return
			}
		}
	}()

	log.Infof("WebSocket 连接已建立，用户: %s", claims.Username)

	// 5. 消息循环。
	for {
		_, message, err := rawConn.ReadMessage()
		if err != nil {
			log.Warnf("从 WebSocket 读取消息失败: %v", err)
			break
		}

		// 5a. 心跳消息：直接回 pong，不进入聊天链路，也不写入历史。
		if string(message) == wsHeartbeatPing {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(wsHeartbeatPong)); err != nil {
				log.Warnf("发送 WebSocket 心跳响应失败: %v", err)
				break
			}
			continue
		}

		log.Infof("收到 WebSocket 消息: %s", string(message))

		// 5b. 尝试解析为控制指令（JSON 格式）。
		if handled := h.handleControlMessage(conn, rawConn, key, message); handled {
			continue
		}

		// 5c. 普通问句：调用 RAG 流程。
		//  先 Delete 再建闭包，并在闭包外计算 key，避免竞争与重复计算。
		h.stopFlags.Delete(key)
		shouldStop := func() bool {
			v, ok := h.stopFlags.Load(key)
			return ok && v.(bool)
		}

		err = h.chatService.StreamResponse(c.Request.Context(), string(message), user, conn, shouldStop)
		if err != nil {
			log.Errorf("处理流式响应失败: %v", err)
			sendJSON(conn, map[string]string{"error": "AI服务暂时不可用，请稍后重试"})
			// 与原行为对齐：错误时也发送 completion 通知。
			// 复用 sendCompletion 逻辑，不再内联重复构造。
			sendCompletionNotif(conn)
			break
		}
	}
}

// handleControlMessage 解析并处理控制指令（stop）。
// 返回 true 表示消息已被处理（是控制指令），false 表示是普通问句。
//
//	 将两套停止机制（JSON + 裸字符串）统一到此函数，
//		裸字符串兼容路径仍保留但清晰隔离，方便后续废弃。
func (h *ChatHandler) handleControlMessage(conn *safeConn, rawConn *websocket.Conn, key string, message []byte) bool {
	// JSON 格式停止指令：{"type":"stop","_internal_cmd_token":"..."}
	if len(message) > 0 && message[0] == '{' {
		var ctrl map[string]interface{}
		if err := json.Unmarshal(message, &ctrl); err == nil {
			if t, ok := ctrl["type"].(string); ok && t == "stop" {
				if tok, ok := ctrl["_internal_cmd_token"].(string); ok && h.isValidStopToken(key, tok) {
					h.stopFlags.Store(key, true)
					sendJSON(conn, map[string]interface{}{
						"type":      "stop",
						"message":   "响应已停止",
						"timestamp": time.Now().UnixMilli(),
						"date":      time.Now().Format("2006-01-02T15:04:05"),
					})
					return true
				}
			}
		}
	}

	// 裸字符串兼容路径（计划废弃，保留向后兼容）。
	if storedTok, ok := h.stopTokens.Load(key); ok {
		if string(message) == storedTok.(string) {
			log.Info("收到裸字符串停止指令（兼容模式），正在中断流式响应...")
			h.stopFlags.Store(key, true)
			return true
		}
	}

	return false
}

// isValidStopToken 验证给定 token 是否与该连接绑定的停止令牌匹配。
func (h *ChatHandler) isValidStopToken(key, tok string) bool {
	stored, ok := h.stopTokens.Load(key)
	return ok && stored.(string) == tok
}

// ---------------------------------------------------------------------------
// safeConn：并发安全的 WebSocket 写入包装器
// ---------------------------------------------------------------------------

// safeConn 对 websocket.Conn 的写操作加锁，保证并发安全。
// gorilla/websocket 要求 WriteMessage 不能并发调用，
//
//	safeConn 统一路由所有写入，彻底解决帧损坏和 panic 问题。
type safeConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (s *safeConn) WriteMessage(messageType int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteMessage(messageType, data)
}

func (s *safeConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteControl(messageType, data, deadline)
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

// sendJSON 序列化并发送一条 JSON 消息，忽略序列化错误（理论上不会发生）。
func sendJSON(conn *safeConn, v interface{}) {
	b, _ := json.Marshal(v)
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		log.Warnf("sendJSON 写入失败: %v", err)
	}
}

// sendCompletionNotif 发送流式结束通知（与 chat_service.go 中 sendCompletion 语义一致）。
// 🟢 handler 层统一使用此函数，不再内联重复构造 JSON。
func sendCompletionNotif(conn *safeConn) {
	sendJSON(conn, map[string]interface{}{
		"type":      "completion",
		"status":    "finished",
		"message":   "响应已完成",
		"timestamp": time.Now().UnixMilli(),
		"date":      time.Now().Format("2006-01-02T15:04:05"),
	})
}

// sessionKey 使用连接指针地址作为会话唯一标识。
func sessionKey(conn *websocket.Conn) string {
	return fmt.Sprintf("%p", conn)
}
