package handler

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"letshare-server/internal/model"
	"letshare-server/internal/service"
)

var fileTransferV2Magic = []byte("LSF2")

const (
	fileTransferV2HeaderMax     = 1024
	fileTransferV2MaxFrameBytes = 4 + 2 + fileTransferV2HeaderMax + service.FileTransferV2MaxFramePayload
)

type fileTransferV2FrameHeader struct {
	Version     int    `json:"version"`
	TransferID  string `json:"transferId"`
	Index       int    `json:"index"`
	Offset      int    `json:"offset"`
	TotalLength int    `json:"totalLength"`
	Hash        string `json:"hash"`
}

func (h *WebSocketHandler) processFileTransferV2Control(client *model.Client, message *model.WebSocketMessage) {
	if client == nil || message == nil {
		return
	}
	var control model.FileTransferV2Control
	decoder := json.NewDecoder(bytes.NewReader(message.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil {
		h.sendFileTransferV2Error(client, message.Channel, "", "invalid control JSON")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		h.sendFileTransferV2Error(client, message.Channel, "", "invalid control JSON")
		return
	}
	if control.Version != 2 {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "unsupported file v2 version")
		return
	}
	if !v2BoundedControlString(control.From, 256) || !v2BoundedControlString(control.To, 256) || (control.Reason != "" && v2JSLength(control.Reason) > 1024) || (control.SessionID != "" && v2JSLength(control.SessionID) > 128) {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "invalid file v2 control metadata")
		return
	}
	if control.Capabilities != nil && !validFileTransferV2Capabilities(*control.Capabilities) {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "invalid file v2 capabilities")
		return
	}
	if authenticated, _ := client.Metadata["authenticated"].(bool); !authenticated {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "authenticated WebSocket required")
		return
	}
	identity := fileTransferV2Identity(client)
	if identity == "" {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "missing authenticated identity")
		return
	}

	if control.Kind == "hello" {
		if control.TransferID != "" {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "hello must not include transferId")
			return
		}
		maxSize := h.fileTransferService.MaxFileSize()
		if isPro, _ := client.Metadata["isPro"].(bool); !isPro && maxSize > service.NonProSizeLimit {
			maxSize = service.NonProSizeLimit
		}
		ack := model.FileTransferV2Control{
			Version: 2, Kind: "hello-ack", From: "server", To: identity,
			Capabilities: &model.FileTransferV2Capabilities{
				Version: 2, MaxFileSize: maxSize, MaxFrameBytes: fileTransferV2MaxFrameBytes,
				MaxCreditBytes: service.FileTransferV2MaxCredit, Resume: true, Multipath: true,
			},
		}
		h.sendMessage(client, model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, message.Channel, "", ack))
		return
	}
	if control.Kind == "hello-ack" {
		h.sendFileTransferV2Error(client, message.Channel, "", "hello-ack is server-only")
		return
	}
	if control.TransferID == "" {
		h.sendFileTransferV2Error(client, message.Channel, "", "transferId is required")
		return
	}
	if !service.IsValidFileTransferV2ID(control.TransferID) {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "invalid transferId")
		return
	}
	if message.Channel == "" || !h.fileTransferV2IsMember(client, message.Channel) {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "room membership required")
		return
	}

	if control.Kind == "offer" {
		if control.Manifest == nil || control.Manifest.TransferID != control.TransferID || control.To == "" || control.To == identity || !h.fileTransferV2FindMember(message.Channel, control.To) {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "offer requires a valid manifest and room participant")
			return
		}
		control.From = identity
		control.Manifest.TransferID = control.TransferID
		isPro, _ := client.Metadata["isPro"].(bool)
		if err := h.fileTransferService.V2().Create(*control.Manifest, identity, control.To, message.Channel, isPro); err != nil {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, err.Error())
			return
		}
		if err := h.forwardFileTransferV2(message.Channel, control.To, control); err != nil {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "receiver is unavailable; offer remains recoverable")
		}
		return
	}

	task, err := h.fileTransferService.V2().Get(control.TransferID)
	if err != nil {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "transfer is unavailable or expired")
		return
	}
	if task.Room != message.Channel || (identity != task.From && identity != task.To) {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "transfer participant or room mismatch")
		return
	}
	if control.Kind == "accept" && identity != task.To {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "only the receiver may accept")
		return
	}
	if control.Kind == "resume" && identity != task.To {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "only the receiver may resume")
		return
	}
	if control.Kind == "accept" && task.Status != "offered" && task.Status != "accepted" && task.Status != "transferring" {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "transfer cannot be accepted in its current state")
		return
	}
	if control.Kind == "reject" && identity != task.To {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "only the receiver may reject")
		return
	}
	if control.Kind == "reject" && task.Status != "offered" && task.Status != "rejected" {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "transfer cannot be rejected in its current state")
		return
	}
	if control.Kind == "checkpoint" && identity != task.To {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "only the receiver may checkpoint")
		return
	}
	if control.Kind == "checkpoint" && task.Status != "accepted" && task.Status != "transferring" {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "transfer cannot be checkpointed in its current state")
		return
	}
	if control.CreditBytes != nil && (*control.CreditBytes < 0 || *control.CreditBytes > service.FileTransferV2MaxCredit) {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "creditBytes exceeds the supported window")
		return
	}
	other := task.To
	if identity == task.To {
		other = task.From
	}
	if control.To != other {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "control target does not match transfer participant")
		return
	}
	if control.Ranges != nil && control.Kind != "accept" && control.Kind != "checkpoint" && control.Kind != "resume" {
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "ranges are not valid for this control kind")
		return
	}
	if control.Ranges != nil {
		if err := service.ValidateFileTransferV2Ranges(*control.Ranges, task.Manifest.TotalBlocks); err != nil {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, err.Error())
			return
		}
	}
	control.From, control.To = identity, other
	control.Manifest = nil
	control.Capabilities = nil
	switch control.Kind {
	case "accept":
		if control.Ranges != nil {
			if err := h.fileTransferService.V2().Checkpoint(control.TransferID, *control.Ranges); err != nil {
				h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "could not persist receiver checkpoint")
				return
			}
		}
		if err := h.fileTransferService.V2().SetStatus(control.TransferID, "accepted", false); err != nil {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "could not persist acceptance")
			return
		}
	case "reject":
		if err := h.fileTransferService.V2().SetStatus(control.TransferID, "rejected", true); err != nil {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "could not persist rejection")
			return
		}
	case "checkpoint":
		var ranges [][]int
		if control.Ranges != nil {
			ranges = *control.Ranges
		}
		if err := h.fileTransferService.V2().Checkpoint(control.TransferID, ranges); err != nil {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "could not persist receiver checkpoint")
			return
		}
	case "complete":
		if identity != task.To {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "only the receiver may complete")
			return
		}
		if task.Status != "completed" {
			if task.Status != "accepted" && task.Status != "transferring" {
				h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "transfer cannot complete in its current state")
				return
			}
			if err := h.fileTransferService.V2().SetStatus(control.TransferID, "completed", true); err != nil {
				h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "could not persist completion")
				return
			}
		}
	case "cancel":
		if task.Status != "offered" && task.Status != "accepted" && task.Status != "transferring" && task.Status != "cancelled" {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "transfer cannot be cancelled in its current state")
			return
		}
		if err := h.fileTransferService.V2().SetStatus(control.TransferID, "cancelled", true); err != nil {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "could not persist cancellation")
			return
		}
	case "resume":
		if task.Status != "offered" && task.Status != "accepted" && task.Status != "transferring" {
			h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "transfer cannot be resumed in its current state")
			return
		}
		if control.Ranges != nil {
			if err := h.fileTransferService.V2().Checkpoint(control.TransferID, *control.Ranges); err != nil {
				h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "could not persist receiver checkpoint")
				return
			}
		}
		if task.Status == "offered" {
			if err := h.fileTransferService.V2().SetStatus(control.TransferID, "accepted", false); err != nil {
				h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "could not persist resumed acceptance")
				return
			}
		}
	case "error":
	default:
		h.sendFileTransferV2Error(client, message.Channel, control.TransferID, "unknown file v2 control kind")
		return
	}
	if err := h.forwardFileTransferV2(task.Room, other, control); err != nil {
		h.sendFileTransferV2Error(client, task.Room, control.TransferID, "participant is unavailable; transfer remains recoverable")
	}
}

