package handler

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"letshare-server/internal/sfu"

	"github.com/pion/opus"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

const (
	meetingAudioSampleRate          = 16_000
	meetingAudioFrameSample         = 320 // 20ms, matches the client Opus packet cadence
	meetingAudioFrameBytes          = meetingAudioFrameSample * 2
	meetingAudioBatchSample         = meetingAudioSampleRate * 10 // one ASR request per 10s
	meetingAudioBatchBytes          = meetingAudioBatchSample * 2
	meetingAudioGapPad              = 300 * time.Millisecond
	meetingAudioBatchDuration       = 10 * time.Second
	meetingAudioQueueCapacity       = 16
	meetingAudioFinalQueueReserve   = 2
	meetingAudioRingSample          = meetingAudioSampleRate // 1s jitter buffer per track
	meetingAudioDecoderIdle         = 30 * time.Second
	meetingAudioStaleGap            = 500 * time.Millisecond
	meetingMinutesASRErrorThrottle  = 15 * time.Second
	meetingAudioDiagnosticsInterval = 10 * time.Second
)

type meetingAudioBatch struct {
	pcm         []byte
	startMs     int64
	endMs       int64
	recordingID string
	speakerID   string
	speakerName string
}

type meetingAudioDecoder struct {
	decoder  opus.Decoder
	samples  []int16
	ring     []int16
	head     int
	size     int
	lastSeen time.Time
}

type meetingAudioSpeaker struct {
	id          string
	name        string
	tracks      map[*webrtc.TrackRemote]*meetingAudioDecoder
	vad         *meetingAudioVAD
	buffer      []byte
	bufferStart time.Time
	bufferEnd   time.Time
}

type meetingAudioPipelineDiagnostics struct {
	startedAt         time.Time
	rtpPackets        uint64
	decoderInitErrors uint64
	decodeErrors      uint64
	decodedPackets    uint64
	decodedSamples    uint64
	speakerFrames     uint64
	voiceFrames       uint64
	vadSegments       uint64
	asrBatches        uint64
	asrQueueDrops     uint64
}

func newMeetingAudioDecoder() (*meetingAudioDecoder, error) {
	decoder, err := opus.NewDecoderWithOutput(meetingAudioSampleRate, 1)
	if err != nil {
		return nil, err
	}
	return &meetingAudioDecoder{
		decoder: decoder,
		samples: make([]int16, 5760),
		ring:    make([]int16, meetingAudioRingSample),
	}, nil
}

func (d *meetingAudioDecoder) push(samples []int16) {
	for _, sample := range samples {
		if d.size == len(d.ring) {
			// Keep the newest second of audio; a stalled publisher must not
			// grow the buffer or make capture drift permanently behind.
			d.head = (d.head + 1) % len(d.ring)
			d.size--
		}
		d.ring[(d.head+d.size)%len(d.ring)] = sample
		d.size++
	}
}

func (d *meetingAudioDecoder) popFrame(out []int16) bool {
	if d.size < len(out) {
		return false
	}
	first := len(d.ring) - d.head
	if first > len(out) {
		first = len(out)
	}
	copy(out[:first], d.ring[d.head:d.head+first])
	if first < len(out) {
		copy(out[first:], d.ring[:len(out)-first])
	}
	d.head = (d.head + len(out)) % len(d.ring)
	d.size -= len(out)
	return true
}

func (d *meetingAudioDecoder) clear() {
	d.head = 0
	d.size = 0
}

// meetingAudioMinutesSession decodes each publisher's Opus RTP at 16kHz mono.
// VAD and ASR batches stay per publisher so each result has one known speaker.
type meetingAudioMinutesSession struct {
	roomID      string
	room        *sfu.Room
	asr         *MeetingASRStreamService
	handler     *WebSocketHandler
	meta        *meetingMeta
	recordingID string

	ctx            context.Context
	cancel         context.CancelFunc
	asrCtx         context.Context
	asrCancel      context.CancelFunc
	done           chan struct{}
	transcribeDone chan struct{}
	closed         atomic.Bool

	mu          sync.Mutex
	speakers    map[string]*meetingAudioSpeaker
	diagnostics meetingAudioPipelineDiagnostics

	queue  chan meetingAudioBatch
	onText func(text string, startMs, endMs int64, recordingID, speakerID, speakerName string)
}

