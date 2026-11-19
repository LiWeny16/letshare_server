package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

func testMeetingAudioSpeaker(id string, decoder *meetingAudioDecoder) *meetingAudioSpeaker {
	return &meetingAudioSpeaker{id: id, name: id + " name", vad: newMeetingAudioVAD(), tracks: map[*webrtc.TrackRemote]*meetingAudioDecoder{nil: decoder}}
}

func TestMeetingAudioRingKeepsNewestSamples(t *testing.T) {
	decoder := &meetingAudioDecoder{ring: make([]int16, 4)}
	decoder.push([]int16{1, 2, 3, 4, 5})
	out := make([]int16, 4)
	if !decoder.popFrame(out) {
		t.Fatal("popFrame returned false")
	}
	want := []int16{2, 3, 4, 5}
	for index := range want {
		if out[index] != want[index] {
			t.Fatalf("ring[%d] = %d, want %d", index, out[index], want[index])
		}
	}
}

func TestMeetingAudioMinutesKeepsOverlappingLatePublisherAudioSeparate(t *testing.T) {
	meta := newMeetingMeta(meetingMetaInit{})
	meta.minutes.Running = true
	meta.minutes.RecordingID = "recording-1"
	base := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	session := &meetingAudioMinutesSession{
		meta: meta, recordingID: "recording-1", queue: make(chan meetingAudioBatch, 4),
		speakers: make(map[string]*meetingAudioSpeaker),
	}

	firstPublisher := &meetingAudioDecoder{
		ring: make([]int16, meetingAudioRingSample), lastSeen: base,
	}
	firstPublisher.push(repeatedSamples(1000, 2*meetingAudioFrameSample))
	session.speakers["first"] = testMeetingAudioSpeaker("first", firstPublisher)
	session.captureTick(base.Add(meetingAudioVADFrameDuration))

	// This publisher joins after minutes start and has no legacy consent entry.
	latePublisher := &meetingAudioDecoder{
		ring: make([]int16, meetingAudioRingSample), lastSeen: base.Add(meetingAudioVADFrameDuration),
	}
	latePublisher.push(repeatedSamples(2000, meetingAudioFrameSample))
	session.speakers["late"] = testMeetingAudioSpeaker("late", latePublisher)
	session.captureTick(base.Add(2 * meetingAudioVADFrameDuration))
	session.finalFlush()

	if len(session.queue) != 2 {
		t.Fatalf("expected one ASR batch per publisher, got %d", len(session.queue))
	}
	for _, expected := range []struct {
		id     string
		start  time.Time
		frames int
		sample int16
	}{
		{"first", base, 2, 1000},
		{"late", base.Add(meetingAudioVADFrameDuration), 1, 2000},
	} {
		batch := <-session.queue
		if batch.speakerID != expected.id || batch.speakerName != expected.id+" name" || batch.recordingID != "recording-1" {
			t.Fatalf("batch speaker=%q name=%q recording=%q, want %q in recording-1", batch.speakerID, batch.speakerName, batch.recordingID, expected.id)
		}
		if batch.startMs != expected.start.UnixMilli() || batch.endMs != base.Add(2*meetingAudioVADFrameDuration).UnixMilli() {
			t.Fatalf("%s capture interval = [%d, %d)", expected.id, batch.startMs, batch.endMs)
		}
		if len(batch.pcm) != expected.frames*meetingAudioFrameBytes {
			t.Fatalf("%s has %d PCM bytes", expected.id, len(batch.pcm))
		}
		for offset := 0; offset < len(batch.pcm); offset += 2 {
			if got := int16(binary.LittleEndian.Uint16(batch.pcm[offset:])); got != expected.sample {
				t.Fatalf("%s PCM contained another participant's audio: got %d, want %d", expected.id, got, expected.sample)
			}
		}
	}
	if _, consentEntryExists := meta.minutes.Consented["late"]; consentEntryExists {
		t.Fatal("test setup unexpectedly created a consent entry for the late publisher")
	}
}

