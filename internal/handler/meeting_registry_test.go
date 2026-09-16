package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestMeetingRegistryRebindsByClientSession(t *testing.T) {
	r := newMeetingRegistry()
	first, previous := r.join("1234", "alice", "Alice", "ws-old")
	if first.ClientID != "ws-old" || len(previous) != 0 {
		t.Fatalf("unexpected first join: member=%+v previous=%+v", first, previous)
	}

	second, previous := r.join("1234", "alice", "Alice", "ws-new")
	if second.ClientID != "ws-new" || len(previous) != 1 || previous[0].ClientID != "ws-old" {
		t.Fatalf("rebind did not return the replaced session: member=%+v previous=%+v", second, previous)
	}
	if r.owns("1234", "alice", "ws-old") {
		t.Fatal("old websocket still owns the meeting member")
	}
	if !r.owns("1234", "alice", "ws-new") {
		t.Fatal("new websocket does not own the meeting member")
	}

	if removed := r.leaveClient("ws-old"); len(removed) != 0 {
		t.Fatalf("cleanup of replaced websocket removed active membership: %+v", removed)
	}
	if !r.owns("1234", "alice", "ws-new") {
		t.Fatal("active membership was lost after old websocket cleanup")
	}
}

func TestMeetingRegistryRejectsWrongSessionLeaveAndHeartbeat(t *testing.T) {
	r := newMeetingRegistry()
	r.join("1234", "alice", "Alice", "ws-1")

	if _, ok := r.leave("1234", "alice", "ws-2"); ok {
		t.Fatal("wrong websocket was allowed to leave the meeting")
	}
	if r.touch("1234", "ws-2") {
		t.Fatal("wrong websocket was allowed to refresh the meeting")
	}
	if !r.owns("1234", "alice", "ws-1") {
		t.Fatal("valid meeting ownership was changed by the wrong websocket")
	}
}