func (h *WebSocketHandler) startMeetingAudioMinutes(roomID string, room *sfu.Room, meta *meetingMeta) *meetingAudioMinutesSession {
	if h.meetingASRStream == nil || !h.meetingASRStream.Enabled() {
		return nil
	}
	if meta == nil || room == nil {
		return nil
	}
	meta.mu.Lock()
	recordingID := meta.minutes.RecordingID
	running := meta.minutes.Running
	meta.mu.Unlock()
	if !running || recordingID == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	asrCtx, asrCancel := context.WithCancel(context.Background())
	session := &meetingAudioMinutesSession{
		roomID:         roomID,
		room:           room,
		asr:            h.meetingASRStream,
		handler:        h,
		meta:           meta,
		recordingID:    recordingID,
		ctx:            ctx,
		cancel:         cancel,
		asrCtx:         asrCtx,
		asrCancel:      asrCancel,
		done:           make(chan struct{}),
		transcribeDone: make(chan struct{}),
		speakers:       make(map[string]*meetingAudioSpeaker),
		queue:          make(chan meetingAudioBatch, meetingAudioQueueCapacity),
		onText: func(text string, startMs, endMs int64, recordingID, speakerID, speakerName string) {
			h.broadcastMeetingAudioSegment(roomID, meta, text, startMs, endMs, recordingID, speakerID, speakerName)
		},
	}
	var replaced *meetingAudioMinutesSession
	meta.meetingASRPublishMu.Lock()
	currentMeta, active := h.activeMeetingRoom(roomID)
	meta.mu.Lock()
	stillRunning := meta.minutes.Running && meta.minutes.RecordingID == recordingID
	meta.mu.Unlock()
	if !active || currentMeta != meta || !stillRunning {
		meta.meetingASRPublishMu.Unlock()
		cancel()
		asrCancel()
		return nil
	}
	if value, ok := h.meetingAudioSessions.Load(roomID); ok {
		if existing, ok := value.(*meetingAudioMinutesSession); ok && existing != nil {
			if existing.meta == meta && existing.recordingID == recordingID && !existing.closed.Load() {
				meta.meetingASRPublishMu.Unlock()
				cancel()
				asrCancel()
				return existing
			}
			if h.meetingAudioSessions.CompareAndDelete(roomID, existing) {
				replaced = existing
			}
		}
	}
	oldStreams := h.lockMeetingASRStreamsForRoom(roomID)
	h.finishMeetingASRStreamTransition(oldStreams, nil)
	h.meetingAudioSessions.Store(roomID, session)
	room.SetAudioTap(session.handlePacket)
	go session.captureLoop()
	go session.transcribeLoop()
	meta.meetingASRPublishMu.Unlock()
	if replaced != nil {
		go replaced.Abort()
	}
	log.WithField("roomID", roomID).Info("会议按发言人转写已启动")
	return session
}

func (h *WebSocketHandler) stopMeetingAudioMinutes(roomID string) {
	h.stopMeetingAudioMinutesMode(roomID, false)
}

func (h *WebSocketHandler) stopMeetingAudioMinutesAndDrain(roomID string) {
	h.stopMeetingAudioMinutesMode(roomID, true)
}

func (h *WebSocketHandler) stopMeetingAudioMinutesMode(roomID string, drain bool) {
	session := h.detachMeetingAudioMinutes(roomID)
	if session == nil {
		return
	}
	if drain {
		session.Stop()
	} else {
		session.Abort()
	}
	log.WithField("roomID", roomID).Info("会议按发言人转写已停止")
}

// detachMeetingAudioMinutes revokes the room tap and removes its session.
// Callers that coordinate a recording transition can do this under the room
// publication lock, then stop capture after releasing that lock.
func (h *WebSocketHandler) detachMeetingAudioMinutes(roomID string) *meetingAudioMinutesSession {
	value, ok := h.meetingAudioSessions.LoadAndDelete(roomID)
	if !ok {
		return nil
	}
	session, ok := value.(*meetingAudioMinutesSession)
	if !ok || session == nil {
		return nil
	}
	session.room.SetAudioTap(nil)
	return session
}

