package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	meetingASRDefaultBaseURL = "https://api.fullive.xyz/v1"
	meetingASRDefaultModel   = "gemini-3.5-flash-lite"
	meetingASRDefaultTimeout = 45 * time.Second
	meetingASRMaxConcurrent  = 2
	// 10s of 16 kHz PCM16 is 320 KB; anything larger is not a real chunk.
	meetingASRMaxPCMBytes = 320 * 1024
	meetingASRMaxTextLen  = 2000
)

var (
	// ErrMeetingASRDisabled: the operator has not configured a provider key.
	ErrMeetingASRDisabled = errors.New("meeting ASR is not configured")
	// ErrMeetingASROverloaded: the per-process concurrency slots are busy.
	ErrMeetingASROverloaded = errors.New("meeting ASR is overloaded")
	// ErrMeetingASRUpstream: the provider call failed or returned nothing usable.
	ErrMeetingASRUpstream = errors.New("meeting ASR upstream request failed")
)

// MeetingASRService transcribes short PCM chunks through an OpenAI-compatible
// chat-completions provider that accepts audio input. Audio is built into a WAV
// in memory, sent once, and never written to disk or logs.
type MeetingASRService struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
	sem     chan struct{}
}

func NewMeetingASRService(apiKey, baseURL, model string, client *http.Client) *MeetingASRService {
	if baseURL == "" {
		baseURL = meetingASRDefaultBaseURL
	}
	if model == "" {
		model = meetingASRDefaultModel
	}
	if client == nil {
		client = &http.Client{Timeout: meetingASRDefaultTimeout}
	}
	return &MeetingASRService{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  client,
		sem:     make(chan struct{}, meetingASRMaxConcurrent),
	}
}

func (s *MeetingASRService) Enabled() bool { return s != nil && s.apiKey != "" }

// pcm16WAV wraps little-endian PCM16 mono samples in a minimal WAV container.
func pcm16WAV(pcm []byte, sampleRate int) []byte {
	if sampleRate <= 0 {
		sampleRate = 16_000
	}
	const headerSize = 44
	out := make([]byte, headerSize+len(pcm))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(pcm)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:24], 1) // mono
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}

// Transcribe sends one speaker chunk and returns plain transcript text.
// Callers must already have authorized the member and checked consent.
func (s *MeetingASRService) Transcribe(ctx context.Context, pcm []byte, sampleRate int, language string) (string, error) {
	if !s.Enabled() {
		return "", ErrMeetingASRDisabled
	}
	if len(pcm) == 0 || len(pcm) > meetingASRMaxPCMBytes {
		return "", ErrMeetingASRUpstream
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	default:
		return "", ErrMeetingASROverloaded
	}
	if language != "en" {
		language = "zh"
	}
	prompt := "Transcribe the speech verbatim in the spoken language (" + map[string]string{"zh": "Chinese", "en": "English"}[language] + "). Output only the transcript text, with punctuation. If there is no speech, output nothing."
	body := map[string]interface{}{
		"model":       s.model,
		"temperature": 0,
		"messages": []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": prompt},
					{"type": "input_audio", "input_audio": map[string]string{
						"data":   base64.StdEncoding.EncodeToString(pcm16WAV(pcm, sampleRate)),
						"format": "wav",
					}},
				},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", ErrMeetingASRUpstream
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", ErrMeetingASRUpstream
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", ErrMeetingASRUpstream
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ErrMeetingASRUpstream
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil || len(decoded.Choices) == 0 {
		return "", ErrMeetingASRUpstream
	}
	text := strings.TrimSpace(decoded.Choices[0].Message.Content)
	text = strings.Trim(text, "\"'` \n\t")
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", ErrMeetingASRUpstream
	}
	if len([]rune(text)) > meetingASRMaxTextLen {
		text = string([]rune(text)[:meetingASRMaxTextLen])
	}
	return text, nil
}
