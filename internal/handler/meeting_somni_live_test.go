package handler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/joho/godotenv"
)

const (
	meetingAILiveOptInEnv     = "LETSHARE_RUN_MEETING_AI_LIVE"
	meetingASRLiveWAVEnv      = "LETSHARE_ASR_TEST_WAV"
	meetingLiveSyntheticInput = `Meeting title: Opt-in integration check
Participants: Speaker A and Speaker B
Transcript:
Speaker A: We should prepare the synthetic integration check today.
Speaker B: Agreed. Please record the outcome without copying the conversation.`
)

// TestMeetingAISomniLiveOptIn exercises the same Somni batch recognizer and
// fixed Gemini summary service configured by cmd/server. It is intentionally
// inert during normal test runs: reading server/.env or contacting a provider
// only happens after the exact opt-in value is set.
func TestMeetingAISomniLiveOptIn(t *testing.T) {
	if os.Getenv(meetingAILiveOptInEnv) != "1" {
		t.Skip("set LETSHARE_RUN_MEETING_AI_LIVE=1 to enable provider integration checks")
	}

	envPath := strings.TrimSpace(os.Getenv("LETSHARE_MEETING_ENV_FILE"))
	if envPath == "" {
		envPath = filepath.Join("..", "..", ".env")
	}
	if _, err := os.Stat(envPath); err == nil {
		if err := godotenv.Load(envPath); err != nil {
			t.Fatal("server/.env exists but could not be loaded (details withheld)")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("server/.env could not be inspected (details withheld)")
	}

	wavPath := strings.TrimSpace(os.Getenv(meetingASRLiveWAVEnv))
	if wavPath == "" {
		t.Fatal("live provider integration requires LETSHARE_ASR_TEST_WAV to point to the authorized synthetic WAV")
	}
	pcm, err := readMeetingASRTestWAV(wavPath)
	if err != nil {
		t.Fatalf("configured synthetic WAV is unavailable or invalid: %s", err)
	}

	asr := NewMeetingASRStreamService(MeetingASRStreamConfig{
		HTTPBase:       meetingLiveEnvFirst("MEETING_ASR_STREAM_HTTP_BASE", "SOMNI_ASR_HTTP_BASE"),
		WSBase:         meetingLiveEnvFirst("MEETING_ASR_STREAM_WS_BASE", "SOMNI_ASR_WS_BASE"),
		Email:          meetingLiveEnvFirst("MEETING_ASR_STREAM_EMAIL", "SOMNI_ASR_EMAIL"),
		Password:       meetingLiveEnvFirst("MEETING_ASR_STREAM_PASSWORD", "SOMNI_ASR_PASSWORD"),
		LoginType:      meetingLiveEnvFirst("MEETING_ASR_STREAM_LOGIN_TYPE", "SOMNI_ASR_LOGIN_TYPE"),
		DevicePlatform: meetingLiveEnvFirst("MEETING_ASR_STREAM_DEVICE_PLATFORM", "SOMNI_ASR_DEVICE_PLATFORM"),
		DeviceName:     meetingLiveEnvFirst("MEETING_ASR_STREAM_DEVICE_NAME", "SOMNI_ASR_DEVICE_NAME"),
		AppVersion:     meetingLiveEnvFirst("MEETING_ASR_STREAM_APP_VERSION", "SOMNI_ASR_APP_VERSION"),
		Timezone:       meetingLiveEnvFirst("MEETING_ASR_STREAM_TIMEZONE", "SOMNI_ASR_TIMEZONE"),
	})

	aiKey := meetingLiveEnvFirst("MEETING_AI_API_KEY", "SOMNI_LLM_API_KEY")
	aiEnabledEnv := strings.TrimSpace(os.Getenv("MEETING_AI_ENABLED"))
	aiEnabled := strings.EqualFold(aiEnabledEnv, "true") ||
		(aiEnabledEnv == "" && strings.TrimSpace(aiKey) != "")
	billingEnv := strings.TrimSpace(os.Getenv("MEETING_AI_BILLING_DISABLED"))
	billingDisabled := billingEnv == "" || strings.EqualFold(billingEnv, "true")
	ai := NewMeetingAIService(
		aiKey,
		meetingLiveEnvFirst("MEETING_AI_BASE_URL", "SOMNI_LLM_BASE_URL"),
		aiEnabled,
		billingDisabled,
		nil,
	)
	if model := meetingLiveEnvFirst("MEETING_AI_MODEL", "SOMNI_LLM_MODEL"); model != "" {
		// Match server startup behavior: unsupported operator values leave the
		// allowlisted fixed model unchanged.
		_ = ai.SetModel(model)
	}

	if !asr.Enabled() {
		t.Error("Somni batch ASR integration failed: required provider configuration is incomplete")
	} else {
		t.Run("Somni batch ASR", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			transcript, err := asr.Transcribe(ctx, pcm)
			if err != nil {
				t.Fatalf("Somni batch ASR failed: %s", safeMeetingSomniLiveError(err))
			}
			if strings.TrimSpace(transcript) == "" {
				t.Fatal("Somni batch ASR failed: provider returned no transcript")
			}
			t.Logf("Somni batch ASR passed; PCM bytes=%d; transcript characters=%d", len(pcm), utf8.RuneCountInString(transcript))
		})
		t.Run("Somni VAD mixer ASR", func(t *testing.T) {
			const silenceDuration = 3 * time.Second
			silenceBytes := meetingAudioSampleRate * 2 * int(silenceDuration/time.Second)
			mixedInput := make([]byte, silenceBytes+len(pcm)+silenceBytes)
			copy(mixedInput[silenceBytes:], pcm)
			if remainder := len(mixedInput) % meetingAudioFrameBytes; remainder != 0 {
				mixedInput = append(mixedInput, make([]byte, meetingAudioFrameBytes-remainder)...)
			}

			meta := newMeetingMeta(meetingMetaInit{})
			meta.minutes.Running = true
			meta.minutes.RecordingID = "live-synthetic-vad"
			queue := make(chan meetingAudioBatch, 4)
			session := &meetingAudioMinutesSession{
				meta:        meta,
				recordingID: "live-synthetic-vad",
				vad:         newMeetingAudioVAD(),
				buffer:      make([]byte, 0, meetingAudioBatchBytes),
				decoders:    make(map[string]*meetingAudioDecoder),
				energy:      make(map[string]int64),
				queue:       queue,
			}
			decoder := &meetingAudioDecoder{
				participantID: "synthetic-speaker",
				ring:          make([]int16, meetingAudioRingSample),
			}
			session.decoders["synthetic-track"] = decoder
			base := time.Now().UTC().Truncate(meetingAudioVADFrameDuration)
			samples := make([]int16, meetingAudioFrameSample)
			for frameOffset := 0; frameOffset < len(mixedInput); frameOffset += meetingAudioFrameBytes {
				frame := mixedInput[frameOffset : frameOffset+meetingAudioFrameBytes]
				for sampleOffset := range samples {
					sampleStart := sampleOffset * 2
					samples[sampleOffset] = int16(binary.LittleEndian.Uint16(frame[sampleStart : sampleStart+2]))
				}
				now := base.Add(time.Duration(frameOffset/meetingAudioFrameBytes+1) * meetingAudioVADFrameDuration)
				decoder.lastSeen = now
				decoder.push(samples)
				session.mixTick(now)
			}
			session.finalFlush()

			var queuedBatchCount, voicedBatchCount, queuedVoicedPCMBytes int
			var batchToTranscribe meetingAudioBatch
			for len(queue) > 0 {
				batch := <-queue
				queuedBatchCount++
				queuedVoicedPCMBytes += len(batch.pcm)
				if !meetingLiveBatchContainsVoice(batch.pcm) {
					t.Fatal("Somni VAD mixer failed: a silence-only batch entered the ASR queue")
				}
				voicedBatchCount++
				if len(batch.pcm) > len(batchToTranscribe.pcm) {
					batchToTranscribe = batch
				}
			}
			if queuedBatchCount == 0 || voicedBatchCount == 0 || len(batchToTranscribe.pcm) == 0 {
				t.Fatal("Somni VAD mixer failed: no voiced batch was produced")
			}
			if queuedVoicedPCMBytes >= len(mixedInput) || len(batchToTranscribe.pcm) >= len(mixedInput) {
				t.Fatalf("Somni VAD mixer failed to trim silence: input PCM bytes=%d; queued voiced PCM bytes=%d", len(mixedInput), queuedVoicedPCMBytes)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			transcript, err := asr.Transcribe(ctx, batchToTranscribe.pcm)
			if err != nil {
				t.Fatalf("Somni VAD batch transcription failed: %s", safeMeetingSomniLiveError(err))
			}
			if strings.TrimSpace(transcript) == "" {
				t.Fatal("Somni VAD batch transcription failed: provider returned no transcript")
			}
			t.Logf("Somni VAD mixer passed; input PCM bytes=%d; queued PCM bytes=%d; voiced batches=%d; transcribed batch bytes=%d", len(mixedInput), queuedVoicedPCMBytes, voicedBatchCount, len(batchToTranscribe.pcm))
		})
	}

	if !ai.Enabled() {
		t.Error("fixed Gemini summary integration failed: service is disabled, billing gate is closed, or API key is missing")
	} else if ai.Model() != meetingAIModel {
		t.Errorf("fixed Gemini summary integration failed: unexpected server model configuration")
	} else {
		t.Run("fixed Gemini meeting summary", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			output, err := ai.Complete(ctx, MeetingAITaskSummary, meetingLiveSyntheticInput, "en")
			if err != nil {
				t.Fatalf("fixed Gemini meeting summary failed: %s", safeMeetingAILiveError(err))
			}
			format, validObject, hasNotes := parseMeetingLiveSummaryOutput(output)
			if !validObject {
				t.Fatalf("fixed Gemini meeting summary failed: malformed JSON object; format=%s; output characters=%d", format, utf8.RuneCountInString(output))
			}
			if !hasNotes {
				t.Fatalf("fixed Gemini meeting summary failed: no usable summary notes; format=%s; output characters=%d", format, utf8.RuneCountInString(output))
			}
			t.Logf("fixed Gemini meeting summary passed; format=%s; output characters=%d", format, utf8.RuneCountInString(output))
		})
	}
}

