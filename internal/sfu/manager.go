package sfu

import (
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/pion/webrtc/v4"
)

// Manager 按 roomID 管理多个 Room，是 SFU 的顶层入口。
// 主线可通过 Manager 让某个会议房间进入/退出媒体会话。
type Manager struct {
	mu    sync.RWMutex
	rooms map[string]*Room
	api   *API
}

// NewManager 创建一个 SFU Manager。
// se 通常传空 SettingEngine（生产）；离线回环测试传 OfflineSettingEngine()。
func NewManager(se *webrtc.SettingEngine) (*Manager, error) {
	api, err := NewAPI(se)
	if err != nil {
		return nil, err
	}
	return &Manager{rooms: map[string]*Room{}, api: api}, nil
}

// MustNewManager 与 NewManager 等价，失败时直接 panic。
// 仅在进程启动阶段、配置固定且不可能失败时使用。
func MustNewManager(se *webrtc.SettingEngine) *Manager {
	m, err := NewManager(se)
	if err != nil {
		log.WithError(err).Panic("sfu: 初始化 Manager 失败")
	}
	return m
}

// JoinRoom 返回指定 roomID 对应的 Room；不存在则创建一个。
// 幂等，可在同一 roomID 上反复调用。
func (m *Manager) JoinRoom(roomID string) *Room {
	m.mu.RLock()
	r, ok := m.rooms[roomID]
	m.mu.RUnlock()
	if ok {
		return r
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// 双重检查，防止并发 JoinRoom 重复建 Room
	if r, ok := m.rooms[roomID]; ok {
		return r
	}
	r = &Room{
		ID:           roomID,
		participants: map[string]*Participant{},
		api:          m.api,
	}
	m.rooms[roomID] = r
	return r
}

// GetRoom 返回指定 roomID 的 Room 及是否存在。
func (m *Manager) GetRoom(roomID string) (*Room, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rooms[roomID]
	return r, ok
}

// RemoveRoom 移除并关闭指定 Room。不存在时静默返回。
func (m *Manager) RemoveRoom(roomID string) {
	m.mu.Lock()
	r, ok := m.rooms[roomID]
	if ok {
		delete(m.rooms, roomID)
	}
	m.mu.Unlock()
	if ok && r != nil {
		_ = r.Close()
	}
}

// Rooms 返回当前所有 Room 的 ID 列表。
func (m *Manager) Rooms() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.rooms))
	for id := range m.rooms {
		ids = append(ids, id)
	}
	return ids
}

// API 暴露共享 API，供需要与服务端同配置创建连接（如测试客户端）时使用。
func (m *Manager) API() *API { return m.api }
