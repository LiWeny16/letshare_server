package handler

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func waitMinutes(r *wsRPC, timeout time.Duration) (model.WebSocketMessage, error) {
	select {
	case message := <-r.minutes:
		return message, nil
	case <-time.After(timeout):
		return model.WebSocketMessage{}, fmt.Errorf("wait %s timeout", "meeting:minutes")
	}
}

func waitMinutesKind(r *wsRPC, kind string, timeout time.Duration) (model.WebSocketMessage, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		message, err := waitMinutes(r, time.Until(deadline))
		if err != nil {
			return model.WebSocketMessage{}, err
		}
		if minutesPayloadKind(message) == kind {
			return message, nil
		}
	}
	return model.WebSocketMessage{}, fmt.Errorf("wait meeting:minutes kind=%s timeout", kind)
}

func minutesPayloadKind(message model.WebSocketMessage) string {
	var payload map[string]interface{}
	_ = json.Unmarshal(message.Data, &payload)
	kind, _ := payload["kind"].(string)
	return kind
}

func minutesPayload(t *testing.T, message model.WebSocketMessage) map[string]interface{} {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal(message.Data, &payload); err != nil {
		t.Fatalf("decode meeting:minutes: %v", err)
	}
	return payload
}

func sendMinutes(t *testing.T, r *wsRPC, meetingID string, payload map[string]interface{}) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode meeting:minutes: %v", err)
	}
	if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingMinutes, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("send meeting:minutes: %v", err)
	}
}

func TestMeetingMinutes_HostConfigAndSegmentIsolation(t *testing.T) {
	_, host, member, outsider, meetingID := meetingChatFlow(t)

	// Only an SFU participant can use the minutes plane; a source-room
	// subscriber must not be able to inject transcript data.
	sendMinutes(t, outsider, meetingID, map[string]interface{}{"action": "segment", "text": "not allowed"})
	if message, err := outsider.waitError(3 * time.Second); err != nil || !strings.Contains(message, "meeting:minutes") {
		t.Fatalf("outsider should be rejected, message=%q err=%v", message, err)
	}

	// A key-shaped field is deliberately ignored by the server and must never
	// be reflected in a public state frame.
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "mimo-asr", "asrModel": "mimo-v2.5-asr",
		"summaryProvider": "mimo", "summaryModel": "mimo-v2.5-pro",
		"apiKey": "must-not-leak",
	})
	configured, err := waitMinutes(member, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if payload := minutesPayload(t, configured); payload["kind"] != "configured" {
		t.Fatalf("expected configured frame, got %#v", payload)
	} else if strings.Contains(string(configured.Data), "must-not-leak") {
		t.Fatal("meeting:minutes echoed an API key-shaped field")
	}

	markMinutesRunningForRelayTests(t, host, meetingID, host, member)

	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "consent", "accepted": false})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "client-controlled-id", "text": "after decline", "startMs": 10, "endMs": 20, "final": true,
	})
	segment, err := waitMinutesKind(host, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitMinutesKind(member, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	segmentPayload := minutesPayload(t, segment)
	batch, ok := segmentPayload["segments"].([]interface{})
	if segmentPayload["kind"] != "segments" || !ok || len(batch) != 1 {
		t.Fatalf("unexpected segment payload: %#v", segmentPayload)
	}
	first, _ := batch[0].(map[string]interface{})
	if first["from"] != "bob:chat-2" || first["speakerName"] != "bob" {
		t.Fatalf("server should canonicalize speakerName, got %#v", first)
	}
	recordingID := currentMinutesRecordingID(t, host.handler, meetingID)
	sendMixedSegment(host.handler, meetingID, "synthetic cursor segment", recordingID)
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "segments", 3*time.Second); err != nil {
			t.Fatal("server mixed segment did not arrive")
		}
	}
	coordinator := currentMinutesCoordinatorRPC(t, host.handler, meetingID, host, member)
	sendMinutes(t, coordinator, meetingID, map[string]interface{}{
		"action": "summary", "summary": "验收安排已确认", "recordingId": recordingID,
		"coordinatorEpoch": currentMinutesCoordinatorEpoch(t, host.handler, meetingID), "throughSeq": 1,
	})
	summary, err := waitMinutesKind(member, "summary", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if payload := minutesPayload(t, summary); payload["kind"] != "summary" || payload["summary"] != "验收安排已确认" {
		t.Fatalf("unexpected summary payload: %#v", payload)
	}
}

func TestMeetingMinutesConfigureUsesFixedServerProviders(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)
	assertFixedProviders := func(frame model.WebSocketMessage) {
		t.Helper()
		payload := minutesPayload(t, frame)
		minutes, ok := payload["minutes"].(map[string]interface{})
		if !ok {
			t.Fatalf("configured frame is missing minutes state: %#v", payload)
		}
		if minutes["asrSource"] != "server-stream" || minutes["asrModel"] != "server-asr" {
			t.Fatalf("client ASR selection must normalize to Somni: %#v", minutes)
		}
		if minutes["summaryProvider"] != "gemini" || minutes["summaryModel"] != meetingAIModel {
			t.Fatalf("client summary selection must normalize to fixed Gemini: %#v", minutes)
		}
	}
	waitConfigured := func() {
		t.Helper()
		for _, r := range []*wsRPC{host, member} {
			frame, err := waitMinutesKind(r, "configured", 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			assertFixedProviders(frame)
		}
	}

	// The current frontend sends no legacy summary provider/model fields.
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "server-stream", "asrModel": "server-asr",
	})
	waitConfigured()

	// Older clients may still send local-provider choices. They are accepted
	// as wire-compatible input but never select a backend or model.
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "browser-speech", "asrModel": "browser-engine",
		"summaryProvider": "deepseek", "summaryModel": "client-selected-model",
	})
	waitConfigured()
}

