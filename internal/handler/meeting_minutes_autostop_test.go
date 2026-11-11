package handler

import (
	"testing"
	"time"
)

// assertMinutesStoppedState checks the server-side minutes state after an
// auto stop: not running, coordinator cleared, recording epoch retained.
func assertMinutesStoppedState(t *testing.T, h *WebSocketHandler, roomID, wantRecordingID string) {
	t.Helper()
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	coordinator := meta.minutes.SummaryCoordinatorID
	recordingID := meta.minutes.RecordingID
	meta.mu.Unlock()
	if running {
		t.Fatal("minutes are still running after every member paused")
	}
	if coordinator != "" {
		t.Fatalf("auto stop retained coordinator %q", coordinator)
	}
	if recordingID != wantRecordingID || recordingID == "" {
		t.Fatalf("auto stop did not retain the recording epoch: got %q want %q", recordingID, wantRecordingID)
	}
}

func TestMeetingMinutesStopsWhenEveryMemberPauses(t *testing.T) {
	_, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)

	sendMinutes(t, host, roomID, map[string]interface{}{"action": "receive", "enabled": false})
	if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
		t.Fatal("host pause was not acknowledged")
	}
	// One paused member must not end a shared session: the coordinator hand
	// over to the still-receiving member is a no-op state change for the
	// session itself.
	assertNoMinutesKind(t, member, "stopped", 300*time.Millisecond)
	meta, ok := host.handler.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	meta.mu.Unlock()
	if !running {
		t.Fatal("single pause stopped shared minutes")
	}

	sendMinutes(t, member, roomID, map[string]interface{}{"action": "receive", "enabled": false})
	if _, err := waitMinutesKind(member, "subscription", 3*time.Second); err != nil {
		t.Fatal("member pause was not acknowledged")
	}
	for _, r := range []*wsRPC{host, member} {
		stopped, err := waitMinutesKind(r, "stopped", 3*time.Second)
		if err != nil {
			t.Fatalf("%s did not receive stopped: %v", r.userID, err)
		}
		state := minutesMapFromFrame(t, stopped)
		if state["running"] != false {
			t.Fatalf("stopped frame still reports running: %#v", state)
		}
		if state["summaryCoordinatorId"] != "" {
			t.Fatalf("stopped frame retained a coordinator: %#v", state)
		}
		if state["recordingId"] != recordingID {
			t.Fatalf("stopped frame lost the recording epoch: %#v", state)
		}
	}
	assertMinutesStoppedState(t, host.handler, roomID, recordingID)
	if _, ok := host.handler.meetingAudioSessions.Load(roomID); ok {
		t.Fatal("auto stop left a room audio session registered")
	}
}

func TestMeetingMinutesKeepsRunningWhileAnyMemberReceives(t *testing.T) {
	_, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)

	sendMinutes(t, host, roomID, map[string]interface{}{"action": "receive", "enabled": false})
	if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
		t.Fatal("host pause was not acknowledged")
	}
	assertNoMinutesKind(t, member, "stopped", 300*time.Millisecond)
	meta, ok := host.handler.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	meta.mu.Unlock()
	if !running {
		t.Fatal("minutes stopped while a member still receives")
	}
}

func TestMeetingMinutesStopsWhenLastEnabledMemberLeaves(t *testing.T) {
	ts, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)

	sendMinutes(t, host, roomID, map[string]interface{}{"action": "receive", "enabled": false})
	if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
		t.Fatal("host pause was not acknowledged")
	}
	assertNoMinutesKind(t, host, "stopped", 300*time.Millisecond)

	leaveMeeting(t, member, roomID)
	waitMembersRegistered(t, ts, roomID, 1)
	stopped, err := waitMinutesKind(host, "stopped", 3*time.Second)
	if err != nil {
		t.Fatalf("paused host did not receive stopped after the last receiver left: %v", err)
	}
	state := minutesMapFromFrame(t, stopped)
	if state["running"] != false {
		t.Fatalf("stopped frame still reports running: %#v", state)
	}
	assertMinutesStoppedState(t, host.handler, roomID, recordingID)
}

func TestMeetingMinutesAutoStopRemovesServerMixer(t *testing.T) {
	ts, host, member, _, roomID := meetingChatFlowWithSetup(t, func(h *WebSocketHandler) {
		h.SetMeetingASRStreamService(NewMeetingASRStreamService(MeetingASRStreamConfig{
			HTTPBase: "https://somni.test", WSBase: "wss://somni.test/stream",
			Email: "test@example.test", Password: "test-only",
		}))
		h.SetMeetingAIService(NewMeetingAIService("test-only", "https://gemini.test/v1", true, true, nil))
	})
	configureAndStartMinutesWithReadyServices(t, host, roomID, host, member)
	if _, ok := ts.handler.meetingAudioSessions.Load(roomID); !ok {
		t.Fatal("ready provider start did not install the server mixer")
	}
	recordingID := currentMinutesRecordingID(t, ts.handler, roomID)

	for _, r := range []*wsRPC{host, member} {
		sendMinutes(t, r, roomID, map[string]interface{}{"action": "receive", "enabled": false})
		if _, err := waitMinutesKind(r, "subscription", 3*time.Second); err != nil {
			t.Fatalf("%s pause was not acknowledged: %v", r.userID, err)
		}
	}
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "stopped", 3*time.Second); err != nil {
			t.Fatalf("%s did not receive stopped after every member paused: %v", r.userID, err)
		}
	}
	assertMinutesStoppedState(t, ts.handler, roomID, recordingID)
	if _, ok := ts.handler.meetingAudioSessions.Load(roomID); ok {
		t.Fatal("auto stop left the room audio mixer registered")
	}
}
