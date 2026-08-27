package handler

import (
	"testing"
)

// buildMediaFrame 构造一个最小媒体帧：24B 头（"medi" 魔数 + callId + seq + track）+ payload
// 布局：[0..4) "medi" | [4..20) callId(16B) | [20..22) seq(u16 BE) | [22) track | [23) padding
func buildMediaFrame(callID string, seq uint16, track byte, payloadLen int) []byte {
	frame := make([]byte, mediaFrameHeaderSize+payloadLen)
	frame[0], frame[1], frame[2], frame[3] = 'm', 'e', 'd', 'i'
	for i := 0; i < len(callID) && i < mediaFrameCallIDBytes; i++ {
		frame[4+i] = callID[i]
	}
	frame[20] = byte(seq >> 8)
	frame[21] = byte(seq & 0xff)
	frame[22] = track
	return frame
}

func TestIsMediaFrame(t *testing.T) {
	if !isMediaFrame(buildMediaFrame("c_abc", 1, 0, 4)) {
		t.Error("expected media frame with 'medi' magic to be detected")
	}
	// 文件传输帧（JSON 元数据头开头）不是媒体帧
	fileChunk := make([]byte, 300)
	fileChunk[0] = '{'
	if isMediaFrame(fileChunk) {
		t.Error("expected file chunk to NOT be detected as media frame")
	}
	// 过短帧
	if isMediaFrame([]byte("medi")) {
		t.Error("expected short frame to NOT be detected as media frame")
	}
}

func TestMediaFrameCallIDAndSeq(t *testing.T) {
	frame := buildMediaFrame("c_abc123", 0x1234, 1, 8)
	if got := mediaFrameCallID(frame); got != "c_abc123" {
		t.Errorf("callID = %q, want %q", got, "c_abc123")
	}
	if got := mediaFrameSeq(frame); got != 0x1234 {
		t.Errorf("seq = %#x, want %#x", got, 0x1234)
	}
}

func TestMediaFrameCallIDTruncatesAtNUL(t *testing.T) {
	frame := buildMediaFrame("abc", 0, 0, 4)
	if got := mediaFrameCallID(frame); got != "abc" {
		t.Errorf("callID = %q, want %q (should truncate at first NUL)", got, "abc")
	}
}
