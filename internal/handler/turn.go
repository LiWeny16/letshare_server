package handler

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"letshare-server/internal/turnauth"
)

// TurnHandler 签发短效 TURN 凭据（RFC 5766 use-auth-secret 模式）。
//
// 与 coturn 的 static-auth-secret 配合：username 编码为 "<unix时间戳>:<ttl秒>"，
// credential = Base64(HMAC-SHA1(secret, username))。凭据短时效过期，前端不落明文密码。
type TurnHandler struct {
	enabled     bool
	secret      string
	uris        []string
	ttlSeconds  int
	rateLimiter map[string][]time.Time
	rateMu      sync.Mutex
}

// NewTurnHandler 创建 TURN 凭据处理器。
func NewTurnHandler(enabled bool, secret string, uris []string, ttlSeconds int) *TurnHandler {
	return &TurnHandler{enabled: enabled, secret: secret, uris: uris, ttlSeconds: ttlSeconds, rateLimiter: make(map[string][]time.Time)}
}

// allowRate 按 IP 限流（默认每分钟最多 30 次），防止公网端点被刷导致带宽/计算资源被滥用。
func (h *TurnHandler) allowRate(key string) bool {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Minute)
	valid := h.rateLimiter[key][:0]
	for _, t := range h.rateLimiter[key] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	h.rateLimiter[key] = append(valid, now)
	return len(h.rateLimiter[key]) <= 30
}

// Credentials 签发并下发短效 TURN 凭据。
func (h *TurnHandler) Credentials(c *gin.Context) {
	if !h.enabled || h.secret == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "TURN 服务未启用"})
		return
	}

	// IP 限流：防公网端点被刷（短效凭据本身无害，但需防滥用签发）。
	if !h.allowRate(c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后再试"})
		return
	}

	// RFC 5766 use-auth-secret 短效凭据（构造与校验统一在 turnauth 包，
	// 与嵌入式 TURN 服务的 AuthHandler 共享同一事实源）。
	expiry := time.Now().Unix() + int64(h.ttlSeconds)
	username := turnauth.BuildUsername(time.Unix(expiry, 0))
	credential := turnauth.Credential(h.secret, username)

	iceServers := make([]gin.H, 0, len(h.uris))
	for _, u := range h.uris {
		iceServers = append(iceServers, gin.H{
			"urls":       u,
			"username":   username,
			"credential": credential,
		})
	}

	logrus.WithFields(logrus.Fields{
		"client_ip":  c.ClientIP(),
		"expires_in": h.ttlSeconds,
	}).Debug("签发 TURN 短效凭据")

	c.JSON(http.StatusOK, gin.H{
		"ice_servers": iceServers,
		"ttl_seconds": h.ttlSeconds,
	})
}
