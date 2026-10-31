package handler

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"letshare-server/internal/model"
)

type breakoutRoomView struct {
	Room    string              `json:"room"`
	Name    string              `json:"name"`
	Members []map[string]string `json:"members"`
}

func (h *WebSocketHandler) breakoutParent(roomID string) (string, *meetingMeta) {
	v, ok := h.activeMeetingRooms.Load(roomID)
	meta, valid := v.(*meetingMeta)
	if !ok || !valid || meta == nil {
		return "", nil
	}
	if meta.Parent == "" {
		return roomID, meta
	}
	v, ok = h.activeMeetingRooms.Load(meta.Parent)
	parent, valid := v.(*meetingMeta)
	if !ok || !valid {
		return "", nil
	}
	return meta.Parent, parent
}

func (h *WebSocketHandler) breakoutRooms(parentID string) []breakoutRoomView {
	rooms := make([]breakoutRoomView, 0)
	h.activeMeetingRooms.Range(func(key, value any) bool {
		id, ok := key.(string)
		meta, valid := value.(*meetingMeta)
		if !ok || !valid || meta == nil || !meta.OpenBreakout || meta.Parent != parentID {
			return true
		}
		meta.mu.Lock()
		name := meta.BreakoutName
		meta.mu.Unlock()
		members := make([]map[string]string, 0)
		for _, m := range h.meetings.members(id) {
			members = append(members, map[string]string{"uniqId": m.UniqID, "name": m.UserName})
		}
		rooms = append(rooms, breakoutRoomView{Room: id, Name: name, Members: members})
		return true
	})
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].Room < rooms[j].Room })
	return rooms
}

func (h *WebSocketHandler) broadcastBreakoutRooms(roomID string) {
	h.broadcastBreakoutRoomsSnapshot(roomID, false)
}

func (h *WebSocketHandler) broadcastBreakoutRoomsSnapshot(roomID string, includeEmpty bool) {
	parentID, parent := h.breakoutParent(roomID)
	if parent == nil {
		return
	}
	rooms := h.breakoutRooms(parentID)
	if len(rooms) == 0 && !includeEmpty {
		return
	}
	channels := []string{parentID}
	for _, room := range rooms {
		channels = append(channels, room.Room)
	}
	for _, channel := range channels {
		for _, member := range h.meetings.members(channel) {
			host, _ := meetingHostSeats(parent)
			h.sendMeetingToClient(member.ClientID, channel, model.MessageTypeMeetingBreakout, map[string]interface{}{"action": "rooms", "main": parentID, "hostId": host, "rooms": rooms})
		}
	}
}

func (h *WebSocketHandler) hasActiveBreakoutMembers(parentID string) bool {
	active := false
	h.activeMeetingRooms.Range(func(key, value any) bool {
		id, ok := key.(string)
		meta, valid := value.(*meetingMeta)
		if ok && valid && meta != nil && meta.Parent == parentID && len(h.meetings.members(id)) > 0 {
			active = true
			return false
		}
		return true
	})
	return active
}

