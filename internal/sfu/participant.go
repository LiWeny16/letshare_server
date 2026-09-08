package sfu

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

// noReadDeadline 返回“无读截止”的 time.Time（零值），用于显式解除远端 track 的读超时。
func noReadDeadline() time.Time { return time.Time{} }

// Participant 是一名参与者的连接端点。
//
// 它持有一条“主 PeerConnection”：参与者把自身的音/视频/屏幕 track publish 上来，
// 服务器把它当远端 track 登记；同时它管理若干条 *Subscriber 订阅连接，
// 用于把房间内其它参与者的 track 扇出给该参与者。
//
// 每个 Participant 拥有独立的 PeerConnection 与独立错误域，一个参与者不可用
// 不会波及其它参与者（广播脆弱隔离）。
type Participant struct {
	ID   string
	room *Room
	pc   *webrtc.PeerConnection
	pcMu sync.RWMutex

	mu            sync.RWMutex
	tracks        map[string]*webrtc.TrackRemote // 该参与者 publish 的权威 track（id -> track）
	subscriptions map[string]*Subscriber         // publisherID -> 扇出给本参与者的订阅连接
	closed        atomic.Bool

	onICECandidate func(*webrtc.ICECandidate)
}

// IsClosed reports whether the participant's primary PeerConnection has been
// retired. Callers use this when reconciling subscriptions concurrently with
// disconnect cleanup.
func (p *Participant) IsClosed() bool { return p.closed.Load() }

func (p *Participant) currentPeerConnection() *webrtc.PeerConnection {
	p.pcMu.RLock()
	defer p.pcMu.RUnlock()
	return p.pc
}

func (p *Participant) isCurrentPeerConnection(candidate *webrtc.PeerConnection) bool {
	p.pcMu.RLock()
	defer p.pcMu.RUnlock()
	return p.pc == candidate
}

// bindPeerConnection 注册参与者主连接的回调。坏掉的首次 offer 会替换主 PC，
// 所以这些回调必须同时绑定到新连接；旧连接关闭时也不能误关闭新连接。
func (p *Participant) bindPeerConnection(pc *webrtc.PeerConnection) {
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.WithFields(log.Fields{
			"sfu":           "participant",
			"roomID":        p.room.ID,
			"participantID": p.ID,
			"state":         state.String(),
		}).Debug("参与者的主连接状态变化")
		if (state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed) && p.isCurrentPeerConnection(pc) {
			_ = p.Close()
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		_ = track.SetReadDeadline(noReadDeadline())
		p.registerPublishedTrack(track)
		p.room.notifyTrackPublished(p.ID, track)
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		p.emitICECandidate(c)
	})
}

// Offer 接收远端的 offer，在服务器侧 SetRemoteDescription、CreateAnswer、
// SetLocalDescription，并返回 answer。
//
// 这是参与者“发布”方向的信号接入点：客户端把它的音/视频/屏幕 track 放进
// offer，服务器接受并登记这些 track。
func (p *Participant) Offer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	var empty webrtc.SessionDescription
	if p.closed.Load() {
		return empty, fmt.Errorf("sfu: 参与者 %q 已关闭", p.ID)
	}
	pc := p.currentPeerConnection()
	if err := pc.SetRemoteDescription(offer); err != nil {
		p.resetFailedInitialOffer(pc)
		return empty, fmt.Errorf("sfu: 参与者 %q SetRemoteDescription(offer) 失败: %w", p.ID, err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		p.resetFailedInitialOffer(pc)
		return empty, fmt.Errorf("sfu: 参与者 %q CreateAnswer 失败: %w", p.ID, err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		p.resetFailedInitialOffer(pc)
		return empty, fmt.Errorf("sfu: 参与者 %q SetLocalDescription(answer) 失败: %w", p.ID, err)
	}
	return answer, nil
}

// resetFailedInitialOffer 清理一次失败的首次 publish offer。
//
// Pion 会先把合法的 SDP 外壳推进到 have-remote-offer，再继续校验媒体描述。
// 因此像“缺少 ice-ufrag”这类后置校验失败，如果不回滚，参与者会永久卡在
// have-remote-offer。当前 Pion 版本没有实现 SetLocalDescription(rollback)，
// 因此这里替换尚未建立过远端描述的主 PeerConnection，并保留 Participant 身份。
func (p *Participant) resetFailedInitialOffer(failedPC *webrtc.PeerConnection) {
	if !p.isCurrentPeerConnection(failedPC) ||
		failedPC.SignalingState() != webrtc.SignalingStateHaveRemoteOffer ||
		failedPC.CurrentRemoteDescription() != nil {
		return
	}
	newPC, err := p.room.api.newPeerConnection()
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"sfu":           "participant",
			"roomID":        p.room.ID,
			"participantID": p.ID,
		}).Warn("重建 publish PeerConnection 失败")
		return
	}
	p.bindPeerConnection(newPC)
	p.pcMu.Lock()
	p.pc = newPC
	p.pcMu.Unlock()
	_ = failedPC.Close()
}

