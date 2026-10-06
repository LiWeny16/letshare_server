package sfu

import (
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

// 订阅侧观测（V0.1 归因缺口②）：每条订阅 PC 对应「一个订阅者 × 一个发布者」，
// 是回答「为什么没声音」的第一手证据，而生产日志级别是 info。
// 与主连接观测（diagnostics.go）同构：状态跃迁落 Info + 断连 reason +
// ICE 选中候选对（直连/中继）；只读连接状态落日志，不参与任何控制流。
// 新逻辑放本文件：participant.go 已超行数阈值，那里只保留一行接线。

// logConnectionStateChange 把订阅连接状态跃迁落为 Info。日志必须带
// publisherID：一个订阅者有多条订阅 PC（每个发布者一条），否则无法区分。
func (s *Subscriber) logConnectionStateChange(pc *webrtc.PeerConnection, state webrtc.PeerConnectionState) {
	prev, heldFor := s.connTrace.advance(state, time.Now())
	base := log.Fields{
		"sfu":                    "subscriber",
		"roomID":                 s.roomIDForLog(),
		"participantID":          s.participantIDForLog(),
		"publisherID":            s.publisherID,
		"state":                  state.String(),
		"prev_state":             prev.String(),
		"prev_state_duration_ms": heldFor.Milliseconds(),
		"ice_state":              iceConnectionStateForLog(pc),
	}
	entry := log.WithFields(base)
	if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateFailed {
		entry = entry.WithField("reason", peerConnectionStateReason(state))
	}
	entry.Info("参与者的订阅连接状态变化")

	if state != webrtc.PeerConnectionStateConnected {
		return
	}
	pairFields, ok := selectedCandidatePairFields(pc)
	if !ok {
		log.WithFields(base).Warn("未取到订阅连接 ICE 选中候选对")
		return
	}
	log.WithFields(base).WithFields(pairFields).Info("订阅连接 ICE 选中候选对")
}

// roomIDForLog 观测路径上取不到 roomID 时返回空串，不得 panic。
func (s *Subscriber) roomIDForLog() string {
	if s.forParticipant == nil {
		return ""
	}
	return s.forParticipant.roomIDForLog()
}

// participantIDForLog 订阅连接所属订阅者（收流方）的 ID。
func (s *Subscriber) participantIDForLog() string {
	if s.forParticipant == nil {
		return ""
	}
	return s.forParticipant.ID
}