func (h *WebSocketHandler) meetingAudioMinutesMixerConfigured() bool {
	return h.meetingASRStream != nil && h.meetingASRStream.Enabled()
}

func (s *meetingAudioMinutesSession) Stop() {
	s.stop(false)
}

// Abort discards pending upstream work when a room is reconfigured or torn
// down. A host's ordinary stop uses Stop so its final voiced tail can drain.
func (s *meetingAudioMinutesSession) Abort() {
	s.stop(true)
}

func (s *meetingAudioMinutesSession) stop(cancelASR bool) {
	if s.closed.Swap(true) {
		return
	}
	s.cancel()
	<-s.done
	close(s.queue)
	if cancelASR && s.asrCancel != nil {
		s.asrCancel()
	}
}

func (s *meetingAudioMinutesSession) minutesActive() bool {
	if s == nil || s.meta == nil {
		return false
	}
	if s.handler != nil {
		meta, ok := s.handler.activeMeetingRoom(s.roomID)
		if !ok || meta != s.meta {
			return false
		}
	}
	s.meta.mu.Lock()
	defer s.meta.mu.Unlock()
	return s.meta.minutes.Running && s.meta.minutes.RecordingID == s.recordingID
}

// handlePacket is called from the publisher's single RTP fanout loop. It must
// stay quick: decode into the track's 1s ring and return.
func (s *meetingAudioMinutesSession) handlePacket(participantID string, track *webrtc.TrackRemote, packet *rtp.Packet) {
	if s.closed.Load() || !s.minutesActive() || participantID == "" || packet == nil || len(packet.Payload) == 0 {
		return
	}
	if track == nil || track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	if track.Codec().MimeType != webrtc.MimeTypeOpus {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() || !s.minutesActive() {
		return
	}
	var name string
	if s.handler != nil {
		member, ok := s.handler.meetings.member(s.roomID, participantID)
		if !ok {
			return
		}
		name = member.UserName
	}
	speaker := s.speakers[participantID]
	if speaker == nil {
		speaker = &meetingAudioSpeaker{
			id:     participantID,
			tracks: make(map[*webrtc.TrackRemote]*meetingAudioDecoder),
			vad:    newMeetingAudioVAD(),
		}
		s.speakers[participantID] = speaker
	}
	if name != "" {
		speaker.name = name
	}
	state := speaker.tracks[track]
	if state == nil {
		decoded, err := newMeetingAudioDecoder()
		if err != nil {
			s.diagnostics.decoderInitErrors++
			return
		}
		state = decoded
		speaker.tracks[track] = state
		log.WithField("roomID", s.roomID).Info("会议纪要开始解码音频")
	}
	s.diagnostics.rtpPackets++
	count, err := state.decoder.DecodeToInt16(packet.Payload, state.samples)
	if err != nil {
		s.diagnostics.decodeErrors++
	} else if count > 0 {
		s.diagnostics.decodedPackets++
		s.diagnostics.decodedSamples += uint64(count)
		state.push(state.samples[:count])
	}
	state.lastSeen = time.Now()
}

func (s *meetingAudioMinutesSession) removePublisher(participantID string) {
	if s == nil || participantID == "" {
		return
	}
	s.mu.Lock()
	if speaker := s.speakers[participantID]; speaker != nil {
		// The capture loop seals any pending speech with this publisher's last known name.
		clear(speaker.tracks)
	}
	s.mu.Unlock()
}

func (s *meetingAudioMinutesSession) captureLoop() {
	defer func() {
		s.finalFlush()
		close(s.done)
	}()
	ticker := time.NewTicker(meetingAudioVADFrameDuration)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.captureTick(now)
		}
	}
}

func (p *meetingAudioSpeaker) nextFrame(now time.Time) [meetingAudioFrameBytes]byte {
	var frame [meetingAudioFrameSample]int16
	var acc [meetingAudioFrameSample]int32
	for track, state := range p.tracks {
		if now.Sub(state.lastSeen) > meetingAudioDecoderIdle {
			delete(p.tracks, track)
			continue
		}
		if now.Sub(state.lastSeen) > meetingAudioStaleGap {
			// DTX / a network gap: do not replay stale samples when the
			// publisher resumes.
			state.clear()
			continue
		}
		if !state.popFrame(frame[:]) {
			continue
		}
		for index, sample := range frame {
			acc[index] += int32(sample)
		}
	}
	var encoded [meetingAudioFrameBytes]byte
	for index, sample := range acc {
		binary.LittleEndian.PutUint16(encoded[index*2:], uint16(int16(max(-32768, min(32767, sample)))))
	}
	return encoded
}

