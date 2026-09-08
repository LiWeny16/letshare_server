package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

const (
	serverURL = "ws://localhost:8080/ws"
	fileRoom  = "proto-file"
)

type wireMessage struct {
	Type    string          `json:"type"`
	Channel string          `json:"channel,omitempty"`
	Event   string          `json:"event,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type frame struct {
	messageType int
	data        []byte
	message     wireMessage
}

type client struct {
	name string
	conn *websocket.Conn
	in   chan frame
}

func authToken() string {
	h := sha256.Sum256([]byte("sever_auth_123"))
	return hex.EncodeToString(h[:])
}

func targetServerURL() string {
	if value := os.Getenv("PROTOCOL_CHECK_URL"); value != "" {
		return value
	}
	return serverURL
}

func dial(name string) (*client, error) {
	q := url.Values{}
	q.Set("token", authToken())
	q.Set("userId", name)
	conn, _, err := websocket.DefaultDialer.Dial(targetServerURL()+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", name, err)
	}
	c := &client{name: name, conn: conn, in: make(chan frame, 128)}
	go func() {
		defer close(c.in)
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f := frame{messageType: messageType, data: data}
			if messageType == websocket.TextMessage {
				if err := json.Unmarshal(data, &f.message); err != nil {
					continue
				}
				if f.message.Type == "membership:snapshot" || f.message.Event == "membership:changed" {
					continue
				}
			}
			c.in <- f
		}
	}()
	return c, nil
}

func (c *client) close() {
	_ = c.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "protocol check done"),
		time.Now().Add(time.Second))
	_ = c.conn.Close()
}

func (c *client) send(kind, channel string, data any) error {
	var raw json.RawMessage
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		raw = encoded
	}
	return c.conn.WriteJSON(wireMessage{Type: kind, Channel: channel, Data: raw})
}

func (c *client) wait(label string, predicate func(frame) bool) (frame, error) {
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-c.in:
			if !ok {
				return frame{}, fmt.Errorf("%s: websocket closed", label)
			}
			if predicate(f) {
				return f, nil
			}
			if f.messageType == websocket.TextMessage && f.message.Type == "error" {
				if f.message.Error != nil {
					return f, fmt.Errorf("%s: server error %d: %s", label, f.message.Error.Code, f.message.Error.Message)
				}
				return f, fmt.Errorf("%s: server error", label)
			}
		case <-timer.C:
			return frame{}, fmt.Errorf("%s: timeout", label)
		}
	}
}

func waitType(c *client, label, typ, channel string) (frame, error) {
	return c.wait(label, func(f frame) bool {
		return f.messageType == websocket.TextMessage && f.message.Type == typ &&
			(channel == "" || f.message.Channel == channel)
	})
}

func jsonData[T any](f frame, out *T) error {
	if f.messageType != websocket.TextMessage {
		return fmt.Errorf("expected text message")
	}
	return json.Unmarshal(f.message.Data, out)
}

func checkMeeting() error {
	host, err := dial("protocol-host")
	if err != nil {
		return err
	}
	defer host.close()
	guest, err := dial("protocol-guest")
	if err != nil {
		return err
	}
	defer guest.close()

	if err := host.send("meeting:create", "", map[string]string{"title": "protocol check"}); err != nil {
		return fmt.Errorf("meeting:create send: %w", err)
	}
	created, err := waitType(host, "meeting:create response", "meeting:create", "")
	if err != nil {
		return err
	}
	var createdData struct {
		RoomID string `json:"roomId"`
	}
	if err := jsonData(created, &createdData); err != nil {
		return fmt.Errorf("meeting:create data: %w", err)
	}
	if !regexp.MustCompile(`^\d{4}$`).MatchString(createdData.RoomID) {
		return fmt.Errorf("meeting:create returned invalid roomId %q", createdData.RoomID)
	}
	room := createdData.RoomID
	fmt.Printf("[PASS] meeting:create -> room=%s\n", room)

	for _, c := range []*client{host, guest} {
		if err := c.send("subscribe", room, nil); err != nil {
			return fmt.Errorf("subscribe %s: %w", c.name, err)
		}
		if _, err := waitType(c, "subscribe "+c.name, "subscribed", room); err != nil {
			return err
		}
		if err := c.send("meeting:join", room, map[string]string{"roomId": room}); err != nil {
			return fmt.Errorf("meeting:join %s: %w", c.name, err)
		}
		if _, err := waitType(c, "meeting:info "+c.name, "meeting:info", room); err != nil {
			return err
		}
	}
	fmt.Printf("[PASS] meeting:join -> host+guest in room=%s\n", room)
	if err := checkBreakout(host, guest, room); err != nil {
		return err
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("client peer connection: %w", err)
	}
	defer pc.Close()
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"protocol-audio", "protocol-stream")
	if err != nil {
		return fmt.Errorf("create audio track: %w", err)
	}
	if _, err := pc.AddTrack(audioTrack); err != nil {
		return fmt.Errorf("add audio track: %w", err)
	}
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"protocol-video", "protocol-stream")
	if err != nil {
		return fmt.Errorf("create video track: %w", err)
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		return fmt.Errorf("add video track: %w", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local offer: %w", err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	offer = *pc.LocalDescription()
	if !strings.Contains(offer.SDP, "a=ice-ufrag:") {
		return fmt.Errorf("client offer unexpectedly has no ICE ufrag")
	}

	// Negative control: the exact failure reported by the browser is reproduced
	// by removing ICE credentials from an otherwise valid Pion offer.
	withoutICE := strings.ReplaceAll(offer.SDP, "a=ice-ufrag:", "a=x-removed-ice-ufrag:")
	withoutICE = strings.ReplaceAll(withoutICE, "a=ice-pwd:", "a=x-removed-ice-pwd:")
	if err := host.send("meeting:sdp", room, map[string]string{"type": "offer", "sdp": withoutICE}); err != nil {
		return fmt.Errorf("send no-ICE offer: %w", err)
	}
	bad, err := host.wait("no-ICE rejection", func(f frame) bool {
		return f.messageType == websocket.TextMessage && f.message.Type == "error"
	})
	if err != nil {
		return err
	}
	badMessage := ""
	if bad.message.Error != nil {
		badMessage = bad.message.Error.Message
	}
	if !strings.Contains(badMessage, "no ice-ufrag") {
		return fmt.Errorf("negative control returned unexpected error %q", badMessage)
	}
	fmt.Printf("[PASS] negative SDP control -> server rejects missing ICE credentials: %q\n", badMessage)

	// Positive control: retry the valid offer in the same room. This proves the
	// server rolled back the failed remote offer instead of poisoning the
	// participant's signaling state.
	if err := host.send("meeting:sdp", room, map[string]string{"type": "offer", "sdp": offer.SDP}); err != nil {
		return fmt.Errorf("send valid offer: %w", err)
	}
	answer, err := waitType(host, "valid meeting:sdp answer after retry", "meeting:sdp", room)
	if err != nil {
		return err
	}
	var answerData struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	if err := jsonData(answer, &answerData); err != nil {
		return fmt.Errorf("answer data: %w", err)
	}
	if answerData.Type != "answer" || !strings.Contains(answerData.SDP, "a=ice-ufrag:") {
		return fmt.Errorf("invalid server answer: type=%q hasICE=%t", answerData.Type, strings.Contains(answerData.SDP, "a=ice-ufrag:"))
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerData.SDP}); err != nil {
		return fmt.Errorf("client rejected server answer: %w", err)
	}
	fmt.Printf("[PASS] meeting:sdp -> server answer accepted by client (sdp=%d bytes)\n", len(answerData.SDP))
	return nil
}

func checkBreakout(host, guest *client, room string) error {
	childRoom := room + "B1"
	if err := host.send("meeting:breakout", room, map[string]any{
		"action":      "create",
		"assignments": []map[string]any{{"room": childRoom, "members": []string{guest.name}}},
	}); err != nil {
		return fmt.Errorf("breakout create send: %w", err)
	}
	invite, err := guest.wait("breakout invite", func(f frame) bool {
		return f.messageType == websocket.TextMessage && f.message.Type == "meeting:breakout" && f.message.Channel == room
	})
	if err != nil {
		return err
	}
	var inviteData struct {
		Action string `json:"action"`
		Room   string `json:"room"`
	}
	if err := jsonData(invite, &inviteData); err != nil {
		return fmt.Errorf("breakout invite data: %w", err)
	}
	if inviteData.Action != "invite" || inviteData.Room != childRoom {
		return fmt.Errorf("unexpected breakout invite: action=%q room=%q", inviteData.Action, inviteData.Room)
	}
	if err := guest.send("subscribe", childRoom, nil); err != nil {
		return fmt.Errorf("breakout subscribe: %w", err)
	}
	if _, err := waitType(guest, "breakout subscribed", "subscribed", childRoom); err != nil {
		return err
	}
	if err := guest.send("meeting:join", childRoom, map[string]string{"roomId": childRoom}); err != nil {
		return fmt.Errorf("breakout join: %w", err)
	}
	if _, err := waitType(guest, "breakout meeting info", "meeting:info", childRoom); err != nil {
		return err
	}
	if err := host.send("meeting:breakout", room, map[string]string{"action": "recall"}); err != nil {
		return fmt.Errorf("breakout recall send: %w", err)
	}
	recall, err := guest.wait("breakout recall", func(f frame) bool {
		return f.messageType == websocket.TextMessage && f.message.Type == "meeting:breakout" && f.message.Channel == childRoom
	})
	if err != nil {
		return err
	}
	var recallData struct {
		Action string `json:"action"`
		Room   string `json:"room"`
	}
	if err := jsonData(recall, &recallData); err != nil {
		return fmt.Errorf("breakout recall data: %w", err)
	}
	if recallData.Action != "recall" || recallData.Room != room {
		return fmt.Errorf("unexpected breakout recall: action=%q room=%q", recallData.Action, recallData.Room)
	}
	fmt.Printf("[PASS] meeting:breakout -> invite and recall delivered (%s)\n", childRoom)
	return nil
}

func checkFileTransfer() error {
	sender, err := dial("protocol-sender")
	if err != nil {
		return err
	}
	defer sender.close()
	receiver, err := dial("protocol-receiver")
	if err != nil {
		return err
	}
	defer receiver.close()

	for _, c := range []*client{sender, receiver} {
		if err := c.send("subscribe", fileRoom, nil); err != nil {
			return fmt.Errorf("file subscribe %s: %w", c.name, err)
		}
		if _, err := waitType(c, "file subscribe "+c.name, "subscribed", fileRoom); err != nil {
			return err
		}
	}

	payload := []byte("LetShare backend protocol file check: 3.8")
	transferID := fmt.Sprintf("protocol-transfer-%d", time.Now().UnixNano())
	request := map[string]any{
		"transfer_id":  transferID,
		"file_name":    "protocol-check.txt",
		"file_size":    len(payload),
		"file_type":    "text/plain",
		"chunk_size":   len(payload),
		"total_chunks": 1,
		"to_user_id":   "protocol-receiver",
		"room_name":    fileRoom,
	}
	if err := sender.send("file:transfer:request", fileRoom, request); err != nil {
		return fmt.Errorf("file request send: %w", err)
	}
	reqFrame, err := waitType(receiver, "file request", "file:transfer:request", fileRoom)
	if err != nil {
		return err
	}
	var requestData map[string]any
	if err := jsonData(reqFrame, &requestData); err != nil {
		return fmt.Errorf("file request data: %w", err)
	}
	if requestData["transfer_id"] != transferID || requestData["from_user_id"] != "protocol-sender" {
		return fmt.Errorf("server rewrote request incorrectly: %#v", requestData)
	}

	if err := receiver.send("file:transfer:accept", fileRoom, map[string]string{"transfer_id": transferID}); err != nil {
		return fmt.Errorf("file accept send: %w", err)
	}
	if _, err := waitType(sender, "file accept", "file:transfer:accept", fileRoom); err != nil {
		return err
	}
	if err := sender.send("file:transfer:start", fileRoom, map[string]string{"transfer_id": transferID}); err != nil {
		return fmt.Errorf("file start send: %w", err)
	}
	if _, err := waitType(receiver, "file start", "file:transfer:start", fileRoom); err != nil {
		return err
	}

	meta, _ := json.Marshal(map[string]any{
		"transfer_id": transferID, "chunk_index": 0,
		"chunk_size": len(payload), "total_chunks": 1,
	})
	framed := make([]byte, 256+len(payload))
	copy(framed, meta)
	copy(framed[256:], payload)
	if err := sender.conn.WriteMessage(websocket.BinaryMessage, framed); err != nil {
		return fmt.Errorf("file binary send: %w", err)
	}
	chunk, err := receiver.wait("file binary chunk", func(f frame) bool {
		return f.messageType == websocket.BinaryMessage
	})
	if err != nil {
		return err
	}
	if !bytes.Equal(chunk.data, framed) || !bytes.Equal(chunk.data[256:], payload) {
		return fmt.Errorf("binary relay mismatch: got=%d expected=%d", len(chunk.data), len(framed))
	}
	fmt.Printf("[PASS] file binary relay -> %d payload bytes preserved\n", len(payload))

	if err := sender.send("file:transfer:end", fileRoom, map[string]string{"transfer_id": transferID}); err != nil {
		return fmt.Errorf("file end send: %w", err)
	}
	if _, err := waitType(receiver, "file end", "file:transfer:end", fileRoom); err != nil {
		return err
	}
	if err := receiver.send("file:transfer:complete", fileRoom, map[string]string{"transfer_id": transferID}); err != nil {
		return fmt.Errorf("file complete send: %w", err)
	}
	if _, err := waitType(sender, "file complete", "file:transfer:complete", fileRoom); err != nil {
		return err
	}
	fmt.Println("[PASS] file transfer lifecycle -> request -> accept -> start -> chunk -> end -> complete")
	return nil
}

func main() {
	fmt.Println("LetShare external Go WebSocket protocol check")
	fmt.Println("target:", targetServerURL())
	if os.Getenv("PROTOCOL_CHECK_MODE") != "file" {
		if err := checkMeeting(); err != nil {
			fmt.Println("[FAIL] meeting:", err)
			return
		}
	}
	if err := checkFileTransfer(); err != nil {
		fmt.Println("[FAIL] file:", err)
		return
	}
	fmt.Println("RESULT: PASS (running Go server accepted meeting and file-transfer protocols)")
}
