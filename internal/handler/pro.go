package handler

import (
	"letshare-server/internal/service"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ProHandler 处理 PRO 身份激活的 HTTP handler
type ProHandler struct {
	jwtService    *service.JWTService
	proInviteCode string
	rateLimiter   map[string][]time.Time
	rateMu        sync.Mutex
}

// NewProHandler 创建 PRO 激活处理器
func NewProHandler(jwtService *service.JWTService, proInviteCode string) *ProHandler {
	return &ProHandler{
		jwtService:    jwtService,
		proInviteCode: proInviteCode,
		rateLimiter:   make(map[string][]time.Time),
	}
}

// allowRate checks if the given key is within the rate limit (5 attempts per minute).
func (h *ProHandler) allowRate(key string) bool {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Minute)

	// clean old entries
	valid := h.rateLimiter[key][:0]
	for _, t := range h.rateLimiter[key] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	h.rateLimiter[key] = append(valid, now)

	return len(h.rateLimiter[key]) <= 5
}

// Activate 验证邀请码并签发 PRO JWT
func (h *ProHandler) Activate(c *gin.Context) {
	var req struct {
		UserID     string `json:"user_id"`
		InviteCode string `json:"invite_code"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	// Rate limit: 5 attempts per IP per minute
	clientIP := c.ClientIP()
	if !h.allowRate(clientIP) {
		logrus.WithField("client_ip", clientIP).Warn("PRO 激活频率过高")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请1分钟后重试"})
		return
	}

	if req.UserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少用户ID"})
		return
	}

	if req.InviteCode != h.proInviteCode {
		logrus.WithFields(logrus.Fields{
			"user_id":    req.UserID,
			"client_ip":  clientIP,
		}).Warn("PRO 激活邀请码无效")
		c.JSON(http.StatusForbidden, gin.H{"error": "邀请码无效"})
		return
	}

	token, expiresAt, err := h.jwtService.GenerateProToken(req.UserID)
	if err != nil {
		logrus.WithError(err).Error("生成 PRO token 失败")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器内部错误"})
		return
	}

	logrus.WithFields(logrus.Fields{
		"user_id":    req.UserID,
		"expires_at": expiresAt.Format(time.RFC3339),
	}).Info("PRO 身份已激活")

	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}