func TestMeetingMinutes_AnyMemberCanConfigureAndStartIsReadinessGated(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "configure", "asrSource": "browser-speech", "summaryProvider": "mimo",
	})
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "configured", 3*time.Second); err != nil {
			t.Fatalf("non-host member could not configure shared minutes: %v", err)
		}
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "start"})
	if message, err := member.waitError(3 * time.Second); err != nil || !strings.Contains(message, "Gemini and Somni") {
		t.Fatalf("start should fail only because providers are unavailable, message=%q err=%v", message, err)
	}

	sendMinutes(t, host, meetingID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(member, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestMeetingMinutes_BatchesSegmentsRelayToAllMembers(t *testing.T) {
	_, host, member, outsider, meetingID := meetingChatFlow(t)

	configureAndMarkMinutesRunningForRelayTests(t, host, meetingID, false, host, member)

	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segments",
		"segments": []map[string]interface{}{
			{"segmentId": "s-1", "text": "第一段", "startMs": 10, "endMs": 20, "final": true},
			{"segmentId": "s-2", "text": "第二段", "startMs": 30, "endMs": 40, "final": true},
		},
	})

	batch, err := waitMinutesKind(host, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hostSegments := segmentsFromBatch(t, batch)
	if len(hostSegments) != 2 {
		t.Fatalf("expected two segments in one batch, got %#v", hostSegments)
	}
	if hostSegments[0]["speakerName"] != "bob" || hostSegments[0]["from"] != "bob:chat-2" {
		t.Fatalf("server should attach canonical speaker identity, got %#v", hostSegments[0])
	}

	// The sender receives the same batch as an echo; the client dedupes its
	// own copy by the stable segmentId, so both recipients must see identical
	// IDs in identical order.
	echo, err := waitMinutesKind(member, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	echoSegments := segmentsFromBatch(t, echo)
	if len(echoSegments) != 2 ||
		echoSegments[0]["segmentId"] != hostSegments[0]["segmentId"] ||
		echoSegments[1]["segmentId"] != hostSegments[1]["segmentId"] {
		t.Fatalf("sender echo must match the host batch, got %#v vs %#v", echoSegments, hostSegments)
	}

	// A non-member must never receive room transcript batches.
	assertNoMinutesKind(t, outsider, "segments", 600*time.Millisecond)
}

// drainMinutes removes any buffered meeting:minutes frames without asserting.
func drainMinutes(r *wsRPC) {
	for {
		select {
		case <-r.minutes:
		default:
			return
		}
	}
}

// assertNoMinutesKind fails when a meeting:minutes frame of the given kind
// arrives before the timeout elapses. Other kinds are skipped so configured/
// started broadcasts already consumed by earlier waits do not interfere.
func assertNoMinutesKind(t *testing.T, r *wsRPC, kind string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		message, err := waitMinutes(r, time.Until(deadline))
		if err != nil {
			return
		}
		if minutesPayloadKind(message) == kind {
			t.Fatalf("unexpected meeting:minutes %s frame: %s", kind, string(message.Data))
		}
	}
}

func segmentsFromBatch(t *testing.T, message model.WebSocketMessage) []map[string]interface{} {
	t.Helper()
	payload := minutesPayload(t, message)
	if payload["kind"] != "segments" {
		t.Fatalf("expected segments batch, got %#v", payload)
	}
	raw, ok := payload["segments"].([]interface{})
	if !ok {
		t.Fatalf("segments field missing: %#v", payload)
	}
	batch := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		segment, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("unexpected segment entry: %#v", item)
		}
		batch = append(batch, segment)
	}
	return batch
}

func waitMeetingWireEvent(r *wsRPC, timeout time.Duration) (model.WebSocketMessage, error) {
	select {
	case message := <-r.minutesWire:
		return message, nil
	case <-time.After(timeout):
		return model.WebSocketMessage{}, fmt.Errorf("wait meeting wire event timeout")
	}
}

func drainMeetingWireEvents(r *wsRPC) {
	for {
		select {
		case <-r.minutesWire:
		default:
			return
		}
	}
}

// meetingMinutesRoom creates a meeting on ts hosted by host with the given
// members, waits until every participant is registered as a meeting member
// (SFU join path) and returns the meeting room ID.
func meetingMinutesRoom(t *testing.T, ts *meetingTestServer, host *wsRPC, members ...*wsRPC) string {
	t.Helper()
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingCreate, Data: []byte(`{}`)}); err != nil {
		t.Fatalf("meeting:create: %v", err)
	}
	roomID, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	everyone := append([]*wsRPC{host}, members...)
	for _, r := range everyone {
		r.handler = ts.handler
		data, _ := json.Marshal(map[string]interface{}{"roomId": roomID})
		if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: data}); err != nil {
			t.Fatalf("meeting:join: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ts.handler.meetings.members(roomID)) != len(everyone) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(ts.handler.meetings.members(roomID)) != len(everyone) {
		t.Fatalf("meeting members not registered: %d/%d", len(ts.handler.meetings.members(roomID)), len(everyone))
	}
	return roomID
}

