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
)

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

func TestMeetingAudioMinutesMixIncludesLatePublisherWithoutConsent(t *testing.T) {
	meta := newMeetingMeta(meetingMetaInit{})
	meta.minutes.Running = true
	meta.minutes.RecordingID = "recording-1"
	base := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	session := &meetingAudioMinutesSession{
		meta: meta, recordingID: "recording-1", vad: newMeetingAudioVAD(),
		queue: make(chan meetingAudioBatch, 4), decoders: make(map[string]*meetingAudioDecoder),
		energy: make(map[string]int64),
	}

	firstPublisher := &meetingAudioDecoder{
		participantID: "first", ring: make([]int16, meetingAudioRingSample), lastSeen: base,
	}
	firstPublisher.push(repeatedSamples(1000, 2*meetingAudioFrameSample))
	session.decoders["first-track"] = firstPublisher
	session.mixTick(base.Add(meetingAudioVADFrameDuration))

	// This publisher joins after minutes start and has no legacy consent entry.
	latePublisher := &meetingAudioDecoder{
		participantID: "late", ring: make([]int16, meetingAudioRingSample), lastSeen: base.Add(meetingAudioVADFrameDuration),
	}
	latePublisher.push(repeatedSamples(2000, meetingAudioFrameSample))
	session.decoders["late-track"] = latePublisher
	session.mixTick(base.Add(2 * meetingAudioVADFrameDuration))
	session.finalFlush()

	batch := <-session.queue
	if batch.recordingID != "recording-1" {
		t.Fatalf("recording ID = %q", batch.recordingID)
	}
	if batch.startMs != base.UnixMilli() || batch.endMs != base.Add(2*meetingAudioVADFrameDuration).UnixMilli() {
		t.Fatalf("capture interval = [%d, %d], want [%d, %d)", batch.startMs, batch.endMs, base.UnixMilli(), base.Add(2*meetingAudioVADFrameDuration).UnixMilli())
	}
	got := int16(binary.LittleEndian.Uint16(batch.pcm[len(batch.pcm)-2:]))
	if got != 3000 {
		t.Fatalf("mixed late-publisher sample = %d, want 3000", got)
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
		meta: meta, recordingID: "recording-2", vad: newMeetingAudioVAD(),
		queue: make(chan meetingAudioBatch, 4), decoders: make(map[string]*meetingAudioDecoder),
		energy: make(map[string]int64),
	}
	for frame := 0; frame < 100; frame++ {
		at := base.Add(time.Duration(frame+1) * meetingAudioVADFrameDuration)
		session.mixTick(at)
	}
	if len(session.queue) != 0 || len(session.buffer) != 0 {
		t.Fatalf("silence entered ASR queue/buffer: queue=%d buffer=%d", len(session.queue), len(session.buffer))
	}

	voice := meetingAudioVADPCMFrame(180) // quiet speech below the old fixed threshold
	for frame := 100; frame < 105; frame++ {
		at := base.Add(time.Duration(frame+1) * meetingAudioVADFrameDuration)
		session.mu.Lock()
		segments := session.vad.PushFrame(voice, at.Add(-meetingAudioVADFrameDuration))
		for _, segment := range segments {
			session.appendSpeechSegmentLocked(segment)
		}
		session.mu.Unlock()
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
		meta: meta, recordingID: "recording-boundary", vad: newMeetingAudioVAD(),
		queue: make(chan meetingAudioBatch, 4), decoders: make(map[string]*meetingAudioDecoder),
		energy: make(map[string]int64), buffer: make([]byte, 0, meetingAudioBatchBytes),
	}

	firstVoice := &meetingAudioDecoder{participantID: "first", ring: make([]int16, meetingAudioRingSample), lastSeen: base}
	firstVoice.push(repeatedSamples(500, meetingAudioFrameSample))
	session.decoders["first-track"] = firstVoice
	session.mixTick(base.Add(meetingAudioVADFrameDuration))
	delete(session.decoders, "first-track")
	for frame := 1; frame < meetingAudioBatchSample/meetingAudioFrameSample; frame++ {
		session.mixTick(base.Add(time.Duration(frame+1) * meetingAudioVADFrameDuration))
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
	lateVoice := &meetingAudioDecoder{participantID: "late", ring: make([]int16, meetingAudioRingSample), lastSeen: voiceStart.Add(meetingAudioVADFrameDuration)}
	lateVoice.push(repeatedSamples(180, meetingAudioFrameSample))
	session.decoders["late-track"] = lateVoice
	session.mixTick(voiceStart.Add(meetingAudioVADFrameDuration))
	for frame := 1; frame <= 5; frame++ {
		session.mixTick(voiceStart.Add(time.Duration(frame+1) * meetingAudioVADFrameDuration))
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
	session := &meetingAudioMinutesSession{
		recordingID: "recording-3", buffer: make([]byte, 0, meetingAudioBatchBytes),
		energy: make(map[string]int64),
	}
	oneFrame := meetingAudioVADPCMFrame(500)
	segmentOne := meetingAudioSpeechSegment{Start: base, End: base.Add(100 * time.Millisecond), PCM: bytes.Repeat(oneFrame, 5)}
	segmentTwo := meetingAudioSpeechSegment{Start: base.Add(time.Second), End: base.Add(1100 * time.Millisecond), PCM: bytes.Repeat(oneFrame, 5)}
	session.mu.Lock()
	if batches := session.appendSpeechSegmentLocked(segmentOne); len(batches) != 0 {
		t.Fatalf("first short span flushed early: %d", len(batches))
	}
	if batches := session.appendSpeechSegmentLocked(segmentTwo); len(batches) != 0 {
		t.Fatalf("coalesced spans flushed early: %d", len(batches))
	}
	batches := session.takeBatchLocked()
	session.mu.Unlock()
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
	session := &meetingAudioMinutesSession{
		recordingID: "recording-reserve", vad: newMeetingAudioVAD(),
		queue: make(chan meetingAudioBatch, 4), energy: make(map[string]int64),
	}
	for index := 0; index < 3; index++ {
		session.enqueue(meetingAudioBatch{recordingID: "recording-reserve", pcm: []byte{1}})
	}
	if len(session.queue) != cap(session.queue)-meetingAudioFinalQueueReserve {
		t.Fatalf("normal queue use = %d, want to preserve %d final slots", len(session.queue), meetingAudioFinalQueueReserve)
	}
	session.vad.PushFrame(meetingAudioVADPCMFrame(500), base)
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
