package handler

import (
	"sort"
	"sync"
	"time"

	"letshare-server/internal/meeting"

	logrus "github.com/sirupsen/logrus"
)

// meetingMembership is the authoritative identity inside the meeting domain.
// UniqID is the stable product identity; ClientID is the websocket session
// that currently owns this meeting member. Ordinary LetShare room state is
// intentionally not part of this structure.
type meetingMembership struct {
	RoomID   string
	UniqID   string
	UserName string
	ClientID string
	JoinedAt time.Time
	LastSeen time.Time
	Media    meetingMediaState
}

// meetingMediaState belongs to the meeting domain. SFU tracks are transport,
// not authoritative presence or camera/microphone intent.
type meetingMediaState struct {
	Muted         bool   `json:"muted"`
	CameraOn      bool   `json:"cameraOn"`
	ScreenOn      bool   `json:"screenOn"`
	CameraTrackID string `json:"cameraTrackId,omitempty"`
	ScreenTrackID string `json:"screenTrackId,omitempty"`
}

type meetingRoomMembers struct {
	byUniqID map[string]meetingMembership
}

type meetingRegistry struct {
	mu    sync.RWMutex
	rooms map[string]*meetingRoomMembers
}

func newMeetingRegistry() *meetingRegistry {
	return &meetingRegistry{rooms: make(map[string]*meetingRoomMembers)}
}

// join makes meeting membership authoritative for this websocket session.
// A stable user can have only one active session in a meeting; a newer join
// replaces the older owner without consulting ordinary room membership.
func (r *meetingRegistry) join(roomID, uniqID, userName, clientID string) (meetingMembership, []meetingMembership) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	room := r.rooms[roomID]
	if room == nil {
		room = &meetingRoomMembers{byUniqID: make(map[string]meetingMembership)}
		r.rooms[roomID] = room
	}
	previous := make([]meetingMembership, 0, 1)
	if old, ok := room.byUniqID[uniqID]; ok && old.ClientID != clientID {
		previous = append(previous, old)
	}
	media := meetingMediaState{Muted: true}
	if old, ok := room.byUniqID[uniqID]; ok && old.ClientID == clientID {
		media = old.Media
	}
	membership := meetingMembership{RoomID: roomID, UniqID: uniqID, UserName: userName, ClientID: clientID, JoinedAt: now, LastSeen: now, Media: media}
	room.byUniqID[uniqID] = membership
	return membership, previous
}

func (r *meetingRegistry) updateMedia(roomID, uniqID, clientID string, media meetingMediaState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[roomID]
	if room == nil {
		return false
	}
	member, ok := room.byUniqID[uniqID]
	if !ok || member.ClientID != clientID {
		return false
	}
	member.Media = media
	member.LastSeen = time.Now()
	room.byUniqID[uniqID] = member
	return true
}

func (r *meetingRegistry) updateName(roomID, uniqID, clientID, userName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[roomID]
	if room == nil {
		return false
	}
	member, ok := room.byUniqID[uniqID]
	if !ok || member.ClientID != clientID {
		return false
	}
	member.UserName = userName
	member.LastSeen = time.Now()
	room.byUniqID[uniqID] = member
	return true
}

func (r *meetingRegistry) touch(roomID, clientID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[roomID]
	if room == nil {
		return false
	}
	for uniqID, member := range room.byUniqID {
		if member.ClientID == clientID {
			member.LastSeen = time.Now()
			room.byUniqID[uniqID] = member
			return true
		}
	}
	return false
}

func (r *meetingRegistry) owns(roomID, uniqID, clientID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	room := r.rooms[roomID]
	if room == nil {
		return false
	}
	member, ok := room.byUniqID[uniqID]
	return ok && member.ClientID == clientID
}

func (r *meetingRegistry) leave(roomID, uniqID, clientID string) (meetingMembership, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[roomID]
	if room == nil {
		return meetingMembership{}, false
	}
	member, ok := room.byUniqID[uniqID]
	if !ok || member.ClientID != clientID {
		return meetingMembership{}, false
	}
	delete(room.byUniqID, uniqID)
	if len(room.byUniqID) == 0 {
		delete(r.rooms, roomID)
	}
	return member, true
}

func (r *meetingRegistry) leaveClient(clientID string) []meetingMembership {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := make([]meetingMembership, 0, 1)
	for roomID, room := range r.rooms {
		for uniqID, member := range room.byUniqID {
			if member.ClientID != clientID {
				continue
			}
			delete(room.byUniqID, uniqID)
			removed = append(removed, member)
		}
		if len(room.byUniqID) == 0 {
			delete(r.rooms, roomID)
		}
	}
	return removed
}

func (r *meetingRegistry) clear(roomID string) []meetingMembership {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[roomID]
	if room == nil {
		return nil
	}
	removed := make([]meetingMembership, 0, len(room.byUniqID))
	for _, member := range room.byUniqID {
		removed = append(removed, member)
	}
	delete(r.rooms, roomID)
	return removed
}

