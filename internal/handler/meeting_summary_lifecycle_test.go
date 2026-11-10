package handler

import (
	"testing"
	"time"
)

func TestMeetingMinutes_PeriodicSummaryAndPersonalPauseDoNotStopCapture(t *testing.T) {
	_, host, member, _, room := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, room, true, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, room)
	sendMixedSegment(host.handler, room, "synthetic first segment", recordingID)
	for _, recipient := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(recipient, "segments", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	sendMinutes(t, member, room, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(member, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	publishCurrentMinutesSummary(t, currentMinutesCoordinatorRPC(t, host.handler, room, host, member), room, "ongoing", 1)
	message, err := waitMinutesKind(member, "summary", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload := minutesPayload(t, message)
	minutes, ok := payload["minutes"].(map[string]interface{})
	if !ok || minutes["running"] != true {
		t.Fatalf("periodic summary stopped recording: %#v", payload)
	}
	sendMinutes(t, member, room, map[string]interface{}{"action": "segment", "segmentId": "after-summary", "text": "after summary", "final": true})
	if _, err := waitMinutesKind(host, "segments", 3*time.Second); err != nil {
		t.Fatalf("capture stopped after summary: %v", err)
	}
	sendMinutes(t, host, room, map[string]interface{}{"action": "stop"})
	if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMixedSegment(host.handler, room, "synthetic second segment", recordingID)
	if _, err := waitMinutesKind(member, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	assertNoMinutesKind(t, host, "segments", 200*time.Millisecond)
	publishCurrentMinutesSummary(t, member, room, "final", 2)
	final, err := waitMinutesKind(member, "summary", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	finalMinutes, _ := minutesPayload(t, final)["minutes"].(map[string]interface{})
	if finalMinutes["running"] != true {
		t.Fatalf("personal pause stopped the shared recording: %#v", finalMinutes)
	}
}