func (s *meetingAudioMinutesSession) captureTick(now time.Time) {
	if !s.minutesActive() {
		return
	}
	var report *meetingAudioPipelineDiagnostics
	var batches []meetingAudioBatch
	s.mu.Lock()
	for id, speaker := range s.speakers {
		frame := speaker.nextFrame(now)
		if len(speaker.tracks) == 0 {
			batches = append(batches, speaker.flush()...)
			delete(s.speakers, id)
			continue
		}
		s.diagnostics.speakerFrames++
		if meetingAudioVADHasVoice(frame[:]) {
			s.diagnostics.voiceFrames++
		}
		segments := speaker.vad.PushFrame(frame[:], now.Add(-meetingAudioVADFrameDuration))
		s.diagnostics.vadSegments += uint64(len(segments))
		for _, segment := range segments {
			batches = append(batches, speaker.appendSpeechSegment(segment)...)
		}
		if len(speaker.buffer) > 0 && now.Sub(speaker.bufferStart) >= meetingAudioBatchDuration {
			batches = append(batches, speaker.takeBatch()...)
		}
	}
	if s.diagnostics.startedAt.IsZero() {
		s.diagnostics.startedAt = now
	} else if now.Sub(s.diagnostics.startedAt) >= meetingAudioDiagnosticsInterval {
		snapshot := s.diagnostics
		snapshot.startedAt = now
		report = &snapshot
		s.diagnostics = meetingAudioPipelineDiagnostics{startedAt: now}
	}
	s.mu.Unlock()

	if report != nil {
		log.WithFields(log.Fields{
			"roomID":      s.roomID,
			"rtp_packets": report.rtpPackets, "decoder_init_errors": report.decoderInitErrors,
			"decode_errors": report.decodeErrors, "decoded_packets": report.decodedPackets,
			"decoded_samples": report.decodedSamples, "speaker_frames": report.speakerFrames,
			"voice_frames": report.voiceFrames, "vad_segments": report.vadSegments,
			"asr_batches": report.asrBatches, "asr_queue_drops": report.asrQueueDrops,
			"asr_queue_depth": len(s.queue),
		}).Info("meeting audio pipeline diagnostic")
	}
	s.enqueueBatches(batches, false)
}

func (s *meetingAudioSpeaker) appendSpeechSegment(segment meetingAudioSpeechSegment) []meetingAudioBatch {
	if len(segment.PCM) == 0 || segment.Start.IsZero() || !segment.End.After(segment.Start) {
		return nil
	}
	pcm := segment.PCM
	start := segment.Start
	if !s.bufferStart.IsZero() {
		gap := start.Sub(s.bufferEnd)
		if segment.End.Sub(s.bufferStart) > meetingAudioBatchDuration {
			batches := s.takeBatch()
			return append(batches, s.appendSpeechSegment(segment)...)
		}
		if gap < 0 {
			overlapFrames := int((-gap + meetingAudioVADFrameDuration - 1) / meetingAudioVADFrameDuration)
			dropBytes := overlapFrames * meetingAudioFrameBytes
			if dropBytes >= len(pcm) {
				return nil
			}
			pcm = pcm[dropBytes:]
			start = start.Add(time.Duration(overlapFrames) * meetingAudioVADFrameDuration)
			gap = 0
		}
		if gap > 0 {
			pad := gap
			if pad > meetingAudioGapPad {
				pad = meetingAudioGapPad
			}
			padBytes := int(pad/meetingAudioVADFrameDuration) * meetingAudioFrameBytes
			if len(s.buffer)+padBytes+len(pcm) > meetingAudioBatchBytes && len(s.buffer) > 0 {
				batches := s.takeBatch()
				return append(batches, s.appendSpeechSegment(segment)...)
			}
			s.buffer = append(s.buffer, make([]byte, padBytes)...)
		}
		if len(s.buffer)+len(pcm) > meetingAudioBatchBytes && len(s.buffer) > 0 {
			batches := s.takeBatch()
			return append(batches, s.appendSpeechSegment(segment)...)
		}
	} else {
		s.bufferStart = start
	}
	if s.bufferStart.IsZero() {
		s.bufferStart = start
	}
	s.buffer = append(s.buffer, pcm...)
	s.bufferEnd = segment.End
	if segment.End.Sub(s.bufferStart) >= meetingAudioBatchDuration || len(s.buffer) >= meetingAudioBatchBytes {
		return s.takeBatch()
	}
	return nil
}

