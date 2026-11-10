package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestMeetingMinutesSubscriptionIsPerMemberAndResumeReplaysCurrentEpoch(t *testing.T) {
	ts, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)

	sendMixedSegment(host.handler, roomID, "synthetic first segment", recordingID)
	for _, recipient := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(recipient, "segments", 3*time.Second); err != nil {
			t.Fatal("enabled member did not receive first segment")
		}
	}
	coordinator := currentMinutesCoordinatorRPC(t, host.handler, roomID, host, member)
	publishCurrentMinutesSummary(t, coordinator, roomID, "shared summary one", 1)
	for _, recipient := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(recipient, "summary", 3*time.Second); err != nil {
			t.Fatal("enabled member did not receive shared summary")
		}
	}

	sendMinutes(t, member, roomID, map[string]interface{}{
		"action": "receive", "enabled": false, "recordingId": recordingID + "-stale",
	})
	ack, err := waitMinutesKind(member, "subscription", 3*time.Second)
	if err != nil {
		t.Fatal("personal pause was not acknowledged")
	}
	ackPayload := minutesPayload(t, ack)
	if ackPayload["minutesSubscription"].(map[string]interface{})["enabled"] != false {
		t.Fatal("personal pause did not disable this member")
	}
	if minutes := ackPayload["minutes"].(map[string]interface{}); minutes["summary"] != nil && minutes["summary"] != "" {
		t.Fatal("paused member received summary content in its status")
	}
	if _, err := waitMinutesKind(host, "coordinator", 200*time.Millisecond); err == nil {
		// The incumbent host remains eligible, so pausing the non-coordinator
		// must not trigger an unnecessary coordinator change.
		t.Fatal("pausing a non-coordinator changed the current coordinator")
	}

	sendMixedSegment(host.handler, roomID, "synthetic second segment", recordingID)
	if _, err := waitMinutesKind(host, "segments", 3*time.Second); err != nil {
		t.Fatal("remaining subscriber did not receive the next segment")
	}
	assertNoMinutesKind(t, member, "segments", 200*time.Millisecond)
	publishCurrentMinutesSummary(t, host, roomID, "shared summary two", 2)
	if _, err := waitMinutesKind(host, "summary", 3*time.Second); err != nil {
		t.Fatal("coordinator could not publish the next summary")
	}
	assertNoMinutesKind(t, member, "summary", 200*time.Millisecond)

	sendMinutes(t, member, roomID, map[string]interface{}{
		"action": "replay", "recordingId": recordingID, "afterSeq": 0,
	})
	if message, err := member.waitError(3 * time.Second); err != nil || message == "" {
		t.Fatal("paused member replay request was not explicitly rejected")
	}
	assertNoMinutesKind(t, member, "segments", 200*time.Millisecond)

	// Rebinding the same stable identity must preserve the disabled preference.
	_ = member.conn.Close()
	waitMembersRegistered(t, ts, roomID, 1)
	reconnected := newWSRPC(t, ts.srv, "bob:chat-2")
	reconnected.handler = ts.handler
	joinData, _ := json.Marshal(map[string]interface{}{"roomId": roomID})
	if err := reconnected.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: joinData}); err != nil {
		t.Fatal(err)
	}
	waitMembersRegistered(t, ts, roomID, 2)
	info, err := waitMeetingWireEvent(reconnected, 3*time.Second)
	if err != nil || info.Type != model.MessageTypeMeetingInfo {
		t.Fatal("reconnected member did not receive meeting info first")
	}
	infoPayload := minutesPayload(t, info)
	if infoPayload["minutesSubscription"].(map[string]interface{})["enabled"] != false {
		t.Fatal("receive preference did not survive reconnect")
	}
	if minutes := infoPayload["minutes"].(map[string]interface{}); minutes["summary"] != nil && minutes["summary"] != "" {
		t.Fatal("disabled reconnect received shared summary content")
	}
	assertNoMinutesKind(t, reconnected, "segments", 200*time.Millisecond)
	assertNoMinutesKind(t, reconnected, "summary", 200*time.Millisecond)

	sendMinutes(t, reconnected, roomID, map[string]interface{}{
		"action": "receive", "enabled": true, "recordingId": recordingID, "afterSeq": 1,
	})
	resumeAck, err := waitMinutesKind(reconnected, "subscription", 3*time.Second)
	if err != nil || minutesPayload(t, resumeAck)["minutesSubscription"].(map[string]interface{})["enabled"] != true {
		t.Fatal("re-enabled member did not receive a positive subscription ack")
	}
	stateFrame, err := waitMinutesKind(reconnected, "replay-state", 3*time.Second)
	if err != nil {
		t.Fatal("resume did not send the current shared summary state")
	}
	state := minutesPayload(t, stateFrame)["minutes"].(map[string]interface{})
	if state["summary"] != "shared summary two" || state["summaryThroughSeq"] != float64(2) {
		t.Fatalf("resume state did not include the latest shared summary: %#v", state)
	}
	replay, err := waitMinutesKind(reconnected, "segments", 3*time.Second)
	if err != nil {
		t.Fatal("resume did not replay retained transcript")
	}
	batch := segmentsFromBatch(t, replay)
	if len(batch) != 1 || segmentSeq(t, batch[0]) != 2 || batch[0]["recordingId"] != recordingID {
		t.Fatalf("resume replay did not honor the sequence cursor: %#v", batch)
	}
}

