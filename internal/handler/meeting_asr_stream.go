package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	meetingASRStreamDefaultLoginType = "password"
	meetingASRStreamDefaultPlatform  = "android"
	meetingASRStreamDefaultDevice    = "letshare-server"
	meetingASRStreamDefaultVersion   = "1.0.0"
	meetingASRStreamDefaultTimezone  = "Asia/Shanghai"
	meetingASRStreamMaxFrameBytes    = 65_536
	meetingASRStreamReadTimeout      = 90 * time.Second
	meetingASRStreamWriteTimeout     = 5 * time.Second
	// The provider accepts at most 120s of 16kHz PCM16 in one session.
	meetingASRStreamMaxBatchBytes = 120 * 16_000 * 2
	meetingASRStreamUploadFrame   = 3_200 // 100ms of 16kHz PCM16
	meetingASRStreamStartTimeout  = 20 * time.Second
	meetingASRStreamStopTimeout   = 30 * time.Second
)

var (
	ErrMeetingASRStreamDisabled = errors.New("meeting streaming ASR is not configured")
	ErrMeetingASRStreamUpstream = errors.New("meeting streaming ASR upstream failed")
	ErrMeetingASRStreamNotReady = errors.New("meeting streaming ASR session not ready")
)

// MeetingASRStreamConfig mirrors the provider's login + websocket contract.
type MeetingASRStreamConfig struct {
	HTTPBase       string
	WSBase         string
	Email          string
	Password       string
	LoginType      string
	DevicePlatform string
	DeviceName     string
	AppVersion     string
	Timezone       string
}

func (c MeetingASRStreamConfig) normalized() MeetingASRStreamConfig {
	if c.LoginType == "" {
		c.LoginType = meetingASRStreamDefaultLoginType
	}
	if c.DevicePlatform == "" {
		c.DevicePlatform = meetingASRStreamDefaultPlatform
	}
	if c.DeviceName == "" {
		c.DeviceName = meetingASRStreamDefaultDevice
	}
	if c.AppVersion == "" {
		c.AppVersion = meetingASRStreamDefaultVersion
	}
	if c.Timezone == "" {
		c.Timezone = meetingASRStreamDefaultTimezone
	}
	c.HTTPBase = strings.TrimRight(c.HTTPBase, "/")
	c.WSBase = strings.TrimSpace(c.WSBase)
	return c
}

// MeetingASRStreamService logs in once, caches the bearer token, and opens one
// upstream stream per speaker. Audio is only relayed: never persisted or logged.
type MeetingASRStreamService struct {
	cfg    MeetingASRStreamConfig
	http   *http.Client
	dialer *websocket.Dialer
	mu     sync.Mutex
	token  string
	expiry time.Time
}

func NewMeetingASRStreamService(cfg MeetingASRStreamConfig) *MeetingASRStreamService {
	// Honour standard proxy settings: the ECS dials directly (no proxy env),
	// while restricted networks can route through their configured proxy.
	dialer := &websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	httpClient := &http.Client{Timeout: 15 * time.Second}
	return &MeetingASRStreamService{
		cfg:    cfg.normalized(),
		http:   httpClient,
		dialer: dialer,
	}
}

func (s *MeetingASRStreamService) Enabled() bool {
	return s != nil && s.cfg.HTTPBase != "" && s.cfg.WSBase != "" && s.cfg.Email != "" && s.cfg.Password != ""
}

func (s *MeetingASRStreamService) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expiry) {
		return s.token, nil
	}
	body := map[string]string{
		"login_type":      s.cfg.LoginType,
		"device_platform": s.cfg.DevicePlatform,
		"device_name":     s.cfg.DeviceName,
		"app_version":     s.cfg.AppVersion,
		"email":           s.cfg.Email,
		"password":        s.cfg.Password,
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.HTTPBase+"/auth/login", bytes.NewReader(payload))
	if err != nil {
		return "", ErrMeetingASRStreamUpstream
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Timezone", s.cfg.Timezone)
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()
	var decoded struct {
		Code int `json:"code"`
		Data struct {
			Token     string  `json:"access_token"`
			ExpiresIn float64 `json:"access_token_expires_in"`
		} `json:"data"`
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("login http %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("login decode: %w", err)
	}
	if decoded.Code != 0 {
		return "", fmt.Errorf("login code %d", decoded.Code)
	}
	if decoded.Data.Token == "" {
		return "", fmt.Errorf("login empty token")
	}
	margin := 60.0
	if decoded.Data.ExpiresIn <= margin {
		margin = 0
	}
	s.token = decoded.Data.Token
	s.expiry = time.Now().Add(time.Duration((decoded.Data.ExpiresIn - margin) * float64(time.Second)))
	return s.token, nil
}

// MeetingASRStreamEvent is one decoded upstream event.
type MeetingASRStreamEvent struct {
	Kind  string
	Text  string
	Final bool
}

type MeetingASRStream struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	closed  atomic.Bool
}

// Open dials the provider and starts a PCM 16k raw session.
func (s *MeetingASRStreamService) Open(ctx context.Context) (*MeetingASRStream, error) {
	if !s.Enabled() {
		return nil, ErrMeetingASRStreamDisabled
	}
	token, err := s.accessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := s.dialer.DialContext(ctx, s.cfg.WSBase, header)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	stream := &MeetingASRStream{conn: conn}
	if err := stream.writeJSON(map[string]interface{}{"event": "start", "format": "pcm", "rate": 16_000, "codec": "raw"}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("start: %w", err)
	}
	return stream, nil
}

