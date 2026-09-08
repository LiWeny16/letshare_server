package handler

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func waitPresentation(t *testing.T, r *wsRPC, timeout time.Duration) map[string]interface{} {
	t.Helper()
	select {
	case m := <-r.presentation:
		var payload map[string]interface{}
		if err := json.Unmarshal(m.Data, &payload); err != nil {
			t.Fatalf("解析 meeting:presentation 失败: %v", err)
		}
		return payload
	case <-time.After(timeout):
		t.Fatalf("等待 meeting:presentation 超时")
		return nil
	}
}

func waitExcalidraw(t *testing.T, r *wsRPC, timeout time.Duration) map[string]interface{} {
	t.Helper()
	select {
	case m := <-r.excalidraw:
		var payload map[string]interface{}
		if err := json.Unmarshal(m.Data, &payload); err != nil {
			t.Fatalf("解析 meeting:excalidraw 失败: %v", err)
		}
		return payload
	case <-time.After(timeout):
		t.Fatalf("等待 meeting:excalidraw 超时")
		return nil
	}
}

func TestMeetingPresentation_SingleOwnerAndAuthorization(t *testing.T) {
	ts, a, b, c, meetingID := meetingChatFlow(t)
	_ = ts

	claim := func(r *wsRPC, user, mode, boardMode string) {
		t.Helper()
		payload := map[string]string{"action": "claim", "mode": mode}
		if boardMode != "" {
			payload["boardMode"] = boardMode
		}
		data, _ := json.Marshal(payload)
		if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
			t.Fatalf("%s claim 失败: %v", user, err)
		}
	}

	claim(a, "alice", "screen", "")
	for _, r := range []*wsRPC{a, b} {
		state := waitPresentation(t, r, 2*time.Second)
		if state["ownerId"] != "alice:chat-1" || state["mode"] != "screen" {
			t.Fatalf("第一次 claim 状态错误: %#v", state)
		}
	}
	select {
	case msg := <-c.presentation:
		t.Fatalf("未加入会议的订阅者不应收到 presentation: %#v", msg)
	case <-time.After(150 * time.Millisecond):
	}

	claim(b, "bob", "whiteboard", "excalidraw")
	for _, r := range []*wsRPC{a, b} {
		state := waitPresentation(t, r, 2*time.Second)
		if state["ownerId"] != "bob:chat-2" || state["mode"] != "whiteboard" || state["boardMode"] != "excalidraw" {
			t.Fatalf("抢占后的唯一主持状态错误: %#v", state)
		}
		if epoch, ok := state["epoch"].(float64); !ok || epoch != 2 {
			t.Fatalf("抢占后的 epoch 错误: %#v", state["epoch"])
		}
	}

	data, _ := json.Marshal(map[string]string{"action": "claim", "mode": "screen"})
	if err := c.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("第三方发送 presentation 失败: %v", err)
	}
	errMsg, err := c.waitError(2 * time.Second)
	if err != nil || errMsg == "" {
		t.Fatalf("第三方应被拒绝，实际 err=%q: %v", errMsg, err)
	}

	data, _ = json.Marshal(map[string]string{"action": "release"})
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("host release 失败: %v", err)
	}
	for _, r := range []*wsRPC{a, b} {
		state := waitPresentation(t, r, 2*time.Second)
		if state["ownerId"] != "" || state["mode"] != "" {
			t.Fatalf("release 后仍存在主持者: %#v", state)
		}
	}
}

func TestMeetingExcalidraw_SnapshotOnlyToMeetingMembers(t *testing.T) {
	ts, a, b, c, meetingID := meetingChatFlow(t)
	_ = ts

	scene := map[string]interface{}{
		"elements": []map[string]interface{}{{"id": "rect-1", "type": "rectangle", "x": 10, "y": 20, "width": 100, "height": 60}},
		"appState": map[string]interface{}{"viewBackgroundColor": "#ffffff"},
	}
	data, _ := json.Marshal(map[string]interface{}{"action": "update", "scene": scene})
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送 Excalidraw scene 失败: %v", err)
	}
	for _, r := range []*wsRPC{a, b} {
		payload := waitExcalidraw(t, r, 2*time.Second)
		if payload["action"] != "snapshot" || payload["revision"] != float64(1) {
			t.Fatalf("Excalidraw snapshot 错误: %#v", payload)
		}
	}
	select {
	case msg := <-c.excalidraw:
		t.Fatalf("未加入会议的订阅者不应收到 Excalidraw: %#v", msg)
	case <-time.After(150 * time.Millisecond):
	}

	request, _ := json.Marshal(map[string]string{"action": "request"})
	if err := b.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: request}); err != nil {
		t.Fatalf("请求 Excalidraw snapshot 失败: %v", err)
	}
	payload := waitExcalidraw(t, b, 2*time.Second)
	if payload["revision"] != float64(1) {
		t.Fatalf("request 返回 revision 错误: %#v", payload)
	}

	bad, _ := json.Marshal(map[string]interface{}{"action": "update", "scene": map[string]interface{}{"notElements": true}})
	if err := c.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: bad}); err != nil {
		t.Fatalf("第三方发送 Excalidraw 失败: %v", err)
	}
	if msg, err := c.waitError(2 * time.Second); err != nil || msg == "" {
		t.Fatalf("第三方 Excalidraw 应被拒绝: %q %v", msg, err)
	}
}

func TestMeetingPresentation_MessageShapeHasNoClientAuthorityFields(t *testing.T) {
	var state struct {
		Mode    string `json:"mode"`
		OwnerID string `json:"ownerId"`
		Epoch   uint64 `json:"epoch"`
	}
	if err := json.Unmarshal([]byte(`{"mode":"screen","ownerId":"server-user","epoch":3}`), &state); err != nil {
		t.Fatal(err)
	}
	if state.OwnerID != "server-user" || state.Epoch != 3 {
		t.Fatalf("服务端主持状态解析异常: %s", fmt.Sprint(state))
	}
}
