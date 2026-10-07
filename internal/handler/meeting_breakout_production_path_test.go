package handler

// P0 回归锁：breakout 子房间「创建 → join → 级联拆除」必须走生产构造路径。
//
// 事故根因：生产 meeting:breakout create 漏了 life，而测试接缝 registerBreakoutChild
// 显式建了 life —— 接缝替生产补齐初始化，把 nil 指针 bug 永久隐藏（X2 分叉）。
// 本文件三个测试分别锁：
//  ① 生产 create 建出的子房间带 life（初值 PhaseReserved），成员生产 meeting:join
//     走 enterMeetingLive 不 panic 且相位到 PhaseLive；
//  ② 级联拆除不 panic、子房间相位到 PhaseEnded、在册成员恰好一条 meeting:ended；
//  ③ meetingMeta 复合字面量全包恰好一处（newMeetingMeta 内），接缝与生产无分叉。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"letshare-server/internal/meeting"
	"letshare-server/internal/model"
)

// serverPanicLog 显式声明的 registry（STANDARDS Go 基线允许的例外，全部经方法访问）：
// 登记各测试服务器 WS 服务循环 recover 到的 panic，使「服务端不 panic」可断言。
type serverPanicLog struct {
	mu   sync.Mutex
	byTS map[*meetingTestServer][]string
}

// record 由 meeting_e2e_test.go 的 WS 循环 defer 调用（recover 结果转存）。
// 旧 harness recover 后静默吞掉（X4），nil 指针 panic 因此隐身。
func (l *serverPanicLog) record(ts *meetingTestServer, rec any) {
	if rec == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byTS == nil {
		l.byTS = make(map[*meetingTestServer][]string)
	}
	l.byTS[ts] = append(l.byTS[ts], fmt.Sprint(rec))
}

// take 取走并清空该测试服务器登记到的 panic 列表。
func (l *serverPanicLog) take(ts *meetingTestServer) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.byTS[ts]
	delete(l.byTS, ts)
	return out
}

// serverPanics panic 登记处（唯一写者：WS 循环 defer；唯一读者：assertNoServerPanics）。
var serverPanics serverPanicLog

// assertNoServerPanics 断言服务端 WS 循环全程没有 panic（含 recover 兜底的那些）。
func assertNoServerPanics(t *testing.T, ts *meetingTestServer) {
	t.Helper()
	if panics := serverPanics.take(ts); len(panics) > 0 {
		t.Fatalf("服务端 WS 循环 panic（不得被 recover 静默吞掉）: %v", panics)
	}
}

// assertMeetingPhase 断言 meta 相位；life 为 nil（P0 原形）在此显式失败而非静默。
func assertMeetingPhase(t *testing.T, meta *meetingMeta, want meeting.Phase) {
	t.Helper()
	if meta == nil || meta.life == nil {
		t.Fatal("meetingMeta.life 未初始化：构造必须走 newMeetingMeta（P0 回归）")
	}
	if got := meta.life.Phase(); got != want {
		t.Fatalf("相位 = %s，应为 %s", got, want)
	}
}

// breakoutScenario 生产信令场景：主会议 + 一个由 meeting:breakout create 建出的子房间。
type breakoutScenario struct {
	ts        *meetingTestServer
	host      *wsRPC
	member    *wsRPC
	mainID    string
	childID   string
	memberTag string
}

// startBreakoutScenario 全程走生产信令建出子房间：meeting:create → 双方 meeting:join
// 主会 → 房主 meeting:breakout create（子房间由生产构造路径登记）→ 成员收到 invite。
// 不使用任何测试接缝 —— 这正是上一轮 bug 藏身之处。
func startBreakoutScenario(t *testing.T, hostTag, memberTag string) *breakoutScenario {
	t.Helper()
	ts := newMeetingTestServer(t)
	t.Cleanup(ts.Close)
	mainID, host := createMeetingOnly(t, ts, hostTag)
	joinMeetingMustSucceed(t, ts, host, mainID, hostTag)
	member := newWSRPC(t, ts.srv, memberTag)
	joinMeetingMustSucceed(t, ts, member, mainID, memberTag)
	childID := mainID + "B1"
	payload, _ := json.Marshal(map[string]interface{}{
		"action": "create",
		"assignments": []map[string]interface{}{
			{"room": childID, "members": []string{memberTag}},
		},
	})
	msg := model.WebSocketMessage{Type: model.MessageTypeMeetingBreakout, Channel: mainID, Data: payload}
	if err := host.sendJSON(msg); err != nil {
		t.Fatalf("send meeting:breakout create: %v", err)
	}
	waitBreakoutInvite(t, member, childID)
	return &breakoutScenario{ts: ts, host: host, member: member, mainID: mainID, childID: childID, memberTag: memberTag}
}

