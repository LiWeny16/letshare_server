package handler

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func waitPresentation(t *testing.T, r *wsRPC, timeout time.Duration) map[string]interface{} {
	t.Helper()
	select {
	case m := <-r.presentation:
		var payload map[string]interface{}
		if err := json.Unmarshal(m.Data, &payload); err != nil {
			t.Fatalf("解析 meeting:presentation 失败: %v", err)
		}
		return payload
	case <-time.After(timeout):
		t.Fatalf("等待 meeting:presentation 超时")
		return nil
	}
}

func waitExcalidraw(t *testing.T, r *wsRPC, timeout time.Duration) map[string]interface{} {
	t.Helper()
	select {
	case m := <-r.excalidraw:
		var payload map[string]interface{}
		if err := json.Unmarshal(m.Data, &payload); err != nil {
			t.Fatalf("解析 meeting:excalidraw 失败: %v", err)
		}
		return payload
	case <-time.After(timeout):
		t.Fatalf("等待 meeting:excalidraw 超时")
		return nil
	}
}

func TestMeetingPresentation_ScreenOwnershipAndAuthorization(t *testing.T) {
	ts, a, b, c, meetingID := meetingChatFlow(t)
	_ = ts

	claim := func(r *wsRPC, user, mode, boardMode string) {
		t.Helper()
		payload := map[string]string{"action": "claim", "mode": mode}
		if boardMode != "" {
			payload["boardMode"] = boardMode
		}
		data, _ := json.Marshal(payload)
		if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
			t.Fatalf("%s claim 失败: %v", user, err)
		}
	}

	claim(a, "alice", "screen", "")
	for _, r := range []*wsRPC{a, b} {
		state := waitPresentation(t, r, 2*time.Second)
		if state["ownerId"] != "alice:chat-1" || state["mode"] != "screen" {
			t.Fatalf("第一次 claim 状态错误: %#v", state)
		}
	}
	select {
	case msg := <-c.presentation:
		t.Fatalf("未加入会议的订阅者不应收到 presentation: %#v", msg)
	case <-time.After(150 * time.Millisecond):
	}

	claim(b, "bob", "whiteboard", "excalidraw")
	for _, r := range []*wsRPC{a, b} {
		state := waitPresentation(t, r, 2*time.Second)
		if state["ownerId"] != "alice:chat-1" || state["mode"] != "screen" || state["screenOwnerId"] != "alice:chat-1" || state["whiteboardActive"] != true || state["boardMode"] != "excalidraw" {
			t.Fatalf("抢占后的唯一主持状态错误: %#v", state)
		}
		if epoch, ok := state["epoch"].(float64); !ok || epoch != 2 {
			t.Fatalf("抢占后的 epoch 错误: %#v", state["epoch"])
		}
	}

	data, _ := json.Marshal(map[string]string{"action": "claim", "mode": "screen"})
	if err := c.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("第三方发送 presentation 失败: %v", err)
	}
	errMsg, err := c.waitError(2 * time.Second)
	if err != nil || errMsg == "" {
		t.Fatalf("第三方应被拒绝，实际 err=%q: %v", errMsg, err)
	}

	data, _ = json.Marshal(map[string]string{"action": "release", "mode": "screen"})
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("host release 失败: %v", err)
	}
	for _, r := range []*wsRPC{a, b} {
		state := waitPresentation(t, r, 2*time.Second)
		if state["ownerId"] != "bob:chat-2" || state["mode"] != "whiteboard" || state["screenOwnerId"] != "" || state["whiteboardActive"] != true {
			t.Fatalf("release 后仍存在主持者: %#v", state)
		}
	}
}

