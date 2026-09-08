package sfu

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

// subTrack 描述一条“把发布者的远端 track 转发给订阅客户端”的轨道：
// remote 是发布者 publish 上来的权威 track，local 是写向订阅客户端的本地轨道。
type subTrack struct {
	remote *webrtc.TrackRemote
	local  *webrtc.TrackLocalStaticRTP
	sender *webrtc.RTPSender
}

// Subscriber 是一条“订阅连接”：服务器据此把发布者的 track 扇出给某位订阅者。
// 每个订阅者-发布者 对拥有一条独立 PeerConnection，故障互不影响（广播脆弱隔离）。
//
// 生命周期：
//  1. Participant.SubscribeTo 返回 *Subscriber（内部已建好 delivery PC 并生成 offer）；
//  2. 主线把 Offer() 交给订阅者客户端，客户端 answer 后经 SetRemoteDescription 回传；
//  3. ICE 候选经 AddICECandidate / OnICECandidate 双向交换；
//  4. 服务器从发布者 track 读 RTP 并 WriteRTP 给订阅者。
type Subscriber struct {
	publisherID    string
	forParticipant *Participant
	pc             *webrtc.PeerConnection

	mu        sync.RWMutex
	locTracks map[string]*subTrack
	offer     webrtc.SessionDescription
	// offerMu 串行化同一订阅 PC 的 AddTrack/CreateOffer/SetRemoteDescription。
	// 发布者的音频、摄像头和屏幕轨可能在同一个事件循环内连续到达；若每条轨
	// 都立即 CreateOffer，会把第二个 offer 发在第一个 answer 之前，浏览器
	// 可能丢弃其中一条，形成“偶尔看不到对方视频/共享屏幕”的竞态。
	offerMu              sync.Mutex
	offerGeneration      uint64
	negotiatedGeneration uint64
	awaitingAnswer       bool

	stop chan struct{}
	once sync.Once
	off  atomic.Bool

	onICECandidate func(*webrtc.ICECandidate)
}

// Offer 返回需要信令给订阅者客户端的 SDP offer。
func (s *Subscriber) Offer() webrtc.SessionDescription {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.offer
}

// OfferForRetry 返回仍在等待客户端 answer 的当前 offer。
// 仅用于客户端丢帧/重试恢复：稳定连接不重复发 offer。
func (s *Subscriber) OfferForRetry() (webrtc.SessionDescription, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.awaitingAnswer || s.offer.SDP == "" {
		return webrtc.SessionDescription{}, false
	}
	return s.offer, true
}

// AddPublishedTrack 发布者后续新增 track（如中途开始屏幕共享）时：
// 为其建转发轨道并生成重协商 offer，由主线把新 offer 定向推给订阅客户端。
// 已转发过的 track 幂等跳过（返回空 SDP 表示无需重协商）。
func (s *Subscriber) AddPublishedTrack(remote *webrtc.TrackRemote) (webrtc.SessionDescription, error) {
	var empty webrtc.SessionDescription
	s.offerMu.Lock()
	defer s.offerMu.Unlock()
	if s.off.Load() {
		return empty, errors.New("sfu: 订阅连接已关闭")
	}
	s.mu.RLock()
	_, exists := s.locTracks[remote.ID()]
	s.mu.RUnlock()
	if exists {
		return empty, nil
	}
	if err := s.addForwardTrack(remote); err != nil {
		return empty, err
	}
	s.mu.Lock()
	s.offerGeneration++
	awaitingAnswer := s.awaitingAnswer
	s.mu.Unlock()
	// 第一个 offer 尚未收到 answer：只登记轨道，等 answer 到达后统一生成
	// 一个包含所有新增轨道的 offer，避免连续 offer 破坏浏览器的状态机。
	if awaitingAnswer {
		return empty, nil
	}
	return s.createOfferLocked()
}

