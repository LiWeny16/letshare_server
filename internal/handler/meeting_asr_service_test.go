package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMeetingASRServiceSendsWAVAndParsesText(t *testing.T) {
	var gotAuth, gotModel string
	var wav []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct {
					Type       string `json:"type"`
					InputAudio struct {
						Data   string `json:"data"`
						Format string `json:"format"`
					} `json:"input_audio"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		for _, part := range body.Messages[0].Content {
			if part.Type == "input_audio" {
				if part.InputAudio.Format != "wav" {
					t.Errorf("format=%q", part.InputAudio.Format)
				}
				wav, _ = base64.StdEncoding.DecodeString(part.InputAudio.Data)
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  \"我们先确认预算。\"  "}}]}`))
	}))
	defer srv.Close()

	svc := NewMeetingASRService("test-key", srv.URL, "gemini-test", srv.Client())
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	text, err := svc.Transcribe(context.Background(), pcm, 16_000, "zh")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "我们先确认预算。" {
		t.Fatalf("text=%q", text)
	}
	if gotAuth != "Bearer test-key" || gotModel != "gemini-test" {
		t.Fatalf("auth=%q model=%q", gotAuth, gotModel)
	}
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[36:40]) != "data" {
		t.Fatalf("not a wav: % x", wav[:min(12, len(wav))])
	}
	if len(wav) != 44+len(pcm) {
		t.Fatalf("wav length=%d", len(wav))
	}
}

func TestMeetingASRServiceDisabledWithoutKey(t *testing.T) {
	svc := NewMeetingASRService("", "", "", nil)
	if svc.Enabled() {
		t.Fatal("service should be disabled without a key")
	}
	if _, err := svc.Transcribe(context.Background(), []byte{1, 2}, 16_000, "zh"); err == nil {
		t.Fatal("expected disabled error")
	}
}

func TestMeetingASRServiceRejectsOversizedChunk(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	svc := NewMeetingASRService("k", srv.URL, "m", srv.Client())
	if _, err := svc.Transcribe(context.Background(), make([]byte, meetingASRMaxPCMBytes+1), 16_000, "zh"); err == nil {
		t.Fatal("expected rejection")
	}
	if calls.Load() != 0 {
		t.Fatal("oversized chunk reached the provider")
	}
	if !strings.Contains(meetingASRDefaultBaseURL, "http") {
		t.Fatal("default base url")
	}
}
