package handler

import (
	"time"

	"letshare-server/internal/meeting"
	"letshare-server/internal/model"
)

func meetingMinutesRoomCurrent(h *WebSocketHandler, roomID string, meta *meetingMeta) bool {
	if h == nil || meta == nil || meta.life == nil || meta.life.Phase() == meeting.PhaseEnded {
		return false
	}
	current, active := h.activeMeetingRoom(roomID)
	return active && current == meta
}

// minutesSubscriptionEnabledLocked treats an unset preference as enabled.
// Preferences are keyed by stable uniqID so they survive websocket rebinds.
func minutesSubscriptionEnabledLocked(meta *meetingMeta, uniqID string) bool {
	if meta == nil || uniqID == "" {
		return false
	}
	enabled, configured := meta.minutesSubscriptions[uniqID]
	return !configured || enabled
}

func setMinutesSubscriptionLocked(meta *meetingMeta, uniqID string, enabled bool) {
	if meta.minutesSubscriptions == nil {
		meta.minutesSubscriptions = make(map[string]bool)
	}
	meta.minutesSubscriptions[uniqID] = enabled
}

// electMeetingMinutesCoordinatorLocked keeps an eligible incumbent. If it is
// no longer eligible, the preferred current member wins; otherwise the first
// enabled member in registry join order is selected.
func electMeetingMinutesCoordinatorLocked(meta *meetingMeta, members []meetingMembership, preferred string) bool {
	if meta == nil {
		return false
	}
	enabled := make(map[string]bool, len(members))
	for _, member := range members {
		if minutesSubscriptionEnabledLocked(meta, member.UniqID) {
			enabled[member.UniqID] = true
		}
	}
	current := meta.minutes.SummaryCoordinatorID
	if meta.minutes.RecordingID == "" {
		if current == "" {
			return false
		}
		meta.minutes.SummaryCoordinatorID = ""
		meta.minutes.SummaryCoordinatorEpoch++
		meta.minutesSummaryRequestedRecordingID = ""
		meta.minutesSummaryRequestedCoordinatorEpoch = 0
		meta.minutesSummaryRequestedThroughSeq = 0
		meta.minutesSummaryRequestedAt = time.Time{}
		meta.minutesSummaryCachedOutput = ""
		meta.minutesSummaryCacheValid = false
		return true
	}
	if current != "" && enabled[current] {
		return false
	}
	next := ""
	if preferred != "" && enabled[preferred] {
		next = preferred
	} else {
		for _, member := range members {
			if enabled[member.UniqID] {
				next = member.UniqID
				break
			}
		}
	}
	if current == next {
		return false
	}
	meta.minutes.SummaryCoordinatorID = next
	meta.minutes.SummaryCoordinatorEpoch++
	meta.minutesSummaryRequestedRecordingID = ""
	meta.minutesSummaryRequestedCoordinatorEpoch = 0
	meta.minutesSummaryRequestedThroughSeq = 0
	meta.minutesSummaryRequestedAt = time.Time{}
	meta.minutesSummaryCachedOutput = ""
	meta.minutesSummaryCacheValid = false
	return true
}

func (h *WebSocketHandler) minutesStateForRecipient(meta *meetingMeta, uniqID string) (meetingMinutesState, bool) {
	if meta == nil {
		return meetingMinutesState{}, false
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	state := meetingMinutesWireState(meta.minutes)
	enabled := minutesSubscriptionEnabledLocked(meta, uniqID)
	if !enabled {
		state.Summary = ""
	}
	return state, enabled
}

// broadcastMeetingMinutesState is the only room fanout for public minutes
// state. A paused member still sees session/coordinator status, with summary
// content removed. Summary updates are sent only to enabled members.
func (h *WebSocketHandler) broadcastMeetingMinutesState(roomID, kind string, state meetingMinutesState) {
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		return
	}
	members := h.meetings.members(roomID)
	meta.minutesDeliveryMu.Lock()
	defer meta.minutesDeliveryMu.Unlock()
	if !meetingMinutesRoomCurrent(h, roomID, meta) {
		return
	}
	for _, member := range members {
		if !h.meetings.owns(roomID, member.UniqID, member.ClientID) {
			continue
		}
		personalized, enabled := h.minutesStateForRecipient(meta, member.UniqID)
		if kind == "summary" && !enabled {
			continue
		}
		payload := map[string]interface{}{
			"kind":                kind,
			"minutes":             personalized,
			"minutesSubscription": map[string]bool{"enabled": enabled},
		}
		if kind == "summary" {
			payload["summary"] = personalized.Summary
			payload["recordingId"] = personalized.RecordingID
			payload["throughSeq"] = personalized.SummaryThroughSeq
			payload["coordinatorEpoch"] = personalized.SummaryCoordinatorEpoch
		}
		h.sendMeetingToClient(member.ClientID, roomID, model.MessageTypeMeetingMinutes, payload)
	}
	_ = state // State is retained in the signature for call-site clarity; the
	// current locked snapshot is authoritative for per-recipient redaction.
}