// SetRemoteDescription 是信号接入点：把远端的 SDP（answer 或后续 renegotiation）交给连接。
func (p *Participant) SetRemoteDescription(sd webrtc.SessionDescription) error {
	if p.closed.Load() {
		return fmt.Errorf("sfu: 参与者 %q 已关闭", p.ID)
	}
	return p.currentPeerConnection().SetRemoteDescription(sd)
}

// AddICECandidate 是 ICE 信号接入点：接收远端发来的候选（串行化到信令通道）。
func (p *Participant) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if p.closed.Load() {
		return fmt.Errorf("sfu: 参与者 %q 已关闭", p.ID)
	}
	return p.currentPeerConnection().AddICECandidate(candidate)
}

// NewICECandidate 是 ICE 信号接入点：接收一次握手过程中已收集的候选对象。
func (p *Participant) NewICECandidate(candidate *webrtc.ICECandidate) error {
	if p.closed.Load() {
		return fmt.Errorf("sfu: 参与者 %q 已关闭", p.ID)
	}
	return p.currentPeerConnection().AddICECandidate(candidate.ToJSON())
}

// writeRTCP sends feedback to the browser that publishes this participant's
// tracks. Subscriber-side PLI/FIR feedback must be routed back to that
// publisher; otherwise a subscriber that joins after the current keyframe can
// receive a live video track but never decode a frame.
func (p *Participant) writeRTCP(pkts []rtcp.Packet) error {
	if p.closed.Load() {
		return fmt.Errorf("sfu: 参与者 %q 已关闭", p.ID)
	}
	return p.currentPeerConnection().WriteRTCP(pkts)
}

// OnICECandidate 注册回调，用于把本地 ICE 候选转发给远端的客户端。
func (p *Participant) OnICECandidate(cb func(*webrtc.ICECandidate)) {
	if cb == nil || p.closed.Load() {
		return
	}
	p.mu.Lock()
	p.onICECandidate = cb
	p.mu.Unlock()
}

// emitICECandidate 把本地候选分发给已注册回调（供房间内部使用）。
func (p *Participant) emitICECandidate(c *webrtc.ICECandidate) {
	p.mu.RLock()
	cb := p.onICECandidate
	p.mu.RUnlock()
	if cb != nil {
		cb(c)
	}
}

// OnConnectionStateChange 注册连接态变化回调。
func (p *Participant) OnConnectionStateChange(cb func(webrtc.PeerConnectionState)) {
	p.currentPeerConnection().OnConnectionStateChange(cb)
}

// ConnectionState 返回主连接的 PeerConnection 状态（供测试 / 监控使用）。
func (p *Participant) ConnectionState() webrtc.PeerConnectionState {
	return p.currentPeerConnection().ConnectionState()
}

// registerPublishedTrack 登记参与者 publish 上来的 track。
func (p *Participant) registerPublishedTrack(track *webrtc.TrackRemote) {
	if track == nil {
		return
	}
	p.mu.Lock()
	p.tracks[track.ID()] = track
	p.mu.Unlock()
	log.WithFields(log.Fields{
		"sfu":           "participant",
		"roomID":        p.room.ID,
		"participantID": p.ID,
		"trackID":       track.ID(),
		"kind":          track.Kind().String(),
	}).Info("参与者发布了媒体 track")
}

// PublishedTracks 返回该参与者当前所有已发布的 track（快照）。
func (p *Participant) PublishedTracks() []*webrtc.TrackRemote {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*webrtc.TrackRemote, 0, len(p.tracks))
	for _, t := range p.tracks {
		out = append(out, t)
	}
	return out
}

// GetPublishedTrack 返回指定 id 的已发布 track 及是否存在。
func (p *Participant) GetPublishedTrack(id string) (*webrtc.TrackRemote, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, ok := p.tracks[id]
	return t, ok
}

