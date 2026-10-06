package handler

import (
	logrus "github.com/sirupsen/logrus"
)

// 会议拆除 / 成员移除 / 房主转移的观测日志（V0.1 归因缺口归因缺口①③）。
//
// 事故复盘：某成员主动离开后，其他成员重连立刻拿到「会议不存在或已结束」，
// 团队只能靠客户端日志反推。会议生命周期的每条拆除/决策路径都必须在服务端
// 留下可检索日志：message 固定带 "meeting-end:reason=<enum>" /
// "meeting-host-changed:trigger=<enum>" 稳定前缀，fields 携带判定上下文
// （members_remaining / room_empty / grace_expired / is_host 等）。
//
// 本文件只定义枚举与落日志函数，不参与任何控制流；所有调用点均为纯观测。

// 会议拆除/成员移除的稳定 reason 枚举（server 日志检索键，只增不改）。
const (
	meetingEndReasonLastMemberLeft     = "last-member-left"             // 显式离开清空会议 → 立即拆除
	meetingEndReasonMemberLeft         = "member-left"                  // 有成员离开但会议继续（房主可能转移）
	meetingEndReasonGraceArmed         = "disconnect-grace-armed"       // 非显式离开清空会议 → 排定宽限期
	meetingEndReasonGraceSkipped       = "grace-expired-room-not-empty" // 宽限到点但房间非空 → 跳过（DIVERGENCE F10 现场）
	meetingEndReasonGraceExpired       = "host-left-grace-expired"      // 宽限到点且房间仍空 → 拆除
	meetingEndReasonHostEnded          = "host-ended"                   // 房主显式结束会议
	meetingEndReasonKicked             = "kicked"                       // 房主移出成员（成员拆除）
	meetingEndReasonReservationExpired = "reservation-expired"          // 从未 join 的会议到期回收
	meetingEndReasonParentTeardown     = "parent-meeting-teardown"      // 父会议拆除级联拆 breakout 子房间
	meetingEndReasonBreakoutRecalled   = "breakout-recalled"            // 房主召回 breakout，子房间拆除
	// 当前不可达的防御性兜底：clientReason 实际取值域只有 "timeout"/"host-ended"（见 meetingEndReasonFromClientReason）。
	meetingEndReasonServerTeardown = "server-teardown" // 其余服务端拆除（兜底，暂不可达，保留勿删）
)

// 拆除/决策动作枚举：区分「真拆除」与「决策了但不动作」。
const (
	meetingActionDecision      = "decision"       // 评估通过，即将拆除/通知
	meetingActionTeardown      = "teardown"       // 会议注册与 SFU 资源真被拆除
	meetingActionSkip          = "skip"           // 决策了但不动作（必须留痕，F10 现场）
	meetingActionScheduleGrace = "schedule-grace" // 排定宽限期后拆除
	meetingActionMemberRemoved = "member-removed" // 仅移除一名成员
)

// 房主变更触发原因枚举。
const (
	meetingHostTriggerHostLeft       = "host-left"       // 房主离开/断线后的自动转移
	meetingHostTriggerHostReassigned = "host-reassigned" // 房主显式指定新房主
)

// logMeetingEnd 落一条会议拆除/决策日志。message 固定为
// "meeting-end:reason=<enum>"，便于按原因检索；fields 携带判定上下文。
// 不修改传入的 fields（内部合并出新 map），调用点可安全复用同一份快照。
func logMeetingEnd(reason, action string, fields logrus.Fields) {
	merged := logrus.Fields{
		"event":  "meeting-end",
		"reason": reason,
		"action": action,
	}
	for key, value := range fields {
		merged[key] = value
	}
	logrus.WithFields(merged).Info("meeting-end:reason=" + reason)
}

// logMeetingTeardown 落一条「会议真的被拆除」的权威记录（回答「会议为什么没了」）。
func logMeetingTeardown(reason, roomID, parentRoomID string, notify, wasRegistered bool, membersRemaining int) {
	logMeetingEnd(reason, meetingActionTeardown, logrus.Fields{
		"roomID":            roomID,
		"parent_room_id":    parentRoomID,
		"notified":          notify,
		"was_registered":    wasRegistered,
		"members_remaining": membersRemaining,
		"room_empty":        membersRemaining == 0,
	})
}

// meetingHostChangeFields 纯函数：把房主变更日志的字段快照压成 logrus.Fields。
// 只收基本类型入参、不读共享状态；快照读取时机由调用点决定，保证与内联构造逐字段等价。
func meetingHostChangeFields(roomID, prevHost, prevHostClient, newHost, newHostClient string, membersRemaining int) logrus.Fields {
	return logrus.Fields{
		"roomID":            roomID,
		"prev_host":         prevHost,
		"prev_host_client":  prevHostClient,
		"new_host":          newHost,
		"new_host_client":   newHostClient,
		"members_remaining": membersRemaining,
	}
}

// logMeetingKick 落一条踢人（成员拆除）日志。纯读快照在此求值：
// is_host 必须实时求值而不是硬编码 false —— 调用点守卫（不能移出房主）与
// 本日志之间存在极窄 TOCTOU：并发 transferMeetingHost 可能把 target 提升为
// 房主，对诊断片来说「日志说谎」比「没有日志」更严重。meetingHostUniqID 是
// 短锁纯读，且本方法在调用点不持任何锁时进入，不会嵌进已持锁的临界区。
func (h *WebSocketHandler) logMeetingKick(roomID, target, by string) {
	membersRemaining := len(h.meetings.members(roomID))
	logMeetingEnd(meetingEndReasonKicked, meetingActionMemberRemoved, logrus.Fields{
		"roomID":            roomID,
		"target":            target,
		"by":                by,
		"is_host":           h.meetingHostUniqID(roomID) == target,
		"members_remaining": membersRemaining,
		"room_empty":        membersRemaining == 0,
		"explicit_leave":    false,
	})
}

// logMeetingHostChange 落一条房主变更日志（旧房主 / 新房主 / 触发原因）。
func logMeetingHostChange(trigger string, fields logrus.Fields) {
	merged := logrus.Fields{"event": "meeting-host-changed"}
	for key, value := range fields {
		merged[key] = value
	}
	merged["trigger"] = trigger
	logrus.WithFields(merged).Info("meeting-host-changed:trigger=" + trigger)
}

// meetingEndReasonFromClientReason 把 endMeetingRoom 的客户端协议 reason
// （"timeout" / "host-ended"，只广播给客户端、语义不可改）映射为服务端稳定枚举。
func meetingEndReasonFromClientReason(clientReason string) string {
	switch clientReason {
	case "timeout":
		return meetingEndReasonGraceExpired
	case "host-ended":
		return meetingEndReasonHostEnded
	default:
		return meetingEndReasonServerTeardown
	}
}

// meetingHostUniqID 观测专用：读取当前房主 uniqID，读不到返回空串。
func (h *WebSocketHandler) meetingHostUniqID(roomID string) string {
	value, loaded := h.activeMeetingRooms.Load(roomID)
	meta, ok := value.(*meetingMeta)
	if !loaded || !ok || meta == nil {
		return ""
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	return meta.Host
}
