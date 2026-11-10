package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"letshare-server/internal/model"

	"github.com/sirupsen/logrus"
)

type meetingASRStreamRequest struct {
	Event    string `json:"event"`
	PCM      string `json:"pcm"`
	Language string `json:"language"`
}

type meetingASRStreamState struct {
	key         string
	mu          sync.Mutex
	meta        *meetingMeta
	stream      *MeetingASRStream
	cancel      context.CancelFunc
	language    string
	recordingID string
	client      *model.Client
	ready       atomic.Bool
}

func asrStreamKey(roomID, clientID string) string { return roomID + "+" + clientID }

// SetMeetingASRStreamService attaches the streaming recognizer.
func (h *WebSocketHandler) SetMeetingASRStreamService(service *MeetingASRStreamService) {
	h.meetingASRStream = service
}

func (h *WebSocketHandler) closeMeetingASRStreamByClient(roomID, clientID string) {
	key := asrStreamKey(roomID, clientID)
	meta, hasMeeting := h.activeMeetingRoom(roomID)
	if hasMeeting {
		meta.meetingASRPublishMu.Lock()
	}
	states := h.lockMeetingASRStreams(func(state *meetingASRStreamState) bool { return state.key == key })
	h.finishMeetingASRStreamTransition(states, nil)
	if hasMeeting {
		meta.meetingASRPublishMu.Unlock()
	}
}

func (h *WebSocketHandler) beginMeetingASRMemberTransition(roomID, clientID string) (*meetingMeta, bool, []*meetingASRStreamState) {
	meta, hasMeeting := h.activeMeetingRoom(roomID)
	if hasMeeting {
		meta.meetingASRPublishMu.Lock()
	}
	key := asrStreamKey(roomID, clientID)
	states := h.lockMeetingASRStreams(func(state *meetingASRStreamState) bool { return state.key == key })
	return meta, hasMeeting, states
}

func (h *WebSocketHandler) finishMeetingASRMemberTransition(roomID, clientID string, meta *meetingMeta, hasMeeting, changed bool, states []*meetingASRStreamState) {
	if changed && hasMeeting && meta != nil {
		meta.minutesDeliveryMu.Lock()
		meta.mu.Lock()
		delete(meta.minutesReplayPending, clientID)
		meta.mu.Unlock()
		meta.minutesDeliveryMu.Unlock()
	}
	if changed {
		h.finishMeetingASRStreamTransition(states, nil)
	} else {
		h.finishMeetingASRStreamTransition(states, func(*meetingASRStreamState) bool { return true })
	}
	if !changed || !hasMeeting {
		if hasMeeting {
			meta.meetingASRPublishMu.Unlock()
		}
		return
	}
	if captionState, changed := h.removeMeetingCaptionViewer(roomID, clientID); changed {
		h.broadcastMeetingCaptions(roomID, captionState)
	}
	roomStates := h.lockMeetingASRStreamsForRoom(roomID)
	h.finishMeetingASRStreamsForCurrentState(roomID, roomStates)
	meta.meetingASRPublishMu.Unlock()
}

// lockMeetingASRStreams snapshots matching sessions, then locks them in key
// order. Callers serialize creation and authorization changes with the room's
// meetingASRPublishMu; sync.Map compare-and-delete protects stale cleanup.
func (h *WebSocketHandler) lockMeetingASRStreams(match func(*meetingASRStreamState) bool) []*meetingASRStreamState {
	var states []*meetingASRStreamState
	h.meetingASRStreams.Range(func(_, value any) bool {
		state, ok := value.(*meetingASRStreamState)
		if ok && state != nil && match(state) {
			states = append(states, state)
		}
		return true
	})
	sort.Slice(states, func(i, j int) bool { return states[i].key < states[j].key })
	for _, state := range states {
		state.mu.Lock()
	}
	return states
}

func (h *WebSocketHandler) lockMeetingASRStreamsForRoom(roomID string) []*meetingASRStreamState {
	prefix := roomID + "+"
	return h.lockMeetingASRStreams(func(state *meetingASRStreamState) bool {
		return strings.HasPrefix(state.key, prefix)
	})
}

