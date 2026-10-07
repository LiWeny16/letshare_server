package handler

// A1 房主权威回归（V1a 阻塞项）：空会重入后会议不得停留在「有人无主」。
//
// 事故原形（TestMeetingNumberSurvivesLastExplicitLeaveAndRejoinSucceeds 的同路径）：
// 会议空置进 PhaseGrace → 非房主成员在宽限期内重入 → 会议转 PhaseLive，但 meta.Host
// 仍指向已离场的原房主 ⇒ 重入者不是房主 ⇒ meeting:end / kick / breakout 无人可触发。
//
// 本文件锁四件事：
//  ① 重入者接管房主席位（最早加入的在册成员），并落 meeting-host-changed:trigger=…；
//  ② 重入者随后能真正结束会议（端到端走生产信令 meeting:end）；
//  ③ 原房主重入：席位无人在场持有 → 夺回；席位已被在场者持有 → 不夺回；
//  ④ PhaseReserved：登记房主断线（→ Grace）后他人入会 → 接管席位；发起人只是
//     「尚未到场」（未断线）时他人先入会 → 不夺席。

import (
	"testing"
	"time"

	"letshare-server/internal/meeting"
	"letshare-server/internal/model"
)

// hostChangeRecords 取该会议号的 meeting-host-changed 观测记录（与 endRecords 同一登记处）。
func hostChangeRecords(hook *teardownLogHook, roomID string) []hookRecord {
	hook.mu.Lock()
	defer hook.mu.Unlock()
	out := []hookRecord{}
	for _, record := range hook.records {
		if record.data["event"] != "meeting-host-changed" || record.data["roomID"] != roomID {
			continue
		}
		out = append(out, record)
	}
	return out
}

// waitHostSeat 等待房主席位收敛到 want（转移与 join 并发时的收敛窗口）。
func waitHostSeat(t *testing.T, ts *meetingTestServer, roomID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ts.handler.meetingHostUniqID(roomID) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("房主席位 = %q，应为 %q（有人在会就必须有在册房主）", ts.handler.meetingHostUniqID(roomID), want)
}

// waitMeetingPhase 等待会议相位收敛到 want。
func waitMeetingPhase(t *testing.T, ts *meetingTestServer, roomID string, want meeting.Phase) {
	t.Helper()
	meta := loadMeta(t, ts, roomID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if meta.life.Phase() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("相位 = %s，应为 %s", meta.life.Phase(), want)
}

// requireMeetingEndAccepted 端到端断言：该客户端 meeting:end 被接受、会议真被拆除。
func requireMeetingEndAccepted(t *testing.T, ts *meetingTestServer, client *wsRPC, roomID string) {
	t.Helper()
	if err := client.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: roomID}); err != nil {
		t.Fatalf("send meeting:end: %v", err)
	}
	select {
	case <-client.ended:
	case rejected := <-client.err:
		msg := ""
		if rejected.Error != nil {
			msg = rejected.Error.Message
		}
		t.Fatalf("meeting:end 必须被接受（房主权威失守 = 阻塞项）: %s", msg)
	case <-time.After(5 * time.Second):
		t.Fatal("meeting:end 无响应")
	}
	waitMeetingReleased(t, ts, roomID)
}

// requireMeetingEndRejected 端到端断言：非房主 meeting:end 被 403 拒绝。
func requireMeetingEndRejected(t *testing.T, client *wsRPC, roomID string) {
	t.Helper()
	if err := client.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: roomID}); err != nil {
		t.Fatalf("send meeting:end: %v", err)
	}
	if msg, err := client.waitError(5 * time.Second); err != nil || msg == "" {
		t.Fatalf("非房主 meeting:end 必须被拒绝: msg=%q err=%v", msg, err)
	}
}

// rejoinAfterEmptyMeeting 事故原形路径：发起人只登记不入会 → 成员入会 → 显式离开清空
// 会议（PhaseGrace、会议号存活）→ 另一成员在宽限期内重入。返回会议号与重入者连接。
func rejoinAfterEmptyMeeting(t *testing.T, ts *meetingTestServer, tag string) (string, *wsRPC) {
	t.Helper()
	roomID, _ := createMeetingOnly(t, ts, "host:"+tag)
	member := newWSRPC(t, ts.srv, "wd:"+tag)
	joinMeetingMustSucceed(t, ts, member, roomID, "wd:"+tag)
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 0)
	if _, ok := ts.handler.activeMeetingRooms.Load(roomID); !ok {
		t.Fatal("空会必须存活到宽限期结束（C-17/C-20）")
	}
	rejoiner := newWSRPC(t, ts.srv, "rog13:"+tag)
	joinMeetingMustSucceed(t, ts, rejoiner, roomID, "rog13:"+tag)
	return roomID, rejoiner
}

