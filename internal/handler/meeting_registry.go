package handler

import (
	"sort"
	"sync"
	"time"
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