func (h *WebSocketHandler) lockMeetingASRStreamsForUser(roomID, uniqID string) []*meetingASRStreamState {
	prefix := roomID + "+"
	return h.lockMeetingASRStreams(func(state *meetingASRStreamState) bool {
		return strings.HasPrefix(state.key, prefix) && state.client != nil && state.client.UniqID == uniqID
	})
}

func (h *WebSocketHandler) finishMeetingASRStreamsForCurrentState(roomID string, states []*meetingASRStreamState) {
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		h.finishMeetingASRStreamTransition(states, nil)
		return
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	captions := meta.minutes.CaptionActive
	recordingID := meta.minutes.RecordingID
	consented := make(map[string]bool, len(meta.minutes.Consented))
	for uniqID, accepted := range meta.minutes.Consented {
		consented[uniqID] = accepted
	}
	meta.mu.Unlock()
	h.finishMeetingASRStreamTransition(states, func(state *meetingASRStreamState) bool {
		if state.client == nil {
			return false
		}
		if running && !h.meetingAudioMinutesMixerConfigured() && state.recordingID != "" && state.recordingID == recordingID {
			return true
		}
		return captions && consented[state.client.UniqID]
	})
}

// finishMeetingASRStreamTransition optionally preserves matching sessions,
// closes the rest, then releases the locks acquired by lockMeetingASRStreams.
func (h *WebSocketHandler) finishMeetingASRStreamTransition(states []*meetingASRStreamState, keep func(*meetingASRStreamState) bool) {
	var closing []*MeetingASRStream
	for _, state := range states {
		if keep == nil || !keep(state) {
			if h.meetingASRStreams.CompareAndDelete(state.key, state) {
				if state.cancel != nil {
					state.cancel()
				}
				if state.stream != nil {
					closing = append(closing, state.stream)
				}
			}
		}
		state.mu.Unlock()
	}
	for _, stream := range closing {
		stream.Close()
	}
}

func (h *WebSocketHandler) closeMeetingASRStreamState(state *meetingASRStreamState) {
	if state == nil {
		return
	}
	state.mu.Lock()
	var closing *MeetingASRStream
	if h.meetingASRStreams.CompareAndDelete(state.key, state) {
		if state.cancel != nil {
			state.cancel()
		}
		closing = state.stream
	}
	state.mu.Unlock()
	if closing != nil {
		closing.Close()
	}
}

func (h *WebSocketHandler) meetingASRStreamIsCurrent(state *meetingASRStreamState) bool {
	if state == nil {
		return false
	}
	value, ok := h.meetingASRStreams.Load(state.key)
	return ok && value == state
}

