package meeting

import "testing"

// TestMachineFollowsReservedLiveGraceEndedTransitions 锁死 §4.6 相位跃迁表：
// Reserved → Live → Grace → Live → End，任何相位都可 End。
func TestMachineFollowsReservedLiveGraceEndedTransitions(t *testing.T) {
	m := NewMachine()
	if got := m.Phase(); got != PhaseReserved {
		t.Fatalf("new machine phase = %v, want reserved", got)
	}
	if !m.ToLive() || m.Phase() != PhaseLive {
		t.Fatalf("reserved -> live failed: %v", m.Phase())
	}
	if !m.ToGrace() || m.Phase() != PhaseGrace {
		t.Fatalf("live -> grace failed: %v", m.Phase())
	}
	if !m.ToLive() || m.Phase() != PhaseLive {
		t.Fatalf("grace -> live (rejoin) failed: %v", m.Phase())
	}
	if !m.End() || m.Phase() != PhaseEnded {
		t.Fatalf("live -> ended failed: %v", m.Phase())
	}
}

// TestMachineEndIsIdempotentSoSecondCallHasNoSecondSideEffect 锁死 C-18 的机制层闸门：
// End 只有第一次返回 true，重复调用不得再触发任何拆除副作用。
func TestMachineEndIsIdempotentSoSecondCallHasNoSecondSideEffect(t *testing.T) {
	m := NewMachine()
	if !m.End() {
		t.Fatal("first End must claim the transition")
	}
	if m.End() {
		t.Fatal("second End must not claim a second transition (C-18)")
	}
	if m.Phase() != PhaseEnded {
		t.Fatalf("phase = %v, want ended", m.Phase())
	}
}

// TestMachineRejectsJoinAfterEndSoReleasedMeetingCannotBeRevived 锁死竞争裁决：
// 拆除赢了竞争后 join 必须被拒（ToLive=false），已释放的会议号不得复活。
func TestMachineRejectsJoinAfterEndSoReleasedMeetingCannotBeRevived(t *testing.T) {
	m := NewMachine()
	if !m.End() {
		t.Fatal("End must succeed")
	}
	if m.ToLive() {
		t.Fatal("ToLive after End must fail so join cannot revive an ended meeting")
	}
	if m.ToGrace() {
		t.Fatal("ToGrace after End must fail")
	}
	if m.Phase() != PhaseEnded {
		t.Fatalf("phase = %v, want ended", m.Phase())
	}
}

// TestPhaseNamesAreStableLogVocabulary 锁死相位的日志词汇（小写稳定名），
// 日志检索键不得漂移。
func TestPhaseNamesAreStableLogVocabulary(t *testing.T) {
	want := map[Phase]string{
		PhaseReserved: "reserved",
		PhaseLive:     "live",
		PhaseGrace:    "grace",
		PhaseEnded:    "ended",
		Phase(99):     "unknown",
	}
	for phase, name := range want {
		if got := phase.String(); got != name {
			t.Fatalf("Phase(%d).String() = %q, want %q", phase, got, name)
		}
	}
}
