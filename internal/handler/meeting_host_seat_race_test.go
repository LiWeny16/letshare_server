package handler

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// G2 契约门回归 · T1（D1 固化）：房主席位（Host / HostClientID）并发读写。
//
// 被保护的不变量：两字段必须同刻一致 —— 读侧只能走 meetingHostSeats 一次性快照，
// 任何时刻读到的必须是同一瞬间写入的一对（Host=host:<tag> ⇔ HostClientID=client:<tag>），
// 绝不允许出现「新房主 + 旧会话」的撕裂对。
//
// 纯 registry + hostMu，不起 WebSocket：不触 service 层既有竞态，可独立 -race 运行。
// A/B 双向可证（一次性探针，原始输出见回报）：
//   A 修复形态：读侧走 meetingHostSeats → go test -race 干净、0 撕裂读；
//   B 还原形态：把 meetingHostSeats 读侧还原为裸读 meta.Host → -race 必报 DATA RACE。

// hostSeatWriter 与生产 setMeetingHost / enterMeetingLive 同构：持 hostMu 成对写两字段；
// 同频在 registry 上 join/leave（registry 本身也要过 -race）。
func hostSeatWriter(meta *meetingMeta, registry *meetingRegistry, rounds int) {
	for i := 0; i < rounds; i++ {
		tag := fmt.Sprintf("seat-%d", i)
		meta.hostMu.Lock()
		meta.Host, meta.HostClientID = "host:"+tag, "client:"+tag
		meta.hostMu.Unlock()
		registry.join("race-room", tag, tag, "client:"+tag)
		registry.leave("race-room", tag, "client:"+tag)
	}
}

// hostSeatReader 走 meetingHostSeats 一次性快照读房主席位，逐次校验两字段配对一致；
// 撕裂读计入 torn（修复形态下必须恒为 0）。
func hostSeatReader(meta *meetingMeta, registry *meetingRegistry, rounds int, torn *atomic.Int64) {
	for i := 0; i < rounds; i++ {
		host, hostClient := meetingHostSeats(meta)
		if strings.TrimPrefix(host, "host:") != strings.TrimPrefix(hostClient, "client:") {
			torn.Add(1)
		}
		registry.members("race-room")
	}
}

// TestMeetingHostSeatConcurrentAccessKeepsHostSeatFieldsConsistent 不变量：
// 并发写入下房主席位两字段（Host / HostClientID）必须同刻一致（撕裂读恒为 0）。
func TestMeetingHostSeatConcurrentAccessKeepsHostSeatFieldsConsistent(t *testing.T) {
	const rounds = 20000
	meta := newMeetingMeta(meetingMetaInit{})
	registry := newMeetingRegistry()
	var torn atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); hostSeatWriter(meta, registry, rounds) }()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() { defer wg.Done(); hostSeatReader(meta, registry, rounds, &torn) }()
	}
	wg.Wait()
	if n := torn.Load(); n > 0 {
		t.Fatalf("房主席位撕裂读 %d 次：Host/HostClientID 必须同刻一致", n)
	}
	host, hostClient := meetingHostSeats(meta)
	if strings.TrimPrefix(host, "host:") != strings.TrimPrefix(hostClient, "client:") {
		t.Fatalf("最终房主席位仍是撕裂对: %s / %s", host, hostClient)
	}
}
