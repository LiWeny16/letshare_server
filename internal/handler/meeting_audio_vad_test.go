package handler

import (
	"encoding/binary"
	"testing"
	"time"
)

func meetingAudioVADPCMFrame(level int16) []byte {
	pcm := make([]byte, meetingAudioFrameBytes)
	for offset := 0; offset < len(pcm); offset += 2 {
		binary.LittleEndian.PutUint16(pcm[offset:offset+2], uint16(level))
	}
	return pcm
}

func meetingAudioVADPush(vad *meetingAudioVAD, frame []byte, start time.Time, index int) []meetingAudioSpeechSegment {
	return vad.PushFrame(frame, start.Add(time.Duration(index)*meetingAudioVADFrameDuration))
}

func TestMeetingAudioVADKeepsLeadingAndTrailingPadding(t *testing.T) {
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	vad := newMeetingAudioVAD()
	quiet := meetingAudioVADPCMFrame(0)
	voice := meetingAudioVADPCMFrame(400)
	var segments []meetingAudioSpeechSegment

	for index := 0; index < 12; index++ {
		segments = append(segments, meetingAudioVADPush(vad, quiet, start, index)...)
	}
	for index := 12; index < 15; index++ {
		segments = append(segments, meetingAudioVADPush(vad, voice, start, index)...)
	}
	for index := 15; index < 30; index++ {
		segments = append(segments, meetingAudioVADPush(vad, quiet, start, index)...)
	}

	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(segments))
	}
	segment := segments[0]
	wantStart := start.Add(2 * meetingAudioVADFrameDuration)
	wantEnd := start.Add(30 * meetingAudioVADFrameDuration)
	if !segment.Start.Equal(wantStart) || !segment.End.Equal(wantEnd) {
		t.Fatalf("interval = [%s, %s), want [%s, %s)", segment.Start, segment.End, wantStart, wantEnd)
	}
	wantFrames := meetingAudioVADPreRollFrames + 3 + meetingAudioVADHangoverFrames
	if got := len(segment.PCM) / meetingAudioFrameBytes; got != wantFrames {
		t.Fatalf("segment frames = %d, want %d", got, wantFrames)
	}
}

func TestMeetingAudioVADCapturesLowLevelShortSpeechButRejectsQuietNoise(t *testing.T) {
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	vad := newMeetingAudioVAD()
	noise := meetingAudioVADPCMFrame(30)
	quietSpeech := meetingAudioVADPCMFrame(180) // below the previous 300-level sample cutoff

	for index := 0; index < 20; index++ {
		if segments := meetingAudioVADPush(vad, noise, start, index); len(segments) != 0 {
			t.Fatalf("quiet noise emitted %d segments", len(segments))
		}
	}
	// 100ms is deliberately shorter than the legacy 250ms voice threshold.
	for index := 20; index < 25; index++ {
		if segments := meetingAudioVADPush(vad, quietSpeech, start, index); len(segments) != 0 {
			t.Fatalf("short speech closed early with %d segments", len(segments))
		}
	}
	segments := vad.Flush()
	if len(segments) != 1 {
		t.Fatalf("short speech flush returned %d segments, want 1", len(segments))
	}
	if got := len(segments[0].PCM) / meetingAudioFrameBytes; got != meetingAudioVADPreRollFrames+5 {
		t.Fatalf("short speech segment frames = %d, want %d", got, meetingAudioVADPreRollFrames+5)
	}
}

func TestMeetingAudioVADSeparatesSpeechAcrossLongSilence(t *testing.T) {
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	vad := newMeetingAudioVAD()
	voice := meetingAudioVADPCMFrame(500)
	quiet := meetingAudioVADPCMFrame(0)
	var segments []meetingAudioSpeechSegment

	for index := 0; index < 2; index++ {
		segments = append(segments, meetingAudioVADPush(vad, voice, start, index)...)
	}
	for index := 2; index < 52; index++ { // one second of silence between short utterances
		segments = append(segments, meetingAudioVADPush(vad, quiet, start, index)...)
	}
	for index := 52; index < 54; index++ {
		segments = append(segments, meetingAudioVADPush(vad, voice, start, index)...)
	}
	for index := 54; index < 69; index++ {
		segments = append(segments, meetingAudioVADPush(vad, quiet, start, index)...)
	}

	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(segments))
	}
	if gap := segments[1].Start.Sub(segments[0].End); gap <= 0 {
		t.Fatalf("padded intervals overlap across long silence: gap = %s", gap)
	}
	if segments[1].Start.Before(start.Add(52*meetingAudioVADFrameDuration - meetingAudioVADPreRoll)) {
		t.Fatalf("second segment start = %s, lost original capture timing", segments[1].Start)
	}
}

