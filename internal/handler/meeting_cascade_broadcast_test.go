package handler

import (
	"encoding/json"
	"testing"
	"time"
)

// G2 契约门回归 · T2（D3）：级联拆除的广播面。
//
// 既有 TestTeardownLogCascadeEmitsOneRecordPerMeeting 只断言了日志面（每会议恰好一条
// action=teardown）；它登记的子房间没有在册成员，收件人集合为空 —— 「每子会议在册成员
// 恰好收到一条 meeting:ended」从未被断言。本测试补上广播面，并保留日志面断言。
//
// 协议口径：meetingClientReasonForEnd(parent-meeting-teardown) → "ended"
//（breakout-recalled 返回空串不广播；其余路径映射 "host-ended"/"timeout"/"ended"）。
// 真实 breakout 流程是成员 leave 主会 + join 子会，故子会议成员只在子房间在册。

// registerCascadeChildFor 直接登记 breakout 子房间（与既有接缝 registerBreakoutChild
// 同构），仅允许指定成员加入；与 meeting:breakout create 走同一构造入口 newMeetingMeta，
// life / minutes 等初始化不再由接缝自行补齐。
func registerCascadeChildFor(t *testing.T, ts *meetingTestServer, parentID, childID string, allowed ...string) {
	t.Helper()
	allowedSet := make(map[string]bool, len(allowed))
	for _, uniqID := range allowed {
		allowedSet[uniqID] = true
	}
	meta := newMeetingMeta(meetingMetaInit{
		Host:            "host:cascade",
		HostClientID:    "client-host-cascade",
		Parent:          parentID,
		BreakoutMembers: allowedSet,
	})
	if _, exists := ts.handler.activeMeetingRooms.LoadOrStore(childID, meta); exists {
		t.Fatalf("child room %s already registered", childID)
	}
}

// TestCascadeTeardownBroadcastsExactlyOneMeetingEndedPerChildMember 不变量：
// 级联拆除时每个子会议在册成员恰好一条 meeting:ended（reason=ended）、无第二条；
// 主/子会议各恰好一条 action=teardown（日志面保留）。
func TestCascadeTeardownBroadcastsExactlyOneMeetingEndedPerChildMember(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	mainID, _ := createMeetingOnly(t, ts, "host:cascade")
	childID := mainID + "C1"
	registerCascadeChildFor(t, ts, mainID, childID, "member:cascade")
	member := newWSRPC(t, ts.srv, "member:cascade")
	joinMeetingMustSucceed(t, ts, member, childID, "member:cascade")

	ts.handler.End(mainID, meetingEndReasonServerTeardown)

	// 广播面：子房间在册成员恰好一条 meeting:ended，协议 reason=ended。
	select {
	case ended := <-member.ended:
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ended.Data, &payload); err != nil || payload.Reason != "ended" {
			t.Fatalf("meeting:ended payload = %s err=%v, want reason=ended", ended.Data, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("级联拆除必须给子房间在册成员发 meeting:ended")
	}
	assertNoMeetingEnded(t, member, 300*time.Millisecond, "级联拆除每成员只能有一条 meeting:ended")
	// 日志面（保留既有断言）：主/子会议各恰好一条 action=teardown 且 reason 正确。
	assertSingleTeardown(t, hook, mainID, meetingEndReasonServerTeardown)
	assertSingleTeardown(t, hook, childID, meetingEndReasonParentTeardown)
	if notified := hook.endRecords(childID, meetingActionTeardown)[0].data["notified"]; notified != true {
		t.Fatalf("级联拆除子房间必须 notified=true（会广播 meeting:ended），got %v", notified)
	}
}
