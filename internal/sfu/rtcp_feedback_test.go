package sfu

import (
	"testing"

	"github.com/pion/rtcp"
)

func TestHasKeyframeFeedback(t *testing.T) {
	tests := []struct {
		name string
		pkts []rtcp.Packet
		want bool
	}{
		{name: "empty", want: false},
		{name: "receiver report only", pkts: []rtcp.Packet{&rtcp.ReceiverReport{}}, want: false},
		{name: "pli", pkts: []rtcp.Packet{&rtcp.PictureLossIndication{}}, want: true},
		{name: "fir", pkts: []rtcp.Packet{&rtcp.FullIntraRequest{}}, want: true},
		{name: "mixed", pkts: []rtcp.Packet{&rtcp.ReceiverReport{}, &rtcp.PictureLossIndication{}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasKeyframeFeedback(tt.pkts); got != tt.want {
				t.Fatalf("hasKeyframeFeedback() = %v, want %v", got, tt.want)
			}
		})
	}
}
