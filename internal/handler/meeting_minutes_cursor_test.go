package handler

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func currentMinutesRecordingID(t *testing.T, h *WebSocketHandler, roomID string) string {
	t.Helper()
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	return meta.minutes.RecordingID
}

func currentMinutesCoordinatorEpoch(t *testing.T, h *WebSocketHandler, roomID string) uint64 {
	t.Helper()
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	return meta.minutes.SummaryCoordinatorEpoch
}

func currentMinutesCoordinatorRPC(t *testing.T, h *WebSocketHandler, roomID string, clients ...*wsRPC) *wsRPC {
	t.Helper()
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	meta.mu.Lock()
	coordinatorID := meta.minutes.SummaryCoordinatorID
	meta.mu.Unlock()
	for _, client := range clients {
		if client != nil && client.userID == coordinatorID {
			return client
		}
	}
	t.Fatal("minutes coordinator is not among the current test clients")
	return nil
}

func publishCurrentMinutesSummary(t *testing.T, sender *wsRPC, roomID, summary string, throughSeq uint64) {
	t.Helper()
	recordingID := currentMinutesRecordingID(t, sender.handler, roomID)
	epoch := currentMinutesCoordinatorEpoch(t, sender.handler, roomID)
	sendMinutes(t, sender, roomID, map[string]interface{}{
		"action": "summary", "summary": summary, "recordingId": recordingID,
		"coordinatorEpoch": epoch, "throughSeq": throughSeq,
	})
}

func sendMixedSegment(h *WebSocketHandler, roomID string, text, recordingID string) {
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		return
	}
	h.broadcastMeetingAudioSegment(roomID, meta, text, 10, 20, recordingID, "speaker", "Speaker")
}

func segmentSeq(t *testing.T, segment map[string]interface{}) uint64 {
	t.Helper()
	value, ok := segment["seq"].(float64)
	if !ok || value < 1 {
		t.Fatal("segment sequence is missing")
	}
	return uint64(value)
}

func TestMeetingMinutes_ACMINSEQ001_ServerMixedSegmentsGetOrderedSequence(t *testing.T) {
	_, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	drainMinutes(host)
	drainMinutes(member)

	recordingID := currentMinutesRecordingID(t, host.handler, roomID)
	const count = 16
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			sendMixedSegment(host.handler, roomID, "synthetic segment", recordingID)
		}()
	}
	wg.Wait()

	for _, recipient := range []*wsRPC{host, member} {
		for expected := uint64(1); expected <= count; expected++ {
			frame, err := waitMinutesKind(recipient, "segments", 3*time.Second)
			if err != nil {
				t.Fatal("ordered segment delivery timed out")
			}
			batch := segmentsFromBatch(t, frame)
			if len(batch) != 1 || segmentSeq(t, batch[0]) != expected {
				t.Fatalf("recipient sequence was not contiguous at item %d", expected)
			}
			if batch[0]["recordingId"] != recordingID {
				t.Fatal("segment crossed recording epoch")
			}
		}
	}
}

func TestMeetingMinutes_ACMINSUM002_SummaryCursorIsMonotonicAndLegacySafe(t *testing.T) {
	_, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)
	sendMixedSegment(host.handler, roomID, "synthetic segment", recordingID)
	for _, recipient := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(recipient, "segments", 3*time.Second); err != nil {
			t.Fatal("segment delivery timed out")
		}
	}

	// Cursorless legacy summaries cannot be safely scoped to a recording or
	// coordinator generation and are rejected.
	sendMinutes(t, host, roomID, map[string]interface{}{"action": "summary", "summary": "legacy summary"})
	if _, err := host.waitError(3 * time.Second); err != nil {
		t.Fatal("cursorless summary should be rejected")
	}

	coordinator := currentMinutesCoordinatorRPC(t, host.handler, roomID, host, member)
	sendMinutes(t, coordinator, roomID, map[string]interface{}{
		"action": "summary", "summary": "shared summary", "recordingId": recordingID,
		"coordinatorEpoch": currentMinutesCoordinatorEpoch(t, host.handler, roomID), "throughSeq": 1,
	})
	for _, recipient := range []*wsRPC{host, member} {
		frame, err := waitMinutesKind(recipient, "summary", 3*time.Second)
		if err != nil {
			t.Fatal("modern summary was not broadcast")
		}
		minutes, ok := minutesPayload(t, frame)["minutes"].(map[string]interface{})
		if !ok || minutes["summaryThroughSeq"] != float64(1) {
			t.Fatal("modern summary cursor was not shared")
		}
	}

	sendMinutes(t, coordinator, roomID, map[string]interface{}{
		"action": "summary", "summary": "wrong epoch summary", "recordingId": "stale-epoch",
		"coordinatorEpoch": currentMinutesCoordinatorEpoch(t, host.handler, roomID), "throughSeq": 2,
	})
	if _, err := coordinator.waitError(3 * time.Second); err != nil {
		t.Fatal("summary from a stale recording epoch should be rejected")
	}
	assertNoMinutesKind(t, member, "summary", 250*time.Millisecond)

	sendMinutes(t, coordinator, roomID, map[string]interface{}{"action": "summary", "summary": "stale legacy summary"})
	if _, err := coordinator.waitError(3 * time.Second); err != nil {
		t.Fatal("legacy summary should be rejected after cursor advancement")
	}
	assertNoMinutesKind(t, member, "summary", 250*time.Millisecond)

	meta, ok := host.handler.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	stateJSON, err := json.Marshal(meta.minutes)
	if err != nil {
		t.Fatal("minutes state encoding failed")
	}
	var state map[string]interface{}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatal("minutes state decoding failed")
	}
	if state["summaryThroughSeq"] != float64(1) || state["summary"] != "shared summary" {
		t.Fatal("stale summary changed shared state")
	}
}

