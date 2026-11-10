package handler

import (
	"context"
	"encoding/json"
	"time"

	"letshare-server/internal/model"

	"github.com/sirupsen/logrus"
)

type meetingAIRateWindow struct {
	started time.Time
	count   int
}

// Limit each current connection as a defense-in-depth fairness measure. This
// is not a durable billing quota and never stores or examines message text.
func (h *WebSocketHandler) allowMeetingAIRequest(roomID, clientID string, now time.Time) bool {
	h.meetingAIRateMu.Lock()
	defer h.meetingAIRateMu.Unlock()
	if h.meetingAIRates == nil {
		h.meetingAIRates = make(map[string]meetingAIRateWindow)
	}
	if len(h.meetingAIRates) >= 4096 {
		for key, window := range h.meetingAIRates {
			if now.Sub(window.started) >= time.Minute {
				delete(h.meetingAIRates, key)
			}
		}
		if len(h.meetingAIRates) >= 4096 {
			return false
		}
	}
	key := roomID + "\x00" + clientID
	window := h.meetingAIRates[key]
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
		window = meetingAIRateWindow{started: now}
	}
	if window.count >= 30 {
		return false
	}
	window.count++
	h.meetingAIRates[key] = window
	return true
}

type meetingAIRequest struct {
	RequestID        string  `json:"requestId"`
	Task             string  `json:"task"`
	Input            string  `json:"input"`
	TargetLanguage   string  `json:"targetLanguage"`
	RecordingID      string  `json:"recordingId,omitempty"`
	CoordinatorEpoch uint64  `json:"coordinatorEpoch,omitempty"`
	ThroughSeq       *uint64 `json:"throughSeq,omitempty"`
}

// SetMeetingAIService attaches a provider which is disabled unless the
// operator explicitly configured a free-only, non-billable upstream account.
func (h *WebSocketHandler) SetMeetingAIService(service *MeetingAIService) {
	h.meetingAIService = service
}

func validMeetingAIRequestID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, ch := range id {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'z') && !(ch >= 'A' && ch <= 'Z') && ch != '-' && ch != '_' {
			return false
		}
	}
	return true
}

// handleMeetingAIRequest is reachable only through the existing authenticated
// meeting WebSocket. It never logs input/output, accepts no provider URL or
// key from clients, and replies only to the requesting connection.
func (h *WebSocketHandler) handleMeetingAIRequest(client *model.Client, roomID string, message *model.WebSocketMessage) {
	if !h.requireMeetingMember(client, roomID, "meeting:minutes") {
		return
	}
	if len(message.Data) > 40*1024 {
		return
	}
	var req meetingAIRequest
	if json.Unmarshal(message.Data, &req) != nil || !validMeetingAIRequestID(req.RequestID) {
		return
	}
	logrus.WithFields(logrus.Fields{
		"room": roomID, "user": client.UniqID, "task": req.Task, "input_bytes": len(req.Input),
	}).Info("会议 AI 请求")
	value, ok := h.activeMeetingRooms.Load(roomID)
	meta, okMeta := value.(*meetingMeta)
	if !ok || !okMeta || meta == nil || h.meetingAIService == nil {
		h.sendMeetingAIResult(client, roomID, nil, req, "", false, "")
		return
	}
	meta.mu.Lock()
	configured := meta.minutes.Configured
	viewer := meta.minutes.CaptionActive && meta.captionViewers[client.ID]
	minutesEnabled := minutesSubscriptionEnabledLocked(meta, client.UniqID)
	meta.mu.Unlock()
	switch req.Task {
	case MeetingAITaskSummary:
		if !configured || !minutesEnabled || req.ThroughSeq == nil || req.CoordinatorEpoch == 0 ||
			!h.meetingSummaryRequestCurrent(client, roomID, meta, req) {
			h.sendMeetingAIResult(client, roomID, meta, req, "", false, "")
			return
		}
	case MeetingAITaskLiveNotes, MeetingAITaskFinalMinutes:
		// These legacy task names launched parallel LLM requests for the same
		// shared minutes. One coordinator summary per cursor is authoritative.
		h.sendMeetingAIResult(client, roomID, meta, req, "", false, "")
		return
	case MeetingAITaskTranslate:
		if !viewer {
			h.sendMeetingAIResult(client, roomID, meta, req, "", false, "")
			return
		}
	case MeetingAITaskQuestion:
		if !configured {
			h.sendMeetingAIResult(client, roomID, meta, req, "", false, "")
			return
		}
	default:
		h.sendMeetingAIResult(client, roomID, meta, req, "", false, "")
		return
	}
	if !h.allowMeetingAIRequest(roomID, client.ID, time.Now()) {
		h.sendMeetingAIResult(client, roomID, meta, req, "", false, "")
		return
	}
	if req.Task == MeetingAITaskSummary {
		go h.runMeetingSummaryAIRequest(client, roomID, meta, req)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		output, err := h.meetingAIService.Complete(ctx, req.Task, req.Input, req.TargetLanguage)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"room": roomID, "user": client.UniqID, "task": req.Task,
			}).WithError(err).Warn("会议 AI 请求失败")
			h.sendMeetingAIResult(client, roomID, meta, req, "", false, "")
			return
		}
		h.sendMeetingAIResult(client, roomID, meta, req, output, true, "")
	}()
}