func TestMeetingAudioVADDoesNotReplayFramesBetweenUtterances(t *testing.T) {
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	vad := newMeetingAudioVAD()
	voice := meetingAudioVADPCMFrame(500)
	quiet := meetingAudioVADPCMFrame(0)
	var segments []meetingAudioSpeechSegment

	for index := 0; index < 2; index++ {
		segments = append(segments, meetingAudioVADPush(vad, voice, start, index)...)
	}
	for index := 2; index < 17; index++ {
		segments = append(segments, meetingAudioVADPush(vad, quiet, start, index)...)
	}
	meetingAudioVADPush(vad, quiet, start, 17)
	meetingAudioVADPush(vad, voice, start, 18)
	meetingAudioVADPush(vad, voice, start, 19)
	for index := 20; index < 35; index++ {
		segments = append(segments, meetingAudioVADPush(vad, quiet, start, index)...)
	}

	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(segments))
	}
	if !segments[1].Start.Equal(segments[0].End) {
		t.Fatalf("second start = %s, first end = %s; emitted frames overlap or leave a gap", segments[1].Start, segments[0].End)
	}
	wantFrames := 1 + 2 + meetingAudioVADHangoverFrames
	if got := len(segments[1].PCM) / meetingAudioFrameBytes; got != wantFrames {
		t.Fatalf("second segment frames = %d, want one new pre-roll, two speech, and hangover frames (%d)", got, wantFrames)
	}
}

func TestMeetingAudioVADSplitsAtBoundedFrameBoundary(t *testing.T) {
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	vad := newMeetingAudioVAD()
	voice := meetingAudioVADPCMFrame(500)
	var segments []meetingAudioSpeechSegment

	for index := 0; index < meetingAudioVADMaxFrames+1; index++ {
		segments = append(segments, meetingAudioVADPush(vad, voice, start, index)...)
	}
	segments = append(segments, vad.Flush()...)
	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(segments))
	}

	wantFrames := []int{meetingAudioVADMaxFrames, 1}
	for index, segment := range segments {
		frames := len(segment.PCM) / meetingAudioFrameBytes
		if frames != wantFrames[index] {
			t.Fatalf("segment %d has %d frames, want %d", index, frames, wantFrames[index])
		}
		if segment.End.Sub(segment.Start) > meetingAudioVADMaxDuration {
			t.Fatalf("segment %d duration = %s, exceeds %s", index, segment.End.Sub(segment.Start), meetingAudioVADMaxDuration)
		}
		wantStart := start.Add(time.Duration(index*meetingAudioVADMaxFrames) * meetingAudioVADFrameDuration)
		if !segment.Start.Equal(wantStart) {
			t.Fatalf("segment %d starts at %s, want %s", index, segment.Start, wantStart)
		}
	}
}

func TestMeetingAudioVADFlushEmitsPendingSpeechOnly(t *testing.T) {
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	voice := meetingAudioVADPCMFrame(500)
	quiet := meetingAudioVADPCMFrame(0)

	allSilence := newMeetingAudioVAD()
	for index := 0; index < 20; index++ {
		meetingAudioVADPush(allSilence, quiet, start, index)
	}
	if segments := allSilence.Flush(); len(segments) != 0 {
		t.Fatalf("silence flush returned %d segments", len(segments))
	}

	vad := newMeetingAudioVAD()
	meetingAudioVADPush(vad, voice, start, 0)
	meetingAudioVADPush(vad, voice, start, 1)
	segments := vad.Flush()
	if len(segments) != 1 {
		t.Fatalf("speech flush returned %d segments, want 1", len(segments))
	}
	if !segments[0].Start.Equal(start) || !segments[0].End.Equal(start.Add(2*meetingAudioVADFrameDuration)) {
		t.Fatalf("flush interval = [%s, %s), want [%s, %s)", segments[0].Start, segments[0].End, start, start.Add(2*meetingAudioVADFrameDuration))
	}
	if got := len(segments[0].PCM); got != 2*meetingAudioFrameBytes {
		t.Fatalf("flush PCM bytes = %d, want %d", got, 2*meetingAudioFrameBytes)
	}
	if segments := vad.Flush(); len(segments) != 0 {
		t.Fatalf("second flush returned %d segments", len(segments))
	}
}
