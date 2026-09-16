package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestMeetingFileTransfer_UsesMeetingMembershipWithoutOrdinaryRoomSubscription(t *testing.T) {
	_, host, guest, bystander, meetingID := meetingChatFlow(t)

	request := model.FileTransferRequest{
		TransferID: "meeting-file-1", FileName: "photo.png", FileSize: 4,
		FileType: "image/png", ChunkSize: 64 * 1024, TotalChunks: 1,
		FromUserID: "forged-sender", ToUserID: "bob:chat-2", RoomName: meetingID,
	}
	data, _ := json.Marshal(request)
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeFileTransferRequest, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("send meeting file request: %v", err)
	}

	select {
	case message := <-guest.fileTransfer:
		if message.Type != model.MessageTypeFileTransferRequest || message.Channel != meetingID {
			t.Fatalf("unexpected file request: %#v", message)
		}
		var forwarded model.FileTransferRequest
		if err := json.Unmarshal(message.Data, &forwarded); err != nil {
			t.Fatalf("decode file request: %v", err)
		}
		if forwarded.FromUserID != "alice:chat-1" || forwarded.ToUserID != "bob:chat-2" {
			t.Fatalf("meeting transfer must use uniqID identities: %#v", forwarded)
		}
	case message := <-host.err:
		t.Fatalf("meeting transfer rejected: %#v", message)
	case <-time.After(5 * time.Second):
		t.Fatal("meeting member did not receive file request")
	}

	select {
	case message := <-bystander.fileTransfer:
		t.Fatalf("non-member received meeting file request: %#v", message)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestMeetingFileTransfer_RejectsNonMemberSenderAndTarget(t *testing.T) {
	_, host, _, bystander, meetingID := meetingChatFlow(t)

	send := func(sender *wsRPC, transferID, to string) {
		t.Helper()
		request := model.FileTransferRequest{
			TransferID: transferID, FileName: "file.txt", FileSize: 4,
			FileType: "text/plain", ChunkSize: 64 * 1024, TotalChunks: 1,
			ToUserID: to, RoomName: meetingID,
		}
		data, _ := json.Marshal(request)
		if err := sender.sendJSON(model.WebSocketMessage{Type: model.MessageTypeFileTransferRequest, Channel: meetingID, Data: data}); err != nil {
			t.Fatalf("send file request: %v", err)
		}
	}

	send(bystander, "outsider-file", "bob:chat-2")
	select {
	case message := <-bystander.fileTransfer:
		if message.Type != model.MessageTypeFileTransferError {
			t.Fatalf("non-member sender received wrong response: %#v", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("non-member sender was not rejected")
	}
	send(host, "bad-target-file", "carol:chat-3")
	select {
	case message := <-host.fileTransfer:
		if message.Type != model.MessageTypeFileTransferError {
			t.Fatalf("non-member target received wrong response: %#v", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("non-member target was not rejected")
	}
}