// configureAndMarkMinutesRunningForRelayTests configures minutes, then seeds a
// legacy text-only epoch for relay tests. Actual host-start readiness and audio
// mixing are covered by tests with configured Gemini and Somni services.
func configureAndMarkMinutesRunningForRelayTests(t *testing.T, host *wsRPC, roomID string, requireConsent bool, watchers ...*wsRPC) {
	t.Helper()
	if host.handler == nil {
		t.Fatal("relay test host is missing its handler")
	}
	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "configure", "requireConsent": requireConsent,
		"asrSource": "browser-speech", "summaryProvider": "mimo",
	})
	for _, r := range watchers {
		if _, err := waitMinutesKind(r, "configured", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	markMinutesRunningForRelayTests(t, host, roomID, watchers...)
}

func markMinutesRunningForRelayTests(t *testing.T, host *wsRPC, roomID string, watchers ...*wsRPC) {
	t.Helper()
	h := host.handler
	if h == nil {
		t.Fatal("relay test host is missing its handler")
	}
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		t.Fatalf("meeting %q has no active metadata", roomID)
	}
	meta.meetingASRPublishMu.Lock()
	meta.mu.Lock()
	if !meta.minutes.Configured {
		meta.mu.Unlock()
		meta.meetingASRPublishMu.Unlock()
		t.Fatal("relay test minutes must be configured before marking them running")
	}
	meta.minutes.Running = true
	meta.minutes.RecordingID = newMeetingRecordingID()
	meta.minutes.SummaryLanguage = "zh-CN"
	resetMeetingMinutesEpochLocked(meta)
	consented := make(map[string]bool)
	if meta.minutes.CaptionActive {
		for uniqID, accepted := range meta.minutes.Consented {
			if accepted {
				consented[uniqID] = true
			}
		}
	}
	meta.minutes.Consented = consented
	meta.mu.Unlock()
	members := h.meetings.members(roomID)
	preferred := ""
	if len(members) > 0 {
		preferred = members[0].UniqID
	}
	meta.mu.Lock()
	electMeetingMinutesCoordinatorLocked(meta, members, preferred)
	state := meetingMinutesWireState(meta.minutes)
	meta.mu.Unlock()
	h.broadcastMeetingMinutesState(roomID, "started", state)
	meta.meetingASRPublishMu.Unlock()
	for _, r := range watchers {
		if _, err := waitMinutesKind(r, "started", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMeetingMinutes_NonmemberCannotTriggerRelay(t *testing.T) {
	_, host, member, outsider, meetingID := meetingChatFlow(t)

	configureAndMarkMinutesRunningForRelayTests(t, host, meetingID, false, host, member)

	// A connection that never meeting:join'ed must not inject transcript data
	// into the room-wide relay.
	sendMinutes(t, outsider, meetingID, map[string]interface{}{
		"action": "segments",
		"segments": []map[string]interface{}{
			{"segmentId": "intruder-1", "text": "注入尝试", "startMs": 0, "endMs": 10, "final": true},
		},
	})
	if message, err := outsider.waitError(3 * time.Second); err != nil || !strings.Contains(message, "meeting:minutes") {
		t.Fatalf("outsider should be rejected, message=%q err=%v", message, err)
	}
	// Cover the deferred 1.2s flush window: no member may observe the batch.
	assertNoMinutesKind(t, host, "segments", 1600*time.Millisecond)
	assertNoMinutesKind(t, member, "segments", 1600*time.Millisecond)
}

func TestMeetingMinutes_DeclineDoesNotBlockActiveMinutes(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	configureAndMarkMinutesRunningForRelayTests(t, host, meetingID, true, host, member)
	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "consent", "accepted": false})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "declined-notice-1", "text": "decline does not opt out", "startMs": 0, "endMs": 10, "final": true,
	})
	for _, r := range []*wsRPC{host, member} {
		batch, err := waitMinutesKind(r, "segments", 3*time.Second)
		if err != nil {
			t.Fatalf("declined participant should still contribute while minutes run: %v", err)
		}
		segments := segmentsFromBatch(t, batch)
		if len(segments) != 1 || segments[0]["segmentId"] != "declined-notice-1" {
			t.Fatalf("decline suppressed minutes segment: %#v", segments)
		}
	}
}
func TestMeetingMinutes_LegacyStopPausesOnlySender(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	configureAndMarkMinutesRunningForRelayTests(t, host, meetingID, false, host, member)

	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "pre-stop-1", "text": "停止前", "startMs": 0, "endMs": 10, "final": true,
	})
	if _, err := waitMinutesKind(host, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := waitMinutesKind(member, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	sendMinutes(t, host, meetingID, map[string]interface{}{"action": "stop"})
	ack, err := waitMinutesKind(host, "subscription", 3*time.Second)
	if err != nil {
		t.Fatal("legacy stop did not acknowledge personal subscription state")
	}
	if minutesPayload(t, ack)["minutesSubscription"].(map[string]interface{})["enabled"] != false {
		t.Fatal("legacy stop did not disable only the sender")
	}

	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "post-stop-1", "text": "停止后", "startMs": 20, "endMs": 30, "final": true,
	})
	if _, err := waitMinutesKind(member, "segments", 3*time.Second); err != nil {
		t.Fatalf("remaining member did not receive segment after another member paused: %v", err)
	}
	assertNoMinutesKind(t, host, "segments", 1600*time.Millisecond)
}

func TestMeetingMinutes_LateJoinReplaysServerMixedHistory(t *testing.T) {
	ts, host, member, _, meetingID := meetingChatFlow(t)

	configureAndMarkMinutesRunningForRelayTests(t, host, meetingID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, meetingID)
	sendMixedSegment(host.handler, meetingID, "synthetic historical segment", recordingID)
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "segments", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	late := newWSRPC(t, ts.srv, "dave:late-1")
	data, _ := json.Marshal(map[string]interface{}{"roomId": meetingID})
	if err := late.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: meetingID, Data: data}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ts.handler.meetings.members(meetingID)) != 3 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(ts.handler.meetings.members(meetingID)) != 3 {
		t.Fatal("late joiner was not registered as a meeting member")
	}
	first, err := waitMeetingWireEvent(late, 3*time.Second)
	if err != nil || first.Type != model.MessageTypeMeetingInfo {
		t.Fatal("late joiner did not receive meeting info before replay")
	}
	replay, err := waitMeetingWireEvent(late, 3*time.Second)
	if err != nil || minutesPayloadKind(replay) != "segments" {
		t.Fatal("late joiner did not receive prior server-mixed transcript")
	}
	batch := segmentsFromBatch(t, replay)
	if len(batch) != 1 || segmentSeq(t, batch[0]) != 1 || batch[0]["recordingId"] != recordingID {
		t.Fatalf("late join replay was not scoped to the recording: %#v", batch)
	}
}

