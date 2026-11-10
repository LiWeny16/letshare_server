package handler

import (
	"encoding/binary"
	"math"
	"time"
)

const (
	meetingAudioVADFrameDuration  = 20 * time.Millisecond
	meetingAudioVADPreRoll        = 200 * time.Millisecond
	meetingAudioVADHangover       = 300 * time.Millisecond
	meetingAudioVADMaxDuration    = 10 * time.Second
	meetingAudioVADMinRMS         = 110 // about -49 dBFS; intentionally below the old 300-sample cutoff.
	meetingAudioVADClockSlop      = 5 * time.Millisecond
	meetingAudioVADPreRollFrames  = int(meetingAudioVADPreRoll / meetingAudioVADFrameDuration)
	meetingAudioVADHangoverFrames = int(meetingAudioVADHangover / meetingAudioVADFrameDuration)
	meetingAudioVADMaxFrames      = int(meetingAudioVADMaxDuration / meetingAudioVADFrameDuration)
)

// meetingAudioSpeechSegment is a bounded, contiguous PCM16LE audio interval.
// Start and End are capture times; End is exclusive and includes any available
// pre-roll and hangover frames.
type meetingAudioSpeechSegment struct {
	Start time.Time
	End   time.Time
	PCM   []byte
}

type meetingAudioVADFrame struct {
	start time.Time
	pcm   [meetingAudioFrameBytes]byte
}

// meetingAudioVAD performs a small, energy-based voice activity pass over the
// server-side mixed 20ms PCM frames. It retains at most 200ms of pre-roll and
// one segment of at most 10s. The threshold is deliberately permissive: this
// helper is a speech/no-speech heuristic, not a speech recognizer.
type meetingAudioVAD struct {
	history     [meetingAudioVADPreRollFrames]meetingAudioVADFrame
	historyHead int
	historySize int

	segmentPCM      []byte
	segmentStart    time.Time
	segmentEnd      time.Time
	hasSegmentStart bool
	segmentHasVoice bool

	active                bool
	trailingSilenceFrames int
	maxSegmentFrames      int

	lastFrameAt  time.Time
	hasLastFrame bool
}

// newMeetingAudioVAD constructs the production VAD with the fixed padding and
// 10s segment ceiling used by the meeting audio mixer.
func newMeetingAudioVAD() *meetingAudioVAD {
	return &meetingAudioVAD{
		maxSegmentFrames: meetingAudioVADMaxFrames,
		segmentPCM:       make([]byte, 0, meetingAudioVADMaxFrames*meetingAudioFrameBytes),
	}
}

// PushFrame accepts one mixed 20ms mono PCM16LE frame. capturedAt is the
// capture time of the first sample in pcm16le. Returned segments own their PCM
// buffers. Frames of the wrong size are ignored without changing VAD state.
// Speech can start with a single voiced frame; there is no minimum speech
// duration, so short words and interjections are retained.
func (vad *meetingAudioVAD) PushFrame(pcm16le []byte, capturedAt time.Time) []meetingAudioSpeechSegment {
	if vad == nil || len(pcm16le) != meetingAudioFrameBytes {
		return nil
	}

	var emitted []meetingAudioSpeechSegment
	if vad.hasLastFrame {
		gap := capturedAt.Sub(vad.lastFrameAt)
		if gap <= 0 || gap > meetingAudioVADFrameDuration+meetingAudioVADClockSlop {
			if segment, ok := vad.finishSegment(); ok {
				emitted = append(emitted, segment)
			}
			vad.resetActive()
			vad.clearHistory()
		}
	}

	var frame meetingAudioVADFrame
	frame.start = capturedAt
	copy(frame.pcm[:], pcm16le)

	voiced := meetingAudioVADHasVoice(frame.pcm[:])
	if !vad.active {
		if voiced {
			vad.active = true
			vad.trailingSilenceFrames = 0
			vad.segmentHasVoice = false
			vad.appendPreRoll()
			vad.appendSegmentFrame(frame)
			vad.segmentHasVoice = true
		}
	} else {
		if vad.hasSegmentStart && frame.start.Add(meetingAudioVADFrameDuration).Sub(vad.segmentStart) > meetingAudioVADMaxDuration {
			// Capture timestamps can drift slightly from nominal 20ms cadence.
			// Enforce the time bound as well as the frame-count bound below.
			if segment, ok := vad.finishSegment(); ok {
				emitted = append(emitted, segment)
			}
		}
		vad.appendSegmentFrame(frame)
		if voiced {
			vad.segmentHasVoice = true
			vad.trailingSilenceFrames = 0
		} else {
			vad.trailingSilenceFrames++
		}
	}

	segmentClosed := false
	if vad.active && vad.trailingSilenceFrames >= meetingAudioVADHangoverFrames {
		if segment, ok := vad.finishSegment(); ok {
			emitted = append(emitted, segment)
		}
		vad.active = false
		vad.trailingSilenceFrames = 0
		// The emitted segment already owns its trailing frames. Start a fresh
		// pre-roll window so a following utterance cannot replay those samples.
		vad.clearHistory()
		segmentClosed = true
	} else if vad.active && vad.segmentFrameCount() >= vad.maxSegmentFrames {
		// Split exactly at this frame boundary. The next frame starts a fresh
		// segment without replaying pre-roll, so adjacent segments have no gap
		// and no samples are duplicated or dropped.
		if segment, ok := vad.finishSegment(); ok {
			emitted = append(emitted, segment)
		}
	}

	if !segmentClosed {
		vad.appendHistory(frame)
	}
	vad.lastFrameAt = capturedAt
	vad.hasLastFrame = true
	return emitted
}

