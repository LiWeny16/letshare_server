package handler

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"letshare-server/internal/meeting"
	"letshare-server/internal/model"

	"github.com/sirupsen/logrus"
)

// V1 会议号保活的行为测试（线上事故原形 + C-17/C-18/C-19/C-20 + DIVERGENCE F10）。
// 时钟纪律：只注入 meetingGrace / meetingReservationTTL（var 注入惯例），禁止分钟级 sleep。

type hookRecord struct {
	message string
	data    logrus.Fields
}

// teardownLogHook 捕获 meeting-end 日志，断言「恰好一条 action=teardown 权威记录」。
type teardownLogHook struct {
	mu      sync.Mutex
	records []hookRecord
}

func (h *teardownLogHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h *teardownLogHook) Fire(entry *logrus.Entry) error {
	fields := logrus.Fields{}
	for key, value := range entry.Data {
		fields[key] = value
	}
	h.mu.Lock()
	h.records = append(h.records, hookRecord{message: entry.Message, data: fields})
	h.mu.Unlock()
	return nil
}

// endRecords 返回指定会议号的 meeting-end 记录（action 非空时按动作过滤）。
func (h *teardownLogHook) endRecords(roomID, action string) []hookRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []hookRecord{}
	for _, record := range h.records {
		if record.data["event"] != "meeting-end" || record.data["roomID"] != roomID {
			continue
		}
		if action != "" && record.data["action"] != action {
			continue
		}
		out = append(out, record)
	}
	return out
}

func installTeardownLogHook(t *testing.T) *teardownLogHook {
	t.Helper()
	hook := &teardownLogHook{}
	prev := logrus.StandardLogger().ReplaceHooks(logrus.LevelHooks{})
	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(prev) })
	return hook
}

// waitEndRecords 等待至少一条匹配的 meeting-end 记录出现（计时器异步拆除路径防竞态）。
func waitEndRecords(t *testing.T, hook *teardownLogHook, roomID, action string) []hookRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if records := hook.endRecords(roomID, action); len(records) > 0 {
			return records
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no meeting-end action=%s record for meeting %s", action, roomID)
	return nil
}

// injectMeetingGrace 注入空置宽限 TTL（在 meetingClockMu 下写入：var 注入惯例同步化，
// 否则后台定时器/异步清理的读取与测试写入构成数据竞态，-race 实测）。
func injectMeetingGrace(t *testing.T, d time.Duration) {
	t.Helper()
	meetingClockMu.Lock()
	old := meetingGrace
	meetingGrace = d
	meetingClockMu.Unlock()
	t.Cleanup(func() {
		meetingClockMu.Lock()
		meetingGrace = old
		meetingClockMu.Unlock()
	})
}

// injectMeetingReservationTTL 注入「创建但从未 join」的预约 TTL（同 injectMeetingGrace）。
func injectMeetingReservationTTL(t *testing.T, d time.Duration) {
	t.Helper()
	meetingClockMu.Lock()
	old := meetingReservationTTL
	meetingReservationTTL = d
	meetingClockMu.Unlock()
	t.Cleanup(func() {
		meetingClockMu.Lock()
		meetingReservationTTL = old
		meetingClockMu.Unlock()
	})
}

// createMeetingOnly 房主创建会议但不入会（此后入会的成员都是非房主成员）。
func createMeetingOnly(t *testing.T, ts *meetingTestServer, hostID string) (string, *wsRPC) {
	t.Helper()
	host := newWSRPC(t, ts.srv, hostID)
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingCreate}); err != nil {
		t.Fatalf("send meeting:create: %v", err)
	}
	roomID, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatalf("wait meeting:create: %v", err)
	}
	return roomID, host
}