func TestMeetingMinutes_HostSwitchKeepsRelayFlowing(t *testing.T) {
	ts := newMeetingTestServer(t)
	t.Cleanup(ts.Close)

	host := newWSRPC(t, ts.srv, "host:mins-1")
	first := newWSRPC(t, ts.srv, "first:mins-1")
	second := newWSRPC(t, ts.srv, "second:mins-1")
	roomID := meetingMinutesRoom(t, ts, host, first, second)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, first, second)

	// After an explicit host transfer the room-wide relay must keep covering
	// every member, and the new host participates like anyone else.
	data, _ := json.Marshal(map[string]interface{}{"action": "set", "to": "first:mins-1"})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingHost, Channel: roomID, Data: data}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*wsRPC{host, first, second} {
		select {
		case message := <-r.hostChanged:
			var payload struct {
				HostID string `json:"hostId"`
			}
			if err := json.Unmarshal(message.Data, &payload); err != nil || payload.HostID != "first:mins-1" {
				t.Fatalf("invalid host transfer payload: %#v err=%v", payload, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("manual host transfer was not broadcast to every member")
		}
	}

	sendMinutes(t, second, roomID, map[string]interface{}{
		"action": "segment", "segmentId": "after-switch-1", "text": "转移后的发言", "startMs": 0, "endMs": 10, "final": true,
	})
	newHostBatch, err := waitMinutesKind(first, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if segments := segmentsFromBatch(t, newHostBatch); len(segments) != 1 || segments[0]["segmentId"] != "after-switch-1" {
		t.Fatalf("new host received unexpected batch: %#v", segments)
	}
	if _, err := waitMinutesKind(host, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := waitMinutesKind(second, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	// Legacy stop is a personal receive toggle. The new host cannot stop the
	// shared recording for other members.
	sendMinutes(t, first, roomID, map[string]interface{}{"action": "stop"})
	if _, err := waitMinutesKind(first, "subscription", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, second, roomID, map[string]interface{}{
		"action": "segment", "segmentId": "post-switch-stop-1", "text": "停止后", "startMs": 20, "endMs": 30, "final": true,
	})
	if _, err := waitMinutesKind(second, "segments", 3*time.Second); err != nil {
		t.Fatalf("member did not continue receiving after another member paused: %v", err)
	}
	assertNoMinutesKind(t, first, "segments", 1600*time.Millisecond)
}

func TestMeetingMinutes_RoomsAreIsolated(t *testing.T) {
	ts := newMeetingTestServer(t)
	t.Cleanup(ts.Close)

	hostA := newWSRPC(t, ts.srv, "hosta:iso-1")
	memberA := newWSRPC(t, ts.srv, "membera:iso-1")
	hostB := newWSRPC(t, ts.srv, "hostb:iso-1")
	memberB := newWSRPC(t, ts.srv, "memberb:iso-1")

	roomA := meetingMinutesRoom(t, ts, hostA, memberA)
	roomB := meetingMinutesRoom(t, ts, hostB, memberB)
	configureAndMarkMinutesRunningForRelayTests(t, hostA, roomA, false, hostA, memberA)
	configureAndMarkMinutesRunningForRelayTests(t, hostB, roomB, false, hostB, memberB)

	sendMinutes(t, memberA, roomA, map[string]interface{}{
		"action": "segment", "segmentId": "iso-a-1", "text": "A 房间发言", "startMs": 0, "endMs": 10, "final": true,
	})
	if _, err := waitMinutesKind(hostA, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := waitMinutesKind(memberA, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	// The other meeting's members must not observe room A's batch.
	assertNoMinutesKind(t, hostB, "segments", 1600*time.Millisecond)
	assertNoMinutesKind(t, memberB, "segments", 1600*time.Millisecond)

	sendMinutes(t, memberB, roomB, map[string]interface{}{
		"action": "segment", "segmentId": "iso-b-1", "text": "B 房间发言", "startMs": 0, "endMs": 10, "final": true,
	})
	if _, err := waitMinutesKind(hostB, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := waitMinutesKind(memberB, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	assertNoMinutesKind(t, hostA, "segments", 1600*time.Millisecond)
	assertNoMinutesKind(t, memberA, "segments", 1600*time.Millisecond)
	drainMinutes(hostA)
	drainMinutes(memberA)
	drainMinutes(hostB)
	drainMinutes(memberB)
}

// ---------------------------------------------------------------------------
// Personal captions: any member may enable captions without minutes; only
// consenting speakers contribute final text; the transient segment stream
// stays shared while either captions or minutes need transcription.
// ---------------------------------------------------------------------------

// subscribeCaption sends a caption-subscribe toggle for the caller.
func subscribeCaption(t *testing.T, r *wsRPC, roomID string, enabled bool) {
	t.Helper()
	sendMinutes(t, r, roomID, map[string]interface{}{"action": "caption-subscribe", "enabled": enabled})
}

// captionsActiveFromFrame decodes the public minutes state carried by a
// "captions" broadcast and returns its captionActive flag.
func captionsActiveFromFrame(t *testing.T, message model.WebSocketMessage) bool {
	t.Helper()
	payload := minutesPayload(t, message)
	if payload["kind"] != "captions" {
		t.Fatalf("expected captions frame, got %#v", payload)
	}
	minutes, ok := payload["minutes"].(map[string]interface{})
	if !ok {
		t.Fatalf("captions frame must carry the public minutes state: %#v", payload)
	}
	active, _ := minutes["captionActive"].(bool)
	return active
}

// minutesCaptionActiveFromFrame extracts minutes.captionActive from any
// meeting:minutes frame embedding the public state (configured/started/
// stopped/summary).
func minutesCaptionActiveFromFrame(t *testing.T, message model.WebSocketMessage) bool {
	t.Helper()
	payload := minutesPayload(t, message)
	minutes, ok := payload["minutes"].(map[string]interface{})
	if !ok {
		t.Fatalf("frame must carry the public minutes state: %#v", payload)
	}
	active, _ := minutes["captionActive"].(bool)
	return active
}

// minutesMapCaptionActive reads the captionActive flag out of a decoded
// public minutes state map.
func minutesMapCaptionActive(minutes map[string]interface{}) bool {
	active, _ := minutes["captionActive"].(bool)
	return active
}

// minutesMapFromFrame extracts the public minutes state embedded in any
// meeting:minutes frame that carries it (configured/started/stopped/summary).
func minutesMapFromFrame(t *testing.T, message model.WebSocketMessage) map[string]interface{} {
	t.Helper()
	payload := minutesPayload(t, message)
	minutes, ok := payload["minutes"].(map[string]interface{})
	if !ok {
		t.Fatalf("frame must carry the public minutes state: %#v", payload)
	}
	return minutes
}

// minutesRecordingID reads the recordingId out of a decoded public minutes map.
func minutesRecordingID(minutes map[string]interface{}) string {
	id, _ := minutes["recordingId"].(string)
	return id
}

// waitMeetingInfoMinutes waits for the joiner's meeting:info frame and returns
// its public minutes state map.
func waitMeetingInfoMinutes(t *testing.T, r *wsRPC, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case message := <-r.info:
			payload := minutesPayload(t, message)
			if minutes, ok := payload["minutes"].(map[string]interface{}); ok {
				return minutes
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("meeting:info with public minutes state was not delivered")
	return nil
}

func waitMembersRegistered(t *testing.T, ts *meetingTestServer, roomID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ts.handler.meetings.members(roomID)) != want {
		time.Sleep(20 * time.Millisecond)
	}
	if len(ts.handler.meetings.members(roomID)) != want {
		t.Fatalf("expected %d meeting members, got %d", want, len(ts.handler.meetings.members(roomID)))
	}
}

func leaveMeeting(t *testing.T, r *wsRPC, roomID string) {
	t.Helper()
	data, _ := json.Marshal(map[string]interface{}{})
	if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID, Data: data}); err != nil {
		t.Fatalf("meeting:leave: %v", err)
	}
}

func TestMeetingCaptions_CaptionsOnlyLifecycleWithRequesterCleanup(t *testing.T) {
	ts := newMeetingTestServerWithCaptions(t)
	t.Cleanup(ts.Close)

	host := newWSRPC(t, ts.srv, "host:cap-1")
	first := newWSRPC(t, ts.srv, "first:cap-1")
	second := newWSRPC(t, ts.srv, "second:cap-1")
	roomID := meetingMinutesRoom(t, ts, host, first, second)

	// A member enables personal captions without any minutes configuration:
	// every member sees the opt-in notice with only the aggregated flag.
	subscribeCaption(t, first, roomID, true)
	for _, r := range []*wsRPC{host, first, second} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !captionsActiveFromFrame(t, frame) {
			t.Fatal("captionActive should flip true for every member after the first requester subscribes")
		}
		if strings.Contains(string(frame.Data), "first:cap-1") || strings.Contains(string(frame.Data), "second:cap-1") || strings.Contains(string(frame.Data), "host:cap-1") {
			t.Fatalf("captions broadcast must not leak viewer identities: %s", string(frame.Data))
		}
	}

	// A second requester subscribes: the mode stays active and broadcasts keep
	// flowing to everyone.
	subscribeCaption(t, second, roomID, true)
	for _, r := range []*wsRPC{host, second} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || !captionsActiveFromFrame(t, frame) {
			t.Fatalf("second subscribe should broadcast captions=true, err=%v", err)
		}
	}

	// Caption mode enforces individual consent even though minutes were never
	// configured: an unconsented speaker must not contribute text.
	sendMinutes(t, second, roomID, map[string]interface{}{
		"action": "segment", "segmentId": "cap-no-consent-1", "text": "未被同意的字幕", "startMs": 0, "endMs": 10, "final": true,
	})
	if message, err := second.waitError(3 * time.Second); err != nil || !strings.Contains(message, "请先同意字幕转写") {
		t.Fatalf("captions must require individual consent, message=%q err=%v", message, err)
	}
	assertNoMinutesKind(t, host, "segments", 1600*time.Millisecond)
	assertNoMinutesKind(t, first, "segments", 1600*time.Millisecond)

	// A consenting speaker contributes; the transient batch reaches every
	// current member with canonical identity and the per-speaker language.
	sendMinutes(t, first, roomID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(second, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, first, roomID, map[string]interface{}{
		"action": "segment", "segmentId": "cap-zh-1", "text": "中文字幕发言", "startMs": 0, "endMs": 10, "final": true, "language": "zh-CN",
	})
	for _, r := range []*wsRPC{host, first, second} {
		batch, err := waitMinutesKind(r, "segments", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		segments := segmentsFromBatch(t, batch)
		if len(segments) != 1 || segments[0]["segmentId"] != "cap-zh-1" ||
			segments[0]["speakerName"] != "first" || segments[0]["language"] != "zh-CN" {
			t.Fatalf("captions-only segment relay is wrong: %#v", segments)
		}
	}

	// The first requester leaves: the second requester keeps captions alive.
	leaveMeeting(t, first, roomID)
	waitMembersRegistered(t, ts, roomID, 2)
	sendMinutes(t, second, roomID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, second, roomID, map[string]interface{}{
		"action": "segment", "segmentId": "cap-after-leave-1", "text": "仍在字幕中", "startMs": 0, "endMs": 10, "final": true, "language": "en-US",
	})
	hostBatch, err := waitMinutesKind(host, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if segments := segmentsFromBatch(t, hostBatch); len(segments) != 1 || segments[0]["language"] != "en-US" {
		t.Fatalf("captions must stay active while another requester remains: %#v", segments)
	}

	// The last requester leaves: the room-wide caption mode must clear so no
	// stale captionActive keeps member microphones running.
	leaveMeeting(t, second, roomID)
	waitMembersRegistered(t, ts, roomID, 1)
	frame, err := waitMinutesKind(host, "captions", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if captionsActiveFromFrame(t, frame) {
		t.Fatal("captionActive must clear after the last requester leaves")
	}

	// With neither captions nor minutes active, the stream gate is closed.
	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "segment", "segmentId": "cap-closed-1", "text": "关闭后的发言", "startMs": 0, "endMs": 10, "final": true,
	})
	if message, err := host.waitError(3 * time.Second); err != nil || !strings.Contains(message, "尚未启动") {
		t.Fatalf("segment after captions cleared should be rejected, message=%q err=%v", message, err)
	}

	// A newly joined participant sees the cleared public state in meeting:info.
	late := newWSRPC(t, ts.srv, "late:cap-1")
	joinData, _ := json.Marshal(map[string]interface{}{"roomId": roomID})
	if err := late.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: joinData}); err != nil {
		t.Fatal(err)
	}
	if minutes := waitMeetingInfoMinutes(t, late, 5*time.Second); minutesMapCaptionActive(minutes) {
		t.Fatalf("late joiner must observe captionActive=false, got %#v", minutes)
	}
	drainMinutes(host)
	drainMinutes(late)
}

func TestMeetingCaptions_ConsentEnforcedEvenWithLegacyMinutesConsentOff(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": false,
	})
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "configured", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	// In captions-only mode consent remains per speaker even when the shared
	// minutes configuration does not require it.
	subscribeCaption(t, member, meetingID, true)
	for _, r := range []*wsRPC{host, member} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || !captionsActiveFromFrame(t, frame) {
			t.Fatalf("captions=true expected, err=%v", err)
		}
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "cap-consent-1", "text": "未同意的字幕", "startMs": 0, "endMs": 10, "final": true, "language": "zh-CN",
	})
	if message, err := member.waitError(3 * time.Second); err != nil || !strings.Contains(message, "请先同意字幕转写") {
		t.Fatalf("caption mode must enforce consent, message=%q err=%v", message, err)
	}
	assertNoMinutesKind(t, host, "segments", 1600*time.Millisecond)

	// Consent applies to the host as well while captions are active.
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "cap-consent-2", "text": "主持人未同意", "startMs": 0, "endMs": 10, "final": true,
	})
	if message, err := host.waitError(3 * time.Second); err != nil || !strings.Contains(message, "请先同意字幕转写") {
		t.Fatalf("caption consent applies to the host too, message=%q err=%v", message, err)
	}

	// Consenting caption speakers contribute; language is validated per segment so a
	// mixed Chinese/English room never locks one wrong source language.
	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, host, meetingID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(member, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "segments",
		"segments": []map[string]interface{}{
			{"segmentId": "cap-en-1", "text": "english line", "startMs": 0, "endMs": 10, "final": true, "language": "en-US"},
			{"segmentId": "cap-unknown-1", "text": "未知语言", "startMs": 10, "endMs": 20, "final": true, "language": "de-DE"},
			{"segmentId": "cap-zh-1", "text": "中文发言", "startMs": 20, "endMs": 30, "final": true, "language": "zh-CN"},
		},
	})
	batch, err := waitMinutesKind(member, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	segments := segmentsFromBatch(t, batch)
	if len(segments) != 3 {
		t.Fatalf("expected three canonical segments, got %#v", segments)
	}
	if segments[0]["language"] != "en-US" {
		t.Fatalf("supported language must be relayed: %#v", segments[0])
	}
	if lang, _ := segments[1]["language"].(string); lang != "" {
		t.Fatalf("unknown language must be cleared, got %#v", segments[1])
	}
	if segments[2]["language"] != "zh-CN" {
		t.Fatalf("zh-CN must be relayed: %#v", segments[2])
	}
	if _, err := waitMinutesKind(host, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	drainMinutes(host)
	drainMinutes(member)
}

func TestMeetingCaptions_MinutesTransitionsKeepCaptionsAndConsent(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	// Captions only first: member requests captions and consents.
	subscribeCaption(t, member, meetingID, true)
	for _, r := range []*wsRPC{host, member} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || !captionsActiveFromFrame(t, frame) {
			t.Fatalf("captions=true expected, err=%v", err)
		}
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "pre-minutes-1", "text": "纪要未启字幕在", "startMs": 0, "endMs": 10, "final": true, "language": "zh-CN",
	})
	if _, err := waitMinutesKind(host, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := waitMinutesKind(member, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	// Configuring minutes while captions are active must preserve the caption
	// session and the previously accepted consent.
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "browser-speech", "summaryProvider": "mimo",
	})
	configured, err := waitMinutesKind(host, "configured", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !minutesCaptionActiveFromFrame(t, configured) {
		t.Fatal("configure must keep captionActive while captions are on")
	}
	if _, err := waitMinutesKind(member, "configured", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	// No fresh opt-in: the consent accepted before configure still holds.
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "post-configure-1", "text": "配置后无需重新同意", "startMs": 0, "endMs": 10, "final": true,
	})
	if _, err := waitMinutesKind(host, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := waitMinutesKind(member, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	// Host starts minutes while captions stay active; the recording notice does
	// not change which publishers the server mixer includes.
	markMinutesRunningForRelayTests(t, host, meetingID)
	started, err := waitMinutesKind(member, "started", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !minutesCaptionActiveFromFrame(t, started) {
		t.Fatal("started frame must keep captionActive=true")
	}
	if minutesRecordingID(minutesMapFromFrame(t, started)) == "" {
		t.Fatal("started frame must carry a fresh recordingId")
	}
	if _, err := waitMinutesKind(host, "started", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "pre-start-consent-invalid-1", "text": "included in active minutes", "startMs": 0, "endMs": 10, "final": true,
	})
	for _, r := range []*wsRPC{host, member} {
		batch, err := waitMinutesKind(r, "segments", 3*time.Second)
		if err != nil || len(segmentsFromBatch(t, batch)) != 1 {
			t.Fatalf("decline must not suppress active minutes, err=%v", err)
		}
	}

	// Legacy stop pauses only the host's receive path. The room stays in the
	// same recording epoch and captions remain active for the member.
	sendMinutes(t, host, meetingID, map[string]interface{}{"action": "stop"})
	ack, err := waitMinutesKind(host, "subscription", 3*time.Second)
	if err != nil || minutesPayload(t, ack)["minutesSubscription"].(map[string]interface{})["enabled"] != false {
		t.Fatal(err)
	}
	if !minutesCaptionActiveFromFrame(t, ack) {
		t.Fatal("personal pause must keep captionActive while captions remain requested")
	}
	assertNoMinutesKind(t, host, "segments", 200*time.Millisecond)
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "post-pause-cap-1", "text": "caption consent remains valid", "startMs": 0, "endMs": 10, "final": true, "language": "en-US",
	})
	batch, err := waitMinutesKind(member, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if segments := segmentsFromBatch(t, batch); len(segments) != 1 || segments[0]["language"] != "en-US" {
		t.Fatalf("captions must keep the stream alive after minutes stop: %#v", segments)
	}

	// Removing the last captions viewer clears only caption mode. The shared
	// minutes recording remains active.
	subscribeCaption(t, member, meetingID, false)
	for _, r := range []*wsRPC{host, member} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || captionsActiveFromFrame(t, frame) {
			t.Fatalf("captions=false expected after unsubscribe, err=%v", err)
		}
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "minutes-after-captions-1", "text": "minutes remain active", "startMs": 0, "endMs": 10, "final": true,
	})
	if _, err := waitMinutesKind(member, "segments", 3*time.Second); err != nil {
		t.Fatalf("active minutes must continue after captions unsubscribe: %v", err)
	}
	assertNoMinutesKind(t, host, "segments", 200*time.Millisecond)
	drainMinutes(host)
	drainMinutes(member)
}

