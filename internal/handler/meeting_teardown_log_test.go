package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

// V1 验收第 6 条：每条拆除路径必产 meeting-end:reason=<enum> 日志，
// 且恰好一条 action=teardown 权威记录（覆盖 V0.1 的会议层拆除接线）。

// waitMeetingReleased 等待会议号被释放（End 完成）。
func waitMeetingReleased(t *testing.T, ts *meetingTestServer, roomID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !ts.handler.hasRegistered(roomID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("meeting %s was not released", roomID)
}

// assertSingleTeardown 断言该会议号恰好一条 action=teardown 记录且 reason 正确。
func assertSingleTeardown(t *testing.T, hook *teardownLogHook, roomID, reason string) {
	t.Helper()
	records := waitEndRecords(t, hook, roomID, meetingActionTeardown)
	if len(records) != 1 {
		t.Fatalf("meeting %s must have exactly one action=teardown record, got %d: %+v", roomID, len(records), records)
	}
	if records[0].data["reason"] != reason {
		t.Fatalf("teardown reason = %v, want %v", records[0].data["reason"], reason)
	}
	if records[0].message != "meeting-end:reason="+reason {
		t.Fatalf("teardown message = %q, want meeting-end:reason=%s", records[0].message, reason)
	}
}

// assertNoMeetingEnded 断言窗口期内该客户端不会收到任何 meeting:ended（防重复广播 / 防泄露）。
func assertNoMeetingEnded(t *testing.T, client *wsRPC, window time.Duration, context string) {
	t.Helper()
	select {
	case msg := <-client.ended:
		t.Fatalf("%s: unexpected meeting:ended: %#v", context, msg)
	case <-time.After(window):
	}
}

// registerBreakoutChild 直接登记一个 breakout 子房间（与 meeting:breakout create 走
// 同一构造入口 newMeetingMeta —— 接缝不得自行补齐/漏掉生产初始化）。
func registerBreakoutChild(t *testing.T, ts *meetingTestServer, parentID, childID string) {
	t.Helper()
	meta := newMeetingMeta(meetingMetaInit{
		Host:            "host:teardown",
		HostClientID:    "client-host-teardown",
		Parent:          parentID,
		BreakoutMembers: map[string]bool{"member:teardown": true},
	})
	if _, exists := ts.handler.activeMeetingRooms.LoadOrStore(childID, meta); exists {
		t.Fatalf("child room %s already registered", childID)
	}
}

// TestTeardownLogHostEndedEmitsExactlyOneRecordAndOneBroadcast 拆除路径①：
// 房主显式结束（meeting:end）→ 恰好一条 action=teardown 记录 + 恰好一条 meeting:ended。
func TestTeardownLogHostEndedEmitsExactlyOneRecordAndOneBroadcast(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, host := createMeetingOnly(t, ts, "host:teardown")
	member := newWSRPC(t, ts.srv, "member:teardown")
	joinMeetingMustSucceed(t, ts, member, roomID, "member:teardown")

	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	select {
	case ended := <-member.ended:
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ended.Data, &payload); err != nil || payload.Reason != "host-ended" {
			t.Fatalf("meeting:ended payload = %s err=%v, want reason=host-ended", ended.Data, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("member did not receive meeting:ended")
	}
	assertNoMeetingEnded(t, member, 300*time.Millisecond, "one End must broadcast exactly one meeting:ended")
	waitMeetingReleased(t, ts, roomID)
	assertSingleTeardown(t, hook, roomID, meetingEndReasonHostEnded)
}

// TestTeardownLogGraceExpiredEmitsExactlyOneRecord 拆除路径②：
// 空置宽限到期 → action=teardown，reason=host-left-grace-expired。
func TestTeardownLogGraceExpiredEmitsExactlyOneRecord(t *testing.T) {
	hook := installTeardownLogHook(t)
	injectMeetingGrace(t, 150*time.Millisecond)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:grace-end")
	member := newWSRPC(t, ts.srv, "member:grace-end")
	joinMeetingMustSucceed(t, ts, member, roomID, "member:grace-end")
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingReleased(t, ts, roomID)
	assertSingleTeardown(t, hook, roomID, meetingEndReasonGraceExpired)
}

// TestTeardownLogReservationExpiredEmitsExactlyOneRecord 拆除路径③：
// 创建后从未 join 的会议到期 → reason=reservation-expired。
func TestTeardownLogReservationExpiredEmitsExactlyOneRecord(t *testing.T) {
	hook := installTeardownLogHook(t)
	injectMeetingReservationTTL(t, 150*time.Millisecond)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:reservation-end")
	waitMeetingReleased(t, ts, roomID)
	assertSingleTeardown(t, hook, roomID, meetingEndReasonReservationExpired)
}

// TestMemberRemovalLogKickEmitsExactlyOneMemberRemovedRecord 成员拆除接线：
// 房主踢人必产一条 action=member-removed / reason=kicked 记录（V0.1 无痕点①同类）。
func TestMemberRemovalLogKickEmitsExactlyOneMemberRemovedRecord(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, host := createMeetingOnly(t, ts, "host:kick")
	guest := newWSRPC(t, ts.srv, "guest:kick")
	joinMeetingMustSucceed(t, ts, host, roomID, "host:kick")
	joinMeetingMustSucceed(t, ts, guest, roomID, "guest:kick")

	data, _ := json.Marshal(map[string]string{"to": "guest:kick"})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingKick, Channel: roomID, Data: data}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 1)
	records := waitEndRecords(t, hook, roomID, meetingActionMemberRemoved)
	if len(records) != 1 || records[0].data["reason"] != meetingEndReasonKicked {
		t.Fatalf("kick must log exactly one member-removed record, got %+v", records)
	}
}

// TestTeardownLogCascadeEmitsOneRecordPerMeeting 拆除路径④⑤：
// 级联拆除（parent-meeting-teardown）与 breakout 召回（breakout-recalled）
// 各恰好一条 action=teardown；召回不得给成员发 meeting:ended（协议：走 meeting:breakout recall）。
func TestTeardownLogCascadeEmitsOneRecordPerMeeting(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:teardown")
	member := newWSRPC(t, ts.srv, "member:teardown")
	joinMeetingMustSucceed(t, ts, member, roomID, "member:teardown")
	registerBreakoutChild(t, ts, roomID, roomID+"B1")
	registerBreakoutChild(t, ts, roomID, roomID+"B2")

	// 成员进入子房间（真实 breakout 流程），房主召回子房间。
	joinMeetingMustSucceed(t, ts, member, roomID+"B2", "member:teardown")
	ts.handler.End(roomID+"B2", meetingEndReasonBreakoutRecalled)
	assertNoMeetingEnded(t, member, 300*time.Millisecond, "breakout recall must not broadcast meeting:ended")
	assertSingleTeardown(t, hook, roomID+"B2", meetingEndReasonBreakoutRecalled)
	if notified := hook.endRecords(roomID+"B2", meetingActionTeardown)[0].data["notified"]; notified != false {
		t.Fatalf("breakout recall must not notify members with meeting:ended, notified=%v", notified)
	}

	ts.handler.End(roomID, meetingEndReasonServerTeardown) // 主会议拆除 → 级联 B1
	waitMeetingReleased(t, ts, roomID)
	waitMeetingReleased(t, ts, roomID+"B1")
	assertSingleTeardown(t, hook, roomID, meetingEndReasonServerTeardown)
	assertSingleTeardown(t, hook, roomID+"B1", meetingEndReasonParentTeardown)
}