// joinMeetingMustSucceed 断言 meeting:join 成功注册成员；收到 error（含 404）即失败。
func joinMeetingMustSucceed(t *testing.T, ts *meetingTestServer, client *wsRPC, roomID, uniqID string) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"roomId": roomID, "userName": uniqID})
	if err := client.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: data}); err != nil {
		t.Fatalf("send meeting:join: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, member := range ts.handler.meetings.members(roomID) {
			if member.UniqID == uniqID {
				return
			}
		}
		select {
		case errMsg := <-client.err:
			t.Fatalf("meeting:join must succeed during grace (no 404): %#v", errMsg)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("meeting:join did not register %s in %s", uniqID, roomID)
}

// waitMeetingMembers 等待会议在册成员数收敛到 n。
func waitMeetingMembers(t *testing.T, ts *meetingTestServer, roomID string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(ts.handler.meetings.members(roomID)) == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("meeting %s members did not converge to %d: %+v", roomID, n, ts.handler.meetings.members(roomID))
}

// metaTimer 在锁内读取 emptyTimer（C-19 断言"定时器重建"）。
func metaTimer(t *testing.T, ts *meetingTestServer, meta *meetingMeta) *time.Timer {
	t.Helper()
	ts.handler.meetingLifecycleMu.Lock()
	defer ts.handler.meetingLifecycleMu.Unlock()
	return meta.emptyTimer
}

// waitEmptyTimerRebuilt 等待 emptyTimer 被重建（与 prev 不同的新计时器；prev 传 nil 表示等到首次武装）。
func waitEmptyTimerRebuilt(t *testing.T, ts *meetingTestServer, meta *meetingMeta, prev *time.Timer) *time.Timer {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if timer := metaTimer(t, ts, meta); timer != nil && timer != prev {
			return timer
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("empty timer was not rebuilt after a non-empty expiry (DIVERGENCE F10 regression)")
	return nil
}

func loadMeta(t *testing.T, ts *meetingTestServer, roomID string) *meetingMeta {
	t.Helper()
	v, ok := ts.handler.activeMeetingRooms.Load(roomID)
	if !ok {
		t.Fatalf("meeting %s is not registered", roomID)
	}
	meta, ok := v.(*meetingMeta)
	if !ok || meta == nil {
		t.Fatalf("meeting %s has no meta", roomID)
	}
	return meta
}

// TestMeetingNumberSurvivesLastExplicitLeaveAndRejoinSucceeds 事故原形回归
// （02:33:23 成员显式离开 → 02:33:29 另一成员重连被 404）：非房主成员显式离开使
// 房间空置后，会议号必须仍然存活，任何成员 meeting:join 必须成功。
func TestMeetingNumberSurvivesLastExplicitLeaveAndRejoinSucceeds(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:incident")
	guest := newWSRPC(t, ts.srv, "wd:incident")
	joinMeetingMustSucceed(t, ts, guest, roomID, "wd:incident")

	// 最后一名成员（非房主）显式离开 → 房间空置。
	if err := guest.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 0)
	if _, ok := ts.handler.activeMeetingRooms.Load(roomID); !ok {
		t.Fatal("last explicit leave released the meeting number; the next join would 404 (the incident)")
	}
	// 秒级重入（事故中的"离开 → 重连"）：另一成员必须能加入。
	joinMeetingMustSucceed(t, ts, newWSRPC(t, ts.srv, "rog13:incident"), roomID, "rog13:incident")
	// 原成员换新连接重连也必须成功。
	joinMeetingMustSucceed(t, ts, newWSRPC(t, ts.srv, "wd:incident"), roomID, "wd:incident")
}

// TestRejoinDuringGraceAlwaysSucceedsForAnyMember（C-17）：宽限期内任何成员重入
// 100% 成功（非 404）——覆盖连续多轮「离开 → 立即重入」与非原成员首次加入。
func TestRejoinDuringGraceAlwaysSucceedsForAnyMember(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:c17")
	member := newWSRPC(t, ts.srv, "member:c17")
	for round := 0; round < 3; round++ {
		joinMeetingMustSucceed(t, ts, member, roomID, "member:c17")
		if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
			t.Fatal(err)
		}
		waitMeetingMembers(t, ts, roomID, 0)
		member = newWSRPC(t, ts.srv, "member:c17") // 换新连接 = 真实重连
	}
	joinMeetingMustSucceed(t, ts, member, roomID, "member:c17")
	joinMeetingMustSucceed(t, ts, newWSRPC(t, ts.srv, "newcomer:c17"), roomID, "newcomer:c17")
}

