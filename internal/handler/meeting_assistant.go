package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"letshare-server/internal/model"
)

const (
	meetingAssistantID               = "ai:meeting-assistant"
	meetingAssistantName             = "会议助手"
	meetingAssistantMaxContextBytes  = 14 << 10
	meetingAssistantMaxSegments      = 120
	meetingAssistantStreamFlushDelay = 80 * time.Millisecond
)

type meetingAssistantContextSegment struct {
	SpeakerName string `json:"speakerName,omitempty"`
	Text        string `json:"text"`
	StartMs     int64  `json:"startMs,omitempty"`
}

// appendMeetingAITranscript keeps a bounded in-memory transcript for private
// meeting-assistant questions. It is never persisted or broadcast.
func (h *WebSocketHandler) appendMeetingAITranscript(roomID string, segments []meetingMinutesSegment) {
	if roomID == "" || len(segments) == 0 {
		return
	}
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		return
	}
	meta.mu.Lock()
	meta.aiTranscript = append(meta.aiTranscript, segments...)
	if len(meta.aiTranscript) > meetingAssistantMaxSegments {
		meta.aiTranscript = meta.aiTranscript[len(meta.aiTranscript)-meetingAssistantMaxSegments:]
	}
	meta.mu.Unlock()
}

func (h *WebSocketHandler) meetingParticipantNames(roomID string) []string {
	names := make([]string, 0)
	for _, member := range h.meetings.members(roomID) {
		name := strings.TrimSpace(member.UserName)
		if index := strings.IndexByte(name, ':'); index > 0 {
			name = name[:index]
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (h *WebSocketHandler) meetingAssistantContext(roomID string) string {
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		return "{}"
	}
	meta.mu.Lock()
	summary := meta.minutes.Summary
	segments := append([]meetingMinutesSegment(nil), meta.aiTranscript...)
	meta.mu.Unlock()

	selected := make([]meetingAssistantContextSegment, 0, len(segments))
	total := 0
	for index := len(segments) - 1; index >= 0; index-- {
		text := strings.TrimSpace(segments[index].Text)
		if text == "" {
			continue
		}
		item := meetingAssistantContextSegment{
			SpeakerName: segments[index].SpeakerName,
			Text:        text,
			StartMs:     segments[index].StartMs,
		}
		encoded, _ := json.Marshal(item)
		if total+len(encoded) > meetingAssistantMaxContextBytes && len(selected) > 0 {
			break
		}
		selected = append(selected, item)
		total += len(encoded)
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	payload, _ := json.Marshal(map[string]any{
		"summary":      summary,
		"participants": h.meetingParticipantNames(roomID),
		"segments":     selected,
	})
	return string(payload)
}

func (h *WebSocketHandler) sendMeetingAssistantError(client *model.Client, roomID, requestID, text string) {
	if client == nil {
		return
	}
	h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingChat, map[string]interface{}{
		"kind": "ai-error", "requestId": requestID,
		"from": meetingAssistantID, "to": client.UniqID,
		"text": text, "ts": time.Now().UnixMilli(),
	})
}

// handleMeetingAssistantChat is a private chat endpoint: the question and
// answer go only to the asking connection, never to the room.
func (h *WebSocketHandler) handleMeetingAssistantChat(client *model.Client, roomID, text, requestID string) {
	if h.meetingAIService == nil || !h.meetingAIService.Enabled() {
		h.sendMeetingAssistantError(client, roomID, requestID, "会议助手暂未配置")
		return
	}
	if !validMeetingAIRequestID(requestID) {
		requestID = fmt.Sprintf("chat-%d", time.Now().UnixNano())
	}
	if !h.allowMeetingAIRequest(roomID, client.ID, time.Now()) {
		h.sendMeetingAssistantError(client, roomID, requestID, "提问太频繁，请稍后再试")
		return
	}
	h.appendMeetingChatHistory(roomID, meetingChatEntry{
		From: client.UniqID, Text: text, Ts: time.Now().UnixMilli(), To: meetingAssistantID,
	})
	contextJSON := h.meetingAssistantContext(roomID)
	input, _ := json.Marshal(map[string]string{"context": contextJSON, "question": text})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var answer strings.Builder
		var pending strings.Builder
		lastFlush := time.Now()
		flush := func(force bool) {
			if pending.Len() == 0 {
				return
			}
			if !force && time.Since(lastFlush) < meetingAssistantStreamFlushDelay {
				return
			}
			delta := pending.String()
			pending.Reset()
			lastFlush = time.Now()
			h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingChat, map[string]interface{}{
				"kind": "ai-delta", "requestId": requestID,
				"from": meetingAssistantID, "to": client.UniqID,
				"text": delta, "ts": time.Now().UnixMilli(),
			})
		}
		err := h.meetingAIService.StreamChat(ctx, string(input), "zh", func(delta string) error {
			if !h.meetings.owns(roomID, client.UniqID, client.ID) {
				return context.Canceled
			}
			answer.WriteString(delta)
			pending.WriteString(delta)
			flush(false)
			return nil
		})
		flush(true)
		if err != nil {
			h.sendMeetingAssistantError(client, roomID, requestID, "会议助手暂时不可用，请稍后再试")
			return
		}
		finalText := strings.TrimSpace(answer.String())
		if finalText == "" {
			h.sendMeetingAssistantError(client, roomID, requestID, "会议助手没有找到相关信息")
			return
		}
		ts := time.Now().UnixMilli()
		h.appendMeetingChatHistory(roomID, meetingChatEntry{
			From: meetingAssistantID, Text: finalText, Ts: ts, To: client.UniqID,
		})
		h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingChat, map[string]interface{}{
			"kind": "ai-done", "requestId": requestID,
			"from": meetingAssistantID, "to": client.UniqID,
			"text": finalText, "ts": ts,
		})
	}()
}
