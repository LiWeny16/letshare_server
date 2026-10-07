package handler

// 会议号相位生命周期（V1）：emptyTimer 的唯一武装/停表入口、enterMeetingLive /
// enterMeetingGrace 相位跃迁与到点策略。
//
// 本块自 meeting_registry.go 下沉（C.2 文件行数硬门：meeting_registry.go 恰 400 行
// 零余量，A1 房主权威修复必须先腾出空间，故把可整体分离的相位生命周期段落搬到这里）。
// emptyTimer 字段由 meetingLifecycleMu 保护；相位由 internal/meeting 唯一写（A1/A2）。

import (
	"time"

	"letshare-server/internal/meeting"

	logrus "github.com/sirupsen/logrus"
)

// armEmptyTimerLocked 重新武装空置宽限定时器：总是新建，不复用旧计时器（D2）。
// 调用方必须持有 meetingLifecycleMu。
func (h *WebSocketHandler) armEmptyTimerLocked(roomID string, meta *meetingMeta, ttl time.Duration) {
	h.stopEmptyTimerLocked(meta)
	gen := meta.emptyGen
	meta.emptyTimer = time.AfterFunc(ttl, func() {
		h.onMeetingEmptyTimerFired(roomID, meta, gen)
	})
}

// stopEmptyTimerLocked 停掉空置宽限定时器（成员加入 = PhaseLive 停表）。
// 代次递增同时作废已在飞的陈旧回调。调用方必须持有 meetingLifecycleMu。
func (h *WebSocketHandler) stopEmptyTimerLocked(meta *meetingMeta) {
	meta.emptyGen++
	if meta.emptyTimer != nil {
		meta.emptyTimer.Stop()
		meta.emptyTimer = nil
	}
}

// enterMeetingLive 成员加入的唯一入口（成员登记 + 相位跃迁，A1 同一临界区）：在
// meetingLifecycleMu 临界区内复核登记与相位、登记会议成员、置 PhaseLive、停空表
// （C-17：陈旧的宽限回调不得拆掉正在加入的会议），随后复位房主席位（A1：有人在会就
// 必须有在册房主），退出锁后落一条相位日志（C-21）；同时把重连的房主 websocket 会话
// 绑回当前连接（host-only 通道不得继续打到重连前的旧连接）。
// 成员登记先于房主席位修复：否则空会复活时的唯一成员被判为「不在册」，而转移目标又
// 不在册，席位无处可去；登记与跃迁同锁也堵住「join 中途被并发宽限拆除」的窗口。
// 返回 previous = 被新连接顶替的旧会话登记（换连接重入用于摘旧参与者 / 广播 leave）；
// ok=false 表示会议号已释放 / 已 End / 相位竞争失败，调用方必须按「会议不存在」拒绝。
func (h *WebSocketHandler) enterMeetingLive(roomID, uniqID, userName, clientID, sourceRoom string, meta *meetingMeta) ([]meetingMembership, bool) {
	if meta == nil {
		return nil, false
	}
	h.meetingLifecycleMu.Lock()
	phaseFrom := meta.life.Phase()
	current, registered := h.activeMeetingRooms.Load(roomID)
	if !registered || current != meta || !meta.life.ToLive() {
		h.meetingLifecycleMu.Unlock()
		return nil, false
	}
	_, previous := h.meetings.join(roomID, uniqID, userName, clientID)
	h.stopEmptyTimerLocked(meta)
	meta.mu.Lock()
	// A browser reconnect creates a new websocket client session. Keep the
	// host recipient bound to the current meeting connection; otherwise
	// host-only planes (minutes batches, summaries and control messages)
	// continue targeting the stale pre-reconnect client.
	meta.hostMu.Lock()
	if meta.Host == uniqID {
		meta.HostClientID = clientID
	}
	hostIsClient := meta.Host == uniqID
	meta.hostMu.Unlock()
	if meta.SourceRoom == "" && hostIsClient && sourceRoom != "" && h.wsService.RoomHasUniqID(sourceRoom, uniqID) {
		meta.SourceRoom = sourceRoom
	}
	meta.mu.Unlock()
	h.restoreMeetingHostSeat(roomID, meta, phaseFrom)
	h.meetingLifecycleMu.Unlock()
	logMeetingPhase(phaseFrom.String(), meeting.PhaseLive.String(), meetingEndReasonMemberJoined, logrus.Fields{"roomID": roomID, "uniqID": uniqID, "clientID": clientID})
	return previous, true
}