// TestEndIsIdempotentSoSecondCallHasNoSecondSideEffects（C-18）：End 连续调用多次，
// 只产生一次副作用 —— 恰好一条 action=teardown 权威记录、会议号只释放一次。
func TestEndIsIdempotentSoSecondCallHasNoSecondSideEffects(t *testing.T) {
	hook := installTeardownLogHook(t)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:c18")
	meta := loadMeta(t, ts, roomID)
	member := newWSRPC(t, ts.srv, "member:c18")
	joinMeetingMustSucceed(t, ts, member, roomID, "member:c18")

	ts.handler.End(roomID, meetingEndReasonServerTeardown)
	ts.handler.End(roomID, meetingEndReasonServerTeardown) // C-18：不得产生第二次副作用
	ts.handler.End(roomID, meetingEndReasonHostEnded)      // 换 reason 也不得复活

	// C-18 广播副作用同样只允许一次。
	select {
	case <-member.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("member did not receive meeting:ended")
	}
	select {
	case duplicate := <-member.ended:
		t.Fatalf("double End must not broadcast twice: %#v", duplicate)
	case <-time.After(300 * time.Millisecond):
	}

	records := hook.endRecords(roomID, meetingActionTeardown)
	if len(records) != 1 {
		t.Fatalf("End must log exactly one action=teardown record, got %d: %+v", len(records), records)
	}
	if records[0].data["reason"] != meetingEndReasonServerTeardown {
		t.Fatalf("teardown reason = %v, want %v", records[0].data["reason"], meetingEndReasonServerTeardown)
	}
	if ts.handler.hasRegistered(roomID) {
		t.Fatal("meeting number must be released exactly once")
	}
	if meta.life.Phase() != meeting.PhaseEnded {
		t.Fatalf("phase = %v, want ended", meta.life.Phase())
	}
}

// TestEmptyTimerFiresOnOccupiedMeetingRearmsInsteadOfSilentSkip（C-19 / F10 修复）：
// emptyTimer 到点且房间非空时必须重建定时器（重排下一次），而不是跳过后让宽限永久失效。
func TestEmptyTimerFiresOnOccupiedMeetingRearmsInsteadOfSilentSkip(t *testing.T) {
	hook := installTeardownLogHook(t)
	injectMeetingGrace(t, 2*time.Second)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:c19")
	member := newWSRPC(t, ts.srv, "member:c19")
	joinMeetingMustSucceed(t, ts, member, roomID, "member:c19")
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 0)
	meta := loadMeta(t, ts, roomID)
	firedTimer := waitEmptyTimerRebuilt(t, ts, meta, nil) // 等到 emptyTimer 真正武装

	// F10 现场条件：在册成员存在而空置宽限仍在计时（旧代码到点被跳过即永久失效）。
	ts.handler.meetings.join(roomID, "ghost:c19", "Ghost", "client-ghost-c19")
	waitEmptyTimerRebuilt(t, ts, meta, firedTimer) // C-19：断言定时器重建
	if _, ok := ts.handler.activeMeetingRooms.Load(roomID); !ok {
		t.Fatal("occupied meeting must not be ended at grace expiry")
	}
	if meta.life.Phase() != meeting.PhaseGrace {
		t.Fatalf("phase = %v, want grace (occupied room keeps waiting)", meta.life.Phase())
	}
	// F10 现场留痕：重排必须留下 grace-expired-room-not-empty 记录。
	records := waitEndRecords(t, hook, roomID, meetingActionScheduleGrace)
	for _, record := range records {
		if record.data["reason"] == meetingEndReasonGraceSkipped {
			return
		}
	}
	t.Fatalf("re-arm must log grace-expired-room-not-empty, got %+v", records)
}