func TestMeetingMinutesStaleDeliveriesAreDroppedAfterRoomIDReuse(t *testing.T) {
	ts, host, member, _, roomID := meetingChatFlow(t)
	oldMeta, ok := ts.handler.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	members := ts.handler.meetings.members(roomID)
	if len(members) == 0 {
		t.Fatal("meeting membership is missing")
	}
	owner := members[0]
	for _, client := range []*wsRPC{host, member} {
		drainMinutes(client)
		for {
			select {
			case <-client.info:
			default:
				goto drained
			}
		}
	drained:
	}

	oldRecordingID := "old-recording-epoch"
	oldMeta.mu.Lock()
	oldMeta.minutes.Running = true
	oldMeta.minutes.RecordingID = oldRecordingID
	oldMeta.minutes.LatestSegmentSeq = 1
	oldMeta.minutes.SummaryThroughSeq = 1
	oldMeta.minutes.Summary = "old epoch summary"
	oldMeta.minutesSegments = []meetingMinutesSegment{{
		Seq: 1, RecordingID: oldRecordingID, SegmentID: "old-segment", Text: "synthetic old segment", Final: true,
	}}
	oldMeta.mu.Unlock()

	// Model the exact reuse boundary: teardown removes the active pointer and
	// registry while holding the delivery gate; the same room/member keys are
	// then installed for a fresh metadata object.
	ts.handler.meetingLifecycleMu.Lock()
	ts.handler.endMeetingLocked(roomID, "test-room-id-reuse", nil)
	newMeta := newMeetingMeta(meetingMetaInit{Host: owner.UniqID, HostClientID: owner.ClientID})
	newMeta.mu.Lock()
	newMeta.minutes.Running = true
	newMeta.minutes.RecordingID = "new-recording-epoch"
	newMeta.minutes.LatestSegmentSeq = 1
	newMeta.minutes.Summary = "new epoch summary"
	newMeta.mu.Unlock()
	ts.handler.activeMeetingRooms.Store(roomID, newMeta)
	ts.handler.meetings.join(roomID, owner.UniqID, owner.UserName, owner.ClientID)
	ts.handler.meetingLifecycleMu.Unlock()

	ts.handler.sendMeetingMinutesToEnabledMembers(roomID, oldMeta, map[string]interface{}{
		"kind": "segments", "recordingId": oldRecordingID,
		"segments": []meetingMinutesSegment{{Seq: 1, RecordingID: oldRecordingID, Text: "synthetic old segment"}},
	})
	ts.handler.sendMeetingMinutesToEnabledMembers(roomID, oldMeta, map[string]interface{}{
		"kind": "summary", "recordingId": oldRecordingID, "summary": "old epoch summary",
	})
	ts.handler.sendMeetingInfo(roomID, owner.UniqID, oldMeta)
	ts.handler.sendMeetingMinutesReplay(roomID, owner.ClientID, oldMeta, 0, true)

	client := host
	if owner.UniqID == member.userID {
		client = member
	}
	assertNoMinutesKind(t, client, "segments", 200*time.Millisecond)
	assertNoMinutesKind(t, client, "summary", 200*time.Millisecond)
	assertNoMinutesKind(t, client, "replay-state", 200*time.Millisecond)
	select {
	case <-client.info:
		t.Fatal("stale meeting info was delivered into the reused room")
	case <-time.After(200 * time.Millisecond):
	}
	ts.handler.End(roomID, "test-room-id-reuse-cleanup")
}