func (h *WebSocketHandler) processFileTransferV2Binary(client *model.Client, frame []byte) bool {
	if len(frame) < 4 || !bytes.Equal(frame[:4], fileTransferV2Magic) {
		return false
	}
	if len(frame) < 6 || len(frame) > fileTransferV2MaxFrameBytes {
		h.sendFileTransferV2Error(client, "", "", "invalid LSF2 frame size")
		return true
	}
	headerLength := int(binary.BigEndian.Uint16(frame[4:6]))
	if headerLength < 2 || headerLength > fileTransferV2HeaderMax || len(frame) < 6+headerLength || !utf8.Valid(frame[6:6+headerLength]) {
		h.sendFileTransferV2Error(client, "", "", "invalid LSF2 header")
		return true
	}
	headerData := frame[6 : 6+headerLength]
	if !json.Valid(headerData) {
		h.sendFileTransferV2Error(client, "", "", "invalid LSF2 header JSON")
		return true
	}
	var header fileTransferV2FrameHeader
	decoder := json.NewDecoder(bytes.NewReader(headerData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&header); err != nil || header.Version != 2 || header.TransferID == "" {
		h.sendFileTransferV2Error(client, "", header.TransferID, "invalid LSF2 metadata")
		return true
	}
	if !service.IsValidFileTransferV2ID(header.TransferID) {
		h.sendFileTransferV2Error(client, "", header.TransferID, "invalid transferId")
		return true
	}
	payload := frame[6+headerLength:]
	if len(payload) > service.FileTransferV2MaxFramePayload {
		h.sendFileTransferV2Error(client, "", header.TransferID, "LSF2 payload exceeds 16 KiB")
		return true
	}
	task, err := h.fileTransferService.V2().Get(header.TransferID)
	if err != nil {
		h.sendFileTransferV2Error(client, "", header.TransferID, "transfer is unavailable or expired")
		return true
	}
	identity := fileTransferV2Identity(client)
	if !fileTransferV2Authenticated(client) || identity != task.From || !h.fileTransferV2IsMember(client, task.Room) {
		h.sendFileTransferV2Error(client, task.Room, header.TransferID, "only the authenticated sender room member may relay blocks")
		return true
	}
	block, complete, err := h.fileTransferService.V2().AddFragment(header.TransferID, header.Index, header.Offset, header.TotalLength, header.Hash, payload)
	if err != nil {
		reason := "invalid or unavailable LSF2 block"
		if errors.Is(err, service.ErrFileTransferV2Quota) {
			reason = service.ErrFileTransferV2Quota.Error()
		} else if errors.Is(err, service.ErrFileTransferV2Frames) {
			reason = service.ErrFileTransferV2Frames.Error()
		}
		h.sendFileTransferV2Error(client, task.Room, header.TransferID, reason)
		return true
	}
	if complete {
		if err := h.forwardFileTransferV2Block(client, task, header, block); err != nil {
			h.sendFileTransferV2Error(client, task.Room, header.TransferID, "receiver relay failed; resend this block to retry")
		}
	}
	return true
}

func fileTransferV2Authenticated(client *model.Client) bool {
	if client == nil || client.Metadata == nil {
		return false
	}
	value, _ := client.Metadata["authenticated"].(bool)
	return value
}

func v2JSLength(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func v2BoundedControlString(value string, maxLength int) bool {
	length := v2JSLength(value)
	return length > 0 && length <= maxLength
}

func validFileTransferV2Capabilities(capabilities model.FileTransferV2Capabilities) bool {
	if capabilities.Version != 2 || capabilities.MaxFileSize < 0 || capabilities.MaxFileSize > 3*1024*1024*1024 || capabilities.MaxCreditBytes < 0 || capabilities.MaxCreditBytes > service.FileTransferV2MaxCredit {
		return false
	}
	header, err := json.Marshal(fileTransferV2FrameHeader{Version: 2, TransferID: "aaaaaaaa", Index: 0, Offset: 0, TotalLength: 1, Hash: strings.Repeat("0", 64)})
	if err != nil {
		return false
	}
	minimumFrameBytes := 6 + len(header) + 1
	return capabilities.MaxFrameBytes >= minimumFrameBytes && capabilities.MaxFrameBytes <= fileTransferV2MaxFrameBytes
}

func fileTransferV2Identity(client *model.Client) string {
	if client == nil {
		return ""
	}
	if client.UniqID != "" {
		return client.UniqID
	}
	return client.UserID
}

func (h *WebSocketHandler) fileTransferV2IsMember(client *model.Client, room string) bool {
	if h.hasRegistered(room) {
		return h.meetings.owns(room, client.UniqID, client.ID)
	}
	_, ok := h.wsService.GetClientInRoom(client.ID, room)
	return ok
}

func (h *WebSocketHandler) fileTransferV2FindMember(room, identity string) bool {
	if h.hasRegistered(room) {
		clientID, ok := h.meetings.clientForUniqID(room, identity)
		if !ok {
			return false
		}
		client, ok := h.wsService.GetClient(clientID)
		return ok && client != nil && h.meetings.owns(room, client.UniqID, client.ID)
	}
	return h.wsService.FindClientByUniqID(identity, room) != nil || h.wsService.FindClientByUserID(identity, room) != nil
}

func (h *WebSocketHandler) forwardFileTransferV2(room, recipient string, control model.FileTransferV2Control) error {
	if !h.fileTransferV2FindMember(room, recipient) {
		return fmt.Errorf("file v2 participant is not in room")
	}
	message := model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, room, "", control)
	return h.fileTransferService.SendMessageToUser(recipient, room, message)
}

func (h *WebSocketHandler) sendFileTransferV2Error(client *model.Client, room, transferID, reason string) {
	if client == nil {
		return
	}
	control := model.FileTransferV2Control{
		Version: 2, Kind: "error", TransferID: transferID,
		From: "server", To: fileTransferV2Identity(client), Reason: reason,
	}
	var payload interface{} = control
	if transferID == "" {
		payload = map[string]interface{}{
			"version": 2, "kind": "error", "transferId": "", "from": "server",
			"to": fileTransferV2Identity(client), "reason": reason,
		}
	}
	h.sendMessage(client, model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, room, "", payload))
}

