package handler

import (
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"testing"
	"time"
)

// meetingInviteFlow 建立「host / invitee / bystander 同处原始房间 + host 已创建会议」的公共前置。
func meetingInviteFlow(t *testing.T) (ts *meetingTestServer, host, invitee, bystander *wsRPC, meetingID, sourceRoom string) {
	t.Helper()
	ts = newMeetingTestServer(t)
	t.Cleanup(ts.Close)

	sourceRoom = "room-inv-1"
	const userHost = "hostA:uuid-1"
	const userB = "guestB:uuid-2"
	const userC = "watcherC:uuid-3"

	host = newWSRPC(t, ts.srv, userHost)
	invitee = newWSRPC(t, ts.srv, userB)
	bystander = newWSRPC(t, ts.srv, userC)

	// host 创建会议（host 独立创建，不依赖任何成员）
	if err := host.sendJSON(model.WebSocketMessage{Type: "meeting:create", Data: []byte(`{"title":"评审会议"}`)}); err != nil {
		t.Fatalf("发送 meeting:create 失败: %v", err)
	}
	room, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatalf("等待 meeting:create 失败: %v", err)
	}
	meetingID = room

	// 三人各自订阅同一原始房间（presence 名单 + 定向投递通道）
	subscribe := func(r *wsRPC) {
		if err := r.sendJSON(model.WebSocketMessage{Type: "subscribe", Channel: sourceRoom, Event: "signal:all"}); err != nil {
			t.Fatalf("发送 subscribe 失败: %v", err)
		}
		if err := r.waitSubscribed(5 * time.Second); err != nil {
			t.Fatalf("订阅原始房间失败: %v", err)
		}
	}
	subscribe(host)
	subscribe(invitee)
	subscribe(bystander)
	return
}

func invitePayloadField(t *testing.T, m model.WebSocketMessage, key string) interface{} {
	t.Helper()
	var d map[string]interface{}
	if err := json.Unmarshal(m.Data, &d); err != nil {
		t.Fatalf("解析 meeting:invite 数据失败: %v", err)
	}
	return d[key]
}

// waitInvite 阻塞等待一条满足 kind 的 meeting:invite。
func waitInvite(r *wsRPC, kind string, timeout time.Duration) (model.WebSocketMessage, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case m, ok := <-r.invite:
			if !ok {
				return m, fmt.Errorf("invite 通道关闭")
			}
			if kind == "" || invitePayloadKind(m) == kind {
				return m, nil
			}
		case <-t.C:
			return model.WebSocketMessage{}, fmt.Errorf("等待 meeting:invite 超时")
		}
	}
}

// waitInviteStatus 阻塞等待一条 action 匹配的 kind=status 回执。
func waitInviteStatus(r *wsRPC, action string, timeout time.Duration) (model.WebSocketMessage, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case m, ok := <-r.invite:
			if !ok {
				return m, fmt.Errorf("invite 通道关闭")
			}
			if invitePayloadKind(m) == "status" && invitePayloadAction(m) == action {
				return m, nil
			}
		case <-t.C:
			return model.WebSocketMessage{}, fmt.Errorf("等待 kind=status(action=%s) 回执超时", action)
		}
	}
}

func invitePayloadKind(m model.WebSocketMessage) string {
	var d map[string]interface{}
	_ = json.Unmarshal(m.Data, &d)
	k, _ := d["kind"].(string)
	return k
}

func invitePayloadAction(m model.WebSocketMessage) string {
	var d map[string]interface{}
	_ = json.Unmarshal(m.Data, &d)
	a, _ := d["action"].(string)
	return a
}

