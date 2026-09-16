package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestMeetingInviteTerminalStateIsReclaimed(t *testing.T) {
	ts, host, invitee, _, meetingID, sourceRoom := meetingInviteFlow(t)

	sendInvite := func() string {
		t.Helper()
		data, _ := json.Marshal(map[string]interface{}{
			"action": "invite", "to": "guestB:uuid-2", "sourceRoomId": sourceRoom,
		})
		if err := host.sendJSON(model.WebSocketMessage{
			Type: model.MessageTypeMeetingInvite, Channel: meetingID, Data: data,
		}); err != nil {
			t.Fatalf("send invite: %v", err)
		}
		message, err := waitInvite(invitee, "invite", 5*time.Second)
		if err != nil {
			t.Fatalf("wait invite: %v", err)
		}
		inviteID, _ := invitePayloadField(t, message, "inviteId").(string)
		if inviteID == "" {
			t.Fatal("invite id is empty")
		}
		return inviteID
	}

	respond := func(inviteID, action string) {
		t.Helper()
		data, _ := json.Marshal(map[string]interface{}{
			"action": action, "inviteId": inviteID,
		})
		if err := invitee.sendJSON(model.WebSocketMessage{
			Type: model.MessageTypeMeetingInvite, Channel: meetingID, Data: data,
		}); err != nil {
			t.Fatalf("send invite response: %v", err)
		}
		if _, err := waitInviteStatus(host, action, 5*time.Second); err != nil {
			t.Fatalf("wait invite response %s: %v", action, err)
		}
	}

	assertGone := func(inviteID string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if _, loaded := ts.handler.activeMeetingInvites.Load(inviteID); !loaded {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("invite %s remained in activeMeetingInvites", inviteID)
	}

	accepted := sendInvite()
	respond(accepted, "accept")
	assertGone(accepted)

	rejected := sendInvite()
	respond(rejected, "reject")
	assertGone(rejected)
}

func TestMeetingInviteExpiredSweepIsReclaimed(t *testing.T) {
	ts, _, _, _, meetingID, sourceRoom := meetingInviteFlow(t)
	invite := &meetingInvite{
		InviteID:   "expired-sweep",
		MeetingID:  meetingID,
		From:       "hostA:uuid-1",
		To:         "guestB:uuid-2",
		SourceRoom: sourceRoom,
		ExpiresAt:  time.Now().Add(-time.Second),
		Status:     "pending",
	}
	ts.handler.activeMeetingInvites.Store(invite.InviteID, invite)

	ts.handler.cleanupExpiredMeetingInvites()
	if _, loaded := ts.handler.activeMeetingInvites.Load(invite.InviteID); loaded {
		t.Fatal("expired invite remained after maintenance sweep")
	}
	if invite.Status != "expired" {
		t.Fatalf("expired invite status = %q, want expired", invite.Status)
	}
}
