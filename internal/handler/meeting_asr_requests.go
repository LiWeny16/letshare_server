package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"letshare-server/internal/model"

	"github.com/sirupsen/logrus"
)

type meetingASRChunkRequest struct {
	PCM      string `json:"pcm"`
	Language string `json:"language"`
}

const (
	meetingASRChunkMaxConcurrent = 2
	meetingASRChunkMaxPCMBytes   = 320 * 1024
)

// meetingASRUnavailableFrame tells one speaker's tab why no recognition will
// happen. The caption UI keys on this frame: silently dropping audio made a
// deployment without recognizer credentials look like "nothing ever happens".
func (h *WebSocketHandler) meetingASRUnavailableFrame(client *model.Client, roomID, engine, message string) {
	if client == nil || roomID == "" {
		return
	}
	h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingMinutes, map[string]interface{}{
		"kind": "asr-error", "engine": engine, "message": message,
	})
}

// notifyMeetingASRUnavailable sends at most one unavailable frame per five
// seconds per connection, so a streaming client cannot flood its own socket.
func (h *WebSocketHandler) notifyMeetingASRUnavailable(client *model.Client, roomID, engine, message string) {
	if client == nil {
		return
	}
	key := roomID + "\x00" + client.ID
	now := time.Now()
	h.meetingASRRateMu.Lock()
	if h.meetingASRErrorAt == nil {
		h.meetingASRErrorAt = make(map[string]time.Time)
	}
	if last, ok := h.meetingASRErrorAt[key]; ok && now.Sub(last) < 5*time.Second {
		h.meetingASRRateMu.Unlock()
		return
	}
	if len(h.meetingASRErrorAt) >= 4096 {
		for staleKey, at := range h.meetingASRErrorAt {
			if now.Sub(at) >= time.Minute {
				delete(h.meetingASRErrorAt, staleKey)
			}
		}
	}
	h.meetingASRErrorAt[key] = now
	h.meetingASRRateMu.Unlock()
	h.meetingASRUnavailableFrame(client, roomID, engine, message)
}

// sendMeetingASRCapabilities reports which server recognizers the operator did
// configure, so a client never keeps a default engine that cannot produce text.
func (h *WebSocketHandler) sendMeetingASRCapabilities(client *model.Client, roomID string) {
	if client == nil {
		return
	}
	somniEnabled := h.captionsEnabled && h.meetingASRStream != nil && h.meetingASRStream.Enabled()
	h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingMinutes, map[string]interface{}{
		"kind":   "asr-capabilities",
		"stream": somniEnabled,
		// Keep the legacy chunk contract available through Somni's one-shot
		// streaming session; it is not a separate audio-model provider.
		"chunk": somniEnabled,
		"ai":    h.meetingAIService != nil && h.meetingAIService.Enabled(),
	})
}

// LogMeetingAICapabilities states at startup which meeting AI paths exist.
// Operators had no way to discover that captions and translation stay dark
// until MEETING_ASR_* / MEETING_AI_* are configured; no secrets are logged.
func (h *WebSocketHandler) LogMeetingAICapabilities() {
	logrus.WithFields(logrus.Fields{
		"meeting_ai_translate": h.meetingAIService != nil && h.meetingAIService.Enabled(),
		"meeting_ai_model":     meetingAIModelName(h.meetingAIService),
		"meeting_asr_stream":   h.meetingASRStream != nil && h.meetingASRStream.Enabled(),
	}).Info("会议 AI/识别能力（未配置时字幕不可用）")
}

func meetingAIModelName(service *MeetingAIService) string {
	if service == nil {
		return ""
	}
	return service.Model()
}

