package main

import (
	"context"
	"errors"
	"letshare-server/internal/config"
	"letshare-server/internal/handler"
	"letshare-server/internal/middleware"
	"letshare-server/internal/service"
	"letshare-server/internal/sfu"
	"letshare-server/internal/turnserver"
	"letshare-server/pkg/logger"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func main() {
	// 初始化配置
	cfg := config.Load()

	// 设置GOMAXPROCS以充分利用多核CPU
	if cfg.Runtime.GOMAXPROCS > 0 {
		runtime.GOMAXPROCS(cfg.Runtime.GOMAXPROCS)
		logrus.WithField("gomaxprocs", cfg.Runtime.GOMAXPROCS).Info("设置最大CPU核心数")
	} else {
		maxProcs := runtime.GOMAXPROCS(0)
		logrus.WithField("gomaxprocs", maxProcs).Info("使用所有可用CPU核心")
	}

	// 初始化日志
	logger.Init(cfg.Log.Level, cfg.Log.MaxEntries)

	// 根据模式设置Gin
	if cfg.Mode == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 创建服务
	wsService := service.NewWebSocketService(cfg.WebSocket.MaxRoomUsers)
	authService := service.NewAuthService()
	jwtService := service.NewJWTService(cfg.JWT.Secret, cfg.JWT.ExpirationHours)

	// 创建文件传输服务
	var fileTransferService *service.FileTransferService
	if cfg.FileTransfer.Enabled {
		fileTransferService = service.NewFileTransferService(
			wsService,
			cfg.FileTransfer.MaxFileSize,
			cfg.FileTransfer.ChunkSize,
		)
		logrus.WithFields(logrus.Fields{
			"max_file_size_mb": cfg.FileTransfer.MaxFileSize / (1024 * 1024),
			"chunk_size_kb":    cfg.FileTransfer.ChunkSize / 1024,
		}).Info("文件传输服务已启用")
	} else {
		logrus.Info("文件传输服务未启用")
	}

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
				localhostPattern := `^https?://(localhost|127\.0\.0\.1)(:\d+)?/?$`
				matched, err := regexp.MatchString(localhostPattern, origin)
				if err == nil && matched {
					logrus.WithField("origin", origin).Debug("CORS允许：localhost匹配")
					return true
				}

				pattern := `^https?://192\.168\.1\.\d{1,3}(:\d+)?/?$`
				matched, err = regexp.MatchString(pattern, origin)
				if err == nil && matched {
					logrus.WithField("origin", origin).Debug("CORS允许：192.168.1.*网段匹配")
					return true
				}

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
	wsHandler := handler.NewWebSocketHandler(wsService, authService, fileTransferService, jwtService)
	// The SFU is a server-side WebRTC endpoint.  Its ICE host candidates must
	// be the ECS public address; otherwise browsers only receive Docker/private
	// 172.x candidates and the signaling layer succeeds while media never does.
	if net.ParseIP(cfg.TURN.PublicIP) != nil {
		wsHandler.SetSFU(sfu.MustNewManager(sfu.PublicSettingEngine(cfg.TURN.PublicIP)))
	}
	healthHandler := handler.NewHealthHandler(wsService)
	glmHandler := handler.NewGLMHandler(cfg.GLM)

	// 嵌入式 TURN/STUN 中继（与 HTTP/WS 同进程；凭据签发端点共享同一 secret）
	var turnRelay *turnserver.Server
	if cfg.TURN.Enabled && cfg.TURN.Secret != "" {
		relay, err := turnserver.Start(turnserver.Config{
			Secret:       cfg.TURN.Secret,
			PublicIP:     cfg.TURN.PublicIP,
			Port:         cfg.TURN.Port,
			RelayPortMin: cfg.TURN.RelayPortMin,
			RelayPortMax: cfg.TURN.RelayPortMax,
			Realm:        cfg.TURN.Realm,
		})
		if err != nil {
			logrus.WithError(err).Fatal("嵌入式 TURN 服务启动失败")
		}
		turnRelay = relay
	} else {
		logrus.Info("TURN 未启用（turn.enabled=false 或 secret 为空）")
	}

	// 路由
	proHandler := handler.NewProHandler(jwtService, cfg.FileTransfer.ProInviteCode)
	turnHandler := handler.NewTurnHandler(cfg.TURN.Enabled, cfg.TURN.Secret, cfg.TURN.URIs, cfg.TURN.TTLSeconds)

	r.GET("/health", healthHandler.Health)
	r.GET("/metrics", healthHandler.Metrics)
	r.POST("/api/pro/activate", proHandler.Activate)
	r.GET("/api/turn-credentials", turnHandler.Credentials)
	r.GET("/ws", wsHandler.HandleWebSocket)
	r.GET("/", wsHandler.HandleWebSocket)
	r.Any("/api/glm/*path", glmHandler.Proxy)
	// 启动服务器
	logrus.WithField("port", cfg.Server.Port).Info("启动WebSocket服务器")
	logrus.WithField("pro_invite_code_set", cfg.FileTransfer.ProInviteCode != "").Info("PRO 邀请码已配置")

	// Start explicit http.Server instances so shutdown can drain listeners and
	// active connections before the process exits.
	addr := ":" + cfg.Server.Port
	apiServer := &http.Server{Addr: addr, Handler: r}
	var tlsCert, tlsKey string
	var redirectServer *http.Server
	if cfg.TLS.Enabled {
		if _, certErr := os.Stat(cfg.TLS.CertFile); certErr == nil {
			if _, keyErr := os.Stat(cfg.TLS.KeyFile); keyErr == nil {
				tlsCert, tlsKey = cfg.TLS.CertFile, cfg.TLS.KeyFile
				logrus.WithFields(logrus.Fields{
					"port":   cfg.Server.Port,
					"domain": cfg.TLS.Domain,
				}).Info("启动 HTTPS/WSS 服务器")
				redirect := func(w http.ResponseWriter, req *http.Request) {
					target := "https://" + req.Host + req.URL.RequestURI()
					http.Redirect(w, req, target, http.StatusMovedPermanently)
				}
				redirectServer = &http.Server{
					Addr:    ":8080",
					Handler: http.HandlerFunc(redirect),
				}
				logrus.Info("启动 HTTP→HTTPS 重定向 :8080") // :80 已让给 nginx（letshare.fun CDN HTTP 回源）
			} else {
				logrus.WithError(keyErr).WithField("key_file", cfg.TLS.KeyFile).
					Warn("SSL密钥文件不可访问，降级为HTTP模式")
			}
		} else {
			logrus.WithError(certErr).WithField("cert_file", cfg.TLS.CertFile).
				Warn("SSL证书文件不存在，降级为HTTP模式")
		}
	} else {
		logrus.Info("启动 HTTP/WS 服务器")
	}

	apiDone := make(chan struct{})
	go func() {
		defer close(apiDone)
		startWithRetry(apiServer, tlsCert, tlsKey)
	}()
	var redirectDone chan struct{}
	if redirectServer != nil {
		redirectDone = make(chan struct{})
		go func() {
			defer close(redirectDone)
			startWithRetry(redirectServer, "", "")
		}()
	}

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logrus.Info("正在关闭服务器...")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	if redirectServer != nil {
		_ = redirectServer.Shutdown(shutdownCtx)
	}
	_ = apiServer.Shutdown(shutdownCtx)
	cancelShutdown()
	<-apiDone
	if redirectDone != nil {
		<-redirectDone
	}
	if fileTransferService != nil {
		fileTransferService.Shutdown()
	}
	if turnRelay != nil {
		turnRelay.Close()
	}
	wsService.Shutdown()
	logrus.Info("服务器已关闭")
}

func startWithRetry(server *http.Server, certFile, keyFile string) {
	maxRetries := 3
	for i := 0; i < maxRetries; i++ {
		var err error
		if certFile != "" && keyFile != "" {
			err = server.ListenAndServeTLS(certFile, keyFile)
		} else {
			err = server.ListenAndServe()
		}
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}

		msg := formatBindError(err)
		logrus.WithError(err).WithField("addr", server.Addr).Errorf("启动失败: %s", msg)
		if isAddrInUse(err) && i < maxRetries-1 {
			wait := time.Duration(i+1) * time.Second
			logrus.WithField("retry_in", wait.String()).Warn("端口被占用，等待后重试...")
			time.Sleep(wait)
			continue
		}
		return
	}
}

func isAddrInUse(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok {
			return strings.Contains(sysErr.Error(), "address already in use") ||
				strings.Contains(sysErr.Error(), "Only one usage")
		}
		return strings.Contains(opErr.Error(), "address already in use") ||
			strings.Contains(opErr.Error(), "Only one usage")
	}
	return strings.Contains(err.Error(), "address already in use") ||
		strings.Contains(err.Error(), "bind") &&
			strings.Contains(err.Error(), "already in use")
}

func formatBindError(err error) string {
	if isAddrInUse(err) {
		return "端口已被占用，请检查是否有其他进程正在使用该端口，或更换端口后重试"
	}
	return err.Error()
}