func TestMeetingExcalidraw_OperationAckIsAuthoritativeAndDeduplicated(t *testing.T) {
	_, a, b, _, meetingID := meetingChatFlow(t)
	operation := map[string]interface{}{
		"id":           "op-create-1",
		"kind":         "create",
		"baseRevision": 0,
		"elementIds":   []string{"shape-1"},
	}
	scene := map[string]interface{}{
		"delta": false,
		"elements": []map[string]interface{}{
			{"id": "shape-1", "type": "rectangle", "version": 1, "x": 10},
		},
	}
	data, err := json.Marshal(map[string]interface{}{"action": "update", "scene": scene, "operation": operation})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送白板 operation 失败: %v", err)
	}
	ack := waitExcalidraw(t, a, 2*time.Second)
	if ack["action"] != "ack" || ack["accepted"] != true || ack["operationId"] != "op-create-1" {
		t.Fatalf("白板 operation ack 不正确: %#v", ack)
	}
	if revision, ok := ack["revision"].(float64); !ok || revision != 1 {
		t.Fatalf("白板 operation revision 不正确: %#v", ack["revision"])
	}
	snapshot := waitExcalidraw(t, b, 2*time.Second)
	if snapshot["operationId"] != "op-create-1" || snapshot["action"] != "snapshot" {
		t.Fatalf("白板 operation 广播不正确: %#v", snapshot)
	}

	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("重放白板 operation 失败: %v", err)
	}
	duplicateAck := waitExcalidraw(t, a, 2*time.Second)
	if duplicateAck["action"] != "ack" || duplicateAck["revision"] != float64(1) {
		t.Fatalf("重复 operation 未复用原 ack: %#v", duplicateAck)
	}
	select {
	case duplicate := <-b.excalidraw:
		t.Fatalf("重复 operation 不应再次广播: %#v", duplicate)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestMeetingPresentation_ScreenAndWhiteboardAreIndependent(t *testing.T) {
	_, host, guest, _, meetingID := meetingChatFlow(t)

	send := func(r *wsRPC, payload map[string]string) {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	assertState := func(expectedMode, expectedOwner string, expectedScreenOwner string, expectedWhiteboard bool) {
		t.Helper()
		for _, r := range []*wsRPC{host, guest} {
			state := waitPresentation(t, r, 2*time.Second)
			if state["mode"] != expectedMode || state["ownerId"] != expectedOwner || state["screenOwnerId"] != expectedScreenOwner || state["whiteboardActive"] != expectedWhiteboard {
				t.Fatalf("presentation state mismatch: %#v", state)
			}
		}
	}

	send(host, map[string]string{"action": "claim", "mode": "screen"})
	assertState("screen", "alice:chat-1", "alice:chat-1", false)

	send(guest, map[string]string{"action": "claim", "mode": "whiteboard", "boardMode": "excalidraw"})
	assertState("screen", "alice:chat-1", "alice:chat-1", true)

	send(host, map[string]string{"action": "release", "mode": "screen"})
	assertState("whiteboard", "bob:chat-2", "", true)

	// If the screen owner leaves, the still-active whiteboard becomes the stage.
	send(host, map[string]string{"action": "claim", "mode": "screen"})
	assertState("screen", "alice:chat-1", "alice:chat-1", true)
	leaveData, _ := json.Marshal(map[string]string{})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: meetingID, Data: leaveData}); err != nil {
		t.Fatal(err)
	}
	state := waitPresentation(t, guest, 2*time.Second)
	if state["mode"] != "whiteboard" || state["ownerId"] != "bob:chat-2" || state["screenOwnerId"] != "" || state["whiteboardActive"] != true {
		t.Fatalf("screen owner leave should preserve whiteboard: %#v", state)
	}
}