func meetingLiveBatchContainsVoice(pcm []byte) bool {
	for offset := 0; offset+meetingAudioFrameBytes <= len(pcm); offset += meetingAudioFrameBytes {
		if meetingAudioVADHasVoice(pcm[offset : offset+meetingAudioFrameBytes]) {
			return true
		}
	}
	return false
}

// parseMeetingLiveSummaryOutput mirrors parseMeetingMinutesJson's accepted
// envelope: plain JSON or one optional ```/```json fence, containing a JSON
// object with usable overview/summary text or timeline fallback text. It
// returns only a format classification and note-presence bit; it never logs
// content.
func parseMeetingLiveSummaryOutput(output string) (string, bool, bool) {
	body := strings.TrimSpace(output)
	format := "plain"
	if strings.HasPrefix(body, "```") {
		format = "fenced"
		body = body[3:]
		if len(body) >= len("json") && strings.EqualFold(body[:len("json")], "json") {
			body = body[len("json"):]
		}
		body = strings.TrimLeftFunc(body, unicode.IsSpace)
	}
	if strings.HasSuffix(body, "```") {
		body = strings.TrimRightFunc(strings.TrimSuffix(body, "```"), unicode.IsSpace)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &parsed); err != nil || parsed == nil {
		return format, false, false
	}

	if meetingLiveJSONText(parsed["overview"]) || meetingLiveJSONText(parsed["summary"]) {
		return format, true, true
	}
	var timeline []json.RawMessage
	if json.Unmarshal(parsed["timeline"], &timeline) != nil {
		return format, true, false
	}
	for _, rawItem := range timeline {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil || item == nil {
			continue
		}
		for _, key := range []string{"summary", "transcript"} {
			if meetingLiveJSONText(item[key]) {
				return format, true, true
			}
		}
	}
	return format, true, false
}

