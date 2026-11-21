package worker

import "testing"

func TestJobOutputRTPPorts(t *testing.T) {
	for _, tc := range []struct {
		target  string
		rtp     int
		rtcp    int
		wantErr bool
	}{
		{"rtp://stream.example.test:5004", 5004, 5005, false},
		{"rtp://stream.example.test:5004?rtcpport=9876", 5004, 9876, false},
		{"rtp://stream.example.test:5004?rtcp_port=9877", 5004, 9877, false},
		{"rtp://stream.example.test:5004?rtcpport=9876&rtcpport=9878", 5004, 9878, false},
		{"rtp://stream.example.test", 0, 0, true},
		{"rtp://stream.example.test:0", 0, 0, true},
		{"rtp://stream.example.test:5004?rtcpport=0", 0, 0, true},
		{"rtp://stream.example.test:5004?rtcpport=70000", 0, 0, true},
		{"rtp://stream.example.test:65535", 0, 0, true},
	} {
		rtp, rtcp, err := (jobOutput{target: tc.target}).rtpPorts()
		if tc.wantErr {
			if err == nil {
				t.Errorf("rtpPorts(%q) = (%d, %d), want an error", tc.target, rtp, rtcp)
			}
			continue
		}
		if err != nil {
			t.Errorf("rtpPorts(%q): %v", tc.target, err)
			continue
		}
		if rtp != tc.rtp || rtcp != tc.rtcp {
			t.Errorf("rtpPorts(%q) = (%d, %d), want (%d, %d)", tc.target, rtp, rtcp, tc.rtp, tc.rtcp)
		}
	}
}

func TestJobOutputRTPTargetAtHost(t *testing.T) {
	out := jobOutput{target: "rtp://stream.example.test:5004/live?ttl=5&rtcpport=9876"}
	got, err := out.rtpTargetAtHost("127.0.0.1:40000", "127.0.0.1:40001")
	if err != nil {
		t.Fatal(err)
	}
	want := "rtp://127.0.0.1:40000/live?ttl=5&rtcpport=40001"
	if got != want {
		t.Fatalf("rtpTargetAtHost = %q, want %q", got, want)
	}

	alias := jobOutput{target: "rtp://stream.example.test:5004?rtcp_port=9876"}
	got, err = alias.rtpTargetAtHost("127.0.0.1:40000", "127.0.0.1:40001")
	if err != nil {
		t.Fatal(err)
	}
	if got != "rtp://127.0.0.1:40000?rtcpport=40001" {
		t.Fatalf("rtpTargetAtHost with rtcp_port = %q", got)
	}
}
