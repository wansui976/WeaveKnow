package handler

import (
	"WeaveKnow/internal/service"
	"net/http"

	"github.com/gin-gonic/gin"
)

type MetricsHandler struct {
	metricsService service.MetricsService
}

func NewMetricsHandler(metricsService service.MetricsService) *MetricsHandler {
	return &MetricsHandler{metricsService: metricsService}
}

func (h *MetricsHandler) GetRAGMetrics(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"code":    http.StatusOK,
		"message": "success",
		"data":    h.metricsService.Snapshot(),
	})
}