func meetingLiveJSONText(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
}

func meetingLiveEnvFirst(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

// readMeetingASRTestWAV accepts classic PCM16, mono, 16 kHz WAV only. The
// parser bounds the RIFF container and every chunk before slicing, and returns
// only the audio payload so the existing Somni batch adapter can upload it.
func readMeetingASRTestWAV(path string) ([]byte, error) {
	const maxWAVBytes = meetingASRStreamMaxBatchBytes + 64*1024
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("file read failed (details withheld)")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxWAVBytes+1))
	if err != nil {
		return nil, errors.New("file read failed (details withheld)")
	}
	if len(raw) > maxWAVBytes {
		return nil, errors.New("file exceeds the supported size limit")
	}
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, errors.New("container is not a valid RIFF/WAVE file")
	}
	riFFEnd := uint64(binary.LittleEndian.Uint32(raw[4:8])) + 8
	if riFFEnd != uint64(len(raw)) || riFFEnd < 12 {
		return nil, errors.New("RIFF length is inconsistent with the file")
	}

	var haveFormat, haveData bool
	var pcm []byte
	for offset := uint64(12); offset < riFFEnd; {
		if riFFEnd-offset < 8 {
			return nil, errors.New("truncated RIFF chunk header")
		}
		chunk := raw[offset : offset+8]
		chunkSize := uint64(binary.LittleEndian.Uint32(chunk[4:8]))
		chunkStart := offset + 8
		chunkEnd := chunkStart + chunkSize
		if chunkEnd < chunkStart || chunkEnd > riFFEnd {
			return nil, errors.New("RIFF chunk exceeds the declared container")
		}
		switch string(chunk[0:4]) {
		case "fmt ":
			if haveFormat || chunkSize < 16 {
				return nil, errors.New("WAV format chunk is missing or duplicated")
			}
			format := raw[chunkStart:chunkEnd]
			if binary.LittleEndian.Uint16(format[0:2]) != 1 ||
				binary.LittleEndian.Uint16(format[2:4]) != 1 ||
				binary.LittleEndian.Uint32(format[4:8]) != 16_000 ||
				binary.LittleEndian.Uint32(format[8:12]) != 32_000 ||
				binary.LittleEndian.Uint16(format[12:14]) != 2 ||
				binary.LittleEndian.Uint16(format[14:16]) != 16 {
				return nil, errors.New("WAV must be 16 kHz mono PCM16")
			}
			haveFormat = true
		case "data":
			if haveData || chunkSize == 0 || chunkSize%2 != 0 || chunkSize > meetingASRStreamMaxBatchBytes {
				return nil, errors.New("WAV data chunk is empty or outside supported bounds")
			}
			pcm = raw[chunkStart:chunkEnd]
			haveData = true
		}
		offset = chunkEnd + chunkSize%2
		if offset > riFFEnd {
			return nil, errors.New("RIFF padding exceeds the declared container")
		}
	}
	if !haveFormat || !haveData || len(pcm) == 0 {
		return nil, errors.New("WAV is missing its format or audio data chunk")
	}
	return pcm, nil
}

