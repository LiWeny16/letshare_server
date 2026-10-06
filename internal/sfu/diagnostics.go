package sfu

import (
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

// 主连接观测：状态跃迁与 ICE 选中候选对是断连归因的第一手证据，而生产日志
// 级别是 info —— 用 Debug 记录等于丢弃（PURPOSE §4.4「不在缺少诊断能力时替换核心」）。
// 本文件只读连接状态并落日志，不参与任何控制流。

// connStateTrace 只保留「上一状态 + 进入时刻」两个标量。
// 诊断输出必须有界：不随连接次数或时间累积，随 Participant 一起回收。
type connStateTrace struct {
	mu    sync.Mutex
	prev  webrtc.PeerConnectionState
	since time.Time
}

// advance 记录 state 的进入时刻，返回上一状态及其持续时长（首次跃迁时长为 0）。
func (t *connStateTrace) advance(state webrtc.PeerConnectionState, now time.Time) (webrtc.PeerConnectionState, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, since := t.prev, t.since
	t.prev, t.since = state, now
	if since.IsZero() {
		return prev, 0
	}
	return prev, now.Sub(since)
}

// selectedCandidatePairFields 读取 ICE 选中的候选对（走直连还是中继）。
// 取不到返回 false：观测缺失绝不影响主流程。
func selectedCandidatePairFields(pc *webrtc.PeerConnection) (log.Fields, bool) {
	if pc == nil {
		return nil, false
	}
	sctpTransport := pc.SCTP()
	if sctpTransport == nil {
		return nil, false
	}
	dtlsTransport := sctpTransport.Transport()
	if dtlsTransport == nil {
		return nil, false
	}
	iceTransport := dtlsTransport.ICETransport()
	if iceTransport == nil {
		return nil, false
	}
	pair, err := iceTransport.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return nil, false
	}
	return candidatePairFields(pair)
}

// candidatePairFields 把候选对压成日志字段。地址只带 IP 不带端口（脱敏）。
func candidatePairFields(pair *webrtc.ICECandidatePair) (log.Fields, bool) {
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		return nil, false
	}
	return log.Fields{
		"local_type":  pair.Local.Typ.String(),
		"remote_type": pair.Remote.Typ.String(),
		"relayed":     pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay,
		"local_addr":  pair.Local.Address,
		"remote_addr": pair.Remote.Address,
	}, true
}

// logConnectionStateChange 把主连接状态跃迁落为 Info。
// connected 额外落一条 ICE 选中候选对；disconnected/failed 补 reason 字段
// （上一状态、持续时长、ICE 状态），让一次断连可以只靠服务端日志还原因果链。
func (p *Participant) logConnectionStateChange(pc *webrtc.PeerConnection, state webrtc.PeerConnectionState) {
	prev, heldFor := p.connTrace.advance(state, time.Now())
	base := log.Fields{
		"sfu":                    "participant",
		"roomID":                 p.roomIDForLog(),
		"participantID":          p.ID,
		"state":                  state.String(),
		"prev_state":             prev.String(),
		"prev_state_duration_ms": heldFor.Milliseconds(),
		"ice_state":              iceConnectionStateForLog(pc),
	}
	entry := log.WithFields(base)
	if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateFailed {
		entry = entry.WithField("reason", peerConnectionStateReason(state))
	}
	entry.Info("参与者的主连接状态变化")

	if state != webrtc.PeerConnectionStateConnected {
		return
	}
	pairFields, ok := selectedCandidatePairFields(pc)
	if !ok {
		log.WithFields(base).Warn("未取到 ICE 选中候选对")
		return
	}
	log.WithFields(base).WithFields(pairFields).Info("参与者主连接 ICE 选中候选对")
}

// roomIDForLog 观测路径上取不到 roomID 时返回空串，不得 panic。
func (p *Participant) roomIDForLog() string {
	if p.room == nil {
		return ""
	}
	return p.room.ID
}

func iceConnectionStateForLog(pc *webrtc.PeerConnection) string {
	if pc == nil {
		return "unknown"
	}
	return pc.ICEConnectionState().String()
}

// peerConnectionStateReason 是断连日志的可检索原因键；具体细节由
// prev_state / prev_state_duration_ms / ice_state 字段承载。
func peerConnectionStateReason(state webrtc.PeerConnectionState) string {
	switch state {
	case webrtc.PeerConnectionStateDisconnected:
		return "pc-disconnected"
	case webrtc.PeerConnectionStateFailed:
		return "pc-failed"
	default:
		return "pc-state-change"
	}
}