func (h *WebSocketHandler) meetingSummaryRequestCurrent(client *model.Client, roomID string, meta *meetingMeta, req meetingAIRequest) bool {
	if client == nil || meta == nil || req.ThroughSeq == nil {
		return false
	}
	meta.meetingASRPublishMu.Lock()
	defer meta.meetingASRPublishMu.Unlock()
	current, active := h.activeMeetingRoom(roomID)
	if !active || current != meta || !h.meetings.owns(roomID, client.UniqID, client.ID) {
		return false
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	return minutesSubscriptionEnabledLocked(meta, client.UniqID) &&
		meta.minutes.SummaryCoordinatorID == client.UniqID &&
		meta.minutes.SummaryCoordinatorEpoch == req.CoordinatorEpoch &&
		meta.minutes.RecordingID == sanitizeMeetingRecordingID(req.RecordingID) &&
		req.CoordinatorEpoch > 0 && *req.ThroughSeq > meta.minutes.SummaryThroughSeq &&
		*req.ThroughSeq <= meta.minutes.LatestSegmentSeq
}

func (h *WebSocketHandler) runMeetingSummaryAIRequest(client *model.Client, roomID string, meta *meetingMeta, req meetingAIRequest) {
	meta.minutesSummaryMu.Lock()
	defer meta.minutesSummaryMu.Unlock()
	if client == nil || req.ThroughSeq == nil {
		return
	}
	meta.meetingASRPublishMu.Lock()
	if !h.meetingSummaryRequestCurrentLocked(client, roomID, meta, req) {
		meta.meetingASRPublishMu.Unlock()
		h.sendMeetingAIResult(client, roomID, meta, req, "", false, "")
		return
	}
	meta.mu.Lock()
	now := time.Now()
	duplicate := meta.minutesSummaryRequestedRecordingID == req.RecordingID &&
		meta.minutesSummaryRequestedCoordinatorEpoch == req.CoordinatorEpoch &&
		meta.minutesSummaryRequestedThroughSeq >= *req.ThroughSeq &&
		now.Sub(meta.minutesSummaryRequestedAt) < 2*time.Minute
	if duplicate {
		cachedOutput := meta.minutesSummaryCachedOutput
		cached := meta.minutesSummaryCacheValid && meta.minutesSummaryRequestedThroughSeq == *req.ThroughSeq
		summaryLanguage := meta.minutes.SummaryLanguage
		meta.mu.Unlock()
		if cached {
			h.sendMeetingAIResultLocked(client, roomID, req, cachedOutput, true, summaryLanguage)
		} else {
			h.sendMeetingAIResultLocked(client, roomID, req, "", false, summaryLanguage)
		}
		meta.meetingASRPublishMu.Unlock()
		return
	}
	meta.minutesSummaryRequestedRecordingID = req.RecordingID
	meta.minutesSummaryRequestedCoordinatorEpoch = req.CoordinatorEpoch
	meta.minutesSummaryRequestedThroughSeq = *req.ThroughSeq
	meta.minutesSummaryRequestedAt = now
	meta.minutesSummaryCachedOutput = ""
	meta.minutesSummaryCacheValid = false
	summaryLanguage := meta.minutes.SummaryLanguage
	if summaryLanguage != "zh-CN" && summaryLanguage != "en-US" {
		summaryLanguage = "zh-CN"
	}
	targetLanguage := "zh"
	if summaryLanguage == "en-US" {
		targetLanguage = "en"
	}
	meta.mu.Unlock()
	meta.meetingASRPublishMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	output, err := h.meetingAIService.Complete(ctx, req.Task, req.Input, targetLanguage)
	cancel()
	if err != nil {
		logrus.WithFields(logrus.Fields{"room": roomID, "user": client.UniqID, "task": req.Task}).WithError(err).Warn("会议 AI 请求失败")
	}

	meta.meetingASRPublishMu.Lock()
	valid := h.meetingSummaryRequestCurrentLocked(client, roomID, meta, req)
	meta.mu.Lock()
	if meta.minutesSummaryRequestedRecordingID == req.RecordingID &&
		meta.minutesSummaryRequestedCoordinatorEpoch == req.CoordinatorEpoch &&
		meta.minutesSummaryRequestedThroughSeq == *req.ThroughSeq {
		if err != nil {
			meta.minutesSummaryRequestedRecordingID = ""
			meta.minutesSummaryRequestedCoordinatorEpoch = 0
			meta.minutesSummaryRequestedThroughSeq = 0
			meta.minutesSummaryRequestedAt = time.Time{}
			meta.minutesSummaryCachedOutput = ""
			meta.minutesSummaryCacheValid = false
		} else if valid {
			meta.minutesSummaryCachedOutput = output
			meta.minutesSummaryCacheValid = true
		}
	}
	meta.mu.Unlock()
	if valid {
		h.sendMeetingAIResultLocked(client, roomID, req, output, err == nil, summaryLanguage)
	}
	meta.meetingASRPublishMu.Unlock()
}

// meetingSummaryRequestCurrentLocked requires meetingASRPublishMu and checks
// the current subscription/coordinator/recording/cursor atomically.
func (h *WebSocketHandler) meetingSummaryRequestCurrentLocked(client *model.Client, roomID string, meta *meetingMeta, req meetingAIRequest) bool {
	current, active := h.activeMeetingRoom(roomID)
	if !active || current != meta || !h.meetings.owns(roomID, client.UniqID, client.ID) || req.ThroughSeq == nil {
		return false
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	return minutesSubscriptionEnabledLocked(meta, client.UniqID) &&
		meta.minutes.SummaryCoordinatorID == client.UniqID &&
		meta.minutes.SummaryCoordinatorEpoch == req.CoordinatorEpoch &&
		meta.minutes.RecordingID == sanitizeMeetingRecordingID(req.RecordingID) &&
		*req.ThroughSeq > meta.minutes.SummaryThroughSeq && *req.ThroughSeq <= meta.minutes.LatestSegmentSeq
}

func (h *WebSocketHandler) sendMeetingAIResult(client *model.Client, roomID string, meta *meetingMeta, req meetingAIRequest, output string, ok bool, summaryLanguage string) {
	if client == nil {
		return
	}
	if meta != nil {
		meta.meetingASRPublishMu.Lock()
		defer meta.meetingASRPublishMu.Unlock()
		if !meetingMinutesRoomCurrent(h, roomID, meta) || !h.meetings.owns(roomID, client.UniqID, client.ID) {
			return
		}
		if req.Task == MeetingAITaskSummary || req.Task == MeetingAITaskLiveNotes || req.Task == MeetingAITaskFinalMinutes {
			meta.mu.Lock()
			enabled := minutesSubscriptionEnabledLocked(meta, client.UniqID)
			meta.mu.Unlock()
			if !enabled {
				return
			}
		}
	}
	h.sendMeetingAIResultLocked(client, roomID, req, output, ok, summaryLanguage)
}

// sendMeetingAIResultLocked is also used while the room's ASR publication
// mutex is held after a summary request's final eligibility check.
func (h *WebSocketHandler) sendMeetingAIResultLocked(client *model.Client, roomID string, req meetingAIRequest, output string, ok bool, summaryLanguage string) {
	payload := map[string]interface{}{
		"kind": "ai-result", "requestId": req.RequestID, "ok": ok, "output": output,
	}
	if req.Task == MeetingAITaskSummary {
		payload["recordingId"] = sanitizeMeetingRecordingID(req.RecordingID)
		payload["coordinatorEpoch"] = req.CoordinatorEpoch
		if req.ThroughSeq != nil {
			payload["throughSeq"] = *req.ThroughSeq
		}
		payload["summaryLanguage"] = summaryLanguage
	}
	h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingMinutes, payload)
}