func (s *meetingAudioSpeaker) takeBatch() []meetingAudioBatch {
	if len(s.buffer) == 0 {
		return nil
	}
	batch := meetingAudioBatch{
		pcm:         s.buffer,
		startMs:     s.bufferStart.UnixMilli(),
		endMs:       s.bufferEnd.UnixMilli(),
		speakerID:   s.id,
		speakerName: s.name,
	}
	s.buffer = nil
	s.bufferStart = time.Time{}
	s.bufferEnd = time.Time{}
	return []meetingAudioBatch{batch}
}

func (s *meetingAudioSpeaker) flush() []meetingAudioBatch {
	var batches []meetingAudioBatch
	for _, segment := range s.vad.Flush() {
		batches = append(batches, s.appendSpeechSegment(segment)...)
	}
	return append(batches, s.takeBatch()...)
}

func (s *meetingAudioMinutesSession) finalFlush() {
	s.mu.Lock()
	var batches []meetingAudioBatch
	for _, speaker := range s.speakers {
		batches = append(batches, speaker.flush()...)
	}
	clear(s.speakers)
	s.mu.Unlock()
	s.enqueueBatches(batches, true)
}

func (s *meetingAudioMinutesSession) enqueueBatches(batches []meetingAudioBatch, final bool) {
	if len(batches) > 1 {
		sort.Slice(batches, func(i, j int) bool {
			if batches[i].startMs == batches[j].startMs {
				return batches[i].speakerID < batches[j].speakerID
			}
			return batches[i].startMs < batches[j].startMs
		})
	}
	for _, batch := range batches {
		s.enqueue(batch, final)
	}
}

func (s *meetingAudioMinutesSession) enqueue(batch meetingAudioBatch, final bool) {
	if s.recordingID == "" || len(batch.pcm) == 0 {
		return
	}
	if !final && len(s.queue) >= cap(s.queue)-meetingAudioFinalQueueReserve {
		// Keep room in the queue for speech that is sealed when capture stops.
		s.mu.Lock()
		s.diagnostics.asrQueueDrops++
		s.mu.Unlock()
		log.WithField("roomID", s.roomID).Warn("meeting audio ASR queue is near capacity; dropping a non-final batch")
		return
	}
	batch.recordingID = s.recordingID
	select {
	case s.queue <- batch:
		s.mu.Lock()
		s.diagnostics.asrBatches++
		s.mu.Unlock()
	default:
		// Bounded backlog: never let a slow upstream grow memory without limit.
		s.mu.Lock()
		s.diagnostics.asrQueueDrops++
		s.mu.Unlock()
		log.WithField("roomID", s.roomID).Warn("会议纪要 ASR 队列已满，丢弃一个批次")
	}
}

func (s *meetingAudioMinutesSession) transcribeLoop() {
	defer close(s.transcribeDone)
	defer func() {
		if s.asrCancel != nil {
			s.asrCancel()
		}
	}()
	for batch := range s.queue {
		ctx, cancel := context.WithTimeout(s.asrCtx, 90*time.Second)
		text, err := s.asr.Transcribe(ctx, batch.pcm)
		cancel()
		if err != nil {
			log.WithFields(log.Fields{"roomID": s.roomID, "bytes": len(batch.pcm), "error_type": fmt.Sprintf("%T", err)}).Warn("会议纪要 ASR 批次识别失败")
			s.handler.broadcastMeetingAudioASRError(s.roomID, s.meta, batch.recordingID)
			continue
		}
		text = strings.TrimSpace(text)
		if !meaningfulMeetingASRText(text) {
			continue
		}
		log.WithFields(log.Fields{"roomID": s.roomID, "chars": len([]rune(text))}).Info("会议纪要 ASR 批次已转写")
		if s.onText != nil {
			s.onText(text, batch.startMs, batch.endMs, batch.recordingID, batch.speakerID, batch.speakerName)
		}
	}
}