func (h *WebSocketHandler) sendMeetingMinutesSubscriptionAck(clientID, roomID, uniqID string) {
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok {
		return
	}
	meta.minutesDeliveryMu.Lock()
	defer meta.minutesDeliveryMu.Unlock()
	if !meetingMinutesRoomCurrent(h, roomID, meta) {
		return
	}
	state, currentEnabled := h.minutesStateForRecipient(meta, uniqID)
	if !currentEnabled {
		state.Summary = ""
	}
	h.sendMeetingToClient(clientID, roomID, model.MessageTypeMeetingMinutes, map[string]interface{}{
		"kind":                    "subscription",
		"recordingId":             state.RecordingID,
		"minutes":                 state,
		"minutesSubscription":     map[string]bool{"enabled": currentEnabled},
		"summaryCoordinatorId":    state.SummaryCoordinatorID,
		"summaryCoordinatorEpoch": state.SummaryCoordinatorEpoch,
	})
}

func (h *WebSocketHandler) sendMeetingMinutesToEnabledMembers(roomID string, meta *meetingMeta, payload interface{}) {
	h.sendMeetingMinutesToEnabledMembersExcept(roomID, meta, payload, nil)
}

func (h *WebSocketHandler) sendMeetingMinutesToEnabledMembersExcept(roomID string, meta *meetingMeta, payload interface{}, excluded map[string]bool) {
	if meta == nil {
		return
	}
	members := h.meetings.members(roomID)
	meta.minutesDeliveryMu.Lock()
	defer meta.minutesDeliveryMu.Unlock()
	if !meetingMinutesRoomCurrent(h, roomID, meta) {
		return
	}
	meta.mu.Lock()
	enabled := make(map[string]bool, len(members))
	if excluded == nil {
		excluded = make(map[string]bool, len(meta.minutesReplayPending))
	}
	for clientID, pending := range meta.minutesReplayPending {
		if pending {
			excluded[clientID] = true
		}
	}
	for _, member := range members {
		enabled[member.UniqID] = minutesSubscriptionEnabledLocked(meta, member.UniqID)
	}
	meta.mu.Unlock()
	delivered := make(map[string]bool, len(members))
	for _, member := range members {
		if delivered[member.ClientID] || excluded[member.ClientID] || !enabled[member.UniqID] || !h.meetings.owns(roomID, member.UniqID, member.ClientID) {
			continue
		}
		delivered[member.ClientID] = true
		h.sendMeetingToClient(member.ClientID, roomID, model.MessageTypeMeetingMinutes, payload)
	}
}

