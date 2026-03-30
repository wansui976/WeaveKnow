package handler

import (
	"net/http"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/service"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type MemoryHandler struct {
	memoryService service.MemoryService
}

func NewMemoryHandler(memoryService service.MemoryService) *MemoryHandler {
	return &MemoryHandler{memoryService: memoryService}
}

type upsertMemoryRequest struct {
	Workspace  string   `json:"workspace"`
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Keywords   []string `json:"keywords"`
	Confidence float64  `json:"confidence"`
	Source     string   `json:"source"`
}

func (h *MemoryHandler) Upsert(c *gin.Context) {
	user, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}

	var req upsertMemoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数错误"})
		return
	}

	entry, err := h.memoryService.Upsert(c.Request.Context(), user.(*model.User).ID, service.UpsertMemoryInput{
		Workspace:  req.Workspace,
		Category:   req.Category,
		Content:    req.Content,
		Keywords:   req.Keywords,
		Confidence: req.Confidence,
		Source:     req.Source,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "message": "success", "data": entry})
}

func (h *MemoryHandler) Search(c *gin.Context) {
	user, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "5"))
	categories := strings.Split(strings.TrimSpace(c.Query("categories")), ",")
	if len(categories) == 1 && categories[0] == "" {
		categories = nil
	}

	items, err := h.memoryService.Search(c.Request.Context(), user.(*model.User).ID, service.SearchMemoryInput{
		Workspace:  c.DefaultQuery("workspace", "default"),
		Categories: categories,
		Query:      c.Query("query"),
		Limit:      limit,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "message": "success", "data": items})
}

func (h *MemoryHandler) ListByCategory(c *gin.Context) {
	user, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	items, err := h.memoryService.ListByCategory(
		c.Request.Context(),
		user.(*model.User).ID,
		c.DefaultQuery("workspace", "default"),
		c.Param("category"),
		limit,
	)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "message": "success", "data": items})
}