// SetRemoteDescription 接收订阅者客户端对订阅 offer 的 answer（或其重协商）。
func (s *Subscriber) SetRemoteDescription(sd webrtc.SessionDescription) error {
	s.offerMu.Lock()
	defer s.offerMu.Unlock()
	if s.off.Load() {
		return errors.New("sfu: 订阅连接已关闭")
	}
	if err := s.pc.SetRemoteDescription(sd); err != nil {
		return err
	}
	if sd.Type == webrtc.SDPTypeAnswer {
		s.mu.Lock()
		s.awaitingAnswer = false
		s.mu.Unlock()
		// Ask the publisher for a fresh keyframe after the delivery PC is
		// negotiated. This closes the join-after-keyframe gap even before the
		// browser emits its first PLI.
		go s.requestPublisherKeyframes()
	}
	return nil
}

// PendingOffer 在 answer 确认后检查是否有 answer 到达期间新增的轨道。
// 若有，则生成一个合并后的后续 offer；调用方负责通过信令送达客户端。
func (s *Subscriber) PendingOffer() (webrtc.SessionDescription, bool, error) {
	s.offerMu.Lock()
	defer s.offerMu.Unlock()
	if s.off.Load() {
		return webrtc.SessionDescription{}, false, errors.New("sfu: 订阅连接已关闭")
	}
	s.mu.RLock()
	needsOffer := !s.awaitingAnswer && s.offerGeneration > s.negotiatedGeneration
	s.mu.RUnlock()
	if !needsOffer {
		return webrtc.SessionDescription{}, false, nil
	}
	offer, err := s.createOfferLocked()
	return offer, err == nil, err
}

// createOfferLocked 必须在 offerMu 持有时调用。
func (s *Subscriber) createOfferLocked() (webrtc.SessionDescription, error) {
	var empty webrtc.SessionDescription
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return empty, err
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return empty, err
	}
	s.mu.Lock()
	s.offer = offer
	s.awaitingAnswer = true
	s.negotiatedGeneration = s.offerGeneration
	s.mu.Unlock()
	return offer, nil
}

// AddICECandidate 是 ICE 信号接入点。
func (s *Subscriber) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if s.off.Load() {
		return errors.New("sfu: 订阅连接已关闭")
	}
	return s.pc.AddICECandidate(candidate)
}

// NewICECandidate 是 ICE 信号接入点（候选对象形式）。
func (s *Subscriber) NewICECandidate(candidate *webrtc.ICECandidate) error {
	if s.off.Load() {
		return errors.New("sfu: 订阅连接已关闭")
	}
	return s.pc.AddICECandidate(candidate.ToJSON())
}

// OnICECandidate 注册回调，把订阅连接的本地候选转发给订阅者客户端。
func (s *Subscriber) OnICECandidate(cb func(*webrtc.ICECandidate)) {
	if cb == nil || s.off.Load() {
		return
	}
	s.mu.Lock()
	s.onICECandidate = cb
	s.mu.Unlock()
}

// emitICECandidate 分发给已注册回调（供内部使用）。
func (s *Subscriber) emitICECandidate(c *webrtc.ICECandidate) {
	s.mu.RLock()
	cb := s.onICECandidate
	s.mu.RUnlock()
	if cb != nil {
		cb(c)
	}
}

// addForwardTrack 为发布者的一个 track 建本地转发轨道并启动转发。
// 仅转发音频与视频 kind（屏幕共享是另一个视频 track，天然走同一路径）。
func (s *Subscriber) addForwardTrack(remote *webrtc.TrackRemote) error {
	if remote == nil {
		return nil
	}
	if remote.Kind() != webrtc.RTPCodecTypeAudio && remote.Kind() != webrtc.RTPCodecTypeVideo {
		return nil
	}
	c := remote.Codec()
	local, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:     c.MimeType,
		ClockRate:    c.ClockRate,
		Channels:     c.Channels,
		SDPFmtpLine:  c.SDPFmtpLine,
		RTCPFeedback: c.RTCPFeedback,
	}, remote.ID()+"-fwd", remote.StreamID())
	if err != nil {
		return err
	}
	sender, err := s.pc.AddTrack(local)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.locTracks[remote.ID()] = &subTrack{remote: remote, local: local, sender: sender}
	s.mu.Unlock()

	s.startForwarder(remote, local)
	s.startRTCPDrain(sender)
	return nil
}

