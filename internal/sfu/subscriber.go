package sfu

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"

	log "github.com/sirupsen/logrus"
	"github.com/pion/webrtc/v4"
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

// SetRemoteDescription 接收订阅者客户端对订阅 offer 的 answer（或其重协商）。
func (s *Subscriber) SetRemoteDescription(sd webrtc.SessionDescription) error {
	if s.off.Load() {
		return errors.New("sfu: 订阅连接已关闭")
	}
	return s.pc.SetRemoteDescription(sd)
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
					"sfu":          "subscriber",
					"roomID":       s.forParticipant.room.ID,
					"participantID": s.forParticipant.ID,
					"publisherID":  s.publisherID,
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
			_ = len(pkts) > 0
		}
	}()
}

// Close 关闭订阅连接，幂等。
func (s *Subscriber) Close() error {
	var err error
	s.once.Do(func() {
		s.off.Store(true)
		close(s.stop)
		err = s.pc.Close()
	})
	return err
}