func (h *WebSocketHandler) tryAcquireMeetingASRChunk() (chan struct{}, bool) {
	h.meetingASRChunkSemMu.Lock()
	if h.meetingASRChunkSem == nil {
		h.meetingASRChunkSem = make(chan struct{}, meetingASRChunkMaxConcurrent)
	}
	sem := h.meetingASRChunkSem
	h.meetingASRChunkSemMu.Unlock()
	select {
	case sem <- struct{}{}:
		return sem, true
	default:
		return nil, false
	}
}

// allowMeetingASRChunk bounds uploads per connection: one 6s chunk at a time,
// at most ten per minute. This is fairness, not billing control.
func (h *WebSocketHandler) allowMeetingASRChunk(clientID string, now time.Time) bool {
	h.meetingASRRateMu.Lock()
	defer h.meetingASRRateMu.Unlock()
	if h.meetingASRRates == nil {
		h.meetingASRRates = make(map[string]meetingAIRateWindow)
	}
	if len(h.meetingASRRates) >= 4096 {
		for key, window := range h.meetingASRRates {
			if now.Sub(window.started) >= time.Minute {
				delete(h.meetingASRRates, key)
			}
		}
		if len(h.meetingASRRates) >= 4096 {
			return false
		}
	}
	window := h.meetingASRRates[clientID]
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
		window = meetingAIRateWindow{started: now}
	}
	if window.count >= 10 {
		return false
	}
	window.count++
	h.meetingASRRates[clientID] = window
	return true
}

// handleMeetingASRChunk preserves the legacy chunk request shape while
// transcribing through the same Somni service used by streaming clients. The
// chunk belongs to exactly one connection, so the result stays speaker-tagged.
func (h *WebSocketHandler) handleMeetingASRChunk(client *model.Client, roomID string, message *model.WebSocketMessage) {
	drop := func(reason string) {
		// Diagnostics only: never log audio or transcript text.
		logrus.WithFields(logrus.Fields{"room": roomID, "user": client.UniqID, "reason": reason}).Warn("会议 ASR 分片被丢弃")
	}
	if h.meetingASRStream == nil || !h.meetingASRStream.Enabled() {
		drop("disabled")
		h.notifyMeetingASRUnavailable(client, roomID, "server-asr", "服务端未配置 Somni 识别，请联系服务器管理员")
		return
	}
	if !h.requireMeetingMember(client, roomID, "meeting:minutes") {
		drop("not_member")
		return
	}
	if len(message.Data) > 420*1024 {
		drop("too_large_message")
		return
	}
	var req meetingASRChunkRequest
	if json.Unmarshal(message.Data, &req) != nil {
		drop("bad_json")
		return
	}
	pcm, err := base64.StdEncoding.DecodeString(req.PCM)
	if err != nil || len(pcm) < 4096 || len(pcm) > meetingASRChunkMaxPCMBytes {
		drop(fmt.Sprintf("bad_pcm_%d", len(pcm)))
		return
	}
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		return
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	active := running || meta.minutes.CaptionActive
	consented := meta.minutes.Consented[client.UniqID]
	recordingID := ""
	if running {
		recordingID = meta.minutes.RecordingID
	}
	meta.mu.Unlock()
	if !running && !h.captionsEnabled {
		drop("captions_disabled")
		h.notifyMeetingASRUnavailable(client, roomID, "server-asr", "字幕功能暂时关闭")
		return
	}
	if !active || (!running && !consented) {
		drop(fmt.Sprintf("inactive_%v_consent_%v", active, consented))
		h.notifyMeetingASRUnavailable(client, roomID, "server-asr", "字幕已停止或尚未同意转写，本次识别未开始")
		return
	}
	if running && h.meetingAudioMinutesMixerConfigured() {
		h.notifyMeetingASRUnavailable(client, roomID, "server-asr", "The server mixer already includes this meeting audio")
		return
	}
	if !h.allowMeetingASRChunk(client.ID, time.Now()) {
		drop("rate_limited")
		return
	}
	sem, acquired := h.tryAcquireMeetingASRChunk()
	if !acquired {
		drop("overloaded")
		h.notifyMeetingASRUnavailable(client, roomID, "server-asr", "Somni 识别繁忙，请稍后重试")
		return
	}
	language := "zh"
	if req.Language == "en" {
		language = "en"
	}
	go func() {
		defer func() { <-sem }()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		text, err := h.meetingASRStream.Transcribe(ctx, pcm)
		if err != nil || strings.TrimSpace(text) == "" {
			logrus.WithFields(logrus.Fields{"room": roomID, "user": client.UniqID}).WithError(err).Warn("会议 ASR 上游失败")
			return
		}
		h.broadcastMeetingASRChunkIfCurrent(roomID, meta, client, text, language, recordingID)
	}()
}