func TestMeetingAudioMinutesSuppressesSilenceAndFlushesSpeechOnStop(t *testing.T) {
	meta := newMeetingMeta(meetingMetaInit{})
	meta.minutes.Running = true
	meta.minutes.RecordingID = "recording-2"
	base := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	session := &meetingAudioMinutesSession{
		meta: meta, recordingID: "recording-2", queue: make(chan meetingAudioBatch, 4),
		speakers: make(map[string]*meetingAudioSpeaker),
	}
	decoder := &meetingAudioDecoder{ring: make([]int16, meetingAudioRingSample), lastSeen: base}
	speaker := testMeetingAudioSpeaker("speaker", decoder)
	session.speakers[speaker.id] = speaker
	for frame := 0; frame < 100; frame++ {
		at := base.Add(time.Duration(frame+1) * meetingAudioVADFrameDuration)
		session.captureTick(at)
	}
	if len(session.queue) != 0 || len(speaker.buffer) != 0 {
		t.Fatalf("silence entered ASR queue/buffer: queue=%d buffer=%d", len(session.queue), len(speaker.buffer))
	}

	voice := repeatedSamples(180, meetingAudioFrameSample) // quiet speech below the old fixed threshold
	for frame := 100; frame < 105; frame++ {
		at := base.Add(time.Duration(frame+1) * meetingAudioVADFrameDuration)
		decoder.lastSeen = at
		decoder.push(voice)
		session.captureTick(at)
	}
	session.finalFlush()
	select {
	case batch := <-session.queue:
		if batch.recordingID != "recording-2" || len(batch.pcm) == 0 {
			t.Fatalf("stop flush lost pending speech: %#v", batch)
		}
		wantStart := base.Add(time.Duration(100-meetingAudioVADPreRollFrames) * meetingAudioVADFrameDuration).UnixMilli()
		if batch.startMs != wantStart {
			t.Fatalf("flushed speech start = %d, want %d", batch.startMs, wantStart)
		}
	default:
		t.Fatal("stop did not flush pending speech to the bounded ASR queue")
	}
}

func TestMeetingAudioMinutesKeepsVADPreRollAcrossBatchBoundary(t *testing.T) {
	meta := newMeetingMeta(meetingMetaInit{})
	meta.minutes.Running = true
	meta.minutes.RecordingID = "recording-boundary"
	base := time.Now().Truncate(meetingAudioVADFrameDuration)
	session := &meetingAudioMinutesSession{
		meta: meta, recordingID: "recording-boundary", queue: make(chan meetingAudioBatch, 4),
		speakers: make(map[string]*meetingAudioSpeaker),
	}

	decoder := &meetingAudioDecoder{ring: make([]int16, meetingAudioRingSample), lastSeen: base}
	decoder.push(repeatedSamples(500, meetingAudioFrameSample))
	session.speakers["speaker"] = testMeetingAudioSpeaker("speaker", decoder)
	session.captureTick(base.Add(meetingAudioVADFrameDuration))
	for frame := 1; frame < meetingAudioBatchSample/meetingAudioFrameSample; frame++ {
		session.captureTick(base.Add(time.Duration(frame+1) * meetingAudioVADFrameDuration))
	}
	if len(session.queue) != 1 {
		t.Fatalf("first wall-clock batch count = %d, want 1", len(session.queue))
	}
	firstBatch := <-session.queue
	if firstBatch.recordingID != "recording-boundary" {
		t.Fatalf("first batch recording ID = %q", firstBatch.recordingID)
	}

	// The next speech frame begins just after the ten-second batch boundary.
	voiceStart := base.Add(meetingAudioBatchDuration)
	decoder.lastSeen = voiceStart.Add(meetingAudioVADFrameDuration)
	decoder.push(repeatedSamples(180, meetingAudioFrameSample))
	session.captureTick(voiceStart.Add(meetingAudioVADFrameDuration))
	for frame := 1; frame <= 5; frame++ {
		session.captureTick(voiceStart.Add(time.Duration(frame+1) * meetingAudioVADFrameDuration))
	}
	session.finalFlush()
	secondBatch := <-session.queue
	wantStart := voiceStart.Add(-meetingAudioVADPreRoll).UnixMilli()
	if secondBatch.startMs != wantStart {
		t.Fatalf("post-boundary speech start = %d, want %d with retained pre-roll", secondBatch.startMs, wantStart)
	}
}

