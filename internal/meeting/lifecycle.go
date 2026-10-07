// Package meeting 是会议号生命周期的唯一属主（ARCHITECTURE.md §3 / §4.6 属主表：
// "会议号生命周期 ≠ 房间占用" 的唯一写者）。
//
// 本包只持有相位状态机（PhaseReserved → PhaseLive → PhaseGrace → PhaseEnded），
// 不持有任何策略与副作用：何时 End、宽限 TTL 多长、拆除时要广播/关房/释放会议号，
// 全部由调用方（internal/handler）决定。机制与策略分离（A6）。
//
// 空置宽限定时器（emptyTimer）本体在 handler 的 meetingMeta 上（一个概念一个计时器，
// DIVERGENCE.md F10 的结构性修复）；本包只保证"相位只有一个写者"（A1/A2），
// 所有跃迁都经由 Machine 的方法并受其内部锁保护。
package meeting

import "sync"

// Phase 会议号生命周期相位（ARCHITECTURE.md §4.6）。
type Phase int

const (
	// PhaseReserved 已登记会议号，尚无 SFU 房间（含"创建但从未 join"）。
	PhaseReserved Phase = iota
	// PhaseLive 有在册成员（或成员正在加入）。
	PhaseLive
	// PhaseGrace 在册成员为 0，emptyTimer 计时中；会议号仍被占用，重入必须成功（C-17）。
	PhaseGrace
	// PhaseEnded 已 End，会议号释放。
	PhaseEnded
)

// String 供日志与断言使用的稳定小写名。
func (p Phase) String() string {
	switch p {
	case PhaseReserved:
		return "reserved"
	case PhaseLive:
		return "live"
	case PhaseGrace:
		return "grace"
	case PhaseEnded:
		return "ended"
	default:
		return "unknown"
	}
}

// Machine 单个会议号的相位状态机。零值不合法，必须用 NewMachine 创建。
// 所有方法并发安全；返回 false 表示跃迁被拒绝（已 PhaseEnded）。
type Machine struct {
	mu    sync.Mutex
	phase Phase
}

// NewMachine 创建一个 PhaseReserved 状态的会议号状态机。
func NewMachine() *Machine {
	return &Machine{phase: PhaseReserved}
}

// Phase 返回当前相位快照。
func (m *Machine) Phase() Phase {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phase
}

// ToLive 跃迁到 PhaseLive（首个成员 join）。已 PhaseEnded 时返回 false ——
// 拆除赢得了竞争，join 必须按"会议不存在"拒绝，不得复活已释放的会议号（C-17/C-18）。
func (m *Machine) ToLive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.phase == PhaseEnded {
		return false
	}
	m.phase = PhaseLive
	return true
}

// ToGrace 跃迁到 PhaseGrace（在册成员清零，无论显式离开还是断线）。
// 允许 Grace→Grace（再次清零时调用方重新计时）。已 PhaseEnded 时返回 false。
func (m *Machine) ToGrace() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.phase == PhaseEnded {
		return false
	}
	m.phase = PhaseGrace
	return true
}

// End 唯一终止跃迁（C-18 幂等闸门）：任意相位 → PhaseEnded。
// 只有第一次调用返回 true，后续调用返回 false 且不得产生第二次副作用。
func (m *Machine) End() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.phase == PhaseEnded {
		return false
	}
	m.phase = PhaseEnded
	return true
}