func safeMeetingSomniLiveError(err error) string {
	if errors.Is(err, ErrMeetingASRStreamDisabled) {
		return "provider is disabled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "request was canceled"
	}
	message := err.Error()
	if strings.HasPrefix(message, "login http ") {
		status := strings.TrimPrefix(message, "login http ")
		if _, parseErr := strconv.Atoi(status); parseErr == nil {
			return "login returned HTTP " + status
		}
		return "login returned an invalid HTTP status"
	}
	if strings.HasPrefix(message, "login code ") {
		code := strings.TrimPrefix(message, "login code ")
		if _, parseErr := strconv.Atoi(code); parseErr == nil {
			return "provider login code " + code
		}
		return "provider returned an invalid login code"
	}
	if strings.Contains(message, "timeout") || strings.Contains(message, "deadline exceeded") {
		return "provider request timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "provider network failure"
	}
	return "provider protocol or upstream failure (details withheld)"
}

func safeMeetingAILiveError(err error) string {
	switch {
	case errors.Is(err, ErrMeetingAIDisabled):
		return "service is disabled"
	case errors.Is(err, ErrMeetingAIBillingNotDisabled):
		return "billing gate is closed"
	case errors.Is(err, ErrMeetingAINotConfigured):
		return "API key is missing"
	case errors.Is(err, ErrMeetingAIInputEmpty), errors.Is(err, ErrMeetingAIInputTooLarge):
		return "synthetic summary input was rejected"
	case errors.Is(err, ErrMeetingAIUpstream):
		message := err.Error()
		if marker := "upstream status "; strings.Contains(message, marker) {
			status := message[strings.LastIndex(message, marker)+len(marker):]
			if _, parseErr := strconv.Atoi(status); parseErr == nil {
				return "upstream returned HTTP " + status
			}
		}
		if strings.Contains(message, "transport timeout") {
			return "upstream transport timed out"
		}
		if strings.Contains(message, "transport failure") {
			return "upstream transport failed"
		}
		if strings.Contains(message, "response too large") {
			return "upstream response exceeded the size limit"
		}
		return "upstream request failed (details withheld)"
	case errors.Is(err, ErrMeetingAIMalformedResponse):
		return "upstream response was malformed"
	case errors.Is(err, ErrMeetingAIEmptyResponse):
		return "upstream returned an empty completion"
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	case errors.Is(err, context.Canceled):
		return "request was canceled"
	default:
		var netErr net.Error
		if errors.As(err, &netErr) {
			return "provider network failure"
		}
		return fmt.Sprintf("service rejected the request (error class %T; details withheld)", err)
	}
}