// waitBreakoutInvite 等成员收到子房间 invite（证明生产 create 路径已登记该子房间）。
func waitBreakoutInvite(t *testing.T, member *wsRPC, childID string) {
	t.Helper()
	select {
	case m := <-member.breakout:
		var d struct {
			Action string `json:"action"`
			Room   string `json:"room"`
		}
		_ = json.Unmarshal(m.Data, &d)
		if d.Action != "invite" || d.Room != childID {
			t.Fatalf("meeting:breakout = %+v，应为 invite %s", d, childID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("成员未收到子房间 %s 的 breakout invite", childID)
	}
}

// switchMemberToChild 成员真实切换房间：leave 主会 + join 子会（生产 meeting:join 全路径）。
func switchMemberToChild(t *testing.T, s *breakoutScenario) {
	t.Helper()
	leave := model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: s.mainID}
	if err := s.member.sendJSON(leave); err != nil {
		t.Fatal(err)
	}
	joinMeetingMustSucceed(t, s.ts, s.member, s.childID, s.memberTag)
}

// TestBreakoutChildProductionCreateStartsReservedAndJoinReachesLiveWithoutPanic 不变量：
// 生产 meeting:breakout create 构造的子房间必须带 life（初值 PhaseReserved）；成员
// meeting:join 子房间走 enterMeetingLive 不 panic 且相位跃迁到 PhaseLive。
func TestBreakoutChildProductionCreateStartsReservedAndJoinReachesLiveWithoutPanic(t *testing.T) {
	s := startBreakoutScenario(t, "host:bprod", "member:bprod")
	assertMeetingPhase(t, loadMeta(t, s.ts, s.childID), meeting.PhaseReserved)
	switchMemberToChild(t, s)
	assertMeetingPhase(t, loadMeta(t, s.ts, s.childID), meeting.PhaseLive)
	assertNoServerPanics(t, s.ts)
}

// TestBreakoutChildCascadeTeardownReachesEndedExactlyOnceWithoutPanic 不变量：
// 主会议拆除必须级联拆除子房间：子房间相位到 PhaseEnded、在册成员恰好一条
// meeting:ended（协议 reason=ended）、会议号释放，全程不 panic。
func TestBreakoutChildCascadeTeardownReachesEndedExactlyOnceWithoutPanic(t *testing.T) {
	hook := installTeardownLogHook(t)
	s := startBreakoutScenario(t, "host:bcas", "member:bcas")
	switchMemberToChild(t, s)
	childMeta := loadMeta(t, s.ts, s.childID)
	end := model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: s.mainID}
	if err := s.host.sendJSON(end); err != nil {
		t.Fatal(err)
	}
	select {
	case ended := <-s.member.ended:
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ended.Data, &payload); err != nil || payload.Reason != "ended" {
			t.Fatalf("meeting:ended = %s err=%v，级联 reason 应为 ended（协议现状）", ended.Data, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("级联拆除必须给子房间在册成员发 meeting:ended")
	}
	assertNoMeetingEnded(t, s.member, 300*time.Millisecond, "级联拆除每成员只能有一条 meeting:ended")
	assertMeetingPhase(t, childMeta, meeting.PhaseEnded)
	waitMeetingReleased(t, s.ts, s.childID)
	waitMeetingReleased(t, s.ts, s.mainID)
	assertSingleTeardown(t, hook, s.childID, meetingEndReasonParentTeardown)
	assertNoServerPanics(t, s.ts)
}

// TestMeetingMetaLiteralOnlyExistsInsideTheSharedConstructor 不变量：
// meetingMeta 复合字面量全包恰好一处（newMeetingMeta 内）—— 生产与测试只能走同一
// 构造入口，否则「生产漏字段 / 测试接缝补齐」的 P0 分叉会重演。
func TestMeetingMetaLiteralOnlyExistsInsideTheSharedConstructor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// 针子在运行时拼接：避免扫描器自匹配到本文件里的搜索词本身。
	needle := "meeting" + "Meta{"
	total := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		hits := strings.Count(string(src), needle)
		if hits == 0 {
			continue
		}
		total += hits
		if name != "meeting_registry.go" {
			t.Fatalf("%s 出现 %d 处 %s 字面量：必须改走 newMeetingMeta（唯一构造入口）", name, hits, needle)
		}
	}
	if total != 1 {
		t.Fatalf("%s 字面量共 %d 处，应恰好 1 处（newMeetingMeta 内）", needle, total)
	}
}