// Flush closes the current speech interval at the last received frame. It does
// not synthesize padding and never emits a silence-only segment.
func (vad *meetingAudioVAD) Flush() []meetingAudioSpeechSegment {
	if vad == nil {
		return nil
	}
	segment, ok := vad.finishSegment()
	vad.resetActive()
	vad.clearHistory()
	vad.lastFrameAt = time.Time{}
	vad.hasLastFrame = false
	if !ok {
		return nil
	}
	return []meetingAudioSpeechSegment{segment}
}

func meetingAudioVADHasVoice(pcm []byte) bool {
	if len(pcm) != meetingAudioFrameBytes {
		return false
	}
	var sumSquares int64
	for offset := 0; offset < len(pcm); offset += 2 {
		sample := int64(int16(binary.LittleEndian.Uint16(pcm[offset : offset+2])))
		sumSquares += sample * sample
	}
	rms := math.Sqrt(float64(sumSquares) / float64(meetingAudioFrameSample))
	return rms >= meetingAudioVADMinRMS
}

func (vad *meetingAudioVAD) appendPreRoll() {
	if vad.historySize == 0 {
		return
	}
	// Reserve one frame for the voice frame that triggered the segment.
	limit := vad.maxSegmentFrames - 1
	if limit < 0 {
		limit = 0
	}
	skip := vad.historySize - limit
	if skip < 0 {
		skip = 0
	}
	for offset := skip; offset < vad.historySize; offset++ {
		index := (vad.historyHead + offset) % len(vad.history)
		vad.appendSegmentFrame(vad.history[index])
	}
}

func (vad *meetingAudioVAD) appendSegmentFrame(frame meetingAudioVADFrame) {
	if !vad.hasSegmentStart {
		vad.segmentStart = frame.start
		vad.hasSegmentStart = true
	}
	vad.segmentPCM = append(vad.segmentPCM, frame.pcm[:]...)
	vad.segmentEnd = frame.start.Add(meetingAudioVADFrameDuration)
}

func (vad *meetingAudioVAD) appendHistory(frame meetingAudioVADFrame) {
	if len(vad.history) == 0 {
		return
	}
	if vad.historySize < len(vad.history) {
		index := (vad.historyHead + vad.historySize) % len(vad.history)
		vad.history[index] = frame
		vad.historySize++
		return
	}
	vad.history[vad.historyHead] = frame
	vad.historyHead = (vad.historyHead + 1) % len(vad.history)
}

func (vad *meetingAudioVAD) segmentFrameCount() int {
	return len(vad.segmentPCM) / meetingAudioFrameBytes
}

func (vad *meetingAudioVAD) finishSegment() (meetingAudioSpeechSegment, bool) {
	if !vad.segmentHasVoice || !vad.hasSegmentStart || len(vad.segmentPCM) == 0 {
		vad.clearSegment()
		return meetingAudioSpeechSegment{}, false
	}
	segment := meetingAudioSpeechSegment{
		Start: vad.segmentStart,
		End:   vad.segmentEnd,
		PCM:   append([]byte(nil), vad.segmentPCM...),
	}
	vad.clearSegment()
	return segment, true
}

func (vad *meetingAudioVAD) clearSegment() {
	clear(vad.segmentPCM)
	vad.segmentPCM = vad.segmentPCM[:0]
	vad.segmentStart = time.Time{}
	vad.segmentEnd = time.Time{}
	vad.hasSegmentStart = false
	vad.segmentHasVoice = false
}

func (vad *meetingAudioVAD) resetActive() {
	vad.active = false
	vad.trailingSilenceFrames = 0
	vad.clearSegment()
}

func (vad *meetingAudioVAD) clearHistory() {
	for index := range vad.history {
		vad.history[index] = meetingAudioVADFrame{}
	}
	vad.historyHead = 0
	vad.historySize = 0
}