// assertNoInvite 断言窗口期内没有收到任何 meeting:invite（验证不广播、不串扰）。
func assertNoInvite(t *testing.T, r *wsRPC, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		select {
		case m := <-r.invite:
			t.Fatalf("不应收到任何 meeting:invite，实际收到 kind=%s", invitePayloadKind(m))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestMeetingInvite_DirectedDelivery 验证邀请只定向送达目标用户：
// 房主发送 → 被邀请方收到 kind=invite 完整载荷 → 房主收到 sent 回执 → 房间内第三人无任何消息。
func TestMeetingInvite_DirectedDelivery(t *testing.T) {
	_, host, invitee, bystander, meetingID, sourceRoom := meetingInviteFlow(t)

	data, _ := json.Marshal(map[string]interface{}{
		"action":       "invite",
		"to":           "guestB:uuid-2",
		"sourceRoomId": sourceRoom,
		"inviteUrl":    "https://letshare.fun/#/meeting?room=" + meetingID + "&source=" + sourceRoom,
	})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingInvite, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送 meeting:invite 失败: %v", err)
	}

	// 被邀请方收到定向邀请，载荷字段完整
	inv, err := waitInvite(invitee, "invite", 5*time.Second)
	if err != nil {
		t.Fatalf("被邀请方未收到邀请: %v", err)
	}
	if got := invitePayloadField(t, inv, "meetingId"); got != meetingID {
		t.Fatalf("meetingId 应为 %s，实际 %v", meetingID, got)
	}
	if got := invitePayloadField(t, inv, "sourceRoomId"); got != sourceRoom {
		t.Fatalf("sourceRoomId 应与会议号分开，实际 %v", got)
	}
	if got := invitePayloadField(t, inv, "from"); got != "hostA:uuid-1" {
		t.Fatalf("from 应为服务器注入的房主身份，实际 %v", got)
	}
	if got := invitePayloadField(t, inv, "fromName"); got != "hostA" {
		t.Fatalf("fromName 应取 uniqId 前缀，实际 %v", got)
	}
	if got := invitePayloadField(t, inv, "to"); got != "guestB:uuid-2" {
		t.Fatalf("to 应为服务器注入的目标身份，实际 %v", got)
	}
	if got := invitePayloadField(t, inv, "title"); got != "评审会议" {
		t.Fatalf("title 应取服务端登记的会议标题，实际 %v", got)
	}
	expiresAt, _ := invitePayloadField(t, inv, "expiresAt").(float64)
	if expiresAt <= 0 {
		t.Fatalf("expiresAt 必须为正数毫秒时间戳，实际 %v", invitePayloadField(t, inv, "expiresAt"))
	}

	// 房主收到 sent 受理回执
	ack, err := waitInviteStatus(host, "sent", 5*time.Second)
	if err != nil {
		t.Fatalf("房主未收到 sent 回执: %v", err)
	}
	if got := invitePayloadField(t, ack, "action"); got != "sent" {
		t.Fatalf("回执 action 应为 sent，实际 %v", got)
	}
	if got := invitePayloadField(t, ack, "userId"); got != "guestB:uuid-2" {
		t.Fatalf("回执 userId 应为目标用户，实际 %v", got)
	}

	// 原始房间第三人不得收到任何会议邀请（不广播）
	assertNoInvite(t, bystander, 1500*time.Millisecond)
}

// TestMeetingInvite_AcceptAndReject 回执链路：接受/拒绝都定向回执房主，且带 userId。
func TestMeetingInvite_AcceptAndReject(t *testing.T) {
	_, host, invitee, _, meetingID, sourceRoom := meetingInviteFlow(t)

	send := func() string {
		data, _ := json.Marshal(map[string]interface{}{
			"action": "invite", "to": "guestB:uuid-2", "sourceRoomId": sourceRoom,
		})
		if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingInvite, Channel: meetingID, Data: data}); err != nil {
			t.Fatalf("发送 meeting:invite 失败: %v", err)
		}
		inv, err := waitInvite(invitee, "invite", 5*time.Second)
		if err != nil {
			t.Fatalf("被邀请方未收到邀请: %v", err)
		}
		id, _ := invitePayloadField(t, inv, "inviteId").(string)
		return id
	}

	// accept → 房主收到 accept 回执
	inviteID := send()
	resp, _ := json.Marshal(map[string]interface{}{"action": "accept", "inviteId": inviteID})
	if err := invitee.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingInvite, Channel: meetingID, Data: resp}); err != nil {
		t.Fatalf("发送 accept 失败: %v", err)
	}
	st, err := waitInviteStatus(host, "accept", 5*time.Second)
	if err != nil {
		t.Fatalf("房主未收到 accept 回执: %v", err)
	}
	if got := invitePayloadField(t, st, "action"); got != "accept" {
		t.Fatalf("回执 action 应为 accept，实际 %v", got)
	}
	if got := invitePayloadField(t, st, "userId"); got != "guestB:uuid-2" {
		t.Fatalf("回执 userId 应为响应者，实际 %v", got)
	}

	// reject → 房主收到 reject 回执
	inviteID = send()
	resp, _ = json.Marshal(map[string]interface{}{"action": "reject", "inviteId": inviteID})
	if err := invitee.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingInvite, Channel: meetingID, Data: resp}); err != nil {
		t.Fatalf("发送 reject 失败: %v", err)
	}
	st, err = waitInviteStatus(host, "reject", 5*time.Second)
	if err != nil {
		t.Fatalf("房主未收到 reject 回执: %v", err)
	}
	if got := invitePayloadField(t, st, "action"); got != "reject" {
		t.Fatalf("回执 action 应为 reject，实际 %v", got)
	}
}