// TestRejoinAfterEmptyMeetingTakesHostSeatAndLogsHostLeftTransfer（A2.2）：
// 空会复活后房主席位必须转给「最早加入的在册成员」，并落 meeting-host-changed:trigger=host-left
// （复用既有 transferMeetingHost 挑选口径与 trigger 词汇，不另造第二套规则）。
func TestRejoinAfterEmptyMeetingTakesHostSeatAndLogsHostLeftTransfer(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := rejoinAfterEmptyMeeting(t, ts, "seat")
	waitHostSeat(t, ts, roomID, "rog13:seat")

	records := hostChangeRecords(hook, roomID)
	if len(records) != 1 {
		t.Fatalf("空会复活应恰好一次房主转移，got %d: %+v", len(records), records)
	}
	record := records[0]
	if record.data["trigger"] != meetingHostTriggerHostLeft {
		t.Fatalf("trigger = %v，应为 %s（复用既有 host-left 词汇）", record.data["trigger"], meetingHostTriggerHostLeft)
	}
	if record.message != "meeting-host-changed:trigger="+meetingHostTriggerHostLeft {
		t.Fatalf("日志 message = %q，应为 meeting-host-changed:trigger=%s", record.message, meetingHostTriggerHostLeft)
	}
	if record.data["new_host"] != "rog13:seat" {
		t.Fatalf("new_host = %v，应为最早加入的在册成员 rog13:seat", record.data["new_host"])
	}
	if record.data["prev_host"] != "host:seat" {
		t.Fatalf("prev_host = %v，应为已离场的登记房主 host:seat", record.data["prev_host"])
	}
	assertNoServerPanics(t, ts)
}

// TestRejoinAfterEmptyMeetingCanEndMeeting（A2.1 端到端）：重入者接管席位后必须能
// 真正结束会议 —— 修复前这里就是「有人无主」：meeting:end 被 403 拒绝，无人可回收。
func TestRejoinAfterEmptyMeetingCanEndMeeting(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, rejoiner := rejoinAfterEmptyMeeting(t, ts, "end")
	requireMeetingEndAccepted(t, ts, rejoiner, roomID)
	assertSingleTeardown(t, hook, roomID, meetingEndReasonHostEnded)
	assertNoServerPanics(t, ts)
}

// TestOriginalHostRegainsHostSeatWhenRejoiningDuringGrace（A2.3 判定：夺回）：
// 席位已随原房主离开转给他人、随后全员离场进宽限，原房主重入即拿回席位 ——
// 「会议里待得最久的人」正是他（最早加入的在册成员）。
func TestOriginalHostRegainsHostSeatWhenRejoiningDuringGrace(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, host := createMeetingOnly(t, ts, "host:reclaim")
	joinMeetingMustSucceed(t, ts, host, roomID, "host:reclaim")
	guest := newWSRPC(t, ts.srv, "guest:reclaim")
	joinMeetingMustSucceed(t, ts, guest, roomID, "guest:reclaim")

	// 原房主先离开 → 既有 transferMeetingHost 把席位转给在册的 guest（host-left）。
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitHostSeat(t, ts, roomID, "guest:reclaim")
	// 最后一人离开 → 空会 → PhaseGrace：席位指向已离场的 guest。
	if err := guest.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 0)
	waitMeetingPhase(t, ts, roomID, meeting.PhaseGrace)

	rejoined := newWSRPC(t, ts.srv, "host:reclaim")
	joinMeetingMustSucceed(t, ts, rejoined, roomID, "host:reclaim")
	waitHostSeat(t, ts, roomID, "host:reclaim") // 夺回

	records := hostChangeRecords(hook, roomID)
	if len(records) != 2 {
		t.Fatalf("应有两次房主转移（host→guest、guest→host），got %d: %+v", len(records), records)
	}
	last := records[len(records)-1]
	if last.data["trigger"] != meetingHostTriggerHostLeft || last.data["new_host"] != "host:reclaim" {
		t.Fatalf("夺回日志 = %+v，应为 trigger=%s new_host=host:reclaim", last.data, meetingHostTriggerHostLeft)
	}
	requireMeetingEndAccepted(t, ts, rejoined, roomID)
	assertNoServerPanics(t, ts)
}

// TestHostRejoinDuringGraceKeepsSeatWithoutHostChange（A2.3 边界）：席位从未被转走时，
// 原房主重入是「保留」而非「夺回」—— 不得产生多余的 meeting-host-changed 噪声。
func TestHostRejoinDuringGraceKeepsSeatWithoutHostChange(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, host := createMeetingOnly(t, ts, "host:keep")
	joinMeetingMustSucceed(t, ts, host, roomID, "host:keep")
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 0)
	waitMeetingPhase(t, ts, roomID, meeting.PhaseGrace)

	rejoined := newWSRPC(t, ts.srv, "host:keep")
	joinMeetingMustSucceed(t, ts, rejoined, roomID, "host:keep")
	if got := ts.handler.meetingHostUniqID(roomID); got != "host:keep" {
		t.Fatalf("房主席位 = %q，原房主重入应保留席位 host:keep", got)
	}
	if records := hostChangeRecords(hook, roomID); len(records) != 0 {
		t.Fatalf("席位未被转走的重入不得落房主变更日志: %+v", records)
	}
	requireMeetingEndAccepted(t, ts, rejoined, roomID)
	assertNoServerPanics(t, ts)
}

