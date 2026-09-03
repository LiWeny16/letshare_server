package sfu

import (
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// API 包装一个共享的 *webrtc.API。同一个 API 派生出的所有 PeerConnection
// 共享同一套 MediaEngine / Interceptor / SettingEngine，保证房间内各连接
// 的编解码与 ICE 行为一致。
type API struct {
	webrtc *webrtc.API
}

// NewAPI 用给定 SettingEngine 构造一个 SFU 共享 API。
// 生产环境通常传入空的 *webrtc.SettingEngine；离线回环测试传 OfflineSettingEngine()。
func NewAPI(se *webrtc.SettingEngine) (*API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(*se),
	)
	return &API{webrtc: api}, nil
}

// NewPeerConnection 用本 API 创建一个 PeerConnection。
// 主要用于离线回环测试里模拟“客户端”一侧的连接。
func (a *API) NewPeerConnection(config webrtc.Configuration) (*webrtc.PeerConnection, error) {
	return a.webrtc.NewPeerConnection(config)
}

// newPeerConnection 为服务端（Room / Subscriber）创建连接。
func (a *API) newPeerConnection() (*webrtc.PeerConnection, error) {
	return a.webrtc.NewPeerConnection(webrtc.Configuration{})
}

// OfflineSettingEngine 返回一套“离线化”的 SettingEngine：
// 禁用 mDNS 并把候选限制为 UDP4 主机候选，使同一进程内的两个 PeerConnection
// 在无外网 / 无 STUN 的情况下也能通过主机候选连通。
// 仅用于单元测试；生产部署不要使用。
func OfflineSettingEngine() *webrtc.SettingEngine {
	se := &webrtc.SettingEngine{}
	se.SetICETimeouts(500*time.Millisecond, 1*time.Second, 200*time.Millisecond)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	return se
}
