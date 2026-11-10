package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"letshare-server/internal/sfu"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// The fixture is synthesized speech encoded as mono 20 ms Opus packets. This
// verifies the real SFU tap, decoder and VAD with browser-compatible media.
// The opt-in path forwards that same decoded PCM to the real Somni service.
func TestMeetingAudioPipelineOpusRTPToASR(t *testing.T) {
	live := os.Getenv("LETSHARE_RUN_REAL_OPUS_ASR") == "1"
	var liveASR *MeetingASRStreamService
	if live {
		envPath := os.Getenv("LETSHARE_MEETING_ENV_FILE")
		if envPath == "" {
			envPath = filepath.Join("..", "..", ".env")
		}
		if _, err := os.Stat(envPath); err == nil {
			if err := godotenv.Load(envPath); err != nil {
				t.Fatal("could not load the configured local meeting environment")
			}
		}
		liveASR = NewMeetingASRStreamService(MeetingASRStreamConfig{
			HTTPBase:       meetingLiveEnvFirst("MEETING_ASR_STREAM_HTTP_BASE", "SOMNI_ASR_HTTP_BASE"),
			WSBase:         meetingLiveEnvFirst("MEETING_ASR_STREAM_WS_BASE", "SOMNI_ASR_WS_BASE"),
			Email:          meetingLiveEnvFirst("MEETING_ASR_STREAM_EMAIL", "SOMNI_ASR_EMAIL"),
			Password:       meetingLiveEnvFirst("MEETING_ASR_STREAM_PASSWORD", "SOMNI_ASR_PASSWORD"),
			LoginType:      meetingLiveEnvFirst("MEETING_ASR_STREAM_LOGIN_TYPE", "SOMNI_ASR_LOGIN_TYPE"),
			DevicePlatform: meetingLiveEnvFirst("MEETING_ASR_STREAM_DEVICE_PLATFORM", "SOMNI_ASR_DEVICE_PLATFORM"),
			DeviceName:     meetingLiveEnvFirst("MEETING_ASR_STREAM_DEVICE_NAME", "SOMNI_ASR_DEVICE_NAME"),
			AppVersion:     meetingLiveEnvFirst("MEETING_ASR_STREAM_APP_VERSION", "SOMNI_ASR_APP_VERSION"),
			Timezone:       meetingLiveEnvFirst("MEETING_ASR_STREAM_TIMEZONE", "SOMNI_ASR_TIMEZONE"),
		})
		if !liveASR.Enabled() {
			t.Fatal("live Somni ASR configuration is incomplete")
		}
	}
	file, err := os.Open("testdata/meeting-synthetic-speech.opus")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, _, err := oggreader.NewWith(file)
	if err != nil {
		t.Fatal(err)
	}

	receivedPCM := make(chan int, 2)
	upstreamFailure := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"access_token": "test-token", "access_token_expires_in": 3600}})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]string{"event": "started"})
		var pcm []byte
		for {
			kind, data, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				pcm = append(pcm, data...)
				continue
			}
			var event struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(data, &event) == nil && event.Event == "stop" {
				break
			}
		}
		receivedPCM <- len(pcm)
		text := "synthetic speech"
		if liveASR != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
			defer cancel()
			var liveErr error
			text, liveErr = liveASR.Transcribe(ctx, pcm)
			if liveErr != nil || strings.TrimSpace(text) == "" {
				upstreamFailure <- struct{}{}
				return
			}
		}
		_ = conn.WriteJSON(map[string]any{"event": "result", "data": map[string]any{"text": text, "isFinal": true}})
		_ = conn.WriteJSON(map[string]string{"event": "stopped"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	manager := sfu.MustNewManager(sfu.OfflineSettingEngine())
	room := manager.JoinRoom("opus-pipeline-test")
	t.Cleanup(func() { manager.RemoveRoom("opus-pipeline-test") })
	participant, err := room.AddParticipant("synthetic-publisher")
	if err != nil {
		t.Fatal(err)
	}
	client, err := manager.API().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "synthetic-audio", "synthetic-stream")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	serverCandidates := make(chan webrtc.ICECandidateInit, 32)
	participant.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			serverCandidates <- c.ToJSON()
		}
	})
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(client)
	if err = client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher ICE gathering timed out")
	}
	answer, err := participant.Offer(*client.LocalDescription())
	if err != nil {
		t.Fatal(err)
	}
	if err = client.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for client.ConnectionState() != webrtc.PeerConnectionStateConnected {
		select {
		case candidate := <-serverCandidates:
			if err = client.AddICECandidate(candidate); err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("publisher peer connection did not connect")
		}
	}

	meta := newMeetingMeta(meetingMetaInit{})
	meta.minutes.Running = true
	meta.minutes.RecordingID = "synthetic-recording"
	ctx, cancel := context.WithCancel(context.Background())
	asrCtx, asrCancel := context.WithCancel(context.Background())
	defer asrCancel()
	result := make(chan string, 2)
	session := &meetingAudioMinutesSession{
		roomID: "opus-pipeline-test", room: room, meta: meta, recordingID: "synthetic-recording",
		asr: NewMeetingASRStreamService(MeetingASRStreamConfig{
			HTTPBase: server.URL, WSBase: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws",
			Email: "synthetic@example.com", Password: "synthetic-password",
		}),
		ctx: ctx, cancel: cancel, asrCtx: asrCtx, asrCancel: asrCancel,
		done: make(chan struct{}), transcribeDone: make(chan struct{}),
		decoders: make(map[string]*meetingAudioDecoder), vad: newMeetingAudioVAD(),
		energy: make(map[string]int64), queue: make(chan meetingAudioBatch, 4),
		onText: func(text string, _, _ int64, _, _, _ string) { result <- text },
	}
	room.SetAudioTap(session.handlePacket)
	go session.mixLoop()
	go session.transcribeLoop()
	t.Cleanup(session.Abort)
	var seq uint16
	for {
		payload, _, readErr := reader.ParseNextPage()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.HasPrefix(payload, []byte("OpusTags")) {
			continue
		}
		if err = track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960, SSRC: 0x12345678}, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		seq++
		time.Sleep(20 * time.Millisecond)
	}
	// A running meeting must transcribe without waiting for anybody to stop it.
	// The clip ends before the ten-second batch deadline, so this also verifies
	// the timed VAD flush while the room remains active.
	select {
	case size := <-receivedPCM:
		if size < meetingAudioSampleRate { // At least half a second of PCM16 speech.
			t.Fatalf("ASR received only %d bytes of decoded speech", size)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("decoded speech never reached ASR while the meeting was active")
	}
	select {
	case text := <-result:
		if strings.TrimSpace(text) == "" || (!live && text != "synthetic speech") {
			t.Fatal("ASR result was not delivered to the meeting")
		}
	case <-upstreamFailure:
		t.Fatal("real Somni ASR failed or returned an empty result for decoded synthetic speech")
	case <-time.After(90 * time.Second):
		t.Fatal("ASR response did not reach the meeting")
	}
	room.SetAudioTap(nil)
	session.Stop()
}