// TestReturningHostDoesNotStealHostSeatFromPresentHost（A2.3 边界）：席位已被在场成员
// 持有时，原房主重入不得夺回 —— 否则既有「显式转移后旧房主失效」语义会被推翻。
func TestReturningHostDoesNotStealHostSeatFromPresentHost(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, host := createMeetingOnly(t, ts, "host:nosteal")
	joinMeetingMustSucceed(t, ts, host, roomID, "host:nosteal")
	guest := newWSRPC(t, ts.srv, "guest:nosteal")
	joinMeetingMustSucceed(t, ts, guest, roomID, "guest:nosteal")
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitHostSeat(t, ts, roomID, "guest:nosteal")

	rejoined := newWSRPC(t, ts.srv, "host:nosteal")
	joinMeetingMustSucceed(t, ts, rejoined, roomID, "host:nosteal")
	if got := ts.handler.meetingHostUniqID(roomID); got != "guest:nosteal" {
		t.Fatalf("房主席位 = %q，在场房主的席位不得被离场者夺回", got)
	}
	requireMeetingEndRejected(t, rejoined, roomID)
	requireMeetingEndAccepted(t, ts, guest, roomID)
	assertNoServerPanics(t, ts)
}

// TestReservedMeetingHostDisconnectThenGuestJoinTakesHostSeat（A2.4 判定）：
// 只有房主登记、无人 join 时房主断线（→ PhaseGrace），他人入会接管席位 ——
// 断线即「已离场」，会议必须有可问责的房主。
func TestReservedMeetingHostDisconnectThenGuestJoinTakesHostSeat(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, host := createMeetingOnly(t, ts, "host:rsvdrop")
	if err := host.conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitMeetingPhase(t, ts, roomID, meeting.PhaseGrace)

	guest := newWSRPC(t, ts.srv, "guest:rsvdrop")
	joinMeetingMustSucceed(t, ts, guest, roomID, "guest:rsvdrop")
	waitHostSeat(t, ts, roomID, "guest:rsvdrop")

	records := hostChangeRecords(hook, roomID)
	if len(records) != 1 || records[0].data["trigger"] != meetingHostTriggerHostLeft || records[0].data["new_host"] != "guest:rsvdrop" {
		t.Fatalf("房主断线后入会应转一次席位给 guest:rsvdrop，got %+v", records)
	}
	requireMeetingEndAccepted(t, ts, guest, roomID)
	assertNoServerPanics(t, ts)
}

// TestReservedMeetingGuestJoinBeforeHostKeepsSeatForCreator（A1 裁定的边界）：
// 登记房主仍在连接上、只是尚未入会（会议邀请允许受邀者先入会）⇒ 不是「已离场」，
// 先到的成员不得夺走发起人的席位；发起人后到仍是房主。
func TestReservedMeetingGuestJoinBeforeHostKeepsSeatForCreator(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, host := createMeetingOnly(t, ts, "host:early")
	guest := newWSRPC(t, ts.srv, "guest:early")
	joinMeetingMustSucceed(t, ts, guest, roomID, "guest:early")
	if got := ts.handler.meetingHostUniqID(roomID); got != "host:early" {
		t.Fatalf("房主席位 = %q，发起人只是尚未到场，席位不得被先到者夺走", got)
	}
	requireMeetingEndRejected(t, guest, roomID)

	joinMeetingMustSucceed(t, ts, host, roomID, "host:early")
	if got := ts.handler.meetingHostUniqID(roomID); got != "host:early" {
		t.Fatalf("房主席位 = %q，发起人后到仍应是房主", got)
	}
	requireMeetingEndAccepted(t, ts, host, roomID)
	assertNoServerPanics(t, ts)
}

// TestBreakoutChildHostSeatStaysWithMainHostAfterMemberJoins（A1 裁定的边界）：
// breakout 子房间的 Host 按设计恒为主会议房主，且房主不在子房间在册名单内
// （BreakoutMembers 不含房主）—— 子房间不得触发房主转移，否则主会议房主失去对
// 子房间的回收/结束权。
func TestBreakoutChildHostSeatStaysWithMainHostAfterMemberJoins(t *testing.T) {
	s := startBreakoutScenario(t, "host:bseat", "member:bseat")
	switchMemberToChild(t, s)
	if got := s.ts.handler.meetingHostUniqID(s.childID); got != "host:bseat" {
		t.Fatalf("子房间房主席位 = %q，应恒为主会议房主 host:bseat", got)
	}
	rejected := model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: s.childID}
	if err := s.member.sendJSON(rejected); err != nil {
		t.Fatal(err)
	}
	if msg, err := s.member.waitError(5 * time.Second); err != nil || msg == "" {
		t.Fatalf("子房间成员不得结束子房间: msg=%q err=%v", msg, err)
	}
	end := model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: s.childID}
	if err := s.host.sendJSON(end); err != nil {
		t.Fatal(err)
	}
	waitMeetingReleased(t, s.ts, s.childID)
	assertNoServerPanics(t, s.ts)
}