func TestMeetingMinutes_ACMINSTOP004_FinalASRAfterStopCanBeSummarized(t *testing.T) {
	_, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)

	sendMinutes(t, host, roomID, map[string]interface{}{"action": "stop"})
	ack, err := waitMinutesKind(host, "subscription", 3*time.Second)
	if err != nil || minutesPayload(t, ack)["minutesSubscription"].(map[string]interface{})["enabled"] != false {
		t.Fatal("legacy stop did not pause only the sender")
	}

	// Personal stop keeps the server recording active; remaining members still
	// receive final ASR from the same epoch.
	sendMixedSegment(host.handler, roomID, "synthetic final segment", recordingID)
	frame, err := waitMinutesKind(member, "segments", 3*time.Second)
	if err != nil {
		t.Fatal("remaining member did not receive ASR after another member paused")
	}
	batch := segmentsFromBatch(t, frame)
	if len(batch) != 1 || segmentSeq(t, batch[0]) != 1 {
		t.Fatal("ASR segment has an invalid sequence")
	}
	assertNoMinutesKind(t, host, "segments", 250*time.Millisecond)

	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "summary", "summary": "stale final summary", "recordingId": recordingID,
		"coordinatorEpoch": currentMinutesCoordinatorEpoch(t, host.handler, roomID), "throughSeq": 1,
	})
	if _, err := host.waitError(3 * time.Second); err != nil {
		t.Fatal("paused old coordinator should not be able to publish")
	}
	publishCurrentMinutesSummary(t, member, roomID, "shared final summary", 1)
	if _, err := waitMinutesKind(member, "summary", 3*time.Second); err != nil {
		t.Fatal("new coordinator could not publish the current summary")
	}
}