func (h *WebSocketHandler) forwardFileTransferV2Block(sender *model.Client, task *service.FileTransferV2Session, header fileTransferV2FrameHeader, block []byte) error {
	if !h.fileTransferV2FindMember(task.Room, task.To) {
		return errors.New("receiver unavailable")
	}
	var target *model.Client
	if h.hasRegistered(task.Room) {
		clientID, ok := h.meetings.clientForUniqID(task.Room, task.To)
		if ok {
			target, _ = h.wsService.GetClient(clientID)
		}
	} else {
		target = h.wsService.FindClientByUniqID(task.To, task.Room)
		if target == nil {
			target = h.wsService.FindClientByUserID(task.To, task.Room)
		}
	}
	if target == nil {
		return errors.New("receiver unavailable")
	}
	if len(block) == 0 {
		return h.writeFileTransferV2Frame(target, header, nil)
	}
	for offset := 0; offset < len(block); {
		end := offset + service.FileTransferV2MaxFramePayload
		if end > len(block) {
			end = len(block)
		}
		frameHeader := header
		frameHeader.Offset = offset
		frameHeader.TotalLength = len(block)
		if err := h.writeFileTransferV2Frame(target, frameHeader, block[offset:end]); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func (h *WebSocketHandler) writeFileTransferV2Frame(target *model.Client, header fileTransferV2FrameHeader, payload []byte) error {
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(headerBytes) > fileTransferV2HeaderMax {
		return errors.New("LSF2 header exceeds limit")
	}
	frame := make([]byte, 6+len(headerBytes)+len(payload))
	copy(frame[:4], fileTransferV2Magic)
	binary.BigEndian.PutUint16(frame[4:6], uint16(len(headerBytes)))
	copy(frame[6:], headerBytes)
	copy(frame[6+len(headerBytes):], payload)
	return h.wsService.WriteBinaryToClient(target, frame)
}