func (r *meetingRegistry) members(roomID string) []meetingMembership {
	r.mu.RLock()
	defer r.mu.RUnlock()
	room := r.rooms[roomID]
	if room == nil {
		return nil
	}
	members := make([]meetingMembership, 0, len(room.byUniqID))
	for _, member := range room.byUniqID {
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].JoinedAt.Before(members[j].JoinedAt) })
	return members
}

func (r *meetingRegistry) clientForUniqID(roomID, uniqID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	room := r.rooms[roomID]
	if room == nil {
		return "", false
	}
	member, ok := room.byUniqID[uniqID]
	return member.ClientID, ok
}

// meetingHostSeats 一次性快照读取房主席位（Host / HostClientID）。
// 两个字段必须取自同一时刻，否则调用方会拿到「新房主 + 旧会话」的撕裂对。
// hostMu 是叶锁、取完即放；与 meta.mu 的锁序恒为 meta.mu → hostMu，
// 本函数绝不反向取 meta.mu。
func meetingHostSeats(meta *meetingMeta) (string, string) {
	meta.hostMu.Lock()
	defer meta.hostMu.Unlock()
	return meta.Host, meta.HostClientID
}

// meetingUniqIDSet 把 uniqID 列表折叠成集合（与调用点原内联循环逐元素等价）。
func meetingUniqIDSet(uniqIDs []string) map[string]bool {
	set := make(map[string]bool, len(uniqIDs))
	for _, uniqID := range uniqIDs {
		set[uniqID] = true
	}
	return set
}

// meetingMetaInit 是 meetingMeta 的唯一构造入参：随会议类型变化的字段由调用方给出，
// 非零值必需的初始化（life 相位状态机 / minutes 录制同意默认）只在 newMeetingMeta 发生。
type meetingMetaInit struct {
	Host            string
	HostClientID    string
	Title           string
	SourceRoom      string
	Parent          string
	BreakoutMembers map[string]bool
}

// newMeetingMeta meetingMeta 的唯一构造入口（X2 防分叉）：生产与测试同一构造路径，
// 「生产漏字段、测试接缝补齐」类 bug 由 TestMeetingMetaLiteralOnlyExistsInsideTheSharedConstructor 锁死。
func newMeetingMeta(p meetingMetaInit) *meetingMeta {
	return &meetingMeta{
		BreakoutMembers: p.BreakoutMembers,
		Host:            p.Host,
		HostClientID:    p.HostClientID,
		SourceRoom:      p.SourceRoom,
		Title:           p.Title,
		Parent:          p.Parent,
		life:            meeting.NewMachine(),
		minutes:         meetingMinutesState{RequireConsent: true},
	}
}

// ————————————————————————————————————————————————————————————————
// 空置宽限生命周期（V1）：emptyTimer 的唯一武装/停表入口 + 到点策略。
// emptyTimer 字段由 meetingLifecycleMu 保护；相位由 internal/meeting 唯一写（A1/A2）。
// ————————————————————————————————————————————————————————————————

// armEmptyTimerLocked 重新武装空置宽限定时器：总是新建，不复用旧计时器（D2）。
// 调用方必须持有 meetingLifecycleMu。
func (h *WebSocketHandler) armEmptyTimerLocked(roomID string, meta *meetingMeta, ttl time.Duration) {
	h.stopEmptyTimerLocked(meta)
	gen := meta.emptyGen
	meta.emptyTimer = time.AfterFunc(ttl, func() {
		h.onMeetingEmptyTimerFired(roomID, meta, gen)
	})
}

// stopEmptyTimerLocked 停掉空置宽限定时器（成员加入 = PhaseLive 停表）。
// 代次递增同时作废已在飞的陈旧回调。调用方必须持有 meetingLifecycleMu。
func (h *WebSocketHandler) stopEmptyTimerLocked(meta *meetingMeta) {
	meta.emptyGen++
	if meta.emptyTimer != nil {
		meta.emptyTimer.Stop()
		meta.emptyTimer = nil
	}
}