func TestMeetingCaptions_ViewerRemovedOnKickDisconnectAndRebind(t *testing.T) {
	ts := newMeetingTestServerWithCaptions(t)
	t.Cleanup(ts.Close)

	host := newWSRPC(t, ts.srv, "host:cap-2")
	viewer := newWSRPC(t, ts.srv, "viewer:cap-2")
	peer := newWSRPC(t, ts.srv, "peer:cap-2")
	roomID := meetingMinutesRoom(t, ts, host, viewer, peer)

	// Captions active: a kicked requester must be dropped from the mode.
	subscribeCaption(t, viewer, roomID, true)
	for _, r := range []*wsRPC{host, viewer, peer} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || !captionsActiveFromFrame(t, frame) {
			t.Fatalf("captions=true expected before kick, err=%v", err)
		}
	}
	// A joiner while captions are active observes the same public state.
	observer := newWSRPC(t, ts.srv, "observer:cap-2")
	joinData, _ := json.Marshal(map[string]interface{}{"roomId": roomID})
	if err := observer.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: joinData}); err != nil {
		t.Fatal(err)
	}
	if minutes := waitMeetingInfoMinutes(t, observer, 5*time.Second); !minutesMapCaptionActive(minutes) {
		t.Fatalf("late joiner must observe captionActive=true, got %#v", minutes)
	}

	kickData, _ := json.Marshal(map[string]interface{}{"to": "viewer:cap-2"})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingKick, Channel: roomID, Data: kickData}); err != nil {
		t.Fatal(err)
	}
	waitMembersRegistered(t, ts, roomID, 3)
	for _, r := range []*wsRPC{host, peer, observer} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || captionsActiveFromFrame(t, frame) {
			t.Fatalf("kick must broadcast captions=false, err=%v", err)
		}
	}

	// Disconnect cleanup: a requester's websocket drops without an explicit
	// leave; the abandoned caption flag must not linger.
	subscribeCaption(t, peer, roomID, true)
	for _, r := range []*wsRPC{host, peer, observer} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || !captionsActiveFromFrame(t, frame) {
			t.Fatalf("captions=true expected before disconnect, err=%v", err)
		}
	}
	peer.conn.Close()
	waitMembersRegistered(t, ts, roomID, 2)
	for _, r := range []*wsRPC{host, observer} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || captionsActiveFromFrame(t, frame) {
			t.Fatalf("disconnect must broadcast captions=false, err=%v", err)
		}
	}

	// Reconnect rebind: the same user re-joins from a brand-new websocket
	// session; the pre-reconnect viewer flag must not survive the swap.
	fresh := newWSRPC(t, ts.srv, "fresh:cap-2")
	if err := fresh.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: joinData}); err != nil {
		t.Fatal(err)
	}
	waitMembersRegistered(t, ts, roomID, 3)
	subscribeCaption(t, fresh, roomID, true)
	for _, r := range []*wsRPC{host, observer, fresh} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || !captionsActiveFromFrame(t, frame) {
			t.Fatalf("captions=true expected before rebind, err=%v", err)
		}
	}
	reborn := newWSRPC(t, ts.srv, "fresh:cap-2")
	if err := reborn.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: joinData}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*wsRPC{host, observer} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || captionsActiveFromFrame(t, frame) {
			t.Fatalf("reconnect rebind must broadcast captions=false, err=%v", err)
		}
	}
	if minutes := waitMeetingInfoMinutes(t, reborn, 5*time.Second); minutesMapCaptionActive(minutes) {
		t.Fatalf("reborn session must observe captionActive=false, got %#v", minutes)
	}
	drainMinutes(host)
	drainMinutes(observer)
	drainMinutes(fresh)
	drainMinutes(reborn)
}

