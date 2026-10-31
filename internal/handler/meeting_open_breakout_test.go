package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

type breakoutTestMessage struct {
	Action string             `json:"action"`
	Room   string             `json:"room"`
	Rooms  []breakoutRoomView `json:"rooms"`
}

func waitOpenBreakout(t *testing.T, client *wsRPC, match func(breakoutTestMessage) bool) breakoutTestMessage {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case msg := <-client.breakout:
			var data breakoutTestMessage
			if json.Unmarshal(msg.Data, &data) == nil && match(data) {
				return data
			}
		case <-timer.C:
			t.Fatal("timed out waiting for breakout event")
		}
	}
}

func sendOpenBreakout(t *testing.T, client *wsRPC, channel string, data map[string]string) {
	t.Helper()
	payload, _ := json.Marshal(data)
	if err := client.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingBreakout, Channel: channel, Data: payload}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenBreakoutEmptyRoomLateJoinRenameAndRecall(t *testing.T) {
	injectShortMeetingGrace(t)
	ts := newMeetingTestServer(t)
	t.Cleanup(ts.Close)
	main, host := createMeetingOnly(t, ts, "open-host:test")
	joinMeetingMustSucceed(t, ts, host, main, "open-host:test")
	sendOpenBreakout(t, host, main, map[string]string{"action": "create-room"})
	snapshot := waitOpenBreakout(t, host, func(m breakoutTestMessage) bool { return m.Action == "rooms" && len(m.Rooms) == 1 })
	child := snapshot.Rooms[0].Room
	if len(snapshot.Rooms[0].Members) != 0 {
		t.Fatal("new room must be empty")
	}

	outsider := newWSRPC(t, ts.srv, "outsider:open")
	joinData, _ := json.Marshal(map[string]string{"roomId": child})
	if err := outsider.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: child, Data: joinData}); err != nil {
		t.Fatal(err)
	}
	if _, err := outsider.waitError(5 * time.Second); err != nil {
		t.Fatal("direct child join without meeting membership was not rejected")
	}

	member := newWSRPC(t, ts.srv, "late:open")
	joinMeetingMustSucceed(t, ts, member, main, "late:open")
	waitOpenBreakout(t, member, func(m breakoutTestMessage) bool {
		return m.Action == "rooms" && len(m.Rooms) == 1 && m.Rooms[0].Room == child
	})
	sendOpenBreakout(t, member, main, map[string]string{"action": "rename-room", "room": child, "name": "unauthorized"})
	if _, err := member.waitError(5 * time.Second); err != nil {
		t.Fatal("non-host rename must be rejected")
	}
	sendOpenBreakout(t, host, main, map[string]string{"action": "rename-room", "room": child, "name": "设计讨论"})
	waitOpenBreakout(t, member, func(m breakoutTestMessage) bool {
		return m.Action == "rooms" && len(m.Rooms) == 1 && m.Rooms[0].Name == "设计讨论"
	})
	childMeta := loadMeta(t, ts, child)
	childMeta.mu.Lock()
	meetingTitle := childMeta.Title
	childMeta.mu.Unlock()
	if meetingTitle != "" {
		t.Fatal("renaming a breakout must not overwrite its meeting title")
	}
	sendOpenBreakout(t, member, main, map[string]string{"action": "enter-room", "room": child})
	waitOpenBreakout(t, member, func(m breakoutTestMessage) bool { return m.Action == "enter" && m.Room == child })
	leave, _ := json.Marshal(map[string]string{"switchTo": child})
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: main, Data: leave}); err != nil {
		t.Fatal(err)
	}
	joinMeetingMustSucceed(t, ts, member, child, "late:open")
	waitOpenBreakout(t, host, func(m breakoutTestMessage) bool {
		return m.Action == "rooms" && len(m.Rooms) == 1 && len(m.Rooms[0].Members) == 1
	})
	sendOpenBreakout(t, host, main, map[string]string{"action": "recall-rooms"})
	waitOpenBreakout(t, member, func(m breakoutTestMessage) bool { return m.Action == "recall" && m.Room == main })
	back, _ := json.Marshal(map[string]string{"switchTo": main})
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: child, Data: back}); err != nil {
		t.Fatal(err)
	}
	joinMeetingMustSucceed(t, ts, member, main, "late:open")
	time.Sleep(400 * time.Millisecond)
	if _, ok := ts.handler.activeMeetingRooms.Load(child); !ok {
		t.Fatal("empty open room must survive its grace interval")
	}
	assertNoServerPanics(t, ts)
}

