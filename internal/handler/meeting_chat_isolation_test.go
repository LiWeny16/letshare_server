package handler

import (
	"testing"
	"time"
)

func TestMeetingChat_PublicDoesNotLeakToSubscribedNonMember(t *testing.T) {
	_, a, b, c, meetingID := meetingChatFlow(t)
	if err := sendChat(a, meetingID, "only members", ""); err != nil {
		t.Fatalf("发送公聊失败: %v", err)
	}
	if _, err := waitChat(b, 2*time.Second); err != nil {
		t.Fatalf("真实会议成员未收到公聊: %v", err)
	}
	select {
	case msg := <-c.chat:
		t.Fatalf("仅订阅未入会的连接不应收到公聊: %#v", msg)
	case <-time.After(250 * time.Millisecond):
	}
}
