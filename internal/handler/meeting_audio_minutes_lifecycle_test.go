package handler

import (
	"testing"
	"time"

	"letshare-server/internal/model"
)

func meetingASRTestClient(t *testing.T, ts *meetingTestServer, roomID, uniqID string) *model.Client {
	t.Helper()
	clientID, ok := ts.handler.meetings.clientForUniqID(roomID, uniqID)
	if !ok {
		t.Fatalf("meeting member %q is missing", uniqID)
	}
	client, ok := ts.wsService.GetClient(clientID)
	if !ok {
		t.Fatalf("websocket client %q is missing", clientID)
	}
	return client
}

func configureAndStartMinutesWithReadyServices(t *testing.T, host *wsRPC, roomID string, watchers ...*wsRPC) {
	t.Helper()
	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "server-stream", "summaryProvider": "gemini",
	})
	for _, watcher := range watchers {
		if _, err := waitMinutesKind(watcher, "configured", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	sendMinutes(t, host, roomID, map[string]interface{}{"action": "start"})
	for _, watcher := range watchers {
		if _, err := waitMinutesKind(watcher, "started", 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMeetingMinutesStartRequiresGeminiAndSomniReadiness(t *testing.T) {
	cases := []struct {
		name      string
		gemini    bool
		somni     bool
		wantStart bool
	}{
		{name: "neither provider"},
		{name: "Somni only", somni: true},
		{name: "Gemini only", gemini: true},
		{name: "both providers", gemini: true, somni: true, wantStart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, host, member, _, roomID := meetingChatFlowWithSetup(t, func(h *WebSocketHandler) {
				if tc.somni {
					h.SetMeetingASRStreamService(NewMeetingASRStreamService(MeetingASRStreamConfig{
						HTTPBase: "https://somni.test", WSBase: "wss://somni.test/stream",
						Email: "test@example.test", Password: "test-only",
					}))
				}
				if tc.gemini {
					h.SetMeetingAIService(NewMeetingAIService("test-only", "https://gemini.test/v1", true, true, nil))
				}
			})
			sendMinutes(t, host, roomID, map[string]interface{}{"action": "configure"})
			for _, watcher := range []*wsRPC{host, member} {
				if _, err := waitMinutesKind(watcher, "configured", 3*time.Second); err != nil {
					t.Fatal(err)
				}
			}

			starter := host
			startPayload := map[string]interface{}{"action": "start"}
			if tc.wantStart {
				starter = member
				startPayload["summaryLanguage"] = "en-US"
			}
			sendMinutes(t, starter, roomID, startPayload)
			meta, _ := ts.handler.activeMeetingRoom(roomID)
			if !tc.wantStart {
				if message, err := host.waitError(3 * time.Second); err != nil || message == "" {
					t.Fatalf("start without both providers should fail clearly, message=%q err=%v", message, err)
				}
				assertNoMinutesKind(t, member, "started", 100*time.Millisecond)
				meta.mu.Lock()
				running := meta.minutes.Running
				meta.mu.Unlock()
				if running {
					t.Fatal("unready providers marked minutes running")
				}
				if _, ok := ts.handler.meetingAudioSessions.Load(roomID); ok {
					t.Fatal("unready providers installed a server mixer")
				}
				return
			}

			for _, watcher := range []*wsRPC{host, member} {
				started, err := waitMinutesKind(watcher, "started", 3*time.Second)
				if err != nil {
					t.Fatalf("ready providers should start minutes: %v", err)
				}
				if minutesMapFromFrame(t, started)["running"] != true {
					t.Fatalf("ready provider start did not report running: %s", string(started.Data))
				}
			}
			if _, ok := ts.handler.meetingAudioSessions.Load(roomID); !ok {
				t.Fatal("ready provider start did not install the server mixer")
			}
			meta.mu.Lock()
			firstRecordingID := meta.minutes.RecordingID
			firstSummaryLanguage := meta.minutes.SummaryLanguage
			firstCoordinatorID := meta.minutes.SummaryCoordinatorID
			firstCoordinatorEpoch := meta.minutes.SummaryCoordinatorEpoch
			meta.mu.Unlock()
			if firstRecordingID == "" || firstSummaryLanguage != "en-US" || firstCoordinatorID != "bob:chat-2" {
				t.Fatalf("non-host start did not establish the shared epoch: recording=%t language=%q coordinator=%q", firstRecordingID != "", firstSummaryLanguage, firstCoordinatorID)
			}

			// A running start from another member restores only that member's
			// subscription and must leave the room epoch, coordinator, and
			// room-wide summary language unchanged.
			sendMinutes(t, host, roomID, map[string]interface{}{"action": "stop", "recordingId": "stale-recording-epoch"})
			if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
				t.Fatal("legacy stop with a stale recording ID did not pause the requester")
			}
			sendMinutes(t, host, roomID, map[string]interface{}{"action": "start", "summaryLanguage": "zh-CN"})
			if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
				t.Fatal("running start did not resume the requester")
			}
			assertNoMinutesKind(t, member, "started", 150*time.Millisecond)
			meta.mu.Lock()
			unchanged := meta.minutes.Running && meta.minutes.RecordingID == firstRecordingID &&
				meta.minutes.SummaryLanguage == firstSummaryLanguage &&
				meta.minutes.SummaryCoordinatorID == firstCoordinatorID &&
				meta.minutes.SummaryCoordinatorEpoch == firstCoordinatorEpoch
			meta.mu.Unlock()
			if !unchanged {
				t.Fatal("running start reset the shared recording or changed its language/coordinator")
			}
			sendMinutes(t, host, roomID, map[string]interface{}{"action": "stop"})
			if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
				t.Fatal("legacy stop did not pause only the requester")
			}
			if _, ok := ts.handler.meetingAudioSessions.Load(roomID); !ok {
				t.Fatal("personal pause stopped the room mixer")
			}
			meta.mu.Lock()
			running := meta.minutes.Running
			meta.mu.Unlock()
			if !running {
				t.Fatal("personal pause stopped shared minutes")
			}
			ts.handler.End(roomID, meetingEndReasonHostEnded)
		})
	}
}
func TestMeetingAudioMinutesMeetingEndCleansUpCapture(t *testing.T) {
	ts, host, member, _, roomID := meetingChatFlowWithSetup(t, func(h *WebSocketHandler) {
		h.SetMeetingASRStreamService(NewMeetingASRStreamService(MeetingASRStreamConfig{
			HTTPBase: "https://somni.test", WSBase: "wss://somni.test/stream",
			Email: "test@example.test", Password: "test-only",
		}))
		h.SetMeetingAIService(NewMeetingAIService("test-only", "https://gemini.test/v1", true, true, nil))
	})
	configureAndStartMinutesWithReadyServices(t, host, roomID, host, member)

	var session *meetingAudioMinutesSession
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if value, ok := ts.handler.meetingAudioSessions.Load(roomID); ok {
			session, _ = value.(*meetingAudioMinutesSession)
			if session != nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if session == nil {
		t.Fatal("host start did not install the room audio mixer")
	}

	memberClient := meetingASRTestClient(t, ts, roomID, "bob:chat-2")
	session.mu.Lock()
	session.decoders["member-track"] = &meetingAudioDecoder{participantID: memberClient.UniqID}
	session.mu.Unlock()
	leaveMeeting(t, member, roomID)
	waitMembersRegistered(t, ts, roomID, 1)
	session.mu.Lock()
	_, decoderRetained := session.decoders["member-track"]
	session.mu.Unlock()
	if decoderRetained {
		t.Fatal("member leave retained that publisher's decoder ring")
	}
	if _, ok := ts.handler.meetingAudioSessions.Load(roomID); !ok {
		t.Fatal("one member leaving stopped the remaining room's mixer")
	}

	sendMinutes(t, host, roomID, map[string]interface{}{"action": "stop"})
	if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
		t.Fatal("personal stop was not acknowledged")
	}
	if _, ok := ts.handler.meetingAudioSessions.Load(roomID); !ok || session.closed.Load() {
		t.Fatal("personal stop shut down the room mixer")
	}
	ts.handler.End(roomID, meetingEndReasonHostEnded)
	if _, ok := ts.handler.meetingAudioSessions.Load(roomID); ok {
		t.Fatal("meeting end left the room audio mixer registered")
	}
	if !session.closed.Load() {
		t.Fatal("meeting end did not close the mixer session")
	}
	select {
	case <-session.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("meeting end did not cancel mixer capture")
	}
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("meeting end did not finish the mixer loop")
	}
	select {
	case <-session.transcribeDone:
	case <-time.After(time.Second):
		t.Fatal("empty ASR queue did not finish after meeting end")
	}
}