// TestMeetingInvite_RejectsInvalidSend 服务端校验矩阵：非房主 / 离线目标 / 未知会议 / 重复邀请。
func TestMeetingInvite_RejectsInvalidSend(t *testing.T) {
	_, host, invitee, _, meetingID, sourceRoom := meetingInviteFlow(t)

	sendInvite := func(r *wsRPC, channel, to string) error {
		data, _ := json.Marshal(map[string]interface{}{
			"action": "invite", "to": to, "sourceRoomId": sourceRoom,
		})
		return r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingInvite, Channel: channel, Data: data})
	}

	// 1) 非房主不能发送邀请
	if err := sendInvite(invitee, meetingID, "watcherC:uuid-3"); err != nil {
		t.Fatalf("发送非房主邀请失败: %v", err)
	}
	if msg, err := invitee.waitError(5 * time.Second); err != nil || msg == "" {
		t.Fatalf("非房主发送应被 403 拒绝，实际 %v %v", msg, err)
	} else {
		t.Logf("非房主邀请被拒绝：%s", msg)
	}

	// 2) 目标用户不存在/离线 → 404
	if err := sendInvite(host, meetingID, "ghost:uuid-9"); err != nil {
		t.Fatalf("发送离线目标邀请失败: %v", err)
	}
	if msg, err := host.waitError(5 * time.Second); err != nil || msg == "" {
		t.Fatalf("离线目标应被拒绝，实际 %v %v", msg, err)
	}

	// 3) 未登记会议号 → 404
	if err := sendInvite(host, "0000", "guestB:uuid-2"); err != nil {
		t.Fatalf("发送未知会议邀请失败: %v", err)
	}
	if msg, err := host.waitError(5 * time.Second); err != nil || msg == "" {
		t.Fatalf("未知会议号应被拒绝，实际 %v %v", msg, err)
	}

	// 4) 同一（会议,目标）存在待处理邀请 → 409 重复
	if err := sendInvite(host, meetingID, "guestB:uuid-2"); err != nil {
		t.Fatalf("发送第一次邀请失败: %v", err)
	}
	if _, err := waitInvite(invitee, "invite", 5*time.Second); err != nil {
		t.Fatalf("第一次邀请未送达: %v", err)
	}
	if err := sendInvite(host, meetingID, "guestB:uuid-2"); err != nil {
		t.Fatalf("发送重复邀请失败: %v", err)
	}
	if msg, err := host.waitError(5 * time.Second); err != nil || msg == "" {
		t.Fatalf("重复邀请应被 409 拒绝，实际 %v %v", msg, err)
	}
}

// TestMeetingInvite_ExpiredRespond 缩短 TTL：过期后 accept 被拒，双方收到 expired 回执。
func TestMeetingInvite_ExpiredRespond(t *testing.T) {
	oldTTL := meetingInviteTTL
	meetingInviteTTL = 30 * time.Millisecond
	defer func() { meetingInviteTTL = oldTTL }()

	_, host, invitee, _, meetingID, sourceRoom := meetingInviteFlow(t)

	data, _ := json.Marshal(map[string]interface{}{
		"action": "invite", "to": "guestB:uuid-2", "sourceRoomId": sourceRoom,
	})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingInvite, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送 meeting:invite 失败: %v", err)
	}
	inv, err := waitInvite(invitee, "invite", 5*time.Second)
	if err != nil {
		t.Fatalf("被邀请方未收到邀请: %v", err)
	}
	inviteID, _ := invitePayloadField(t, inv, "inviteId").(string)

	time.Sleep(60 * time.Millisecond) // 越过 TTL

	resp, _ := json.Marshal(map[string]interface{}{"action": "accept", "inviteId": inviteID})
	if err := invitee.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingInvite, Channel: meetingID, Data: resp}); err != nil {
		t.Fatalf("发送过期 accept 失败: %v", err)
	}
	// 房主收到 expired 回执
	st, err := waitInviteStatus(host, "expired", 5*time.Second)
	if err != nil {
		t.Fatalf("房主未收到 expired 回执: %v", err)
	}
	if got := invitePayloadField(t, st, "action"); got != "expired" {
		t.Fatalf("回执 action 应为 expired，实际 %v", got)
	}
	// 被邀请方也收到 expired 回执
	st, err = waitInviteStatus(invitee, "expired", 5*time.Second)
	if err != nil {
		t.Fatalf("被邀请方未收到 expired 回执: %v", err)
	}
	if got := invitePayloadField(t, st, "action"); got != "expired" {
		t.Fatalf("被邀请方回执 action 应为 expired，实际 %v", got)
	}
}