// enterMeetingLive 成员加入的相位跃迁入口（与 enterMeetingGrace 对偶）：在
// meetingLifecycleMu 临界区内复核登记与相位、置 PhaseLive、停空表（C-17：陈旧的宽限
// 回调不得拆掉正在加入的会议），退出锁后落一条相位日志（C-21）；同时把重连的房主
// websocket 会话绑回当前连接（host-only 通道不得继续打到重连前的旧连接）。
// 返回 false 表示会议号已释放 / 已 End / 相位竞争失败，调用方必须按「会议不存在」拒绝。
func (h *WebSocketHandler) enterMeetingLive(roomID, uniqID, clientID, sourceRoom string, meta *meetingMeta) bool {
	h.meetingLifecycleMu.Lock()
	phaseFrom := meta.life.Phase()
	current, registered := h.activeMeetingRooms.Load(roomID)
	if !registered || current != meta || !meta.life.ToLive() {
		h.meetingLifecycleMu.Unlock()
		return false
	}
	h.stopEmptyTimerLocked(meta)
	meta.mu.Lock()
	// A browser reconnect creates a new websocket client session. Keep the
	// host recipient bound to the current meeting connection; otherwise
	// host-only planes (minutes batches, summaries and control messages)
	// continue targeting the stale pre-reconnect client.
	meta.hostMu.Lock()
	if meta.Host == uniqID {
		meta.HostClientID = clientID
	}
	hostIsClient := meta.Host == uniqID
	meta.hostMu.Unlock()
	if meta.SourceRoom == "" && hostIsClient && sourceRoom != "" && h.wsService.RoomHasUniqID(sourceRoom, uniqID) {
		meta.SourceRoom = sourceRoom
	}
	meta.mu.Unlock()
	h.meetingLifecycleMu.Unlock()
	logMeetingPhase(phaseFrom.String(), meeting.PhaseLive.String(), meetingEndReasonMemberJoined, logrus.Fields{"roomID": roomID, "uniqID": uniqID, "clientID": clientID})
	return true
}

// enterMeetingGrace 统一的「在册成员清零 → PhaseGrace + 重装 emptyTimer」入口（D2）。
// 每次进入宽限都重新计时；仍有成员或已 End 则不进入且不落日志，返回 false。
func (h *WebSocketHandler) enterMeetingGrace(roomID, reason string, ctx logrus.Fields) bool {
	h.meetingLifecycleMu.Lock()
	defer h.meetingLifecycleMu.Unlock()
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		return false
	}
	meta, _ := v.(*meetingMeta)
	if meta == nil || len(h.meetings.members(roomID)) > 0 {
		return false
	}
	phaseFrom := meta.life.Phase()
	if !meta.life.ToGrace() {
		return false
	}
	grace := currentMeetingGrace()
	h.armEmptyTimerLocked(roomID, meta, grace)
	fields := logrus.Fields{
		"phase_from": phaseFrom.String(),
		"phase_to":   meeting.PhaseGrace.String(),
		"grace_ms":   grace.Milliseconds(),
	}
	for key, value := range ctx {
		fields[key] = value
	}
	logMeetingEnd(reason, meetingActionScheduleGrace, fields)
	return true
}

// onMeetingEmptyTimerFired 空置宽限定时器到点的唯一策略（D2 / C-19 / DIVERGENCE F10）：
//   - 房间非空（在册成员 / SFU 参与者 / 进行中的 join）→ 重新武装下一次，绝不静默跳过；
//   - 房间仍空 → End：PhaseReserved 按 reservation-expired，否则按 host-left-grace-expired。
func (h *WebSocketHandler) onMeetingEmptyTimerFired(roomID string, meta *meetingMeta, gen uint64) {
	h.meetingLifecycleMu.Lock()
	defer h.meetingLifecycleMu.Unlock()
	current, ok := h.activeMeetingRooms.Load(roomID)
	if !ok || current != meta {
		return // 会议号已释放（End 已执行）：陈旧回调不得再动作
	}
	if gen != meta.emptyGen {
		return // 陈旧回调：本次计时已被新计时器替换/停掉，不得拆除它保护的会议
	}
	phase := meta.life.Phase()
	if phase == meeting.PhaseEnded {
		return
	}
	members := len(h.meetings.members(roomID))
	roomCount := 0
	if room, ok := h.sfuManager.GetRoom(roomID); ok {
		roomCount = room.Count()
	}
	ttl := currentMeetingGrace()
	if phase == meeting.PhaseReserved {
		ttl = currentMeetingReservationTTL()
	}
	if members > 0 || roomCount > 0 || phase == meeting.PhaseLive {
		// F10 现场修复：到点但房间非空 → 重排下一次，而非跳过后让宽限永久失效。
		logMeetingEnd(meetingEndReasonGraceSkipped, meetingActionScheduleGrace, logrus.Fields{
			"roomID":            roomID,
			"grace_expired":     true,
			"room_empty":        false,
			"members_remaining": members,
			"room_count":        roomCount,
			"grace_ms":          ttl.Milliseconds(),
			"phase":             phase.String(),
		})
		h.armEmptyTimerLocked(roomID, meta, ttl)
		return
	}
	reason := meetingEndReasonGraceExpired
	if phase == meeting.PhaseReserved {
		reason = meetingEndReasonReservationExpired
	}
	h.endMeetingLocked(roomID, reason, logrus.Fields{
		"grace_expired": true,
		"room_empty":    true,
		"phase":         phase.String(),
	})
}