func TestMeetingPresentation_FollowFocusRequestAndScreenOwnerForceOpen(t *testing.T) {
	_, host, guest, _, meetingID := meetingChatFlow(t)

	send := func(r *wsRPC, payload map[string]interface{}) {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	waitBoth := func() map[string]interface{} {
		t.Helper()
		state := waitPresentation(t, host, 2*time.Second)
		_ = waitPresentation(t, guest, 2*time.Second)
		return state
	}

	send(host, map[string]interface{}{"action": "claim", "mode": "screen"})
	waitBoth()
	send(guest, map[string]interface{}{"action": "claim", "mode": "whiteboard", "boardMode": "excalidraw"})
	waitBoth()

	// The contributor can change the global follow signal without ending the
	// board session. A follower may then choose to remain locally hidden.
	send(guest, map[string]interface{}{"action": "visibility", "visible": false})
	state := waitBoth()
	if state["whiteboardActive"] != true || state["whiteboardVisible"] != false || state["whiteboardForceOpen"] != false {
		t.Fatalf("contributor visibility must be independent from board lifetime: %#v", state)
	}

	// The screen owner can force all clients back to the board without taking
	// ownership away from the whiteboard contributor.
	send(host, map[string]interface{}{"action": "force-open"})
	state = waitBoth()
	if state["whiteboardActive"] != true || state["whiteboardVisible"] != true || state["whiteboardForceOpen"] != true || state["whiteboardLeaderId"] != "bob:chat-2" {
		t.Fatalf("screen owner force-open must preserve the contributor: %#v", state)
	}

	request, _ := json.Marshal(map[string]interface{}{"action": "focus-request"})
	if err := guest.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingSharingRequest, Channel: meetingID, Data: request}); err != nil {
		t.Fatal(err)
	}
	var requestPayload map[string]interface{}
	select {
	case message := <-host.sharingRequest:
		if err := json.Unmarshal(message.Data, &requestPayload); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("screen owner did not receive focus request")
	}
	if requestPayload["action"] != "focus-request" || requestPayload["from"] != "bob:chat-2" {
		t.Fatalf("unexpected focus request: %#v", requestPayload)
	}

	response, _ := json.Marshal(map[string]interface{}{
		"action": "focus-response", "requestId": requestPayload["requestId"], "to": "bob:chat-2", "accepted": true,
	})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingSharingRequest, Channel: meetingID, Data: response}); err != nil {
		t.Fatal(err)
	}
	state = waitBoth()
	if state["whiteboardFocusEpoch"].(float64) < 2 {
		t.Fatalf("accepted focus request must advance focus epoch: %#v", state)
	}

	// The same focus state machine must be able to target the independent
	// screen publisher after a board focus is released.
	send(host, map[string]interface{}{"action": "release-focus"})
	waitBoth()
	send(host, map[string]interface{}{"action": "force-focus", "target": "screen"})
	state = waitBoth()
	if state["focusTarget"] != "screen" || state["whiteboardForceOpen"] == true {
		t.Fatalf("screen focus must not force-open the whiteboard: %#v", state)
	}

	screenRequest, _ := json.Marshal(map[string]interface{}{"action": "focus-request", "target": "screen"})
	if err := guest.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingSharingRequest, Channel: meetingID, Data: screenRequest}); err != nil {
		t.Fatal(err)
	}
	var screenRequestPayload map[string]interface{}
	select {
	case message := <-host.sharingRequest:
		if err := json.Unmarshal(message.Data, &screenRequestPayload); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("screen owner did not receive video focus request")
	}
	if screenRequestPayload["target"] != "screen" {
		t.Fatalf("screen focus request lost its target: %#v", screenRequestPayload)
	}
	screenResponse, _ := json.Marshal(map[string]interface{}{
		"action": "focus-response", "requestId": screenRequestPayload["requestId"], "to": "bob:chat-2", "target": "screen", "accepted": true,
	})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingSharingRequest, Channel: meetingID, Data: screenResponse}); err != nil {
		t.Fatal(err)
	}
	state = waitBoth()
	if state["focusTarget"] != "screen" {
		t.Fatalf("accepted screen focus request must target the screen: %#v", state)
	}
}