// meetingASRStreamDestination checks room membership and the recording epoch.
// Minutes accept every participant; caption-only sessions retain opt-in.
func (h *WebSocketHandler) meetingASRStreamDestination(roomID string, state *meetingASRStreamState) (bool, string) {
	if state == nil || state.client == nil || !h.meetings.owns(roomID, state.client.UniqID, state.client.ID) {
		return false, ""
	}
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok || meta != state.meta {
		return false, ""
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	captions := meta.minutes.CaptionActive
	consented := meta.minutes.Consented[state.client.UniqID]
	recordingID := meta.minutes.RecordingID
	meta.mu.Unlock()
	if running && h.meetingAudioMinutesMixerConfigured() {
		return false, ""
	}
	if !running && !h.captionsEnabled {
		return false, ""
	}
	if !running && !captions {
		return false, ""
	}
	if running && !h.meetingAudioMinutesMixerConfigured() && state.recordingID != "" && state.recordingID == recordingID {
		return true, recordingID
	}
	if running && !captions {
		return false, ""
	}
	if captions && consented {
		return true, ""
	}
	return false, ""
}

func (h *WebSocketHandler) meetingASRStreamCanStart(roomID string, state *meetingASRStreamState) bool {
	if state == nil || state.client == nil || !h.meetings.owns(roomID, state.client.UniqID, state.client.ID) {
		return false
	}
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok || meta != state.meta {
		return false
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	active := running || meta.minutes.CaptionActive
	consented := meta.minutes.Consented[state.client.UniqID]
	recordingID := meta.minutes.RecordingID
	meta.mu.Unlock()
	if !running && !h.captionsEnabled {
		return false
	}
	if running && h.meetingAudioMinutesMixerConfigured() {
		return false
	}
	return active && (running || consented) && (!running || state.recordingID != "" && state.recordingID == recordingID)
}

func (h *WebSocketHandler) storeMeetingASRStreamState(roomID string, state *meetingASRStreamState) bool {
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok || meta != state.meta {
		return false
	}
	meta.meetingASRPublishMu.Lock()
	var closing *MeetingASRStream
	if value, ok := h.meetingASRStreams.Load(state.key); ok {
		if old, oldOK := value.(*meetingASRStreamState); oldOK && old != nil {
			old.mu.Lock()
			if h.meetingASRStreams.CompareAndDelete(old.key, old) {
				if old.cancel != nil {
					old.cancel()
				}
				closing = old.stream
			}
			old.mu.Unlock()
		} else {
			h.meetingASRStreams.CompareAndDelete(state.key, value)
		}
	}
	if !h.meetingASRStreamCanStart(roomID, state) {
		meta.meetingASRPublishMu.Unlock()
		if closing != nil {
			closing.Close()
		}
		return false
	}
	h.meetingASRStreams.Store(state.key, state)
	meta.meetingASRPublishMu.Unlock()
	if closing != nil {
		closing.Close()
	}
	return true
}

// handleMeetingASRStream relays this speaker's 100ms PCM frames to the
// streaming provider. Interim results are broadcast for live captions; final
// results become regular transcript segments tagged with the speaker.
func (h *WebSocketHandler) handleMeetingASRStream(client *model.Client, roomID string, message *model.WebSocketMessage) {
	if !h.requireMeetingMember(client, roomID, "meeting:minutes") {
		return
	}
	if len(message.Data) > 96*1024 {
		return
	}
	var req meetingASRStreamRequest
	if json.Unmarshal(message.Data, &req) != nil {
		return
	}
	// Stop remains available even when captions or Somni are disabled so an
	// older client can clean up a session it opened before the feature closed.
	if req.Event == "stop" {
		h.closeMeetingASRStreamByClient(roomID, client.ID)
		return
	}
	if h.meetingASRStream == nil || !h.meetingASRStream.Enabled() {
		// Never swallow the request: a silent drop leaves the caption bar
		// waiting forever with no error and no text.
		h.notifyMeetingASRUnavailable(client, roomID, "server-stream", "Somni streaming transcription is not configured; ask the meeting host to check server configuration")
		return
	}
	switch req.Event {
	case "start":
		h.closeMeetingASRStreamByClient(roomID, client.ID)
		meta, ok := h.activeMeetingRoom(roomID)
		if !ok {
			return
		}
		meta.mu.Lock()
		active := meta.minutes.Running || meta.minutes.CaptionActive
		running := meta.minutes.Running
		consented := meta.minutes.Consented[client.UniqID]
		recordingID := ""
		if meta.minutes.Running {
			recordingID = meta.minutes.RecordingID
		}
		meta.mu.Unlock()
		if !running && !h.captionsEnabled {
			h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingMinutes, map[string]interface{}{
				"kind": "asr-stream-error", "message": "Live captions are temporarily unavailable",
			})
			return
		}
		if running && h.meetingAudioMinutesMixerConfigured() {
			h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingMinutes, map[string]interface{}{
				"kind": "asr-stream-error", "message": "The server mixer already includes this meeting audio",
			})
			return
		}
		if !active || (!running && !consented) {
			h.notifyMeetingASRUnavailable(client, roomID, "server-stream", "字幕已停止或尚未同意转写，本次识别未开始")
			return
		}
		language := "zh"
		if req.Language == "en" {
			language = "en"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		stream, err := h.meetingASRStream.Open(ctx)
		cancel()
		if err != nil {
			logrus.WithFields(logrus.Fields{"room": roomID, "user": client.UniqID}).WithError(err).Warn("会议流式 ASR 连接失败")
			h.sendMeetingToClient(client.ID, roomID, model.MessageTypeMeetingMinutes, map[string]interface{}{
				"kind": "asr-stream-error", "message": "Somni streaming transcription is unavailable; ask the meeting host to check server configuration",
			})
			return
		}
		streamCtx, streamCancel := context.WithCancel(context.Background())
		state := &meetingASRStreamState{
			key: asrStreamKey(roomID, client.ID), meta: meta, stream: stream, cancel: streamCancel,
			language: language, recordingID: recordingID, client: client,
		}
		if !h.storeMeetingASRStreamState(roomID, state) {
			streamCancel()
			stream.Close()
			h.notifyMeetingASRUnavailable(client, roomID, "server-stream", "字幕已停止或尚未同意转写，本次识别未开始")
			return
		}
		go func() {
			err := stream.ReadLoop(streamCtx, func(event MeetingASRStreamEvent) {
				h.handleMeetingASRStreamEvent(roomID, state, event)
			})
			if err != nil && streamCtx.Err() == nil {
				logrus.WithFields(logrus.Fields{"room": roomID, "user": client.UniqID}).WithError(err).Warn("会议流式 ASR 连接中断")
			}
			h.closeMeetingASRStreamState(state)
		}()
	case "audio":
		value, ok := h.meetingASRStreams.Load(asrStreamKey(roomID, client.ID))
		if !ok {
			return
		}
		state, stateOK := value.(*meetingASRStreamState)
		if !stateOK || state == nil || !state.ready.Load() {
			return
		}
		pcm, err := base64.StdEncoding.DecodeString(req.PCM)
		if err != nil || len(pcm) == 0 || len(pcm) > meetingASRStreamMaxFrameBytes {
			return
		}
		meta, hasMeeting := h.activeMeetingRoom(roomID)
		if !hasMeeting {
			return
		}
		meta.meetingASRPublishMu.Lock()
		state.mu.Lock()
		if !h.meetingASRStreamIsCurrent(state) {
			state.mu.Unlock()
			meta.meetingASRPublishMu.Unlock()
			return
		}
		if !state.ready.Load() {
			state.mu.Unlock()
			meta.meetingASRPublishMu.Unlock()
			return
		}
		allowed, _ := h.meetingASRStreamDestination(roomID, state)
		if !allowed {
			state.mu.Unlock()
			meta.meetingASRPublishMu.Unlock()
			h.closeMeetingASRStreamState(state)
			return
		}
		err = state.stream.SendPCM(pcm)
		state.mu.Unlock()
		meta.meetingASRPublishMu.Unlock()
		if err != nil {
			h.closeMeetingASRStreamState(state)
		}
	}
}

func (h *WebSocketHandler) handleMeetingASRStreamEvent(roomID string, state *meetingASRStreamState, event MeetingASRStreamEvent) {
	if state == nil {
		return
	}
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		return
	}
	meta.meetingASRPublishMu.Lock()
	if event.Kind == "started" {
		state.ready.Store(true)
		meta.meetingASRPublishMu.Unlock()
		return
	}
	if event.Kind != "result" || event.Text == "" {
		meta.meetingASRPublishMu.Unlock()
		return
	}
	state.mu.Lock()
	if !h.meetingASRStreamIsCurrent(state) {
		state.mu.Unlock()
		meta.meetingASRPublishMu.Unlock()
		return
	}
	allowed, recordingID := h.meetingASRStreamDestination(roomID, state)
	if allowed {
		if event.Final {
			h.broadcastMeetingASRSegment(roomID, state.client, event.Text, state.language, recordingID)
		} else {
			h.broadcastMeetingASRInterim(roomID, state.client, event.Text)
		}
	}
	state.mu.Unlock()
	meta.meetingASRPublishMu.Unlock()
	if !allowed {
		h.closeMeetingASRStreamState(state)
	}
}

func (h *WebSocketHandler) broadcastMeetingASRInterim(roomID string, client *model.Client, text string) {
	payload := map[string]interface{}{
		"kind":        "asr-interim",
		"from":        client.UniqID,
		"speakerName": speakerDisplayName(client),
		"text":        text,
	}
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

func speakerDisplayName(client *model.Client) string {
	name := client.UserName
	if name == "" {
		name = client.UniqID
	}
	if index := strings.IndexByte(name, ':'); index > 0 {
		name = name[:index]
	}
	return name
}
