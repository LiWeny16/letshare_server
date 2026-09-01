// Package turnserver 在 Go 后端进程内嵌一个 TURN/STUN 中继（基于 pion/turn/v2）。
//
// 与 gin HTTP/WS 同进程共存：UDP/TCP listener 独立端口（默认 3478），
// 认证复用 turnauth（与 /api/turn-credentials 签发同一 secret），
// 中继端口段可配置。未启用时 Start 返回 nil，行为与旧版完全一致。
package turnserver

import (
	"fmt"
	"net"
	"time"

	"github.com/pion/turn/v2"
	"github.com/sirupsen/logrus"

	"letshare-server/internal/turnauth"
)

// Config 嵌入式 TURN 服务配置（由 config.TURN 映射）。
type Config struct {
	Secret       string // 与 handler 共享的 static-auth-secret
	PublicIP     string // 对外公网 IP（下发 relay 地址用；本地默认 127.0.0.1）
	Port         int    // TURN/STUN listener 端口（UDP+TCP 同端口）
	RelayPortMin int    // 中继端口段下界（含）
	RelayPortMax int    // 中继端口段上界（含）
	Realm        string // TURN realm
}

// Server 包装 pion TURN server，支持优雅关闭。
type Server struct {
	udp *turn.Server
	tcp *turn.Server
	// pion 无 Stop API，close 只保证 listener 退出；进程级关闭语义足够（与 gin 一致）
}

// Start 启动 UDP+TCP TURN listener。secret 为空视为未启用（返回 nil, nil）。
func Start(cfg Config) (*Server, error) {
	if cfg.Secret == "" {
		return nil, fmt.Errorf("turn secret is empty")
	}
	if cfg.Realm == "" {
		cfg.Realm = "letshare.fun"
	}

	relayGen := &turn.RelayAddressGeneratorPortRange{
		RelayAddress: net.ParseIP(cfg.PublicIP), // 下发给客户端的地址（可为 NAT 公网 IP）
		Address:      "0.0.0.0",                 // 实际绑定地址
		MinPort:      uint16(cfg.RelayPortMin),
		MaxPort:      uint16(cfg.RelayPortMax),
	}

	srv := &Server{}

	// UDP listener（主路径：WebRTC 媒体首选 UDP）
	udpAddr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	udpConn, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("turn udp listen %s: %w", udpAddr, err)
	}
	udpSrv, err := turn.NewServer(turn.ServerConfig{
		Realm:         cfg.Realm,
		AuthHandler:   authHandler(cfg.Secret, cfg.Realm),
		PacketConnConfigs: []turn.PacketConnConfig{
			{PacketConn: udpConn, RelayAddressGenerator: relayGen},
		},
	})
	if err != nil {
		udpConn.Close()
		srv.Close()
		return nil, fmt.Errorf("turn udp server: %w", err)
	}
	srv.udp = udpSrv

	// TCP listener（兜底路径：UDP 被禁的网络，浏览器可用 turn:...?transport=tcp）
	tcpAddr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	tcpListener, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		logrus.WithError(err).Warn("TURN TCP listener 启动失败，仅提供 UDP 中继")
	} else {
		tcpSrv, err := turn.NewServer(turn.ServerConfig{
			Realm:       cfg.Realm,
			AuthHandler:   authHandler(cfg.Secret, cfg.Realm),
			ListenerConfigs: []turn.ListenerConfig{
				{Listener: tcpListener, RelayAddressGenerator: relayGen},
			},
		})
		if err != nil {
			tcpListener.Close()
			logrus.WithError(err).Warn("TURN TCP server 启动失败，仅提供 UDP 中继")
		} else {
			srv.tcp = tcpSrv
		}
	}

	logrus.WithFields(logrus.Fields{
		"listen":       udpAddr,
		"public_ip":    cfg.PublicIP,
		"relay_range":  fmt.Sprintf("%d-%d", cfg.RelayPortMin, cfg.RelayPortMax),
		"tcp_enabled":  srv.tcp != nil,
	}).Info("嵌入式 TURN 服务已启动")

	return srv, nil
}

// authHandler 把 pion 的认证回调桥接到 turnauth.Verify。
// pion 语义：返回 (key, true) 表示该 username 合法，key 为 RFC 5389 长期凭据
// MESSAGE-INTEGRITY key（MD5(username:realm:credential)），pion 用它与客户端
// 呈上的 MESSAGE-INTEGRITY 直接比对。credential 由签发端点下发（浏览器持有）。
func authHandler(secret, realm string) func(username, realm string, srcAddr net.Addr) ([]byte, bool) {
	return func(username, realmFromMsg string, srcAddr net.Addr) ([]byte, bool) {
		// realm 以消息呈上的为准（与本服务配置一致；不一致则 key 必然失配，自然拒绝）
		key, ok := turnauth.Verify(secret, username, realmFromMsg, time.Now())
		if ok {
			return key, true
		}
		logrus.WithFields(logrus.Fields{
			"username": username,
			"src":      srcAddr.String(),
		}).Debug("TURN 认证拒绝")
		return nil, false
	}
}

// Close 关闭 listener（尽力而为；pion v2 无完全 Stop 语义）。
func (s *Server) Close() {
	if s == nil {
		return
	}
	if s.udp != nil {
		if err := s.udp.Close(); err != nil {
			logrus.WithError(err).Warn("TURN UDP server 关闭异常")
		}
	}
	if s.tcp != nil {
		if err := s.tcp.Close(); err != nil {
			logrus.WithError(err).Warn("TURN TCP server 关闭异常")
		}
	}
}