// ---------------------------------------------------------------------------
// Recording-epoch wire contract: every host start mints a unique crypto/rand
// recordingId, and relayed segments carry the sender-supplied recordingId
// only when it exactly matches the recording running at arrival time. The
// server never infers or stamps IDs onto untagged batches.
// ---------------------------------------------------------------------------

func TestMeetingMinutes_StartBroadcastsRecordingIDAndDeclineDoesNotOptOut(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	// Configure minutes without starting them; the recording epoch only opens
	// on the host's explicit start.
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "browser-speech", "summaryProvider": "mimo",
	})
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "configured", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	// Captions-only session first: the member enables captions and consents.
	subscribeCaption(t, member, meetingID, true)
	for _, r := range []*wsRPC{host, member} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || !captionsActiveFromFrame(t, frame) {
			t.Fatalf("captions=true expected, err=%v", err)
		}
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	// Host starts minutes while captions stay active. The started frame must
	// carry running=true and a non-empty recordingId for every attendee.
	markMinutesRunningForRelayTests(t, host, meetingID)
	var recordingID string
	for _, r := range []*wsRPC{host, member} {
		started, err := waitMinutesKind(r, "started", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		minutes := minutesMapFromFrame(t, started)
		if minutes["running"] != true {
			t.Fatalf("started must broadcast running=true: %#v", minutes)
		}
		if id := minutesRecordingID(minutes); id == "" {
			t.Fatalf("started must broadcast a non-empty recordingId: %#v", minutes)
		} else if recordingID == "" {
			recordingID = id
		} else if recordingID != id {
			t.Fatalf("attendees received different recording IDs: %q and %q", recordingID, id)
		}
	}

	// Consent remains a notification during the new mixed recording.
	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "consent", "accepted": false})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "epoch-consent-1", "recordingId": recordingID,
		"text": "included despite decline", "startMs": 0, "endMs": 10, "final": true,
	})
	batch, err := waitMinutesKind(host, "segments", 3*time.Second)
	if err != nil {
		t.Fatalf("decline must not suppress active minutes: %v", err)
	}
	segments := segmentsFromBatch(t, batch)
	if len(segments) != 1 || segments[0]["recordingId"] != recordingID {
		t.Fatalf("active minutes should include the declined participant in this epoch: %#v", segments)
	}
	if _, err := waitMinutesKind(member, "segments", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	drainMinutes(host)
	drainMinutes(member)
}

