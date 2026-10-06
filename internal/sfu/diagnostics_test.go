package sfu

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

type capturedEntry struct {
	level   log.Level
	message string
	fields  log.Fields
}

type captureHook struct {
	mu      sync.Mutex
	entries []capturedEntry
}

func (h *captureHook) Levels() []log.Level { return log.AllLevels }

func (h *captureHook) Fire(entry *log.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	fields := make(log.Fields, len(entry.Data))
	for key, value := range entry.Data {
		fields[key] = value
	}
	h.entries = append(h.entries, capturedEntry{level: entry.Level, message: entry.Message, fields: fields})
	return nil
}

func (h *captureHook) snapshot() []capturedEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]capturedEntry(nil), h.entries...)
}

// installCaptureHook 采集包级日志；日志级别钉在 Info，与生产 log.level 一致。
func installCaptureHook(t *testing.T) *captureHook {
	t.Helper()
	hook := &captureHook{}
	logger := log.StandardLogger()
	previousOutput := logger.Out
	previousLevel := logger.GetLevel()
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.InfoLevel)
	log.AddHook(hook)
	t.Cleanup(func() {
		logger.ReplaceHooks(make(log.LevelHooks))
		logger.SetOutput(previousOutput)
		logger.SetLevel(previousLevel)
	})
	return hook
}

func candidate(typ webrtc.ICECandidateType, address string) *webrtc.ICECandidate {
	return &webrtc.ICECandidate{Typ: typ, Address: address}
}

func TestCandidatePairFieldsFlagsRelayWhenEitherEndUsesRelay(t *testing.T) {
	host := candidate(webrtc.ICECandidateTypeHost, "10.0.0.1")
	srflx := candidate(webrtc.ICECandidateTypeSrflx, "198.51.100.7")
	relay := candidate(webrtc.ICECandidateTypeRelay, "203.0.113.9")

	cases := []struct {
		name     string
		local    *webrtc.ICECandidate
		remote   *webrtc.ICECandidate
		relayed  bool
		localTyp string
	}{
		{"host<->srflx 不算中继", host, srflx, false, "host"},
		{"local 为 relay 即算中继", relay, host, true, "relay"},
		{"remote 为 relay 即算中继", host, relay, true, "host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields, ok := candidatePairFields(webrtc.NewICECandidatePair(tc.local, tc.remote))
			if !ok {
				t.Fatalf("candidatePairFields 返回 ok=false")
			}
			if fields["relayed"] != tc.relayed {
				t.Fatalf("relayed=%v，期望 %v", fields["relayed"], tc.relayed)
			}
			if fields["local_type"] != tc.localTyp {
				t.Fatalf("local_type=%v，期望 %v", fields["local_type"], tc.localTyp)
			}
			if fields["local_addr"] != tc.local.Address || fields["remote_addr"] != tc.remote.Address {
				t.Fatalf("地址字段与候选不符: local=%v remote=%v", fields["local_addr"], fields["remote_addr"])
			}
		})
	}
}

func TestCandidatePairFieldsRejectsIncompletePair(t *testing.T) {
	for name, pair := range map[string]*webrtc.ICECandidatePair{
		"空 pair":   {},
		"缺 local":  {Remote: candidate(webrtc.ICECandidateTypeHost, "10.0.0.1")},
		"缺 remote": {Local: candidate(webrtc.ICECandidateTypeHost, "10.0.0.1")},
	} {
		if fields, ok := candidatePairFields(pair); ok || fields != nil {
			t.Fatalf("%s: 期望 ok=false 且 fields=nil，得到 ok=%v fields=%v", name, ok, fields)
		}
	}
}

func TestSelectedCandidatePairFieldsToleratesNilPeerConnection(t *testing.T) {
	fields, ok := selectedCandidatePairFields(nil)
	if ok || fields != nil {
		t.Fatalf("nil PeerConnection 应取不到候选对，得到 ok=%v fields=%v", ok, fields)
	}
}

func TestMainConnectionTransitionIsLoggedAtInfo(t *testing.T) {
	hook := installCaptureHook(t)
	p := &Participant{ID: "p1", room: &Room{ID: "room-1"}}
	p.logConnectionStateChange(nil, webrtc.PeerConnectionStateConnected)

	var sawTransition, sawWarn bool
	for _, entry := range hook.snapshot() {
		switch {
		case entry.message == "参与者的主连接状态变化":
			sawTransition = true
			if entry.level != log.InfoLevel {
				t.Fatalf("状态跃迁日志级别为 %v，期望 Info", entry.level)
			}
		case entry.message == "未取到 ICE 选中候选对":
			sawWarn = true
			if entry.level != log.WarnLevel {
				t.Fatalf("候选对缺失应为 Warn，得到 %v", entry.level)
			}
		}
	}
	if !sawTransition {
		t.Fatalf("缺少状态跃迁日志: %#v", hook.snapshot())
	}
	if !sawWarn {
		t.Fatalf("取不到候选对时应记录 warn 并继续: %#v", hook.snapshot())
	}
}

func TestFailedTransitionCarriesReasonPrevStateAndDuration(t *testing.T) {
	hook := installCaptureHook(t)
	p := &Participant{ID: "p1", room: &Room{ID: "room-1"}}
	p.connTrace.advance(webrtc.PeerConnectionStateConnected, time.Now().Add(-3*time.Second))
	p.logConnectionStateChange(nil, webrtc.PeerConnectionStateFailed)

	var fields log.Fields
	for _, entry := range hook.snapshot() {
		if entry.message == "参与者的主连接状态变化" {
			fields = entry.fields
		}
	}
	if fields == nil {
		t.Fatalf("缺少状态跃迁日志: %#v", hook.snapshot())
	}
	if fields["reason"] != "pc-failed" {
		t.Fatalf("reason=%v，期望 pc-failed", fields["reason"])
	}
	if fields["prev_state"] != "connected" {
		t.Fatalf("prev_state=%v，期望 connected", fields["prev_state"])
	}
	if ms, ok := fields["prev_state_duration_ms"].(int64); !ok || ms < 3000 {
		t.Fatalf("prev_state_duration_ms=%v，期望 >=3000", fields["prev_state_duration_ms"])
	}
	if fields["ice_state"] != "unknown" {
		t.Fatalf("ice_state=%v，期望 unknown（nil pc）", fields["ice_state"])
	}
}

func TestDisconnectedTransitionReasonKeyIsSearchable(t *testing.T) {
	hook := installCaptureHook(t)
	p := &Participant{ID: "p1", room: &Room{ID: "room-1"}}
	p.logConnectionStateChange(nil, webrtc.PeerConnectionStateDisconnected)

	for _, entry := range hook.snapshot() {
		if entry.message == "参与者的主连接状态变化" && entry.fields["reason"] == "pc-disconnected" {
			return
		}
	}
	t.Fatalf("disconnected 跃迁缺少 reason 字段: %#v", hook.snapshot())
}

func TestConnStateTraceAdvancesOncePerTransition(t *testing.T) {
	trace := &connStateTrace{}
	now := time.Now()
	trace.advance(webrtc.PeerConnectionStateConnected, now)
	prev, held := trace.advance(webrtc.PeerConnectionStateDisconnected, now.Add(2*time.Second))
	if prev != webrtc.PeerConnectionStateConnected || held < 2*time.Second {
		t.Fatalf("prev=%v held=%v，期望 connected + >=2s", prev, held)
	}
	if strings.TrimSpace(prev.String()) == "" {
		t.Fatalf("prev_state 不得为空串")
	}
}
