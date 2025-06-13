package handler

import (
	"letshare-server/internal/service"
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

type HealthHandler struct {
	wsService *service.WebSocketService
	startTime time.Time
}

func NewHealthHandler(wsService *service.WebSocketService) *HealthHandler {
	return &HealthHandler{
		wsService: wsService,
		startTime: time.Now(),
	}
}

// Health 健康检查端点
func (h *HealthHandler) Health(c *gin.Context) {
	uptime := time.Since(h.startTime)
	
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	
	c.JSON(http.StatusOK, gin.H{
		"status":    "healthy",
		"timestamp": time.Now().Format(time.RFC3339),
		"uptime":    uptime.String(),
		"memory": gin.H{
			"alloc_mb":      bToMb(m.Alloc),
			"total_alloc_mb": bToMb(m.TotalAlloc),
			"sys_mb":        bToMb(m.Sys),
			"num_gc":        m.NumGC,
		},
		"goroutines": runtime.NumGoroutine(),
	})
}

// Metrics 监控指标端点
func (h *HealthHandler) Metrics(c *gin.Context) {
	stats := h.wsService.GetStats()
	
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	
	uptime := time.Since(h.startTime)
	
	c.JSON(http.StatusOK, gin.H{
		"server": gin.H{
			"uptime":     uptime.String(),
			"uptime_seconds": int64(uptime.Seconds()),
			"timestamp":  time.Now().Format(time.RFC3339),
		},
		"websocket": stats,
		"system": gin.H{
			"memory": gin.H{
				"alloc_mb":       bToMb(m.Alloc),
				"total_alloc_mb": bToMb(m.TotalAlloc),
				"sys_mb":         bToMb(m.Sys),
				"heap_alloc_mb":  bToMb(m.HeapAlloc),
				"heap_sys_mb":    bToMb(m.HeapSys),
				"num_gc":         m.NumGC,
			},
			"goroutines":     runtime.NumGoroutine(),
			"cpu_count":      runtime.NumCPU(),
			"go_version":     runtime.Version(),
		},
	})
}

// bToMb 转换字节到MB
func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
} 