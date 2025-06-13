package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ErrorHandler 错误处理中间件
func ErrorHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// 记录panic错误
				logrus.WithFields(logrus.Fields{
					"panic":     err,
					"path":      c.Request.URL.Path,
					"method":    c.Request.Method,
					"client_ip": c.ClientIP(),
				}).Error("服务器panic")
				
				// 返回统一的错误响应
				c.JSON(http.StatusInternalServerError, gin.H{
					"error":   "内部服务器错误",
					"message": "服务器遇到了意外错误，请稍后重试",
					"code":    500,
				})
				c.Abort()
			}
		}()
		
		c.Next()
		
		// 处理错误列表中的错误
		if len(c.Errors) > 0 {
			err := c.Errors.Last()
			
			logrus.WithFields(logrus.Fields{
				"error":     err.Error(),
				"path":      c.Request.URL.Path,
				"method":    c.Request.Method,
				"client_ip": c.ClientIP(),
			}).Error("请求处理错误")
			
			// 根据错误类型返回不同的HTTP状态码
			switch err.Type {
			case gin.ErrorTypeBind:
				c.JSON(http.StatusBadRequest, gin.H{
					"error":   "请求数据格式错误",
					"message": err.Error(),
					"code":    400,
				})
			case gin.ErrorTypePublic:
				c.JSON(http.StatusBadRequest, gin.H{
					"error":   "请求错误",
					"message": err.Error(),
					"code":    400,
				})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{
					"error":   "内部服务器错误",
					"message": "服务器处理请求时发生错误",
					"code":    500,
				})
			}
		}
	}
}

// CORSError 处理CORS相关错误
func CORSError() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		
		// 检查是否是预检请求
		if c.Request.Method == "OPTIONS" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, Upgrade, Connection, Sec-WebSocket-Key, Sec-WebSocket-Version, Sec-WebSocket-Protocol")
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Max-Age", "86400")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		
		c.Next()
	}
} 