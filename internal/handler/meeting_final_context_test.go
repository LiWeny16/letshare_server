package handler

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMeetingFinalContextIsBoundedAndPreservesNewestSpeech(t *testing.T) {
	meta := newMeetingMeta(meetingMetaInit{Title: "Planning"})
	meta.minutes.RecordingID = "active"
	meta.minutes.HistoryStartSeq = 2
	meta.minutes.Summary = `{"timeline":[{"startMs":1,"endMs":2,"summary":"Early discussion"}]}`
	for i := 0; i < 30; i++ {
		meta.minutesSegments = append(meta.minutesSegments, meetingMinutesSegment{
			SegmentID: fmt.Sprintf("id-%d", i), RecordingID: "active", SpeakerName: "Ana", Final: true,
			Text: strings.Repeat("会议内容", 500),
		})
	}
	input := buildMeetingFinalContextLocked(meta)
	if len(input) > 14_000 {
		t.Fatalf("context is too large: %d bytes", len(input))
	}
	var parsed struct {
		Timeline         []struct{ Summary string } `json:"timeline"`
		Segments         []struct{ ID string }      `json:"segments"`
		ContextTruncated bool                       `json:"contextTruncated"`
	}
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		t.Fatal(err)
	}
	if !parsed.ContextTruncated || len(parsed.Timeline) != 1 || parsed.Timeline[0].Summary != "Early discussion" || len(parsed.Segments) == 0 || parsed.Segments[len(parsed.Segments)-1].ID != "id-29" {
		t.Fatalf("context lost its outline, newest speech, or truncation marker: %#v", parsed)
	}
}