// TestGraceStillWorksAfterNonEmptyExpirySoNextDisconnectRearms（F10 专项）：
// 宽限到点被重排之后，再次断线仍然必须重新武装宽限并最终 End —— 旧实现此后宽限永久失效。
func TestGraceStillWorksAfterNonEmptyExpirySoNextDisconnectRearms(t *testing.T) {
	hook := installTeardownLogHook(t)
	injectMeetingGrace(t, 2*time.Second)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:f10")
	member := newWSRPC(t, ts.srv, "member:f10")
	joinMeetingMustSucceed(t, ts, member, roomID, "member:f10")
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 0)
	meta := loadMeta(t, ts, roomID)
	armed := waitEmptyTimerRebuilt(t, ts, meta, nil)
	ts.handler.meetings.join(roomID, "ghost:f10", "Ghost", "client-ghost-f10")
	armed = waitEmptyTimerRebuilt(t, ts, meta, armed) // 到点非空 → 重排（第一次跳过）

	// 再次断线（ghost 的 WS 清理）：宽限必须仍然生效 —— 重新武装一个全新计时器。
	ts.handler.cleanupMeetingClientID("client-ghost-f10")
	waitMeetingMembers(t, ts, roomID, 0)
	records := waitEndRecords(t, hook, roomID, meetingActionScheduleGrace)
	if len(records) < 2 {
		t.Fatalf("disconnect after a skipped expiry must arm a fresh grace (schedule-grace logs=%d)", len(records))
	}
	armed = waitEmptyTimerRebuilt(t, ts, meta, armed)
	if armed == nil {
		t.Fatal("disconnect after a skipped expiry must arm a fresh grace timer (F10)")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !ts.handler.hasRegistered(roomID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("meeting was not ended after the re-armed grace expired")
}

// TestMeetingPhaseStaysGraceUntilExpiryThenEnds（C-20）：全员显式离开后
// Phase != Ended（PhaseGrace）、会议号仍被占用，直到宽限期到期才 End 并释放。
func TestMeetingPhaseStaysGraceUntilExpiryThenEnds(t *testing.T) {
	hook := installTeardownLogHook(t)
	injectMeetingGrace(t, 2*time.Second)
	ts := newMeetingTestServer(t)
	defer ts.Close()
	roomID, _ := createMeetingOnly(t, ts, "host:c20")
	member := newWSRPC(t, ts.srv, "member:c20")
	joinMeetingMustSucceed(t, ts, member, roomID, "member:c20")
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, roomID, 0)
	meta := loadMeta(t, ts, roomID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && meta.life.Phase() != meeting.PhaseGrace {
		time.Sleep(10 * time.Millisecond)
	}
	if phase := meta.life.Phase(); phase != meeting.PhaseGrace {
		t.Fatalf("phase after emptying = %v, want grace (C-20: never Ended on leave)", phase)
	}
	if !ts.handler.hasRegistered(roomID) {
		t.Fatal("meeting number must survive the empty room until the grace expires")
	}
	grace := waitEndRecords(t, hook, roomID, meetingActionScheduleGrace)
	if len(grace) != 1 || grace[0].data["reason"] != meetingEndReasonLastMemberLeft {
		t.Fatalf("explicit last leave must log one schedule-grace record, got %+v", grace)
	}
	for time.Now().Before(deadline) {
		if meta.life.Phase() == meeting.PhaseEnded {
			if ts.handler.hasRegistered(roomID) {
				t.Fatal("ended meeting must release the meeting number")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("phase = %v, want ended after the grace expiry", meta.life.Phase())
}