func TestMeetingMinutes_ACMINREPLAY003_LateJoinGetsBoundedHistoryAfterInfo(t *testing.T) {
	_, host, member, lateJoiner, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)
	sendMixedSegment(host.handler, roomID, "synthetic segment one", recordingID)
	sendMixedSegment(host.handler, roomID, "synthetic segment two", recordingID)
	for _, recipient := range []*wsRPC{host, member} {
		for i := 0; i < 2; i++ {
			if _, err := waitMinutesKind(recipient, "segments", 3*time.Second); err != nil {
				t.Fatal("segment delivery timed out")
			}
		}
	}

	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "summary", "summary": "first shared summary", "recordingId": recordingID,
		"coordinatorEpoch": currentMinutesCoordinatorEpoch(t, host.handler, roomID), "throughSeq": 1,
	})
	if _, err := waitMinutesKind(member, "summary", 3*time.Second); err != nil {
		t.Fatal("summary cursor did not advance")
	}
	if _, err := waitMinutesKind(host, "summary", 3*time.Second); err != nil {
		t.Fatal("host summary echo timed out")
	}
	drainMeetingWireEvents(lateJoiner)
	sendMinutes(t, lateJoiner, roomID, map[string]interface{}{
		"action": "replay", "recordingId": recordingID,
	})
	if _, err := lateJoiner.waitError(3 * time.Second); err != nil {
		t.Fatal("non-member replay request should be rejected")
	}

	data, _ := json.Marshal(map[string]interface{}{"roomId": roomID})
	if err := lateJoiner.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: roomID, Data: data}); err != nil {
		t.Fatal("late join request failed")
	}
	deadline := time.Now().Add(3 * time.Second)
	joined := false
	for time.Now().Before(deadline) && !joined {
		for _, member := range host.handler.meetings.members(roomID) {
			if member.UniqID == "carol:chat-3" {
				joined = true
				break
			}
		}
		// Membership is asserted below through the public room registry; this
		// loop only allows the asynchronous join handler to settle.
		if !joined {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !joined {
		t.Fatal("late join did not enter the meeting roster")
	}

	first, err := waitMeetingWireEvent(lateJoiner, 3*time.Second)
	if err != nil || first.Type != model.MessageTypeMeetingInfo {
		t.Fatal("meeting info must precede transcript replay")
	}
	var info map[string]interface{}
	if err := json.Unmarshal(first.Data, &info); err != nil {
		t.Fatal("meeting info decode failed")
	}
	minutes, ok := info["minutes"].(map[string]interface{})
	if !ok || minutes["summaryThroughSeq"] != float64(1) || minutes["latestSegmentSeq"] != float64(2) || minutes["historyStartSeq"] != float64(1) {
		t.Fatal("meeting info did not provide the shared transcript cursor")
	}

	replay, err := waitMeetingWireEvent(lateJoiner, 3*time.Second)
	if err != nil || replay.Type != model.MessageTypeMeetingMinutes {
		t.Fatal("transcript replay was not sent after meeting info")
	}
	payload := minutesPayload(t, replay)
	if payload["kind"] != "segments" || payload["replay"] != true || payload["recordingId"] != recordingID {
		t.Fatal("late join replay has an invalid epoch envelope")
	}
	batch := segmentsFromBatch(t, replay)
	if len(batch) != 2 || segmentSeq(t, batch[0]) != 1 || segmentSeq(t, batch[1]) != 2 {
		t.Fatal("late join replay did not include the bounded transcript history")
	}

	// A client controller may subscribe after meeting:info. It can explicitly
	// request the current epoch again; duplicate sequences are safe to dedupe.
	sendMinutes(t, lateJoiner, roomID, map[string]interface{}{
		"action": "replay", "recordingId": recordingID, "afterSeq": 1,
	})
	stateFrame, err := waitMeetingWireEvent(lateJoiner, 3*time.Second)
	if err != nil || minutesPayloadKind(stateFrame) != "replay-state" {
		t.Fatal("explicit replay request did not return current minutes state")
	}
	requestedReplay, err := waitMeetingWireEvent(lateJoiner, 3*time.Second)
	if err != nil || requestedReplay.Type != model.MessageTypeMeetingMinutes {
		t.Fatal("explicit replay request did not return transcript data")
	}
	requestedBatch := segmentsFromBatch(t, requestedReplay)
	if len(requestedBatch) != 1 || segmentSeq(t, requestedBatch[0]) != 2 {
		t.Fatal("explicit replay request did not honor its sequence cursor")
	}

	sendMinutes(t, lateJoiner, roomID, map[string]interface{}{
		"action": "replay", "recordingId": "stale-epoch", "afterSeq": 0,
	})
	if _, err := lateJoiner.waitError(3 * time.Second); err != nil {
		t.Fatal("replay request from a stale epoch should be rejected")
	}
	sendMinutes(t, lateJoiner, roomID, map[string]interface{}{
		"action": "replay", "recordingId": recordingID, "afterSeq": 3,
	})
	if _, err := lateJoiner.waitError(3 * time.Second); err != nil {
		t.Fatal("replay request beyond the current sequence should be rejected")
	}
}

func TestMeetingMinutes_ACMINREPLAY006_HistoryIsBoundedAndReportsItsStart(t *testing.T) {
	meta := newMeetingMeta(meetingMetaInit{})
	meta.mu.Lock()
	defer meta.mu.Unlock()
	for seq := uint64(1); seq <= meetingMinutesHistoryMaxSegments+5; seq++ {
		meta.minutes.LatestSegmentSeq = seq
		appendMeetingMinutesHistoryLocked(meta, meetingMinutesSegment{
			Seq: seq, RecordingID: "epoch", SegmentID: "segment", Text: "synthetic",
		})
	}
	if len(meta.minutesSegments) != meetingMinutesHistoryMaxSegments || meta.minutesSegmentsBytes > meetingMinutesHistoryMaxBytes {
		t.Fatal("transcript replay history exceeded its configured bound")
	}
	if meta.minutesSegments[0].Seq != 6 || meta.minutes.HistoryStartSeq != 6 {
		t.Fatal("history truncation did not report the earliest retained sequence")
	}

	byteMeta := newMeetingMeta(meetingMetaInit{})
	byteMeta.mu.Lock()
	defer byteMeta.mu.Unlock()
	for seq := uint64(1); seq <= 3; seq++ {
		byteMeta.minutes.LatestSegmentSeq = seq
		appendMeetingMinutesHistoryLocked(byteMeta, meetingMinutesSegment{
			Seq: seq, RecordingID: "epoch", SegmentID: "segment", Text: strings.Repeat("x", 100*1024),
		})
	}
	if byteMeta.minutesSegmentsBytes > meetingMinutesHistoryMaxBytes || len(byteMeta.minutesSegments) != 2 || byteMeta.minutes.HistoryStartSeq != 2 {
		t.Fatal("transcript replay history did not enforce its byte bound")
	}
}

func TestMeetingMinutes_ACMINERR007_ASRErrorIsSanitizedEpochScopedAndRateLimited(t *testing.T) {
	_, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)
	meta, ok := host.handler.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}

	host.handler.broadcastMeetingAudioASRError(roomID, meta, recordingID)
	host.handler.broadcastMeetingAudioASRError(roomID, meta, recordingID)
	for _, recipient := range []*wsRPC{host, member} {
		frame, err := waitMinutesKind(recipient, "asr-error", 3*time.Second)
		if err != nil {
			t.Fatal("current-epoch ASR error event was not delivered")
		}
		payload := minutesPayload(t, frame)
		if payload["code"] != "transcription_failed" || payload["recordingId"] != recordingID || payload["retryAfterMs"] != float64(meetingMinutesASRErrorThrottle.Milliseconds()) {
			t.Fatal("ASR error event did not follow the safe wire contract")
		}
		if _, leaked := payload["error"]; leaked {
			t.Fatal("ASR error event exposed upstream error details")
		}
	}
	assertNoMinutesKind(t, host, "asr-error", 250*time.Millisecond)
	assertNoMinutesKind(t, member, "asr-error", 250*time.Millisecond)

	meta.meetingASRPublishMu.Lock()
	meta.mu.Lock()
	meta.minutes.RecordingID = newMeetingRecordingID()
	meta.mu.Unlock()
	meta.meetingASRPublishMu.Unlock()
	host.handler.broadcastMeetingAudioASRError(roomID, meta, recordingID)
	assertNoMinutesKind(t, host, "asr-error", 250*time.Millisecond)
	assertNoMinutesKind(t, member, "asr-error", 250*time.Millisecond)
}