func (h *WebSocketHandler) broadcastMeetingASRChunkIfCurrent(roomID string, requestMeta *meetingMeta, client *model.Client, text, language, requestRecordingID string) {
	// Serialize the final authorization check and publication with room
	// lifecycle changes. The upstream request may finish after stop or leave.
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok || requestMeta == nil || meta != requestMeta {
		return
	}
	meta.meetingASRPublishMu.Lock()
	defer meta.meetingASRPublishMu.Unlock()
	if client == nil || !h.meetings.owns(roomID, client.UniqID, client.ID) {
		return
	}
	currentMeta, stillActive := h.activeMeetingRoom(roomID)
	if !stillActive || currentMeta != meta {
		return
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	captions := meta.minutes.CaptionActive
	consented := meta.minutes.Consented[client.UniqID]
	currentRecordingID := meta.minutes.RecordingID
	meta.mu.Unlock()
	if !running && (!h.captionsEnabled || !captions || !consented) {
		return
	}
	if running && h.meetingAudioMinutesMixerConfigured() {
		return
	}
	resultRecordingID := ""
	if running {
		if requestRecordingID == "" || requestRecordingID != currentRecordingID {
			return
		}
		resultRecordingID = currentRecordingID
	}
	logrus.WithFields(logrus.Fields{"room": roomID, "user": client.UniqID, "chars": len([]rune(text))}).Info("会议 ASR 分片已转写")
	h.broadcastMeetingASRSegment(roomID, client, text, language, resultRecordingID)
}

func (h *WebSocketHandler) broadcastMeetingASRSegment(roomID string, client *model.Client, text, language, recordingID string) {
	speakerName := client.UserName
	if speakerName == "" {
		speakerName = client.UniqID
	}
	if index := strings.IndexByte(speakerName, ':'); index > 0 {
		speakerName = speakerName[:index]
	}
	now := time.Now().UnixMilli()
	segment := meetingMinutesSegment{
		SegmentID:   fmt.Sprintf("asr:%s:%d", client.UniqID, time.Now().UnixNano()),
		RecordingID: recordingID,
		From:        client.UniqID,
		SpeakerName: speakerName,
		Text:        text,
		StartMs:     now - 6000,
		EndMs:       now,
		Final:       true,
		Language:    normalizeMeetingLanguage(map[string]string{"zh": "zh-CN", "en": "en-US"}[language]),
	}
	payload := map[string]interface{}{"kind": "segments", "segments": []meetingMinutesSegment{segment}}
	meta, active := h.activeMeetingRoom(roomID)
	if !active {
		return
	}
	meta.mu.Lock()
	pending := make(map[string]bool, len(meta.minutesReplayPending))
	for clientID, isPending := range meta.minutesReplayPending {
		pending[clientID] = isPending
	}
	meta.mu.Unlock()
	h.sendMeetingMinutesToEnabledMembersExcept(roomID, meta, payload, pending)
}

// activeMeetingRoom loads the room metadata or returns false.
func (h *WebSocketHandler) activeMeetingRoom(roomID string) (*meetingMeta, bool) {
	value, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		return nil, false
	}
	meta, metaOK := value.(*meetingMeta)
	return meta, metaOK && meta != nil
}