func TestMeetingPresentation_PresenterLeaseAndFollowRequest(t *testing.T) {
	_, host, guest, _, meetingID := meetingChatFlow(t)

	send := func(r *wsRPC, payload map[string]interface{}) {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	waitBoth := func() map[string]interface{} {
		t.Helper()
		state := waitPresentation(t, host, 2*time.Second)
		_ = waitPresentation(t, guest, 2*time.Second)
		return state
	}

	// The first media contributor becomes the presenter and followers see the
	// same target without a separate approval dialog.
	send(host, map[string]interface{}{"action": "claim", "mode": "screen"})
	state := waitBoth()
	if state["presenterId"] != "alice:chat-1" || state["presenterTarget"] != "screen" {
		t.Fatalf("first screen contributor should become presenter: %#v", state)
	}

	// A second independent whiteboard does not silently steal the presenter
	// role; the contributor can explicitly take over from the top bar.
	send(guest, map[string]interface{}{"action": "claim", "mode": "whiteboard", "boardMode": "excalidraw"})
	state = waitBoth()
	if state["presenterId"] != "alice:chat-1" || state["presenterTarget"] != "screen" || state["whiteboardActive"] != true {
		t.Fatalf("independent whiteboard must not change presenter implicitly: %#v", state)
	}

	send(guest, map[string]interface{}{"action": "presenter-claim"})
	state = waitBoth()
	if state["presenterId"] != "bob:chat-2" || state["presenterTarget"] != "whiteboard" {
		t.Fatalf("explicit presenter takeover should target the claimant's board: %#v", state)
	}
	followEpoch, ok := state["presenterFollowEpoch"].(float64)
	if !ok || followEpoch < 1 {
		t.Fatalf("presenter takeover should trigger a new automatic-follow epoch: %#v", state)
	}

	send(guest, map[string]interface{}{"action": "presenter-target", "target": "camera"})
	state = waitBoth()
	if state["presenterTarget"] != "camera" {
		t.Fatalf("presenter should be able to switch the shared target to video: %#v", state)
	}

	send(guest, map[string]interface{}{"action": "presenter-follow-all"})
	state = waitBoth()
	nextFollowEpoch, ok := state["presenterFollowEpoch"].(float64)
	if !ok || nextFollowEpoch <= followEpoch {
		t.Fatalf("follow-all is a one-shot epoch, not a permanent lock: %#v", state)
	}

	// Any member may take the presenter lease explicitly, including a member
	// already publishing another source.
	send(host, map[string]interface{}{"action": "presenter-claim"})
	state = waitBoth()
	if state["presenterId"] != "alice:chat-1" || state["presenterTarget"] != "screen" {
		t.Fatalf("presenter lease should be transferable without host approval: %#v", state)
	}

	// Ending a presenter's own screen share ends that source. If another source
	// is still active, its owner receives the presenter lease.
	send(host, map[string]interface{}{"action": "presenter-release"})
	state = waitBoth()
	if state["screenOwnerId"] != "" || state["presenterId"] != "bob:chat-2" || state["presenterTarget"] != "whiteboard" {
		t.Fatalf("presenter release should end the source and hand off to the remaining source: %#v", state)
	}
}

func TestMeetingPresentation_PresenterViewportIsCentralizedAndBounded(t *testing.T) {
	_, host, guest, _, meetingID := meetingChatFlow(t)

	send := func(r *wsRPC, payload map[string]interface{}) {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingPresentation, Channel: meetingID, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	waitBoth := func() map[string]interface{} {
		t.Helper()
		state := waitPresentation(t, host, 2*time.Second)
		_ = waitPresentation(t, guest, 2*time.Second)
		return state
	}

	send(host, map[string]interface{}{"action": "claim", "mode": "whiteboard", "boardMode": "excalidraw"})
	waitBoth()
	send(host, map[string]interface{}{
		"action":   "viewport",
		"viewport": map[string]interface{}{"centerX": 123.4567, "centerY": -55.6789, "zoom": 99},
	})
	state := waitBoth()
	viewport, ok := state["whiteboardViewport"].(map[string]interface{})
	if !ok || viewport["centerX"] != 123.46 || viewport["centerY"] != -55.68 || viewport["zoom"] != 8.0 {
		t.Fatalf("presenter viewport should be centralized, rounded and bounded: %#v", state)
	}

	send(guest, map[string]interface{}{
		"action":   "viewport",
		"viewport": map[string]interface{}{"centerX": 1, "centerY": 2, "zoom": 1},
	})
	errMsg, err := guest.waitError(2 * time.Second)
	if err != nil || errMsg == "" {
		t.Fatalf("non-presenter viewport update should be rejected, err=%q: %v", errMsg, err)
	}
}

func TestMeetingExcalidraw_SnapshotOnlyToMeetingMembers(t *testing.T) {
	ts, a, b, c, meetingID := meetingChatFlow(t)
	_ = ts

	scene := map[string]interface{}{
		"elements": []map[string]interface{}{{"id": "rect-1", "type": "rectangle", "x": 10, "y": 20, "width": 100, "height": 60}},
		"appState": map[string]interface{}{"viewBackgroundColor": "#ffffff"},
	}
	data, _ := json.Marshal(map[string]interface{}{"action": "update", "scene": scene})
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("发送 Excalidraw scene 失败: %v", err)
	}
	for _, r := range []*wsRPC{b} {
		payload := waitExcalidraw(t, r, 2*time.Second)
		if payload["action"] != "snapshot" || payload["revision"] != float64(1) {
			t.Fatalf("Excalidraw snapshot 错误: %#v", payload)
		}
	}
	select {
	case msg := <-a.excalidraw:
		t.Fatalf("Excalidraw sender should not receive its own snapshot: %#v", msg)
	case <-time.After(150 * time.Millisecond):
	}
	select {
	case msg := <-c.excalidraw:
		t.Fatalf("未加入会议的订阅者不应收到 Excalidraw: %#v", msg)
	case <-time.After(150 * time.Millisecond):
	}

	request, _ := json.Marshal(map[string]string{"action": "request"})
	if err := b.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: request}); err != nil {
		t.Fatalf("请求 Excalidraw snapshot 失败: %v", err)
	}
	payload := waitExcalidraw(t, b, 2*time.Second)
	if payload["revision"] != float64(1) {
		t.Fatalf("request 返回 revision 错误: %#v", payload)
	}

	// An image update carries its binary file once. A later drawing update may
	// omit the unchanged data URL, but a late request must still receive it.
	imageScene := map[string]interface{}{
		"elements": []map[string]interface{}{{"id": "image-element", "type": "image", "fileId": "image-1"}},
		"files": map[string]interface{}{"image-1": map[string]interface{}{
			"id": "image-1", "mimeType": "image/png", "dataURL": "data:image/png;base64,small", "created": 1,
		}},
	}
	imageData, _ := json.Marshal(map[string]interface{}{"action": "update", "scene": imageScene})
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: imageData}); err != nil {
		t.Fatalf("发送带图片的 Excalidraw scene 失败: %v", err)
	}
	_ = waitExcalidraw(t, b, 2*time.Second)
	followup, _ := json.Marshal(map[string]interface{}{
		"action": "update", "scene": map[string]interface{}{
			"elements": []map[string]interface{}{{"id": "image-element", "type": "image", "fileId": "image-1"}, {"id": "rect-2", "type": "rectangle"}},
		},
	})
	if err := a.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: followup}); err != nil {
		t.Fatalf("发送不带重复图片数据的 Excalidraw scene 失败: %v", err)
	}
	_ = waitExcalidraw(t, b, 2*time.Second)
	if err := b.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: request}); err != nil {
		t.Fatalf("请求带图片的 Excalidraw snapshot 失败: %v", err)
	}
	payload = waitExcalidraw(t, b, 2*time.Second)
	snapshot, ok := payload["scene"].(map[string]interface{})
	if !ok {
		t.Fatalf("Excalidraw snapshot scene 类型错误: %#v", payload["scene"])
	}
	if _, ok := snapshot["files"].(map[string]interface{}); !ok {
		t.Fatalf("late join snapshot must retain Excalidraw binary files: %#v", snapshot)
	}

	bad, _ := json.Marshal(map[string]interface{}{"action": "update", "scene": map[string]interface{}{"notElements": true}})
	if err := c.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingExcalidraw, Channel: meetingID, Data: bad}); err != nil {
		t.Fatalf("第三方发送 Excalidraw 失败: %v", err)
	}
	if msg, err := c.waitError(2 * time.Second); err != nil || msg == "" {
		t.Fatalf("第三方 Excalidraw 应被拒绝: %q %v", msg, err)
	}
}

