package handler

import (
	"testing"
	"time"
)

func TestMeetingASRFiltersSingleCharactersAndFillers(t *testing.T) {
	for _, text := range []string{"", "。", "嗯。", "好！", "嗯 啊，呃……", "哈哈", "hmm, um", "嗯 uh"} {
		if meaningfulMeetingASRText(text) {
			t.Errorf("noise %q was not filtered", text)
		}
	}
	for _, text := range []string{"你好", "嗯，可以", "OK", "yes", "嗯，继续讨论"} {
		if !meaningfulMeetingASRText(text) {
			t.Errorf("meaningful speech %q was filtered", text)
		}
	}
}

func TestMeetingASRNoiseDoesNotAdvanceSequenceOrPublish(t *testing.T) {
	_, host, member, _, roomID := meetingChatFlow(t)
	configureAndMarkMinutesRunningForRelayTests(t, host, roomID, false, host, member)
	recordingID := currentMinutesRecordingID(t, host.handler, roomID)
	for _, text := range []string{"嗯。", "啊", "嗯，啊", "！"} {
		sendMixedSegment(host.handler, roomID, text, recordingID)
	}
	meta, _ := host.handler.activeMeetingRoom(roomID)
	meta.mu.Lock()
	sequence, historyCount := meta.minutes.LatestSegmentSeq, len(meta.minutesSegments)
	meta.mu.Unlock()
	if sequence != 0 || historyCount != 0 {
		t.Fatalf("noise advanced shared history: sequence=%d history=%d", sequence, historyCount)
	}
	if _, err := waitMinutesKind(member, "segments", 200*time.Millisecond); err == nil {
		t.Fatal("noise was broadcast to a recipient")
	}
	sendMixedSegment(host.handler, roomID, "我们开始讨论", recordingID)
	message, err := waitMinutesKind(member, "segments", 3*time.Second)
	if err != nil {
		t.Fatal("meaningful speech was not broadcast", err)
	}
	segments := segmentsFromBatch(t, message)
	if len(segments) != 1 || segments[0]["seq"] != float64(1) || segments[0]["text"] != "我们开始讨论" {
		t.Fatalf("noise created a gap or changed the transcript: %#v", segments)
	}
}
