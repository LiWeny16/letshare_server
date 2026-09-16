package handler

import "testing"

func TestIsDirectCallSFURoom(t *testing.T) {
	tests := []struct {
		name string
		room string
		want bool
	}{
		{name: "generated call id", room: "c_mf2g1a_abc123", want: true},
		{name: "minimum length", room: "c_abc", want: true},
		{name: "too short", room: "c_ab", want: false},
		{name: "numbered meeting is not a call room", room: "1130", want: false},
		{name: "ordinary room is not a call room", room: "room-c_abc", want: false},
		{name: "path separator rejected", room: "c_abc/def", want: false},
		{name: "space rejected", room: "c_abc def", want: false},
		{name: "oversized rejected", room: "c_" + string(make([]byte, 63)), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDirectCallSFURoom(tt.room); got != tt.want {
				t.Fatalf("isDirectCallSFURoom(%q) = %v, want %v", tt.room, got, tt.want)
			}
		})
	}
}
