package handler

import "testing"

// TestMeetingEndReasonFromClientReason 锁死 endMeetingRoom 的协议 reason →
// 服务端稳定枚举的映射（server 日志检索键依赖它）：
// "timeout" → host-left-grace-expired；"host-ended" → host-ended；其余 → server-teardown。
func TestMeetingEndReasonFromClientReason(t *testing.T) {
	cases := []struct {
		name         string
		clientReason string
		want         string
	}{
		{"timeout 映射为宽限到期拆除", "timeout", meetingEndReasonGraceExpired},
		{"host-ended 原语义保留", "host-ended", meetingEndReasonHostEnded},
		{"未知 reason 落兜底 server-teardown", "unexpected-reason", meetingEndReasonServerTeardown},
		{"空串也落兜底 server-teardown", "", meetingEndReasonServerTeardown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := meetingEndReasonFromClientReason(tc.clientReason)
			if got != tc.want {
				t.Fatalf("meetingEndReasonFromClientReason(%q) = %q，期望 %q", tc.clientReason, got, tc.want)
			}
		})
	}
}