// startForwarder 起一个 goroutine：从发布者远端 track 读 RTP，写入订阅者本地轨道。
// 全程错误局部化：任何读/写错误都只结束本转发 goroutine，不影响其它轨道/参与者。
func (s *Subscriber) startForwarder(remote *webrtc.TrackRemote, local *webrtc.TrackLocalStaticRTP) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WithFields(log.Fields{
					"sfu":           "subscriber",
					"roomID":        s.forParticipant.room.ID,
					"participantID": s.forParticipant.ID,
					"publisherID":   s.publisherID,
				}).WithField("panic", r).Error("订阅转发轨道异常结束")
			}
		}()
		for {
			select {
			case <-s.stop:
				return
			default:
			}
			pkt, _, err := remote.ReadRTP()
			if err != nil {
				if !errors.Is(err, io.EOF) && !s.off.Load() {
					log.WithError(err).WithFields(log.Fields{
						"sfu":    "subscriber",
						"roomID": s.forParticipant.room.ID,
					}).Warn("订阅转发：读取发布者 RTP 失败")
				}
				return
			}
			if err := local.WriteRTP(pkt); err != nil {
				if !s.off.Load() {
					log.WithError(err).WithFields(log.Fields{
						"sfu": "subscriber",
					}).Warn("订阅转发：写入订阅者 RTP 失败")
				}
				return
			}
		}
	}()
}

// startRTCPDrain 显式处理订阅连接的 RTCP：订阅者侧（receiver report / PLI / NACK）
// 在这里被读取消费，避免阻塞。后续需要把 PLI/NACK 反馈回发布者时，在此挂接即可。
func (s *Subscriber) startRTCPDrain(sender *webrtc.RTPSender) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WithField("panic", r).Warn("订阅 RTCP 处理异常结束")
			}
		}()
		for {
			select {
			case <-s.stop:
				return
			default:
			}
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			// 目前此基座仅消费 RTCP；如需把 PLI/NACK 反馈给发布者，在这里反向转发。
			if hasKeyframeFeedback(pkts) {
				s.requestPublisherKeyframes()
			}
		}
	}()
}

// hasKeyframeFeedback identifies RTCP feedback that asks an encoder to
// produce a decodable intra frame. Receiver reports and transport feedback
// stay local to the delivery PC because they use the delivery SSRC space.
func hasKeyframeFeedback(pkts []rtcp.Packet) bool {
	for _, pkt := range pkts {
		switch pkt.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			return true
		}
	}
	return false
}

// requestPublisherKeyframes translates delivery-side keyframe feedback into
// publisher-side PLI packets. The server creates a new local track for each
// subscriber, so a subscriber PLI's SSRC must not be forwarded verbatim.
func (s *Subscriber) requestPublisherKeyframes() {
	if s.off.Load() {
		return
	}
	publisher, ok := s.forParticipant.room.GetParticipant(s.publisherID)
	if !ok || publisher.IsClosed() {
		return
	}

	for _, track := range s.videoTracks() {
		if err := publisher.writeRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{
			MediaSSRC: uint32(track.remote.SSRC()),
		}}); err != nil && !s.off.Load() {
			log.WithError(err).WithFields(log.Fields{
				"sfu":         "subscriber",
				"roomID":      s.forParticipant.room.ID,
				"publisherID": s.publisherID,
			}).Debug("订阅端关键帧请求回传失败")
		}
	}
}

func (s *Subscriber) videoTracks() []*subTrack {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tracks := make([]*subTrack, 0, len(s.locTracks))
	for _, track := range s.locTracks {
		if track.remote != nil && track.remote.Kind() == webrtc.RTPCodecTypeVideo {
			tracks = append(tracks, track)
		}
	}
	return tracks
}

// IsClosed 返回该订阅连接是否已关闭。
func (s *Subscriber) IsClosed() bool { return s.off.Load() }

// Close 关闭订阅连接，幂等。
// 关闭后从所属参与者的订阅表摘除自身，避免 closed 订阅残留被复用。
func (s *Subscriber) Close() error {
	var err error
	s.once.Do(func() {
		s.off.Store(true)
		close(s.stop)
		err = s.pc.Close()
	})
	s.forParticipant.forgetClosedSubscription(s.publisherID, s)
	return err
}
