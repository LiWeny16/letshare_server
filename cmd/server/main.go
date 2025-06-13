package main

import (
	"letshare-server/internal/config"
	"letshare-server/internal/handler"
	"letshare-server/internal/middleware"
	"letshare-server/internal/service"
	"letshare-server/pkg/logger"
	"os"
	"os/signal"
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
	jwtService := service.NewJWTService(cfg.JWT.Secret, cfg.JWT.ExpirationHours)
	
	// 创建路由
	r := gin.New()
	
	// 中间件
	r.Use(gin.Recovery())
	r.Use(middleware.Logger())
	r.Use(middleware.ErrorHandler())
	
	// CORS配置
	corsConfig := cors.Config{
		AllowOrigins:     cfg.CORS.AllowedOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "Upgrade", "Connection", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Protocol"},
		AllowCredentials: true,
	}
	r.Use(cors.New(corsConfig))
	
	// 创建处理器
	wsHandler := handler.NewWebSocketHandler(wsService, jwtService)
	healthHandler := handler.NewHealthHandler(wsService)
	
	// 路由
	r.GET("/health", healthHandler.Health)
	r.GET("/metrics", healthHandler.Metrics)
	r.GET("/ws", wsHandler.HandleWebSocket)
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