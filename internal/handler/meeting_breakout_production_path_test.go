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
	"go/ast"
	"go/parser"
	"go/token"
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
//
// 扩针（Reviewer 登记项）：旧文本针只抓「类型名紧贴花括号」的字面量，以下四种形态
// 全部未被捕获：var m meetingMeta（逐字段赋值，变量名任意）、new(meetingMeta)、
// 内嵌/字段零值、meetingMeta {（类型名与花括号间有空格）。文本针对它们无解
// （变量名/排版可任意变），故新增语法级（go/ast）穷举层：只要「物化了一个 meetingMeta
// 值」就命中，指针类型不算（var p *meetingMeta 只是 nil 指针）。两层合起来仍只允许
// newMeetingMeta 内那一处。
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
	// 第二层：语法级穷举其余物化形态（含字面量本身的排版变体）。
	sites := meetingMetaConstructionSites(t)
	if len(sites) != 1 {
		t.Fatalf("meetingMeta 物化点共 %d 处，应恰好 1 处（newMeetingMeta 内）: %v", len(sites), sites)
	}
	if site := sites[0]; site.file != "meeting_registry.go" || site.fn != "newMeetingMeta" {
		t.Fatalf("meetingMeta 物化点落在 %s，必须只在 meeting_registry.go 的 newMeetingMeta 内", site)
	}
}

// meetingMetaSite 一处 meetingMeta 物化点（字面量 / 零值 / 内嵌）的定位信息。
type meetingMetaSite struct {
	file string
	line int
	fn   string
	what string
}

func (s meetingMetaSite) String() string {
	return fmt.Sprintf("%s:%d %s() %s", s.file, s.line, s.fn, s.what)
}

// meetingMetaMaterialized 判断类型表达式是否物化一个 meetingMeta 值。
// 只沿类型表达式链递归（Ident / *T / []T / map[K]V / chan T）；指针类型不物化对象本身。
func meetingMetaMaterialized(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "meeting"+"Meta"
	case *ast.StarExpr:
		return false
	case *ast.ArrayType:
		return meetingMetaMaterialized(e.Elt)
	case *ast.MapType:
		return meetingMetaMaterialized(e.Key) || meetingMetaMaterialized(e.Value)
	case *ast.ChanType:
		return meetingMetaMaterialized(e.Value)
	}
	return false
}

// meetingMetaNodeKind 把 AST 节点映射为物化形态标签（空串 = 不是物化点）。
func meetingMetaNodeKind(n ast.Node) string {
	switch v := n.(type) {
	case *ast.CompositeLit:
		if meetingMetaMaterialized(v.Type) {
			return "复合字面量"
		}
	case *ast.ValueSpec:
		if meetingMetaMaterialized(v.Type) {
			return "零值声明"
		}
	case *ast.TypeSpec:
		if meetingMetaMaterialized(v.Type) {
			return "自定义类型"
		}
	case *ast.Field:
		if meetingMetaMaterialized(v.Type) {
			return "字段/内嵌零值"
		}
	case *ast.CallExpr:
		return meetingMetaCallKind(v)
	}
	return ""
}

// meetingMetaCallKind new(meetingMeta) 分配与 meetingMeta(x) 类型转换两类旁路。
func meetingMetaCallKind(call *ast.CallExpr) string {
	id, ok := call.Fun.(*ast.Ident)
	if !ok || len(call.Args) != 1 {
		return ""
	}
	isBuiltinNew := id.Name == "new"
	isConversion := id.Name == "meeting"+"Meta"
	if (!isBuiltinNew && !isConversion) || !meetingMetaMaterialized(call.Args[0]) {
		return ""
	}
	if isConversion {
		return "类型转换"
	}
	return "new 分配"
}

// meetingMetaSitesInFile 扫描单个源文件（注释/字符串不参与，排版差异免疫）。
func meetingMetaSitesInFile(t *testing.T, name string) []meetingMetaSite {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", name, err)
	}
	sites := []meetingMetaSite{}
	for _, decl := range file.Decls {
		fn, node := "<package>", ast.Node(decl)
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn, node = fd.Name.Name, fd
		}
		ast.Inspect(node, func(n ast.Node) bool {
			if what := meetingMetaNodeKind(n); what != "" {
				sites = append(sites, meetingMetaSite{
					file: name, line: fset.Position(n.Pos()).Line, fn: fn, what: what,
				})
			}
			return true
		})
	}
	return sites
}

// meetingMetaConstructionSites 语法级穷举 meetingMeta 物化点：复合字面量
// （类型名紧贴花括号 / & 前缀 / 类型名与花括号间留空格）、new 分配、零值声明
// （var m meetingMeta，任意名字）、字段/内嵌零值（struct{ meetingMeta }）、类型转换与
// 自定义类型（type x meetingMeta）。注释与字符串不参与（解析时丢弃），排版差异免疫。
func meetingMetaConstructionSites(t *testing.T) []meetingMetaSite {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sites := []meetingMetaSite{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		sites = append(sites, meetingMetaSitesInFile(t, name)...)
	}
	return sites
}
