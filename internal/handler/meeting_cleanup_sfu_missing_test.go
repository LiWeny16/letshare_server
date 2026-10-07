package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/meeting"
)

// G2 契约门回归 · T3（D2）：cleanupMeetingClientID 的「SFU 房间已不在」分支。
//
// 旧代码在 GetRoom !ok 时跳过 finishMeetingMemberLeave，没有任何兜底武装宽限 →
// 在册成员清零的生命周期判定被整体跳过，会议号永不回收（不可自愈的泄漏，D2 判定为修 bug）。
// 修复后：room==nil 也必须走完成员级离场（releaseMeetingPresentation / leave 广播 /
// 空房武装宽限），并以 sfu_room_missing=true 留痕。
//
// 观察窗分两个房间（同一 client 的两份在册，一次 cleanupMeetingClientID 全部处理）：
//   roomWatch（ghost + observer）→ 成员级离场的 leave 广播可被 observer 收到；
//   roomEmpty（ghost 独占）→ 空房宽限必须被武装（这一步此前被跳过）。

// waitMembershipLeave 等待 observer 收到指定成员的 leave 成员变更广播
// （broadcastMeetingMembership 只投递给在册成员，故需另一成员在场观察）。
func waitMembershipLeave(t *testing.T, observer *wsRPC, uniqID string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-observer.membership:
			var d struct {
				Type   string `json:"type"`
				UniqID string `json:"uniqId"`
			}
			if err := json.Unmarshal(m.Data, &d); err == nil && d.Type == "leave" && d.UniqID == uniqID {
				return
			}
		case <-deadline:
			t.Fatalf("成员 %s 的 leave 广播未送达在册成员", uniqID)
		}
	}
}

// assertSfuRoomMissingLogged 断言该会议的指定动作日志带 sfu_room_missing=true 留痕。
func assertSfuRoomMissingLogged(t *testing.T, hook *teardownLogHook, roomID, action string) {
	t.Helper()
	for _, record := range hook.endRecords(roomID, action) {
		if record.data["sfu_room_missing"] == true {
			return
		}
	}
	t.Fatalf("meeting %s must log action=%s with sfu_room_missing=true", roomID, action)
}

// TestCleanupMeetingClientIDMissingSFURoomStillProcessesLeaveAndArmsGrace 不变量：
// SFU 房间已不在（!ok）时成员级离场仍被处理（leave 广播 / releaseMeetingPresentation）、
// 空房宽限仍被武装（emptyTimer 非 nil + Phase=grace）、日志仍带 sfu_room_missing。
func TestCleanupMeetingClientIDMissingSFURoomStillProcessesLeaveAndArmsGrace(t *testing.T) {
	hook := installTeardownLogHook(t)
	injectMeetingGrace(t, 2*time.Second)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomWatch, _ := createMeetingOnly(t, ts, "host:sfu-missing")
	roomEmpty, _ := createMeetingOnly(t, ts, "host:sfu-missing")
	ghost := newWSRPC(t, ts.srv, "ghost:sfu-missing")
	observer := newWSRPC(t, ts.srv, "observer:sfu-missing")
	joinMeetingMustSucceed(t, ts, ghost, roomWatch, "ghost:sfu-missing")
	joinMeetingMustSucceed(t, ts, observer, roomWatch, "observer:sfu-missing")
	joinMeetingMustSucceed(t, ts, ghost, roomEmpty, "ghost:sfu-missing")
	meta := loadMeta(t, ts, roomEmpty)
	meta.mu.Lock()
	meta.presentation.ScreenOwnerID = "ghost:sfu-missing" // 预置演示租约，验证 releaseMeetingPresentation
	meta.mu.Unlock()
	clientID, ok := ts.handler.meetings.clientForUniqID(roomEmpty, "ghost:sfu-missing")
	if !ok {
		t.Fatal("ghost must be a registered meeting member")
	}
	// 显式移除两个 SFU 房间 → 制造 cleanupMeetingClientID 的 !ok 分支。
	ts.handler.sfuManager.RemoveRoomWithReason(roomWatch, "test-sfu-room-missing")
	ts.handler.sfuManager.RemoveRoomWithReason(roomEmpty, "test-sfu-room-missing")
	ts.handler.cleanupMeetingClientID(clientID)

	// ② 宽限被武装（这一步此前被跳过 → 会议号永久泄漏）：Phase=grace 且 emptyTimer 非 nil。
	if phase := meta.life.Phase(); phase != meeting.PhaseGrace {
		t.Fatalf("phase = %v, want grace（SFU 房间缺失也必须武装宽限）", phase)
	}
	if metaTimer(t, ts, meta) == nil {
		t.Fatal("emptyTimer must be armed when the SFU room is already gone")
	}
	// ③ 日志含 sfu_room_missing 留痕。
	assertSfuRoomMissingLogged(t, hook, roomEmpty, meetingActionScheduleGrace)
	// ① 成员级离场仍被处理：leave 广播 + 演示租约释放。
	waitMembershipLeave(t, observer, "ghost:sfu-missing")
	meta.mu.Lock()
	owner := meta.presentation.ScreenOwnerID
	meta.mu.Unlock()
	if owner != "" {
		t.Fatalf("presentation lease must be released on leave, owner=%q", owner)
	}
}
