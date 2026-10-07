package handler

import (
	"sort"
	"sync"
	"time"

	"letshare-server/internal/meeting"
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

// 空置宽限生命周期（emptyTimer 武装/停表、enterMeetingLive / enterMeetingGrace
// 相位跃迁、到点策略）已下沉到 meeting_phase_lifecycle.go：本文件行数硬门 400 行
// 零余量，A1 房主权威修复必须先腾出空间（C.2）。