func TestMeetingMinutes_ACMINHOST005_HostTransferKeepsCursorWritable(t *testing.T) {
	_, host, successor, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, successor)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)
	sendMixedSegment(host.handler, roomID, "synthetic segment one", recordingID)
	sendMixedSegment(host.handler, roomID, "synthetic segment two", recordingID)
	for _, recipient := range []*wsRPC{host, successor} {
		for i := 0; i < 2; i++ {
			if _, err := waitMinutesKind(recipient, "segments", 3*time.Second); err != nil {
				t.Fatal("segment delivery timed out")
			}
		}
	}
	coordinator := currentMinutesCoordinatorRPC(t, host.handler, roomID, host, successor)
	sendMinutes(t, coordinator, roomID, map[string]interface{}{
		"action": "summary", "summary": "shared summary", "recordingId": recordingID,
		"coordinatorEpoch": currentMinutesCoordinatorEpoch(t, host.handler, roomID), "throughSeq": 1,
	})
	for _, recipient := range []*wsRPC{host, successor} {
		if _, err := waitMinutesKind(recipient, "summary", 3*time.Second); err != nil {
			t.Fatal("summary cursor did not advance")
		}
	}

	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: roomID, Data: []byte(`{}`)}); err != nil {
		t.Fatal("host leave request failed")
	}
	deadline := time.Now().Add(3 * time.Second)
	hostTransferred := false
	for time.Now().Before(deadline) && !hostTransferred {
		select {
		case changed := <-successor.hostChanged:
			var payload map[string]interface{}
			_ = json.Unmarshal(changed.Data, &payload)
			hostTransferred = payload["hostId"] == "bob:chat-2"
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !hostTransferred {
		t.Fatal("host transfer did not complete")
	}

	sendMinutes(t, successor, roomID, map[string]interface{}{
		"action": "summary", "summary": "successor summary", "recordingId": recordingID,
		"coordinatorEpoch": currentMinutesCoordinatorEpoch(t, host.handler, roomID), "throughSeq": 2,
	})
	if _, err := waitMinutesKind(successor, "summary", 3*time.Second); err != nil {
		t.Fatal("new host could not advance the current epoch cursor")
	}
}
