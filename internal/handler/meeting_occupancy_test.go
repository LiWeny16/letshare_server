package handler

import (
	"testing"
	"time"

	"letshare-server/internal/meeting"
	"letshare-server/internal/model"
)

// C-19 占用判定（onMeetingEmptyTimerFired）三分支的逐支覆盖：
//   ① 在册成员 > 0（登记层幽灵成员）——TestEmptyTimerFiresOnOccupiedMeetingRearmsInsteadOfSilentSkip；
//   ② SFU 参与者 > 0（登记表为空）——TestEmptyTimerFiresWithSFUOnlyOccupancyRearmsInsteadOfEnding；
//   ③ 进行中的 join（相位已 PhaseLive、成员尚未登记/SFU 尚未接入）——
//     TestEmptyTimerFiresDuringInFlightJoinRearmsInsteadOfEnding。
//
// 三分支都必须「重排下一次」而非拆除会议号（DIVERGENCE F10：到点跳过 = 宽限永久失效）。

// TestEmptyTimerFiresWithSFUOnlyOccupancyRearmsInsteadOfEnding（分支②）：
// 在册成员为 0 但 SFU 房间仍有参与者时，宽限到点必须重排，不得 End。
func TestEmptyTimerFiresWithSFUOnlyOccupancyRearmsInsteadOfEnding(t *testing.T) {
	hook := installTeardownLogHook(t)
	injectMeetingGrace(t, 500*time.Millisecond)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:sfu-occupancy")
	meta := loadMeta(t, ts, roomID)
	// 媒体已接入而登记表为空：占用只剩 SFU 参与者这一支。
	room := ts.handler.sfuManager.JoinRoom(roomID)
	if _, err := room.AddParticipant("ghost:sfu-occupancy"); err != nil {
		t.Fatalf("add sfu participant: %v", err)
	}
	reserved := metaTimer(t, ts, meta)
	if !ts.handler.enterMeetingGrace(roomID, meetingEndReasonGraceArmed, nil) {
		t.Fatal("grace must arm while the registry is empty")
	}
	fired := waitEmptyTimerRebuilt(t, ts, meta, reserved)
	rearmed := waitEmptyTimerRebuilt(t, ts, meta, fired) // 到点 SFU 非空 → 必须重排
	if rearmed == nil || !ts.handler.hasRegistered(roomID) {
		t.Fatal("sfu-only occupancy must re-arm the empty timer, not end the meeting")
	}
	assertGraceSkippedRecord(t, hook, roomID)
}

// TestEmptyTimerFiresDuringInFlightJoinRearmsInsteadOfEnding（分支③）：
// join 已完成相位跃迁（PhaseLive）而成员尚未登记、SFU 尚未接入时，到点必须重排，不得 End。
// 注：这是防御态（生产路径上 ToLive 与停表在同一临界区、到点回调另有代次作废检查），
// 用白盒构造该态以锁定「live 即占用」这条判定，防止后续有人删掉该分支。
func TestEmptyTimerFiresDuringInFlightJoinRearmsInsteadOfEnding(t *testing.T) {
	hook := installTeardownLogHook(t)
	injectMeetingGrace(t, 2*time.Second)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:live-occupancy")
	member := newWSRPC(t, ts.srv, "member:live-occupancy")
	joinMeetingMustSucceed(t, ts, member, roomID, "member:live-occupancy")
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 0) // 清空 → PhaseGrace，emptyTimer 已武装
	meta := loadMeta(t, ts, roomID)
	armed := waitEmptyTimerRebuilt(t, ts, meta, nil)
	if !meta.life.ToLive() {
		t.Fatal("in-flight join must win the ToLive transition")
	}
	rearmed := waitEmptyTimerRebuilt(t, ts, meta, armed)
	if rearmed == nil || !ts.handler.hasRegistered(roomID) {
		t.Fatal("in-flight join must re-arm the empty timer, not end the meeting")
	}
	if meta.life.Phase() != meeting.PhaseLive {
		t.Fatalf("phase = %v, want live", meta.life.Phase())
	}
	assertGraceSkippedRecord(t, hook, roomID)
}

// assertGraceSkippedRecord 断言重排留下 grace-expired-room-not-empty 留痕（F10：决策必须可检索）。
func assertGraceSkippedRecord(t *testing.T, hook *teardownLogHook, roomID string) {
	t.Helper()
	records := waitEndRecords(t, hook, roomID, meetingActionScheduleGrace)
	for _, record := range records {
		if record.data["reason"] == meetingEndReasonGraceSkipped {
			return
		}
	}
	t.Fatalf("re-arm must log grace-expired-room-not-empty, got %+v", records)
}