func (st *MeetingASRStream) writeJSON(value interface{}) error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	if st.closed.Load() {
		return ErrMeetingASRStreamUpstream
	}
	_ = st.conn.SetWriteDeadline(time.Now().Add(meetingASRStreamWriteTimeout))
	return st.conn.WriteJSON(value)
}

// SendPCM forwards one even-length PCM16 frame.
func (st *MeetingASRStream) SendPCM(pcm []byte) error {
	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > meetingASRStreamMaxFrameBytes {
		return ErrMeetingASRStreamUpstream
	}
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	if st.closed.Load() {
		return ErrMeetingASRStreamUpstream
	}
	_ = st.conn.SetWriteDeadline(time.Now().Add(meetingASRStreamWriteTimeout))
	return st.conn.WriteMessage(websocket.BinaryMessage, pcm)
}

// SendStop asks the provider to finish the current utterance. Unlike Close it
// keeps the socket open so the caller can still read the final result events.
func (st *MeetingASRStream) SendStop() error {
	return st.writeJSON(map[string]string{"event": "stop"})
}

// Transcribe performs one non-streaming recognition call: open a fresh
// provider session, wait for `started`, upload the complete PCM16 buffer,
// send `stop`, and collect final text. Audio never leaves this stack frame.
func (s *MeetingASRStreamService) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if !s.Enabled() {
		return "", ErrMeetingASRStreamDisabled
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > meetingASRStreamMaxBatchBytes {
		return "", ErrMeetingASRStreamUpstream
	}
	stream, err := s.Open(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.conn.Close() }()

	if err := stream.waitForStarted(ctx); err != nil {
		return "", err
	}
	for offset := 0; offset < len(pcm); offset += meetingASRStreamUploadFrame {
		end := offset + meetingASRStreamUploadFrame
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := stream.SendPCM(pcm[offset:end]); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if err := stream.SendStop(); err != nil {
		return "", err
	}

	deadline := time.Now().Add(meetingASRStreamStopTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = stream.conn.SetReadDeadline(deadline)

	var finalText, interimText string
	for {
		_, payload, err := stream.conn.ReadMessage()
		if err != nil {
			return "", err
		}
		var event struct {
			Event string `json:"event"`
			Data  struct {
				Text    string `json:"text"`
				IsFinal bool   `json:"isFinal"`
			} `json:"data"`
		}
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		switch event.Event {
		case "result":
			text := strings.TrimSpace(event.Data.Text)
			if text == "" {
				continue
			}
			if event.Data.IsFinal {
				finalText = appendASRText(finalText, text)
			} else {
				interimText = text
			}
		case "ping":
			_ = stream.writeJSON(map[string]string{"event": "pong"})
		case "stopped":
			if finalText == "" {
				finalText = interimText
			}
			return finalText, nil
		case "error":
			return "", ErrMeetingASRStreamUpstream
		}
	}
}

func (st *MeetingASRStream) waitForStarted(ctx context.Context) error {
	deadline := time.Now().Add(meetingASRStreamStartTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = st.conn.SetReadDeadline(deadline)
	for {
		_, payload, err := st.conn.ReadMessage()
		if err != nil {
			return err
		}
		var event struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		switch event.Event {
		case "started":
			return nil
		case "ping":
			_ = st.writeJSON(map[string]string{"event": "pong"})
		case "error":
			return ErrMeetingASRStreamUpstream
		}
	}
}

// appendASRText concatenates provider results without inserting a space
// between CJK text, where the provider already emits continuous sentences.
func appendASRText(acc, part string) string {
	if acc == "" {
		return part
	}
	if part == "" {
		return acc
	}
	last, _ := utf8.DecodeLastRuneInString(acc)
	first, _ := utf8.DecodeRuneInString(part)
	if isCJK(last) || isCJK(first) {
		return acc + part
	}
	return acc + " " + part
}

func isCJK(r rune) bool {
	return (r >= 0x2E80 && r <= 0x9FFF) || (r >= 0xF900 && r <= 0xFAFF) || (r >= 0x3040 && r <= 0x30FF)
}

// ReadLoop forwards decoded results until the provider stops or the context ends.
func (st *MeetingASRStream) ReadLoop(ctx context.Context, onEvent func(MeetingASRStreamEvent)) error {
	for {
		if err := st.conn.SetReadDeadline(time.Now().Add(meetingASRStreamReadTimeout)); err != nil {
			return err
		}
		_, payload, err := st.conn.ReadMessage()
		if err != nil {
			return err
		}
		var event struct {
			Event string `json:"event"`
			Data  struct {
				Text    string `json:"text"`
				IsFinal bool   `json:"isFinal"`
			} `json:"data"`
		}
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		switch event.Event {
		case "started":
			onEvent(MeetingASRStreamEvent{Kind: "started"})
		case "result":
			onEvent(MeetingASRStreamEvent{Kind: "result", Text: strings.TrimSpace(event.Data.Text), Final: event.Data.IsFinal})
		case "ping":
			_ = st.writeJSON(map[string]string{"event": "pong"})
		case "stopped":
			return nil
		case "error":
			return ErrMeetingASRStreamUpstream
		}
	}
}

// Close sends stop and closes the upstream socket.
func (st *MeetingASRStream) Close() {
	if st.closed.Swap(true) {
		return
	}
	st.writeMu.Lock()
	_ = st.conn.SetWriteDeadline(time.Now().Add(meetingASRStreamWriteTimeout))
	_ = st.conn.WriteJSON(map[string]string{"event": "stop"})
	st.writeMu.Unlock()
	_ = st.conn.Close()
}