func TestMeetingMinutes_CaptionFragmentQueuedBeforeStartNeverTagged(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	// Configure minutes (not running), then open a captions-only session with
	// a consenting speaker.
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "browser-speech", "summaryProvider": "mimo",
	})
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "configured", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	subscribeCaption(t, member, meetingID, true)
	for _, r := range []*wsRPC{host, member} {
		frame, err := waitMinutesKind(r, "captions", 3*time.Second)
		if err != nil || !captionsActiveFromFrame(t, frame) {
			t.Fatalf("captions=true expected, err=%v", err)
		}
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	// Queue a caption-only fragment whose sender supplies no recordingId, then
	// start minutes BEFORE the 1.2s flush timer fires so the batch is still
	// pending when the new recording epoch opens.
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "queued-pre-start-1", "text": "启动前入队", "startMs": 0, "endMs": 10, "final": true,
	})
	markMinutesRunningForRelayTests(t, host, meetingID)
	for _, r := range []*wsRPC{host, member} {
		started, err := waitMinutesKind(r, "started", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if minutesRecordingID(minutesMapFromFrame(t, started)) == "" {
			t.Fatal("started must carry a fresh recordingId")
		}
	}
	for _, r := range []*wsRPC{host, member} {
		batch, err := waitMinutesKind(r, "segments", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		segments := segmentsFromBatch(t, batch)
		if len(segments) != 1 || segments[0]["segmentId"] != "queued-pre-start-1" {
			t.Fatalf("unexpected flushed batch: %#v", segments)
		}
		// The server must never stamp (infer) the new recording's ID onto a
		// batch that arrived before that recording started.
		if _, tagged := segments[0]["recordingId"]; tagged {
			t.Fatalf("pre-start caption fragment must never carry the new recordingId: %#v", segments[0])
		}
	}
	drainMinutes(host)
	drainMinutes(member)
}

func TestMeetingMinutes_RecordingIDGateOnRelayedSegments(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "browser-speech", "summaryProvider": "mimo",
	})
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "configured", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	markMinutesRunningForRelayTests(t, host, meetingID)
	hostStarted, err := waitMinutesKind(host, "started", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	recordingID := minutesRecordingID(minutesMapFromFrame(t, hostStarted))
	if recordingID == "" {
		t.Fatal("started must carry a fresh recordingId")
	}
	memberStarted, err := waitMinutesKind(member, "started", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if minutesRecordingID(minutesMapFromFrame(t, memberStarted)) != recordingID {
		t.Fatal("every attendee must observe the same recording epoch")
	}
	// One stale/guessed ID, one oversized ID and one matching the running
	// recording: only the matching tag survives, per segment.
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segments",
		"segments": []map[string]interface{}{
			{"segmentId": "stale-1", "recordingId": "deadbeef-stale-epoch", "text": "旧代次批次", "startMs": 0, "endMs": 10, "final": true},
			{"segmentId": "oversize-1", "recordingId": strings.Repeat("x", 300), "text": "超长代次批次", "startMs": 10, "endMs": 20, "final": true},
			{"segmentId": "current-1", "recordingId": recordingID, "text": "当前代次批次", "startMs": 20, "endMs": 30, "final": true},
		},
	})
	batch, err := waitMinutesKind(host, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	segments := segmentsFromBatch(t, batch)
	if len(segments) != 3 {
		t.Fatalf("all three segments relay with independent tags: %#v", segments)
	}
	if _, tagged := segments[0]["recordingId"]; tagged || segments[0]["segmentId"] != "stale-1" {
		t.Fatalf("stale recordingId must be stripped: %#v", segments[0])
	}
	if _, tagged := segments[1]["recordingId"]; tagged || segments[1]["segmentId"] != "oversize-1" {
		t.Fatalf("oversized recordingId must be stripped: %#v", segments[1])
	}
	if segments[2]["segmentId"] != "current-1" || segments[2]["recordingId"] != recordingID {
		t.Fatalf("current recordingId must be relayed verbatim: %#v", segments[2])
	}
	// The sender echo carries the identical tags.
	echo, err := waitMinutesKind(member, "segments", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	echoSegments := segmentsFromBatch(t, echo)
	for i, seg := range echoSegments {
		if seg["recordingId"] != segments[i]["recordingId"] {
			t.Fatalf("sender echo tags must match the host batch: %#v vs %#v", echoSegments, segments)
		}
	}
	drainMinutes(host)
	drainMinutes(member)
}

func TestMeetingMinutes_PersonalStopAndConfigurePreserveRunningEpoch(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "browser-speech", "summaryProvider": "mimo",
	})
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "configured", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	startRecording := func() string {
		t.Helper()
		markMinutesRunningForRelayTests(t, host, meetingID)
		hostFrame, err := waitMinutesKind(host, "started", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		id := minutesRecordingID(minutesMapFromFrame(t, hostFrame))
		if id == "" {
			t.Fatal("each start must mint a fresh recordingId")
		}
		memberFrame, err := waitMinutesKind(member, "started", 3*time.Second)
		if err != nil || minutesRecordingID(minutesMapFromFrame(t, memberFrame)) != id {
			t.Fatalf("attendees must share the recording epoch, err=%v", err)
		}
		return id
	}

	first := startRecording()

	// Legacy stop is a personal receive preference and cannot end or rotate the
	// shared recording epoch.
	sendMinutes(t, host, meetingID, map[string]interface{}{"action": "stop"})
	ack, err := waitMinutesKind(host, "subscription", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if kept := minutesRecordingID(minutesMapFromFrame(t, ack)); kept != first {
		t.Fatalf("personal stop changed the recording epoch: got %q want %q", kept, first)
	}
	meta, ok := host.handler.activeMeetingRoom(meetingID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	meta.mu.Unlock()
	if !running {
		t.Fatal("personal stop ended the room recording")
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": false,
	})
	if _, err := waitMinutesKind(host, "configured", 3*time.Second); err != nil {
		t.Fatal("running configure should be an idempotent state refresh")
	}
	if _, err := waitMinutesKind(member, "configured", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	meta.mu.Lock()
	kept := meta.minutes.RecordingID
	meta.mu.Unlock()
	if kept != first {
		t.Fatalf("configure reset the running recording epoch: got %q want %q", kept, first)
	}
	drainMinutes(host)
	drainMinutes(member)
}
