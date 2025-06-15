package main

import (
	"letshare-server/internal/config"
	"letshare-server/internal/handler"
	"letshare-server/internal/middleware"
	"letshare-server/internal/service"
	"letshare-server/pkg/logger"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func main() {
	// 初始化配置
	cfg := config.Load()

	// 初始化日志
	logger.Init(cfg.Log.Level, cfg.Log.MaxEntries)

	// 根据模式设置Gin
	if cfg.Mode == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 创建服务
	wsService := service.NewWebSocketService(cfg.WebSocket.MaxRoomUsers)
	authService := service.NewAuthService()

	// 创建路由
	r := gin.New()

	// 中间件
	r.Use(gin.Recovery())
	r.Use(middleware.Logger())
	r.Use(middleware.ErrorHandler())

	// CORS配置
	corsConfig := cors.Config{
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "Upgrade", "Connection", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Protocol"},
		AllowCredentials: true,
		AllowOriginFunc: func(origin string) bool {
			logrus.WithField("origin", origin).Debug("检查CORS来源")

			// 检查配置文件中的允许来源
			for _, allowedOrigin := range cfg.CORS.AllowedOrigins {
				if origin == allowedOrigin {
					logrus.WithField("origin", origin).Debug("CORS允许：配置文件匹配")
					return true
				}
			}

			// 检查是否是192.168.1.*网段
			if origin != "" {
				// 使用正则表达式匹配 http(s)://192.168.1.xxx:端口
				pattern := `^https?://192\.168\.1\.\d{1,3}(:\d+)?/?$`
				matched, err := regexp.MatchString(pattern, origin)
				if err == nil && matched {
					logrus.WithField("origin", origin).Debug("CORS允许：192.168.1.*网段匹配")
					return true
				}

				// 额外检查：简单的字符串前缀匹配作为备用
				if strings.HasPrefix(origin, "http://192.168.1.") || strings.HasPrefix(origin, "https://192.168.1.") {
					logrus.WithField("origin", origin).Debug("CORS允许：192.168.1.*前缀匹配")
					return true
				}
			}

			logrus.WithField("origin", origin).Warn("CORS拒绝：未匹配任何规则")
			return false
		},
	}
	r.Use(cors.New(corsConfig))

	// 创建处理器
	wsHandler := handler.NewWebSocketHandler(wsService, authService)
	healthHandler := handler.NewHealthHandler(wsService)

	// 路由
	r.GET("/health", healthHandler.Health)
	r.GET("/metrics", healthHandler.Metrics)
	r.GET("/ws", wsHandler.HandleWebSocket)
	r.GET("/", wsHandler.HandleWebSocket)
	// 启动服务器
	logrus.WithField("port", cfg.Server.Port).Info("启动WebSocket服务器")

	// 优雅关闭
	go func() {
		if cfg.TLS.Enabled {
			// 检查证书文件是否存在
			if _, err := os.Stat(cfg.TLS.CertFile); err == nil {
				logrus.WithFields(logrus.Fields{
					"port":   cfg.Server.Port,
					"domain": cfg.TLS.Domain,
				}).Info("启动 HTTPS/WSS 服务器")
				if err := r.RunTLS(":"+cfg.Server.Port, cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
					logrus.WithError(err).Fatal("HTTPS 服务器启动失败")
				}
			} else {
				logrus.WithError(err).Warn("SSL证书文件不存在，降级为HTTP模式")
				if err := r.Run(":" + cfg.Server.Port); err != nil {
					logrus.WithError(err).Fatal("服务器启动失败")
				}
			}
		} else {
			logrus.Info("启动 HTTP/WS 服务器")
			if err := r.Run(":" + cfg.Server.Port); err != nil {
				logrus.WithError(err).Fatal("服务器启动失败")
			}
		}
	}()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logrus.Info("正在关闭服务器...")
	wsService.Shutdown()
	logrus.Info("服务器已关闭")
}