func TestOpenBreakoutPromotesRemainingChildMemberWhenHostLeaves(t *testing.T) {
	ts := newMeetingTestServer(t)
	t.Cleanup(ts.Close)
	main, host := createMeetingOnly(t, ts, "departing-host:test")
	joinMeetingMustSucceed(t, ts, host, main, "departing-host:test")
	member := newWSRPC(t, ts.srv, "remaining-member:test")
	joinMeetingMustSucceed(t, ts, member, main, "remaining-member:test")
	sendOpenBreakout(t, host, main, map[string]string{"action": "create-room"})
	child := waitOpenBreakout(t, host, func(m breakoutTestMessage) bool { return m.Action == "rooms" && len(m.Rooms) == 1 }).Rooms[0].Room
	for _, entry := range []struct {
		client *wsRPC
		id     string
	}{{member, "remaining-member:test"}, {host, "departing-host:test"}} {
		sendOpenBreakout(t, entry.client, main, map[string]string{"action": "enter-room", "room": child})
		waitOpenBreakout(t, entry.client, func(m breakoutTestMessage) bool { return m.Action == "enter" && m.Room == child })
		leave, _ := json.Marshal(map[string]string{"switchTo": child})
		if err := entry.client.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: main, Data: leave}); err != nil {
			t.Fatal(err)
		}
		joinMeetingMustSucceed(t, ts, entry.client, child, entry.id)
	}
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: child, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ts.handler.meetingHostUniqID(main) == "remaining-member:test" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := ts.handler.meetingHostUniqID(main); got != "remaining-member:test" {
		t.Fatalf("remaining child member did not become host: %s", got)
	}
	if got := ts.handler.meetingHostUniqID(child); got != "remaining-member:test" {
		t.Fatalf("child authority differs from parent: %s", got)
	}
	sendOpenBreakout(t, member, child, map[string]string{"action": "rename-room", "room": child, "name": "New host"})
	waitOpenBreakout(t, member, func(m breakoutTestMessage) bool {
		return m.Action == "rooms" && len(m.Rooms) == 1 && m.Rooms[0].Name == "New host"
	})
}

func TestOpenBreakoutHostKeepsAuthorityAndCanEndMeetingFromChild(t *testing.T) {
	ts := newMeetingTestServer(t)
	t.Cleanup(ts.Close)
	main, host := createMeetingOnly(t, ts, "switch-host:test")
	joinMeetingMustSucceed(t, ts, host, main, "switch-host:test")
	member := newWSRPC(t, ts.srv, "switch-member:test")
	joinMeetingMustSucceed(t, ts, member, main, "switch-member:test")
	sendOpenBreakout(t, host, main, map[string]string{"action": "create-room"})
	snapshot := waitOpenBreakout(t, host, func(m breakoutTestMessage) bool { return m.Action == "rooms" && len(m.Rooms) == 1 })
	child := snapshot.Rooms[0].Room
	sendOpenBreakout(t, host, main, map[string]string{"action": "enter-room", "room": child})
	waitOpenBreakout(t, host, func(m breakoutTestMessage) bool { return m.Action == "enter" && m.Room == child })
	leave, _ := json.Marshal(map[string]string{"switchTo": child})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: main, Data: leave}); err != nil {
		t.Fatal(err)
	}
	waitMeetingMembers(t, ts, main, 1)
	late := newWSRPC(t, ts.srv, "during-switch:test")
	joinMeetingMustSucceed(t, ts, late, main, "during-switch:test")
	if got := ts.handler.meetingHostUniqID(main); got != "switch-host:test" {
		t.Fatalf("host changed while room switch was in flight: %s", got)
	}
	joinMeetingMustSucceed(t, ts, host, child, "switch-host:test")
	if got := ts.handler.meetingHostUniqID(main); got != "switch-host:test" {
		t.Fatalf("host changed during room switch: %s", got)
	}
	sendOpenBreakout(t, host, child, map[string]string{"action": "rename-room", "room": child, "name": "Host room"})
	waitOpenBreakout(t, member, func(m breakoutTestMessage) bool {
		return m.Action == "rooms" && len(m.Rooms) == 1 && m.Rooms[0].Name == "Host room"
	})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingEnd, Channel: child, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, parentExists := ts.handler.activeMeetingRooms.Load(main)
		_, childExists := ts.handler.activeMeetingRooms.Load(child)
		if !parentExists && !childExists {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ending from child did not close the parent and child")
}
