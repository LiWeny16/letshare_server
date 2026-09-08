package handler

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"letshare-server/internal/model"
)

// meetingChatFlow 建立「A/B 已入会（SFU 参与者）、C 仅订阅会议房间未 join」的前置。
func meetingChatFlow(t *testing.T) (ts *meetingTestServer, a, b, c *wsRPC, meetingID string) {
	t.Helper()
	ts = newMeetingTestServer(t)
	t.Cleanup(ts.Close)

	const userA = "alice:chat-1"
	const userB = "bob:chat-2"
	const userC = "carol:chat-3"

	a = newWSRPC(t, ts.srv, userA)
	b = newWSRPC(t, ts.srv, userB)
	c = newWSRPC(t, ts.srv, userC)

	// A 创建会议
	if err := a.sendJSON(model.WebSocketMessage{Type: "meeting:create", Data: []byte(`{}`)}); err != nil {
		t.Fatalf("发送 meeting:create 失败: %v", err)
	}
	room, err := a.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatalf("等待 meeting:create 失败: %v", err)
	}
	meetingID = room

	// 三人订阅会议房间（C 只订阅、不 join —— 非 SFU 成员）
	subscribe := func(r *wsRPC) {
		if err := r.sendJSON(model.WebSocketMessage{Type: "subscribe", Channel: meetingID, Event: "signal:all"}); err != nil {
			t.Fatalf("发送 subscribe 失败: %v", err)
		}
		if err := r.waitSubscribed(5 * time.Second); err != nil {
			t.Fatalf("订阅会议房间失败: %v", err)
		}
	}
	subscribe(a)
	subscribe(b)
	subscribe(c)

	// A、B 加入会议（成为 SFU 参与者 = 会议成员权威名单）
	join := func(r *wsRPC) {
		data, _ := json.Marshal(map[string]interface{}{"roomId": meetingID})
		if err := r.sendJSON(model.WebSocketMessage{Type: "meeting:join", Channel: meetingID, Data: data}); err != nil {
			t.Fatalf("发送 meeting:join 失败: %v", err)
		}
	}
	join(a)
	join(b)

	// 等待 SFU 参与者登记生效
	deadline := time.Now().Add(5 * time.Second)
	for {
		room, ok := ts.handler.sfuManager.GetRoom(meetingID)
		if ok && len(meetingParticipantIDs(room)) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待会议成员登记超时")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return
}

func waitChat(r *wsRPC, timeout time.Duration) (model.WebSocketMessage, error) {
	select {
	case m, ok := <-r.chat:
		if !ok {
			return m, fmt.Errorf("chat 通道关闭")
		}
		return m, nil
	case <-time.After(timeout):
		return model.WebSocketMessage{}, fmt.Errorf("等待 meeting:chat 超时")
	}
}

func chatPayloadField(t *testing.T, m model.WebSocketMessage, key string) interface{} {
	t.Helper()
	var d map[string]interface{}
	if err := json.Unmarshal(m.Data, &d); err != nil {
		t.Fatalf("解析 meeting:chat 数据失败: %v", err)
	}
	return d[key]
}

func sendChat(r *wsRPC, meetingID, text, to string) error {
	payload := map[string]interface{}{"text": text}
	if to != "" {
		payload["to"] = to
	}
	data, _ := json.Marshal(payload)
	return r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingChat, Channel: meetingID, Data: data})
}

// 公聊：A 广播 → B（已入会成员）收到；发送者自身不回环（本地回显）。
func TestMeetingChat_Broadcast(t *testing.T) {
	_, a, b, _, meetingID := meetingChatFlow(t)

	if err := sendChat(a, meetingID, "大家好", ""); err != nil {
		t.Fatalf("发送公聊失败: %v", err)
	}
	m, err := waitChat(b, 5*time.Second)
	if err != nil {
		t.Fatalf("成员未收到公聊: %v", err)
	}
	if got := chatPayloadField(t, m, "text"); got != "大家好" {
		t.Fatalf("公聊文本不符: %v", got)
	}
	if got := chatPayloadField(t, m, "from"); got != "alice:chat-1" {
		t.Fatalf("公聊 from 不符: %v", got)
	}
	if got, exists := chatPayloadField(t, m, "to").(string); exists && got != "" {
		t.Fatalf("公聊不应携带 to: %v", got)
	}
}