// SubscribeTo 让本参与者订阅 publisherID 的发布（音/视频/屏幕 track）。
//
// 它新建一条独立的 PeerConnection，把 publisherID 当前已发布的 track 逐个
// 读取出并转发给本参与者，返回的 *Subscriber 携带为此连接生成的 offer，
// 主线需把该 offer 交给本参与者的客户端并回传 answer 与 ICE。
// 只转发音频与视频 kind 的 track。
//
// 幂等语义（P0 级联防线）：客户端在「订阅已成功但远端 track 未到达」时的重试
// 会再次发起 SubscribeTo —— 返回既有订阅（created=false）而不是报错。
// 旧实现返回「已订阅 …，请先 UnsubscribeFrom」400 错误帧，曾把 joining 重置为
// idle，引发 ICE 永不连接、摄像头/麦克风/共享屏幕全部失效的级联。
// 已关闭的旧订阅（发布者重进等场景）会被替换为新订阅，不得复用 closed 连接。
func (p *Participant) SubscribeTo(publisherID string) (*Subscriber, bool, error) {
	if p.closed.Load() {
		return nil, false, fmt.Errorf("sfu: 参与者 %q 已关闭", p.ID)
	}
	if publisherID == p.ID {
		return nil, false, fmt.Errorf("sfu: 参与者不能订阅自己: %q", p.ID)
	}
	pub, ok := p.room.GetParticipant(publisherID)
	if !ok {
		return nil, false, fmt.Errorf("sfu: 房间内不存在发布者 %q", publisherID)
	}

	p.mu.Lock()
	if existing := p.subscriptions[publisherID]; existing != nil {
		if !existing.IsClosed() {
			p.mu.Unlock()
			return existing, false, nil
		}
		delete(p.subscriptions, publisherID)
	}
	p.mu.Unlock()

	tracks := pub.PublishedTracks()
	if len(tracks) == 0 {
		return nil, false, fmt.Errorf("sfu: 发布者 %q 暂无已发布 track", publisherID)
	}

	sub, err := p.buildSubscriber(pub)
	if err != nil {
		return nil, false, err
	}

	p.mu.Lock()
	// 并发重复建订阅：先到者胜出，后建者关闭丢弃。
	if cur := p.subscriptions[publisherID]; cur != nil && cur != sub && !cur.IsClosed() {
		p.mu.Unlock()
		_ = sub.Close()
		return cur, false, nil
	}
	p.subscriptions[publisherID] = sub
	p.mu.Unlock()
	return sub, true, nil
}

// buildSubscriber 创建一条把发布者 track 转发给本参与者的 PeerConnection。
func (p *Participant) buildSubscriber(pub *Participant) (*Subscriber, error) {
	pc, err := p.room.api.newPeerConnection()
	if err != nil {
		return nil, fmt.Errorf("sfu: 创建订阅连接失败: %w", err)
	}
	sub := &Subscriber{
		publisherID:    pub.ID,
		forParticipant: p,
		pc:             pc,
		locTracks:      map[string]*subTrack{},
		stop:           make(chan struct{}),
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			_ = sub.Close()
		}
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		sub.emitICECandidate(c)
	})

	// 逐 track 建立本地转发轨道
	for _, remote := range pub.PublishedTracks() {
		if err := sub.addForwardTrack(remote); err != nil {
			_ = pc.Close()
			return nil, err
		}
	}
	// 初始 offer 也经过统一的 generation/answer 状态机，保证后续迟到轨道
	// 可以合并到下一次 renegotiation，而不会与首个 offer 并发。
	sub.offerMu.Lock()
	sub.offerGeneration = uint64(len(sub.locTracks))
	_, err = sub.createOfferLocked()
	sub.offerMu.Unlock()
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("sfu: 创建订阅 offer 失败: %w", err)
	}
	return sub, nil
}

// UnsubscribeFrom 停止订阅 publisherID，关闭对应订阅连接。
func (p *Participant) UnsubscribeFrom(publisherID string) {
	p.mu.Lock()
	sub := p.subscriptions[publisherID]
	if sub != nil {
		delete(p.subscriptions, publisherID)
	}
	p.mu.Unlock()
	if sub != nil {
		_ = sub.Close()
	}
}

// forgetClosedSubscription 订阅连接自关闭（PC failed/closed）后从订阅表摘除自身
// （subscriber.go Close 调用）。指针比对防止旧关闭连接误删后续新建的同名订阅。
func (p *Participant) forgetClosedSubscription(publisherID string, s *Subscriber) {
	p.mu.Lock()
	if p.subscriptions[publisherID] == s {
		delete(p.subscriptions, publisherID)
	}
	p.mu.Unlock()
}

// Subscriptions 返回当前订阅的发布者 id 列表。
func (p *Participant) Subscriptions() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.subscriptions))
	for id := range p.subscriptions {
		out = append(out, id)
	}
	return out
}

// GetSubscriber 返回本参与者对 publisherID 的订阅连接（供信令桥接 answer/ICE 用）。
func (p *Participant) GetSubscriber(publisherID string) (*Subscriber, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s, ok := p.subscriptions[publisherID]
	return s, ok
}

// Close 关闭参与者的主连接与全部订阅连接。幂等。
func (p *Participant) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	p.mu.Lock()
	subs := make([]*Subscriber, 0, len(p.subscriptions))
	for _, s := range p.subscriptions {
		subs = append(subs, s)
	}
	p.mu.Unlock()
	for _, s := range subs {
		_ = s.Close()
	}
	if err := p.currentPeerConnection().Close(); err != nil {
		return err
	}
	// 自摘除：PC failed/closed 的参与者不得残留在房间 map —— GetParticipant
	// 返回 closed participant 是「参与者已关闭」「暂无已发布 track」永久报错、
	// 以及重连后新会话复用 closed participant 的根因。
	p.room.forgetClosedParticipant(p)
	log.WithFields(log.Fields{
		"sfu":           "participant",
		"roomID":        p.room.ID,
		"participantID": p.ID,
	}).Info("参与者已离开 SFU")
	return nil
}