func TestMeetingPresentation_MessageShapeHasNoClientAuthorityFields(t *testing.T) {
	var state struct {
		Mode    string `json:"mode"`
		OwnerID string `json:"ownerId"`
		Epoch   uint64 `json:"epoch"`
	}
	if err := json.Unmarshal([]byte(`{"mode":"screen","ownerId":"server-user","epoch":3}`), &state); err != nil {
		t.Fatal(err)
	}
	if state.OwnerID != "server-user" || state.Epoch != 3 {
		t.Fatalf("服务端主持状态解析异常: %s", fmt.Sprint(state))
	}
}

func TestMergeExcalidrawScene_DeltaDoesNotDuplicateFreehandTail(t *testing.T) {
	previous := []byte(`{"elements":[{"id":"stroke","type":"freedraw","version":1,"points":[[0,0],[1,1]]}],"appState":{"viewBackgroundColor":"#ffffff"}}`)
	incoming := []byte(`{"delta":true,"elements":[{"id":"stroke","type":"freedraw","version":2,"pointsAppend":[[2,2]]}]}`)
	merged, changed, err := mergeExcalidrawScene(previous, incoming, true)
	if err != nil || !changed {
		t.Fatalf("第一次增量合并失败: changed=%v err=%v", changed, err)
	}
	mergedAgain, changedAgain, err := mergeExcalidrawScene(merged, incoming, true)
	if err != nil || changedAgain {
		t.Fatalf("重复增量不应再次改变场景: changed=%v err=%v scene=%s", changedAgain, err, mergedAgain)
	}
	var scene map[string]interface{}
	if err := json.Unmarshal(mergedAgain, &scene); err != nil {
		t.Fatal(err)
	}
	elements := scene["elements"].([]interface{})
	points := elements[0].(map[string]interface{})["points"].([]interface{})
	if len(points) != 3 {
		t.Fatalf("增量尾部被重复追加: %s", mergedAgain)
	}
}

