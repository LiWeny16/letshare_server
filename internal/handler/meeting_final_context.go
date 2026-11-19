package handler

import (
	"encoding/json"
	"strings"
)

// buildMeetingFinalContextLocked is used only for a host ending a meeting
// while their own receipt is paused. It uses the room's bounded in-memory
// history without sending that history back to the paused host.
func buildMeetingFinalContextLocked(meta *meetingMeta) string {
	type outlineItem struct {
		StartMS int64  `json:"startMs"`
		EndMS   int64  `json:"endMs"`
		Summary string `json:"summary"`
	}
	type transcriptItem struct {
		ID            string `json:"id"`
		SpeakerName   string `json:"speakerName"`
		StartMS       int64  `json:"startMs"`
		Text          string `json:"text"`
		TextTruncated bool   `json:"textTruncated,omitempty"`
	}
	var shared struct {
		Timeline         []outlineItem `json:"timeline"`
		ContextTruncated bool          `json:"contextTruncated"`
	}
	_ = json.Unmarshal([]byte(meta.minutes.Summary), &shared)
	context := struct {
		MeetingTitle     string           `json:"meetingTitle"`
		Timeline         []outlineItem    `json:"timeline"`
		Segments         []transcriptItem `json:"segments"`
		ContextTruncated bool             `json:"contextTruncated"`
	}{MeetingTitle: meta.Title, Timeline: []outlineItem{}, Segments: []transcriptItem{}, ContextTruncated: true}
	omitted := shared.ContextTruncated || meta.minutes.HistoryStartSeq > 1
	encodedSize := func() int { encoded, _ := json.Marshal(context); return len(encoded) }
	for _, item := range shared.Timeline {
		runes := []rune(item.Summary)
		if len(runes) > 400 {
			item.Summary = string(runes[:400])
			omitted = true
		}
		context.Timeline = append(context.Timeline, item)
		if encodedSize() > 6_000 {
			context.Timeline = context.Timeline[:len(context.Timeline)-1]
			omitted = true
			break
		}
	}
	for i := len(meta.minutesSegments) - 1; i >= 0; i-- {
		segment := meta.minutesSegments[i]
		if !segment.Final || strings.TrimSpace(segment.Text) == "" || segment.RecordingID != meta.minutes.RecordingID {
			continue
		}
		item := transcriptItem{ID: segment.SegmentID, SpeakerName: segment.SpeakerName, StartMS: segment.StartMs, Text: segment.Text}
		context.Segments = append([]transcriptItem{item}, context.Segments...)
		if encodedSize() <= 13_999 {
			continue
		}
		context.Segments = context.Segments[1:]
		omitted = true
		if len(context.Segments) == 0 {
			runes := []rune(item.Text)
			low, high, best := 0, len(runes), ""
			for low <= high {
				middle := (low + high) / 2
				item.Text, item.TextTruncated = string(runes[:middle])+"…", true
				context.Segments = []transcriptItem{item}
				if encodedSize() <= 13_999 {
					best, low = item.Text, middle+1
				} else {
					high = middle - 1
				}
			}
			context.Segments = nil
			if best != "" {
				item.Text = best
				context.Segments = []transcriptItem{item}
			}
		}
		break
	}
	context.ContextTruncated = omitted
	encoded, _ := json.Marshal(context)
	return string(encoded)
}