func (h *WebSocketHandler) handleMeetingMinutesSubscription(client *model.Client, roomID string, meta *meetingMeta, enabled bool, recordingID string, afterSeq *uint64) {
	meta.meetingASRPublishMu.Lock()
	defer meta.meetingASRPublishMu.Unlock()
	current, active := h.activeMeetingRoom(roomID)
	if !active || current != meta || !h.meetings.owns(roomID, client.UniqID, client.ID) {
		h.sendError(client, 403, "meeting:minutes subscription sender is no longer a current member")
		return
	}
	recordingID = sanitizeMeetingRecordingID(recordingID)
	meta.minutesDeliveryMu.Lock()
	meta.mu.Lock()
	activeRecordingID := meta.minutes.RecordingID
	latestSeq := meta.minutes.LatestSegmentSeq
	if enabled && recordingID != "" && recordingID != activeRecordingID {
		meta.mu.Unlock()
		meta.minutesDeliveryMu.Unlock()
		h.sendError(client, 409, "meeting:minutes subscription recording epoch is stale")
		return
	}
	if enabled && afterSeq != nil && *afterSeq > latestSeq {
		meta.mu.Unlock()
		meta.minutesDeliveryMu.Unlock()
		h.sendError(client, 409, "meeting:minutes subscription cursor is ahead of the current epoch")
		return
	}
	setMinutesSubscriptionLocked(meta, client.UniqID, enabled)
	meta.mu.Unlock()
	members := h.meetings.members(roomID)
	meta.mu.Lock()
	coordinatorChanged := electMeetingMinutesCoordinatorLocked(meta, members, client.UniqID)
	state := meetingMinutesWireState(meta.minutes)
	meta.mu.Unlock()
	meta.minutesDeliveryMu.Unlock()

	h.sendMeetingMinutesSubscriptionAck(client.ID, roomID, client.UniqID)
	if enabled && activeRecordingID != "" {
		cursor := uint64(0)
		if afterSeq != nil {
			cursor = *afterSeq
		}
		h.sendMeetingMinutesReplay(roomID, client.ID, meta, cursor, true)
	}
	if coordinatorChanged {
		h.broadcastMeetingMinutesState(roomID, "coordinator", state)
	}
	if !enabled {
		h.stopMeetingMinutesWithNoReceiversLocked(roomID)
	}
}

// stopMeetingMinutesWithNoReceiversLocked ends an active shared-minutes
// session once every current member has explicitly paused reception. An unset
// preference counts as enabled, so a new member or a member who never opened
// the notes panel keeps the session alive. Caller holds meta.meetingASRPublishMu.
// Lock order: meetingASRPublishMu -> minutesDeliveryMu -> meta.mu.
func (h *WebSocketHandler) stopMeetingMinutesWithNoReceiversLocked(roomID string) bool {
	meta, active := h.activeMeetingRoom(roomID)
	if !active {
		return false
	}
	members := h.meetings.members(roomID)
	if len(members) == 0 {
		return false
	}
	meta.mu.Lock()
	if !meta.minutes.Running {
		meta.mu.Unlock()
		return false
	}
	for _, member := range members {
		if minutesSubscriptionEnabledLocked(meta, member.UniqID) {
			meta.mu.Unlock()
			return false
		}
	}
	meta.minutes.Running = false
	meta.minutes.SummaryCoordinatorID = ""
	meta.minutes.SummaryCoordinatorEpoch++
	meta.minutesSummaryRequestedRecordingID = ""
	meta.minutesSummaryRequestedCoordinatorEpoch = 0
	meta.minutesSummaryRequestedThroughSeq = 0
	meta.minutesSummaryRequestedAt = time.Time{}
	meta.minutesSummaryCachedOutput = ""
	meta.minutesSummaryCacheValid = false
	if meta.minutesFlushTimer != nil {
		meta.minutesFlushTimer.Stop()
		meta.minutesFlushTimer = nil
	}
	meta.minutesPending = nil
	clearMeetingMinutesConsentIfIdle(meta)
	state := meetingMinutesWireState(meta.minutes)
	meta.mu.Unlock()

	session := h.detachMeetingAudioMinutes(roomID)
	h.broadcastMeetingMinutesState(roomID, "stopped", state)
	if session != nil {
		go session.Abort()
	}
	return true
}

func (h *WebSocketHandler) reconcileMeetingMinutesCoordinator(roomID string) {
	meta, active := h.activeMeetingRoom(roomID)
	if !active {
		return
	}
	meta.meetingASRPublishMu.Lock()
	defer meta.meetingASRPublishMu.Unlock()
	current, stillActive := h.activeMeetingRoom(roomID)
	if !stillActive || current != meta {
		return
	}
	members := h.meetings.members(roomID)
	meta.mu.Lock()
	changed := electMeetingMinutesCoordinatorLocked(meta, members, "")
	state := meetingMinutesWireState(meta.minutes)
	meta.mu.Unlock()
	if changed {
		h.broadcastMeetingMinutesState(roomID, "coordinator", state)
	}
	h.stopMeetingMinutesWithNoReceiversLocked(roomID)
}