func TestMergeExcalidrawScene_SameVersionTailUsesBaseLength(t *testing.T) {
	previous := []byte(`{"elements":[{"id":"stroke","type":"freedraw","version":7,"points":[[0,0],[1,1]]}]}`)
	incoming := []byte(`{"delta":true,"elements":[{"id":"stroke","type":"freedraw","version":7,"pointsBase":2,"pointsAppend":[[2,2]]}]}`)
	merged, changed, err := mergeExcalidrawScene(previous, incoming, true)
	if err != nil || !changed {
		t.Fatalf("same-version freehand tail should merge: changed=%v err=%v", changed, err)
	}
	mergedAgain, changedAgain, err := mergeExcalidrawScene(merged, incoming, true)
	if err != nil || changedAgain {
		t.Fatalf("same-version tail replay should be ignored: changed=%v err=%v", changedAgain, err)
	}
	var scene struct {
		Elements []struct {
			Points [][]float64 `json:"points"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(mergedAgain, &scene); err != nil {
		t.Fatalf("decode merged same-version scene: %v", err)
	}
	if got := len(scene.Elements[0].Points); got != 3 {
		t.Fatalf("same-version tail was duplicated or lost: %d points", got)
	}
}

func TestMergeExcalidrawScene_StaleTailDoesNotDropPoints(t *testing.T) {
	previous := []byte(`{"elements":[{"id":"stroke","type":"freedraw","version":9,"points":[[0,0],[1,1],[2,2]]}]}`)
	incoming := []byte(`{"delta":true,"elements":[{"id":"stroke","type":"freedraw","version":10,"pointsBase":2,"pointsAppend":[[3,3]]}]}`)
	merged, changed, err := mergeExcalidrawScene(previous, incoming, true)
	if err != nil || changed {
		t.Fatalf("stale tail should be ignored: changed=%v err=%v", changed, err)
	}
	var scene struct {
		Elements []struct {
			Points [][]float64 `json:"points"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(merged, &scene); err != nil || len(scene.Elements) != 1 || len(scene.Elements[0].Points) != 3 {
		t.Fatalf("stale tail changed the authoritative scene: %s", merged)
	}
}

func TestMergeExcalidrawScene_DeltaPreservesConcurrentElements(t *testing.T) {
	previous := []byte(`{"elements":[{"id":"alice","type":"rectangle","version":1},{"id":"bob","type":"ellipse","version":1}]}`)
	incoming := []byte(`{"delta":true,"elements":[{"id":"alice","type":"rectangle","version":2,"x":20},{"id":"carol","type":"line","version":1}]}`)
	merged, changed, err := mergeExcalidrawScene(previous, incoming, true)
	if err != nil || !changed {
		t.Fatalf("并发元素合并失败: changed=%v err=%v", changed, err)
	}
	var scene map[string]interface{}
	if err := json.Unmarshal(merged, &scene); err != nil {
		t.Fatal(err)
	}
	if len(scene["elements"].([]interface{})) != 3 {
		t.Fatalf("并发元素被覆盖: %s", merged)
	}
}
