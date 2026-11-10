package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMeetingAIRequestMembershipPermissionsAndPrivateResponse(t *testing.T) {
	var upstreamCalls atomic.Int32
	var summaryLanguageSeen atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "Simplified Chinese") {
			summaryLanguageSeen.Store("zh")
		}
		if strings.Contains(string(body), "English") {
			summaryLanguageSeen.Store("en")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"private result"}}]}`)
	}))
	defer upstream.Close()

	_, host, member, outsider, meetingID := meetingChatFlowWithSetup(t, func(h *WebSocketHandler) {
		h.captionsEnabled = true
		h.SetMeetingAIService(NewMeetingAIService("test-only", upstream.URL, true, true, upstream.Client()))
	})
	request := func(r *wsRPC, id, task string) {
		payload := map[string]interface{}{"action": "ai", "requestId": id, "task": task, "input": "private input", "targetLanguage": "en"}
		if task == MeetingAITaskSummary {
			payload["recordingId"] = currentMinutesRecordingID(t, r.handler, meetingID)
			payload["coordinatorEpoch"] = currentMinutesCoordinatorEpoch(t, r.handler, meetingID)
			payload["throughSeq"] = uint64(1)
		}
		sendMinutes(t, r, meetingID, payload)
	}
	result := func(r *wsRPC, id string, expected bool) {
		msg, err := waitMinutesKind(r, "ai-result", 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		data := minutesPayload(t, msg)
		if data["requestId"] != id || data["ok"] != expected {
			t.Fatalf("unexpected result: %#v", data)
		}
		if expected && data["output"] != "private result" {
			t.Fatalf("missing private response: %#v", data)
		}
		if !expected && data["output"] != "" {
			t.Fatalf("failed request leaked output: %#v", data)
		}
	}

	request(outsider, "outsider-1", "translate")
	if _, err := outsider.waitError(3 * time.Second); err != nil {
		t.Fatalf("outsider was not rejected: %v", err)
	}
	request(member, "not-viewing", "translate")
	result(member, "not-viewing", false)
	if upstreamCalls.Load() != 0 {
		t.Fatal("unauthorized requests reached provider")
	}

	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "caption-subscribe", "enabled": true})
	if _, err := waitMinutesKind(member, "captions", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	request(member, "viewer", "translate")
	result(member, "viewer", true)
	if _, err := waitMinutesKind(host, "ai-result", 200*time.Millisecond); err == nil {
		t.Fatal("translation result leaked to another member")
	}

	configureAndMarkMinutesRunningForRelayTests(t, host, meetingID, true, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, meetingID)
	sendMixedSegment(host.handler, meetingID, "synthetic summary input", recordingID)
	for _, r := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(r, "segments", 3*time.Second); err != nil {
			t.Fatal("summary test transcript did not arrive")
		}
	}
	summaryCoordinator := currentMinutesCoordinatorRPC(t, host.handler, meetingID, host, member)
	nonCoordinator := host
	if summaryCoordinator == host {
		nonCoordinator = member
	}
	request(nonCoordinator, "not-coordinator", "summary")
	result(nonCoordinator, "not-coordinator", false)
	request(summaryCoordinator, "coordinator-summary", "summary")
	result(summaryCoordinator, "coordinator-summary", true)
	request(summaryCoordinator, "coordinator-summary-retry", "summary")
	result(summaryCoordinator, "coordinator-summary-retry", true)
	if seen, _ := summaryLanguageSeen.Load().(string); seen != "zh" {
		t.Fatalf("room summary language did not map to provider language code: %q", seen)
	}
	request(member, "member-live-notes", "live-notes")
	result(member, "member-live-notes", false)
	request(host, "host-live-notes", "live-notes")
	result(host, "host-live-notes", false)
	request(host, "host-final-minutes", "final-minutes")
	result(host, "host-final-minutes", false)
	request(member, "private-question", "question")
	result(member, "private-question", true)
	if _, err := waitMinutesKind(host, "ai-result", 200*time.Millisecond); err == nil {
		t.Fatal("question result leaked to host")
	}
	if upstreamCalls.Load() != 3 {
		t.Fatalf("provider calls=%d, want 3 (duplicate cursor must use cached completion)", upstreamCalls.Load())
	}
}

func TestMeetingSummaryCoordinatorHandoverDropsStaleResultAndSerializesRequests(t *testing.T) {
	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		current := active.Add(1)
		for previous := maxActive.Load(); current > previous && !maxActive.CompareAndSwap(previous, current); previous = maxActive.Load() {
		}
		defer active.Add(-1)
		if call == 1 {
			firstStarted <- struct{}{}
			<-releaseFirst
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"stale completion"}}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"successor completion"}}]}`)
	}))
	defer upstream.Close()
	defer releaseOnce.Do(func() { close(releaseFirst) })

	_, host, member, _, roomID := meetingChatFlowWithSetup(t, func(h *WebSocketHandler) {
		h.captionsEnabled = true
		h.SetMeetingAIService(NewMeetingAIService("test-only", upstream.URL, true, true, upstream.Client()))
	})
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)
	sendMixedSegment(host.handler, roomID, "synthetic summary input", recordingID)
	for _, client := range []*wsRPC{host, member} {
		if _, err := waitMinutesKind(client, "segments", 3*time.Second); err != nil {
			t.Fatal("summary transcript did not arrive")
		}
	}
	oldCoordinator := currentMinutesCoordinatorRPC(t, host.handler, roomID, host, member)
	successor := member
	if oldCoordinator == member {
		successor = host
	}
	firstEpoch := currentMinutesCoordinatorEpoch(t, host.handler, roomID)
	sendMinutes(t, oldCoordinator, roomID, map[string]interface{}{
		"action": "ai", "requestId": "old-coordinator", "task": MeetingAITaskSummary,
		"input": "synthetic summary input", "recordingId": recordingID,
		"coordinatorEpoch": firstEpoch, "throughSeq": uint64(1),
	})
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("old coordinator AI request did not reach the test provider")
	}

	// The old completion is still in flight while the coordinator opts out.
	sendMinutes(t, oldCoordinator, roomID, map[string]interface{}{"action": "stop"})
	if _, err := waitMinutesKind(oldCoordinator, "subscription", 3*time.Second); err != nil {
		t.Fatal("old coordinator could not pause while AI work was in flight")
	}
	if _, err := waitMinutesKind(successor, "coordinator", 3*time.Second); err != nil {
		t.Fatal("successor coordinator was not elected")
	}
	secondEpoch := currentMinutesCoordinatorEpoch(t, host.handler, roomID)
	if secondEpoch <= firstEpoch {
		t.Fatal("coordinator epoch did not advance after handover")
	}
	sendMinutes(t, successor, roomID, map[string]interface{}{
		"action": "ai", "requestId": "new-coordinator", "task": MeetingAITaskSummary,
		"input": "synthetic summary input", "recordingId": recordingID,
		"coordinatorEpoch": secondEpoch, "throughSeq": uint64(1),
	})
	time.Sleep(150 * time.Millisecond)
	if calls.Load() != 1 || maxActive.Load() != 1 {
		t.Fatalf("room summary calls were not serialized during handover: calls=%d max_active=%d", calls.Load(), maxActive.Load())
	}

	releaseOnce.Do(func() { close(releaseFirst) })
	result, err := waitMinutesKind(successor, "ai-result", 3*time.Second)
	if err != nil {
		t.Fatal("successor AI request did not complete")
	}
	resultPayload := minutesPayload(t, result)
	if resultPayload["requestId"] != "new-coordinator" || resultPayload["ok"] != true || resultPayload["output"] != "successor completion" {
		t.Fatalf("unexpected successor AI result: %#v", resultPayload)
	}
	if _, err := waitMinutesKind(oldCoordinator, "ai-result", 200*time.Millisecond); err == nil {
		t.Fatal("stale AI result leaked to the paused former coordinator")
	}
	if calls.Load() != 2 || maxActive.Load() != 1 {
		t.Fatalf("handover did not retry once without overlapping provider calls: calls=%d max_active=%d", calls.Load(), maxActive.Load())
	}
}