// restoreMeetingHostSeat A1 房主权威修复（空会复活不得留下「有人无主」）：会议转成
// PhaseLive 后，若房主席位指向的 uniqID 不在在册成员中，就把席位转给在册成员 ——
// 完全复用既有 transferMeetingHost（挑选口径：最早加入的在册成员，即 members[0]；
// trigger=host-left），不另造第二套挑选/转移规则（X2）。
// 两处不转：
//   - PhaseReserved 首入：登记房主只是「尚未到场」而非「已离场」（会议邀请允许受邀者
//     先于发起者入会；此刻夺席会让发起人永久失去自己的会议）；
//   - breakout 子房间（Parent != ""）：其 Host 按设计恒为主会议房主，且房主本就不在
//     子房间在册名单内（BreakoutMembers 不含房主），子房间由父会议级联/召回收敛。
//
// 调用方必须持有 meetingLifecycleMu（与相位跃迁同一临界区）；本函数不取 meta.mu，
// 避免在 meta.mu 临界区内做网络发送；hostMu 仍是叶锁，锁序 meta.mu → hostMu 不反向。
func (h *WebSocketHandler) restoreMeetingHostSeat(roomID string, meta *meetingMeta, phaseFrom meeting.Phase) {
	if phaseFrom == meeting.PhaseReserved || meta.Parent != "" {
		return
	}
	host, _ := meetingHostSeats(meta)
	if _, inRoster := h.meetings.clientForUniqID(roomID, host); inRoster {
		return
	}
	h.transferMeetingHost(roomID, host)
}

// enterMeetingGrace 统一的「在册成员清零 → PhaseGrace + 重装 emptyTimer」入口（D2）。
// 每次进入宽限都重新计时；仍有成员或已 End 则不进入且不落日志，返回 false。
func (h *WebSocketHandler) enterMeetingGrace(roomID, reason string, ctx logrus.Fields) bool {
	h.meetingLifecycleMu.Lock()
	defer h.meetingLifecycleMu.Unlock()
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		return false
	}
	meta, _ := v.(*meetingMeta)
	if meta == nil || len(h.meetings.members(roomID)) > 0 {
		return false
	}
	phaseFrom := meta.life.Phase()
	if !meta.life.ToGrace() {
		return false
	}
	grace := currentMeetingGrace()
	h.armEmptyTimerLocked(roomID, meta, grace)
	fields := logrus.Fields{
		"phase_from": phaseFrom.String(),
		"phase_to":   meeting.PhaseGrace.String(),
		"grace_ms":   grace.Milliseconds(),
	}
	for key, value := range ctx {
		fields[key] = value
	}
	logMeetingEnd(reason, meetingActionScheduleGrace, fields)
	return true
}

// onMeetingEmptyTimerFired 空置宽限定时器到点的唯一策略（D2 / C-19 / DIVERGENCE F10）：
//   - 房间非空（在册成员 / SFU 参与者 / 进行中的 join）→ 重新武装下一次，绝不静默跳过；
//   - 房间仍空 → End：PhaseReserved 按 reservation-expired，否则按 host-left-grace-expired。
func (h *WebSocketHandler) onMeetingEmptyTimerFired(roomID string, meta *meetingMeta, gen uint64) {
	h.meetingLifecycleMu.Lock()
	defer h.meetingLifecycleMu.Unlock()
	current, ok := h.activeMeetingRooms.Load(roomID)
	if !ok || current != meta {
		return // 会议号已释放（End 已执行）：陈旧回调不得再动作
	}
	if gen != meta.emptyGen {
		return // 陈旧回调：本次计时已被新计时器替换/停掉，不得拆除它保护的会议
	}
	phase := meta.life.Phase()
	if phase == meeting.PhaseEnded {
		return
	}
	members := len(h.meetings.members(roomID))
	roomCount := 0
	if room, ok := h.sfuManager.GetRoom(roomID); ok {
		roomCount = room.Count()
	}
	ttl := currentMeetingGrace()
	if phase == meeting.PhaseReserved {
		ttl = currentMeetingReservationTTL()
	}
	if members > 0 || roomCount > 0 || phase == meeting.PhaseLive {
		// F10 现场修复：到点但房间非空 → 重排下一次，而非跳过后让宽限永久失效。
		logMeetingEnd(meetingEndReasonGraceSkipped, meetingActionScheduleGrace, logrus.Fields{
			"roomID":            roomID,
			"grace_expired":     true,
			"room_empty":        false,
			"members_remaining": members,
			"room_count":        roomCount,
			"grace_ms":          ttl.Milliseconds(),
			"phase":             phase.String(),
		})
		h.armEmptyTimerLocked(roomID, meta, ttl)
		return
	}
	reason := meetingEndReasonGraceExpired
	if phase == meeting.PhaseReserved {
		reason = meetingEndReasonReservationExpired
	}
	h.endMeetingLocked(roomID, reason, logrus.Fields{
		"grace_expired": true,
		"room_empty":    true,
		"phase":         phase.String(),
	})
}
