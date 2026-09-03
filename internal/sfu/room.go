package sfu

import (
	"fmt"
	"sync"
	"sync/atomic"

	log "github.com/sirupsen/logrus"
	"github.com/pion/webrtc/v4"
)

// Room 代表一个会议的媒体会话容器：包含参与者集合，
// 并为参与者创建 PeerConnection（各自独立，错误局部化）。
type Room struct {
	ID string

	mu           sync.RWMutex
	participants map[string]*Participant
	api          *API
	closed       atomic.Bool

	// OnTrackPublished 可选回调：某参与者 publish 了新 track 时触发。
	// 主线可借此触发“已订阅该发布者的订阅端发起重协商”。
	OnTrackPublished func(roomID, publisherID string, track *webrtc.TrackRemote)
}

// AddParticipant 在房间内新增一名参与者，并为其建立订阅/发布用的
// 主 PeerConnection。participantID 建议使用前端的 uniqId。
//
// 每个参与者的 PeerConnection 独立创建、独立追踪连接态，故障互不影响。
func (r *Room) AddParticipant(participantID string) (*Participant, error) {
	if r.closed.Load() {
		return nil, fmt.Errorf("sfu: room %q 已关闭", r.ID)
	}

	pc, err := r.api.newPeerConnection()
	if err != nil {
		return nil, fmt.Errorf("sfu: 为参与者 %q 创建 PeerConnection 失败: %w", participantID, err)
	}

	p := &Participant{
		ID:           participantID,
		room:         r,
		pc:           pc,
		tracks:       map[string]*webrtc.TrackRemote{},
		subscriptions: map[string]*Subscriber{},
	}

	// 监测连接态，异常仅记录并局部化，不影响房间其它参与者。
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.WithFields(log.Fields{
			"sfu":      "participant",
			"roomID":   r.ID,
			"participantID": participantID,
			"state":    state.String(),
		}).Debug("参与者的主连接状态变化")
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			_ = p.Close()
		}
	})

	// 收到远端 track（即该参与者 publish 上来的媒体），登记为“已发布 track”。
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// 明确解除读超时：远端实时媒体流不应有到期读截止。
		_ = track.SetReadDeadline(noReadDeadline())
		p.registerPublishedTrack(track)
		r.notifyTrackPublished(participantID, track)
	})

	// 本地 ICE 候选通过回调交给主线做信令转发。
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		p.emitICECandidate(c)
	})

	r.mu.Lock()
	r.participants[participantID] = p
	r.mu.Unlock()

	log.WithFields(log.Fields{
		"sfu":      "room",
		"roomID":   r.ID,
		"participantID": participantID,
	}).Info("参与者加入 SFU 房间")
	return p, nil
}

// GetParticipant 返回指定 participantID 的参与者及是否存在。
func (r *Room) GetParticipant(participantID string) (*Participant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.participants[participantID]
	return p, ok
}

// RemoveParticipant 断开并移除一位参与者；其所有订阅连接一并关闭。
// 异常仅局部化，不影响房间其它参与者。
func (r *Room) RemoveParticipant(participantID string) error {
	r.mu.Lock()
	p, ok := r.participants[participantID]
	if ok {
		delete(r.participants, participantID)
	}
	r.mu.Unlock()
	if !ok {
		return nil
	}
	if err := p.Close(); err != nil {
		return fmt.Errorf("sfu: 关闭参与者 %q 失败: %w", participantID, err)
	}
	return nil
}

// Participants 返回当前所有参与者。
func (r *Room) Participants() []*Participant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, p)
	}
	return out
}

// Count 返回参与者数量。
func (r *Room) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.participants)
}

// Close 关闭房间及其所有参与者的 PeerConnection。
func (r *Room) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	r.mu.RLock()
	parts := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		parts = append(parts, p)
	}
	r.mu.RUnlock()

	for _, p := range parts {
		_ = p.Close()
	}
	log.WithField("roomID", r.ID).Info("SFU 房间已关闭")
	return nil
}

func (r *Room) notifyTrackPublished(publisherID string, track *webrtc.TrackRemote) {
	if r.OnTrackPublished != nil {
		r.OnTrackPublished(r.ID, publisherID, track)
	}
}
