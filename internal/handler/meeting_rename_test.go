package handler

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"letshare-server/internal/model"
)

// meetingRenameFlow 建立「A/B 已入会」的前置（复用聊天测试的流程骨架）。
func meetingRenameFlow(t *testing.T) (*wsRPC, *wsRPC, string) {
	t.Helper()
	_, a, b, _, meetingID := meetingChatFlow(t)
	return a, b, meetingID
}

func waitRename(r *wsRPC, timeout time.Duration) (model.WebSocketMessage, error) {
	select {
	case m, ok := <-r.rename:
		if !ok {
			return m, fmt.Errorf("rename 通道关闭")
		}
		return m, nil
	case <-time.After(timeout):
		return model.WebSocketMessage{}, fmt.Errorf("等待 meeting:rename 超时")
	}
}

// 会议内改名：A 改名后 B 实时收到 meeting:rename 广播；随后 B 拿到的成员快照里
// A 的 userName 也应已是新名（登记已更新）。
func TestMeetingRename_BroadcastsToMembers(t *testing.T) {
	a, b, meetingID := meetingRenameFlow(t)

	data, _ := json.Marshal(map[string]string{"userName": "新名字"})
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingRename, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送 meeting:rename 失败: %v", err)
	}

	// 服务器把改名广播给包括发送者在内的所有成员（与 media-state 一致）
	m, err := waitRename(b, 5*time.Second)
	if err != nil {
		t.Fatalf("成员未收到 meeting:rename: %v", err)
	}
	var d map[string]interface{}
	if err := json.Unmarshal(m.Data, &d); err != nil {
		t.Fatalf("解析 meeting:rename 数据失败: %v", err)
	}
	if got, _ := d["uniqId"].(string); got != "alice:chat-1" {
		t.Fatalf("rename uniqId 不符: %v", got)
	}
	if got, _ := d["userName"].(string); got != "新名字" {
		t.Fatalf("rename userName 不符: %v", got)
	}

	// 快照应反映新名（后续 join 的成员从快照读取名字）。membership 通道里
	// 可能积压首次 join 的旧快照（旧名），跳过它们直到看到新名或超时。
	if err := b.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("重入会议失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	seenOldName := false
	for {
		select {
		case snap := <-b.membership:
			var sd map[string]interface{}
			if err := json.Unmarshal(snap.Data, &sd); err != nil {
				continue
			}
			members, _ := sd["members"].([]interface{})
			for _, raw := range members {
				member, _ := raw.(map[string]interface{})
				if id, _ := member["uniqId"].(string); id == "alice:chat-1" {
					if name, _ := member["userName"].(string); name == "新名字" {
						return // 快照确认登记已更新
					}
					seenOldName = true // 旧快照，继续等新的
				}
			}
		default:
		}
		if time.Now().After(deadline) {
			if seenOldName {
				t.Fatalf("成员快照始终未反映新名字")
			}
			t.Fatalf("等待包含新名字的成员快照超时")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 非会议成员（仅订阅未 join）不能改名。
func TestMeetingRename_RejectsNonMember(t *testing.T) {
	_, a, _, c, meetingID := meetingChatFlow(t)

	data, _ := json.Marshal(map[string]string{"userName": "越权改名"})
	if err := c.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingRename, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送 meeting:rename 失败: %v", err)
	}
	if _, err := c.waitError(5 * time.Second); err != nil {
		t.Fatalf("非成员改名应被拒绝: %v", err)
	}
	_ = a
}

// 空名称被拒绝。
func TestMeetingRename_RejectsEmptyName(t *testing.T) {
	a, _, meetingID := meetingRenameFlow(t)

	data, _ := json.Marshal(map[string]string{"userName": "   "})
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingRename, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送 meeting:rename 失败: %v", err)
	}
	if _, err := a.waitError(5 * time.Second); err != nil {
		t.Fatalf("空名称应被拒绝: %v", err)
	}
}