func (h *WebSocketHandler) broadcastMeetingAudioASRError(roomID string, sessionMeta *meetingMeta, recordingID string) {
	if sessionMeta == nil || recordingID == "" {
		return
	}
	meta, active := h.activeMeetingRoom(roomID)
	if !active || meta != sessionMeta {
		return
	}
	meta.meetingASRPublishMu.Lock()
	defer meta.meetingASRPublishMu.Unlock()
	currentMeta, stillActive := h.activeMeetingRoom(roomID)
	if !stillActive || currentMeta != sessionMeta {
		return
	}
	now := time.Now()
	meta.mu.Lock()
	if meta.minutes.RecordingID != recordingID {
		meta.mu.Unlock()
		return
	}
	if meta.minutesASRErrorEpoch == recordingID && now.Sub(meta.minutesASRErrorAt) < meetingMinutesASRErrorThrottle {
		meta.mu.Unlock()
		return
	}
	meta.minutesASRErrorEpoch = recordingID
	meta.minutesASRErrorAt = now
	pending := make(map[string]bool, len(meta.minutesReplayPending))
	for clientID, isPending := range meta.minutesReplayPending {
		pending[clientID] = isPending
	}
	meta.mu.Unlock()
	payload := map[string]interface{}{
		"kind": "asr-error", "code": "transcription_failed", "recordingId": recordingID,
		"retryAfterMs": meetingMinutesASRErrorThrottle.Milliseconds(),
	}
	h.sendMeetingMinutesToEnabledMembersExcept(roomID, meta, payload, pending)
}

func (h *WebSocketHandler) broadcastMeetingAudioSegment(roomID string, sessionMeta *meetingMeta, text string, startMs, endMs int64, recordingID, speakerID, speakerName string) {
	if !meaningfulMeetingASRText(text) || recordingID == "" {
		return
	}
	meta, ok := h.activeMeetingRoom(roomID)
	if !ok || meta != sessionMeta {
		return
	}
	meta.meetingASRPublishMu.Lock()
	defer meta.meetingASRPublishMu.Unlock()
	currentMeta, stillActive := h.activeMeetingRoom(roomID)
	if !stillActive || currentMeta != sessionMeta {
		return
	}
	meta.mu.Lock()
	currentRecordingID := meta.minutes.RecordingID
	meta.mu.Unlock()
	if currentRecordingID != recordingID {
		return
	}
	if speakerID == "" {
		speakerID = "meeting-audio"
	}
	if strings.TrimSpace(speakerName) == "" {
		speakerName = "会议音频"
	}
	meta.mu.Lock()
	if meta.minutes.RecordingID != recordingID || meta.minutes.LatestSegmentSeq == ^uint64(0) {
		meta.mu.Unlock()
		return
	}
	seq := meta.minutes.LatestSegmentSeq + 1
	meta.minutes.LatestSegmentSeq = seq
	segment := meetingMinutesSegment{
		SegmentID:   fmt.Sprintf("mix:%s:%d", recordingID, seq),
		Seq:         seq,
		RecordingID: recordingID,
		From:        speakerID,
		SpeakerName: speakerName,
		Text:        text,
		StartMs:     startMs,
		EndMs:       endMs,
		Final:       true,
		Language:    "zh-CN",
	}
	appendMeetingMinutesHistoryLocked(meta, segment)
	pendingClients := make(map[string]bool, len(meta.minutesReplayPending))
	for clientID, pending := range meta.minutesReplayPending {
		pendingClients[clientID] = pending
	}
	meta.mu.Unlock()
	h.appendMeetingAITranscript(roomID, []meetingMinutesSegment{segment})
	payload := map[string]interface{}{"kind": "segments", "recordingId": recordingID, "segments": []meetingMinutesSegment{segment}}
	h.sendMeetingMinutesToEnabledMembersExcept(roomID, meta, payload, pendingClients)
}