func TestMeetingOldWebsocketCleanupCannotRemoveReboundMember(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()

	host := newWSRPC(t, ts.srv, "host:registry")
	oldConn := newWSRPC(t, ts.srv, "guest:registry")
	newConn := newWSRPC(t, ts.srv, "guest:registry")

	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingCreate}); err != nil {
		t.Fatal(err)
	}
	roomID, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	join := func(r *wsRPC) {
		data, _ := json.Marshal(map[string]string{"roomId": roomID})
		if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	join(host)
	join(oldConn)
	join(newConn)

	deadline := time.Now().Add(5 * time.Second)
	for {
		members := ts.handler.meetings.members(roomID)
		if len(members) == 2 && members[1].UniqID == "guest:registry" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rebound meeting membership not established: %+v", members)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The old websocket is still a valid ordinary connection, but it no longer
	// owns the meeting member. Its disconnect cleanup must be a no-op here.
	if err := oldConn.conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		members := ts.handler.meetings.members(roomID)
		room, roomOK := ts.handler.sfuManager.GetRoom(roomID)
		if len(members) == 2 && roomOK && len(meetingParticipantIDs(room)) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("old websocket cleanup corrupted active meeting state: members=%+v room=%v", members, roomOK)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMeetingHeartbeatIsDispatchedByWebsocketHandler(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()

	client := newWSRPC(t, ts.srv, "heartbeat-user")
	if err := client.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingCreate}); err != nil {
		t.Fatal(err)
	}
	roomID, err := client.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	joinData, _ := json.Marshal(map[string]string{"roomId": roomID})
	if err := client.sendJSON(model.WebSocketMessage{
		Type:    model.MessageTypeMeetingJoin,
		Channel: roomID,
		Data:    joinData,
	}); err != nil {
		t.Fatal(err)
	}

	var before time.Time
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		members := ts.handler.meetings.members(roomID)
		if len(members) == 1 {
			before = members[0].LastSeen
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if before.IsZero() {
		t.Fatal("meeting:join did not register the meeting member")
	}

	time.Sleep(10 * time.Millisecond)
	if err := client.sendJSON(model.WebSocketMessage{
		Type:    model.MessageTypeMeetingHeartbeat,
		Channel: roomID,
	}); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		members := ts.handler.meetings.members(roomID)
		if len(members) == 1 && members[0].LastSeen.After(before) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("meeting:heartbeat was not dispatched to the meeting registry")
}

func TestMeetingHostLeaveTransfersHostAndPreservesMeeting(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()

	host := newWSRPC(t, ts.srv, "host:transfer")
	guest := newWSRPC(t, ts.srv, "guest:transfer")
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingCreate}); err != nil {
		t.Fatal(err)
	}
	roomID, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	join := func(client *wsRPC, uniqID string) {
		data, _ := json.Marshal(map[string]string{"roomId": roomID, "userName": uniqID})
		if err := client.sendJSON(model.WebSocketMessage{
			Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: data,
		}); err != nil {
			t.Fatal(err)
		}
	}
	join(host, "Host")
	join(guest, "Guest")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(ts.handler.meetings.members(roomID)) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if members := ts.handler.meetings.members(roomID); len(members) != 2 {
		t.Fatalf("meeting members did not converge before host leave: %+v", members)
	}

	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID}); err != nil {
		t.Fatal(err)
	}

	select {
	case message := <-guest.hostChanged:
		var payload struct {
			HostID string `json:"hostId"`
		}
		if err := json.Unmarshal(message.Data, &payload); err != nil {
			t.Fatalf("invalid host change payload: %v", err)
		}
		if payload.HostID != "guest:transfer" {
			t.Fatalf("host transfer selected %q, want guest:transfer", payload.HostID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest did not receive meeting:host-changed")
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		members := ts.handler.meetings.members(roomID)
		room, roomOK := ts.handler.sfuManager.GetRoom(roomID)
		v, registered := ts.handler.activeMeetingRooms.Load(roomID)
		meta, _ := v.(*meetingMeta)
		if len(members) == 1 && members[0].UniqID == "guest:transfer" && roomOK && registered && meta != nil && meta.Host == "guest:transfer" && room.Count() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if members := ts.handler.meetings.members(roomID); len(members) != 1 || members[0].UniqID != "guest:transfer" {
		t.Fatalf("host leave removed or corrupted the remaining member: %+v", members)
	}
	if _, ok := ts.handler.activeMeetingRooms.Load(roomID); !ok {
		t.Fatal("host leave destroyed a meeting that still had a member")
	}

	select {
	case ended := <-guest.ended:
		t.Fatalf("host leave must not end the meeting: %#v", ended)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestMeetingUnexpectedLastDisconnectHasReconnectGrace(t *testing.T) {
	oldGrace := hostLeaveGrace
	hostLeaveGrace = 500 * time.Millisecond
	t.Cleanup(func() { hostLeaveGrace = oldGrace })

	ts := newMeetingTestServer(t)
	defer ts.Close()

	host := newWSRPC(t, ts.srv, "host:grace")
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingCreate}); err != nil {
		t.Fatal(err)
	}
	roomID, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	joinData, _ := json.Marshal(map[string]string{"roomId": roomID, "userName": "Host"})
	if err := host.sendJSON(model.WebSocketMessage{
		Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: joinData,
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ts.handler.meetings.members(roomID)) != 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if members := ts.handler.meetings.members(roomID); len(members) != 1 {
		t.Fatalf("host did not join meeting: %+v", members)
	}

	// A transport loss is not an explicit meeting:leave. The empty meeting and
	// its room must remain recoverable instead of being destroyed immediately.
	if err := host.conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ts.handler.meetings.members(roomID)) != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if members := ts.handler.meetings.members(roomID); len(members) != 0 {
		t.Fatalf("unexpected disconnect did not remove the stale member: %+v", members)
	}
	if _, ok := ts.handler.activeMeetingRooms.Load(roomID); !ok {
		t.Fatal("unexpected last disconnect destroyed the meeting before the grace period")
	}
	room, roomOK := ts.handler.sfuManager.GetRoom(roomID)
	if !roomOK || room == nil || room.Count() != 0 {
		count := -1
		if room != nil {
			count = room.Count()
		}
		t.Fatalf("unexpected last disconnect did not preserve an empty SFU room: ok=%v count=%d", roomOK, count)
	}

	// Rejoining within the grace period cancels the pending teardown.
	rejoined := newWSRPC(t, ts.srv, "host:grace")
	if err := rejoined.sendJSON(model.WebSocketMessage{
		Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: joinData,
	}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ts.handler.meetings.members(roomID)) != 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if members := ts.handler.meetings.members(roomID); len(members) != 1 {
		t.Fatalf("host did not rejoin during grace period: %+v", members)
	}
	if _, ok := ts.handler.activeMeetingRooms.Load(roomID); !ok {
		t.Fatal("meeting was destroyed even though the host rejoined during grace period")
	}

	// Once the rejoined transport is also lost and no one returns, the grace
	// timer is allowed to finish and release the meeting.
	if err := rejoined.conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ts.handler.activeMeetingRooms.Load(roomID); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("meeting was not destroyed after the reconnect grace period expired")
}

func TestMeetingHostCanTransferAuthorityToCurrentMember(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()

	host := newWSRPC(t, ts.srv, "host:manual")
	guest := newWSRPC(t, ts.srv, "guest:manual")
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingCreate}); err != nil {
		t.Fatal(err)
	}
	roomID, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	join := func(client *wsRPC, userName string) {
		data, _ := json.Marshal(map[string]string{"roomId": roomID, "userName": userName})
		if err := client.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	join(host, "Host")
	join(guest, "Guest")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ts.handler.meetings.members(roomID)) != 2 {
		time.Sleep(10 * time.Millisecond)
	}

	data, _ := json.Marshal(map[string]string{"action": "set", "to": "guest:manual"})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingHost, Channel: roomID, Data: data}); err != nil {
		t.Fatal(err)
	}
	for _, client := range []*wsRPC{host, guest} {
		select {
		case message := <-client.hostChanged:
			var payload struct {
				HostID string `json:"hostId"`
			}
			if err := json.Unmarshal(message.Data, &payload); err != nil || payload.HostID != "guest:manual" {
				t.Fatalf("invalid manual host transfer payload: %#v err=%v", payload, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("manual host transfer was not broadcast to every member")
		}
	}

	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	if message, err := host.waitError(5 * time.Second); err != nil || message == "" {
		t.Fatalf("old host was allowed to end the meeting after transfer: message=%q err=%v", message, err)
	}
	if err := guest.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: roomID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("new host could not end the meeting")
	}
}