func (h *WebSocketHandler) memberInBreakout(parentID, userID string) bool {
	found := false
	h.activeMeetingRooms.Range(func(key, value any) bool {
		id, ok := key.(string)
		meta, valid := value.(*meetingMeta)
		if ok && valid && meta != nil && meta.Parent == parentID {
			if _, present := h.meetings.clientForUniqID(id, userID); present {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// openBreakoutMember finds a specific member, or the earliest member when
// userID is empty. Only self-service child rooms share host authority.
func (h *WebSocketHandler) openBreakoutMember(parentID, userID string) (meetingMembership, bool) {
	var found meetingMembership
	ok := false
	h.activeMeetingRooms.Range(func(key, value any) bool {
		id, isID := key.(string)
		meta, valid := value.(*meetingMeta)
		if !isID || !valid || meta == nil || !meta.OpenBreakout || meta.Parent != parentID {
			return true
		}
		for _, member := range h.meetings.members(id) {
			if userID != "" && member.UniqID != userID {
				continue
			}
			if !ok || member.JoinedAt.Before(found.JoinedAt) {
				found, ok = member, true
			}
		}
		return true
	})
	return found, ok
}

func (h *WebSocketHandler) syncOpenBreakoutHosts(parentID, hostID, clientID string) {
	h.activeMeetingRooms.Range(func(_, value any) bool {
		child, ok := value.(*meetingMeta)
		if ok && child != nil && child.OpenBreakout && child.Parent == parentID {
			child.hostMu.Lock()
			child.Host, child.HostClientID = hostID, clientID
			child.hostMu.Unlock()
		}
		return true
	})
}

func (h *WebSocketHandler) revokeOpenBreakoutAccess(roomID, userID string) {
	parentID, parent := h.breakoutParent(roomID)
	if parent == nil {
		return
	}
	h.activeMeetingRooms.Range(func(_, value any) bool {
		child, ok := value.(*meetingMeta)
		if ok && child != nil && child.OpenBreakout && child.Parent == parentID {
			child.mu.Lock()
			delete(child.BreakoutMembers, userID)
			child.mu.Unlock()
		}
		return true
	})
}

func (h *WebSocketHandler) isAuthorizedBreakoutSwitch(from, to, userID string) bool {
	parentID, parent := h.breakoutParent(from)
	if parent == nil || to == "" || to == from {
		return false
	}
	if to == parentID {
		return from != parentID
	}
	v, ok := h.activeMeetingRooms.Load(to)
	child, valid := v.(*meetingMeta)
	if !ok || !valid || child == nil || child.Parent != parentID {
		return false
	}
	child.mu.Lock()
	allowed := child.BreakoutMembers[userID]
	child.mu.Unlock()
	return allowed
}

// beginBreakoutHostSwitchLocked preserves authority between leave and join.
// A failed switch expires and allows the remaining main-room members to elect
// a host; otherwise a lost connection could strand the original host seat.
func (h *WebSocketHandler) beginBreakoutHostSwitchLocked(roomID, userID string) {
	parentID, parent := h.breakoutParent(roomID)
	if parent == nil {
		return
	}
	host, _ := meetingHostSeats(parent)
	if host != userID {
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	parent.breakoutHostSwitchID = userID
	parent.breakoutHostSwitchDeadline = deadline
	time.AfterFunc(time.Until(deadline), func() {
		h.meetingLifecycleMu.Lock()
		current, exists := h.activeMeetingRooms.Load(parentID)
		if !exists || current != parent || parent.breakoutHostSwitchID != userID || !parent.breakoutHostSwitchDeadline.Equal(deadline) {
			h.meetingLifecycleMu.Unlock()
			return
		}
		parent.breakoutHostSwitchID = ""
		parent.breakoutHostSwitchDeadline = time.Time{}
		_, inMain := h.meetings.clientForUniqID(parentID, userID)
		if !inMain && !h.memberInBreakout(parentID, userID) {
			h.transferMeetingHost(parentID, userID)
		}
		h.meetingLifecycleMu.Unlock()
		h.broadcastBreakoutRooms(parentID)
	})
}

// finishBreakoutHostSwitchLocked clears the transition when the host is
// registered in either the parent meeting or one of its child rooms.
func (h *WebSocketHandler) finishBreakoutHostSwitchLocked(roomID, userID string) {
	_, parent := h.breakoutParent(roomID)
	if parent != nil && parent.breakoutHostSwitchID == userID {
		parent.breakoutHostSwitchID = ""
		parent.breakoutHostSwitchDeadline = time.Time{}
	}
}

// handleOpenBreakout handles the self-service protocol separately from legacy
// assigned-only breakouts. All operations require an existing meeting member.
func (h *WebSocketHandler) handleOpenBreakout(client *model.Client, message *model.WebSocketMessage) bool {
	var req struct {
		Action string `json:"action"`
		Room   string `json:"room"`
		Name   string `json:"name"`
	}
	if json.Unmarshal(message.Data, &req) != nil {
		return false
	}
	switch req.Action {
	case "create-room", "rename-room", "delete-room", "enter-room", "rooms", "recall-rooms":
	default:
		return false
	}
	current := message.Channel
	parentID, parent := h.breakoutParent(current)
	if parent == nil || !h.requireMeetingMember(client, current, model.MessageTypeMeetingBreakout) {
		return true
	}
	if req.Action == "rooms" {
		h.broadcastBreakoutRoomsSnapshot(current, true)
		return true
	}
	if req.Action == "enter-room" {
		if req.Room == parentID && current != parentID {
			h.sendMeetingToClient(client.ID, current, model.MessageTypeMeetingBreakout, map[string]interface{}{"action": "enter", "room": parentID})
			return true
		}
		v, ok := h.activeMeetingRooms.Load(req.Room)
		child, valid := v.(*meetingMeta)
		if !ok || !valid || child == nil || !child.OpenBreakout || child.Parent != parentID || req.Room == current {
			h.sendError(client, 404, "分组房间不存在")
			return true
		}
		child.mu.Lock()
		child.BreakoutMembers[client.UniqID] = true
		child.mu.Unlock()
		h.sendMeetingToClient(client.ID, current, model.MessageTypeMeetingBreakout, map[string]interface{}{"action": "enter", "room": req.Room})
		return true
	}
	host, _ := meetingHostSeats(parent)
	if host != client.UniqID {
		h.sendError(client, 403, "仅主持人可管理分组房间")
		return true
	}
	if req.Action == "create-room" {
		h.meetingLifecycleMu.Lock()
		currentParent, exists := h.activeMeetingRooms.Load(parentID)
		created := false
		if exists && currentParent == parent {
			_, hostClient := meetingHostSeats(parent)
			for i := 1; i <= 99; i++ {
				id := fmt.Sprintf("%sB%d", parentID, i)
				child := newMeetingMeta(meetingMetaInit{Host: host, HostClientID: hostClient, Parent: parentID, OpenBreakout: true, BreakoutMembers: make(map[string]bool)})
				if _, loaded := h.activeMeetingRooms.LoadOrStore(id, child); !loaded {
					created = true
					break
				}
			}
		}
		h.meetingLifecycleMu.Unlock()
		if !created {
			h.sendError(client, 400, "无法创建更多分组房间")
			return true
		}
		h.broadcastBreakoutRooms(current)
		return true
	}
	if req.Action == "recall-rooms" {
		for _, room := range h.breakoutRooms(parentID) {
			for _, member := range h.meetings.members(room.Room) {
				h.sendMeetingToClient(member.ClientID, room.Room, model.MessageTypeMeetingBreakout, map[string]interface{}{"action": "recall", "room": parentID})
			}
		}
		return true
	}
	v, ok := h.activeMeetingRooms.Load(req.Room)
	child, valid := v.(*meetingMeta)
	if !ok || !valid || child == nil || !child.OpenBreakout || child.Parent != parentID {
		h.sendError(client, 404, "分组房间不存在")
		return true
	}
	if req.Action == "rename-room" {
		name := strings.TrimSpace(req.Name)
		if name == "" || len([]rune(name)) > 40 {
			h.sendError(client, 400, "房间名称需为 1 至 40 个字符")
			return true
		}
		child.mu.Lock()
		child.BreakoutName = name
		child.mu.Unlock()
	} else {
		for _, member := range h.meetings.members(req.Room) {
			h.sendMeetingToClient(member.ClientID, req.Room, model.MessageTypeMeetingBreakout, map[string]interface{}{"action": "recall", "room": parentID})
		}
		h.End(req.Room, meetingEndReasonBreakoutRecalled)
	}
	h.broadcastBreakoutRoomsSnapshot(current, true)
	return true
}