// 私聊：定向投递给目标成员，带 to 字段；第三人只订阅未入会，不应收到私聊副本。
func TestMeetingChat_PrivateDirected(t *testing.T) {
	_, a, b, c, meetingID := meetingChatFlow(t)

	if err := sendChat(a, meetingID, "只给你看", "bob:chat-2"); err != nil {
		t.Fatalf("发送私聊失败: %v", err)
	}
	m, err := waitChat(b, 5*time.Second)
	if err != nil {
		t.Fatalf("目标成员未收到私聊: %v", err)
	}
	if got := chatPayloadField(t, m, "text"); got != "只给你看" {
		t.Fatalf("私聊文本不符: %v", got)
	}
	if got := chatPayloadField(t, m, "to"); got != "bob:chat-2" {
		t.Fatalf("私聊 to 不符: %v", got)
	}

	// 非成员 C 不应收到私聊副本
	select {
	case m := <-c.chat:
		t.Fatalf("未入会成员不应收到私聊: %v", m)
	case <-time.After(800 * time.Millisecond):
	}
}

// 私聊目标不是会议成员 → 服务器拒绝（404），目标与第三方都收不到。
func TestMeetingChat_RejectsNonMemberTarget(t *testing.T) {
	_, a, _, _, meetingID := meetingChatFlow(t)

	if err := sendChat(a, meetingID, "悄悄话", "carol:chat-3"); err != nil {
		t.Fatalf("发送私聊失败: %v", err)
	}
	if _, err := a.waitError(5 * time.Second); err != nil {
		t.Fatalf("非成员目标应返回 error: %v", err)
	}
}

// 未入会成员（只订阅会议房间）不能发送聊天 → 服务器拒绝。
func TestMeetingChat_RejectsNonMemberSender(t *testing.T) {
	_, _, _, c, meetingID := meetingChatFlow(t)

	if err := sendChat(c, meetingID, "我不在会里", ""); err != nil {
		t.Fatalf("发送聊天失败: %v", err)
	}
	if _, err := c.waitError(5 * time.Second); err != nil {
		t.Fatalf("非成员发送应返回 error: %v", err)
	}
}

// 私聊给已离开会议的成员（成员离开后名单失效）→ 服务器拒绝。
func TestMeetingChat_RejectsGoneTarget(t *testing.T) {
	ts, a, b, _, meetingID := meetingChatFlow(t)

	// B 发 meeting:leave 离会（SFU 参与者移除）
	data, _ := json.Marshal(map[string]interface{}{})
	if err := b.sendJSON(model.WebSocketMessage{Type: "meeting:leave", Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送 meeting:leave 失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		room, ok := ts.handler.sfuManager.GetRoom(meetingID)
		if ok && len(meetingParticipantIDs(room)) <= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待成员离开登记超时")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := sendChat(a, meetingID, "人呢？", "bob:chat-2"); err != nil {
		t.Fatalf("发送私聊失败: %v", err)
	}
	if _, err := a.waitError(5 * time.Second); err != nil {
		t.Fatalf("目标离开后私聊应返回 error: %v", err)
	}
}

// 未入会成员即使订阅了会议频道，也不能伪造协作画板操作；否则会绕过
// meeting:chat 的成员校验向真实参会者广播脏数据。
func TestMeetingDraw_RejectsNonMemberSender(t *testing.T) {
	_, a, b, c, meetingID := meetingChatFlow(t)
	payload := []byte(`{"op":"stroke","id":"unauthorized","pts":[0,0,1,1]}`)
	if err := c.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingDraw, Channel: meetingID, Data: payload}); err != nil {
		t.Fatalf("发送未授权画板操作失败: %v", err)
	}
	if _, err := c.waitError(5 * time.Second); err != nil {
		t.Fatalf("未入会成员发送画板操作应被拒绝: %v", err)
	}
	for name, r := range map[string]*wsRPC{"alice": a, "bob": b} {
		select {
		case m := <-r.draw:
			t.Fatalf("未授权画板操作不应投递给 %s: %v", name, m)
		case <-time.After(300 * time.Millisecond):
		}
	}
}