func TestMeetingAudioMinutesCoalescesSpeechWithBoundedSilencePadding(t *testing.T) {
	base := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	speaker := &meetingAudioSpeaker{id: "speaker"}
	oneFrame := meetingAudioVADPCMFrame(500)
	segmentOne := meetingAudioSpeechSegment{Start: base, End: base.Add(100 * time.Millisecond), PCM: bytes.Repeat(oneFrame, 5)}
	segmentTwo := meetingAudioSpeechSegment{Start: base.Add(time.Second), End: base.Add(1100 * time.Millisecond), PCM: bytes.Repeat(oneFrame, 5)}
	if batches := speaker.appendSpeechSegment(segmentOne); len(batches) != 0 {
		t.Fatalf("first short span flushed early: %d", len(batches))
	}
	if batches := speaker.appendSpeechSegment(segmentTwo); len(batches) != 0 {
		t.Fatalf("coalesced spans flushed early: %d", len(batches))
	}
	batches := speaker.takeBatch()
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(batches))
	}
	batch := batches[0]
	wantFrames := 5 + int(meetingAudioGapPad/meetingAudioVADFrameDuration) + 5
	if got := len(batch.pcm) / meetingAudioFrameBytes; got != wantFrames {
		t.Fatalf("coalesced PCM frames = %d, want %d", got, wantFrames)
	}
	if batch.startMs != base.UnixMilli() || batch.endMs != base.Add(1100*time.Millisecond).UnixMilli() {
		t.Fatalf("coalesced capture interval = [%d, %d], want [%d, %d)", batch.startMs, batch.endMs, base.UnixMilli(), base.Add(1100*time.Millisecond).UnixMilli())
	}
}

func TestMeetingAudioMinutesReservesQueueForStopFlush(t *testing.T) {
	base := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	speaker := &meetingAudioSpeaker{id: "speaker", vad: newMeetingAudioVAD()}
	session := &meetingAudioMinutesSession{
		recordingID: "recording-reserve", queue: make(chan meetingAudioBatch, 4),
		speakers: map[string]*meetingAudioSpeaker{speaker.id: speaker},
	}
	for index := 0; index < 3; index++ {
		session.enqueue(meetingAudioBatch{pcm: []byte{1}}, false)
	}
	if len(session.queue) != cap(session.queue)-meetingAudioFinalQueueReserve {
		t.Fatalf("normal queue use = %d, want to preserve %d final slots", len(session.queue), meetingAudioFinalQueueReserve)
	}
	speaker.vad.PushFrame(meetingAudioVADPCMFrame(500), base)
	session.finalFlush()
	if len(session.queue) != cap(session.queue)-meetingAudioFinalQueueReserve+1 {
		t.Fatalf("stop flush was not queued in the reserved slot: queue=%d", len(session.queue))
	}
}

func repeatedSamples(value int16, count int) []int16 {
	samples := make([]int16, count)
	for index := range samples {
		samples[index] = value
	}
	return samples
}

func TestAppendASRText(t *testing.T) {
	if got := appendASRText("", "你好"); got != "你好" {
		t.Fatalf("empty prefix = %q", got)
	}
	if got := appendASRText("你好", "世界"); got != "你好世界" {
		t.Fatalf("CJK join = %q", got)
	}
	if got := appendASRText("hello", "world"); got != "hello world" {
		t.Fatalf("latin join = %q", got)
	}
}

func TestMeetingASRStreamTranscribeBatchProtocol(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("login method = %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"access_token": "test-token", "access_token_expires_in": 3600},
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var start struct {
			Event string `json:"event"`
			Rate  int    `json:"rate"`
		}
		if json.Unmarshal(payload, &start) != nil || start.Event != "start" || start.Rate != meetingAudioSampleRate {
			t.Errorf("unexpected start frame: %s", payload)
			return
		}
		_ = conn.WriteJSON(map[string]string{"event": "started"})

		var pcmBytes int
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if messageType == websocket.BinaryMessage {
				pcmBytes += len(data)
				continue
			}
			var event struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(data, &event) != nil {
				continue
			}
			if event.Event == "stop" {
				break
			}
		}
		if pcmBytes != 6400 {
			t.Errorf("uploaded %d PCM bytes, want 6400", pcmBytes)
		}
		_ = conn.WriteJSON(map[string]any{"event": "result", "data": map[string]any{"text": "你好", "isFinal": true}})
		_ = conn.WriteJSON(map[string]any{"event": "result", "data": map[string]any{"text": "world", "isFinal": true}})
		_ = conn.WriteJSON(map[string]string{"event": "stopped"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	service := NewMeetingASRStreamService(MeetingASRStreamConfig{
		HTTPBase: server.URL,
		WSBase:   "ws" + strings.TrimPrefix(server.URL, "http") + "/ws",
		Email:    "test@example.com",
		Password: "test-password",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, err := service.Transcribe(ctx, make([]byte, 6400))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "你好world" {
		t.Fatalf("text = %q", text)
	}
}
