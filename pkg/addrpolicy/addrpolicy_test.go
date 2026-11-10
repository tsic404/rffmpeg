package addrpolicy

import (
	"context"
	"testing"
)

// TestLiteralAddrCoversNumericSpellings locks the parser agreement C-129 needs:
// ffmpeg resolves its target through the C library, which accepts the inet_aton
// numeric spellings of an IPv4 address. A spelling netip rejects but the C
// library accepts would reach a denied address while the judgement sees a host
// name, so every such spelling must parse to the same address.
func TestLiteralAddrCoversNumericSpellings(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"127.0.0.1", "127.0.0.1"},
		{"127.1", "127.0.0.1"},      // the trailing part fills the remaining bytes
		{"127.0.1", "127.0.0.1"},    // three-part form
		{"2130706433", "127.0.0.1"}, // one 32-bit number
		{"0x7f000001", "127.0.0.1"}, // hexadecimal
		{"017700000001", "127.0.0.1"},
		{"127.0x0.0.1", "127.0.0.1"}, // mixed per-part bases
		{"0xa9fea9fe", "169.254.169.254"},
		{"169.254.169.254", "169.254.169.254"},
		{"10.0.0.5", "10.0.0.5"},
		{"192.168.1.1", "192.168.1.1"},
		{"255.255.255.255", "255.255.255.255"},
	}
	for _, tc := range cases {
		addr, ok := literalAddr(tc.host)
		if !ok {
			t.Errorf("literalAddr(%q) did not parse, want %s", tc.host, tc.want)
			continue
		}
		if got := addr.String(); got != tc.want {
			t.Errorf("literalAddr(%q) = %s, want %s", tc.host, got, tc.want)
		}
	}

	for _, host := range []string{
		"example.test",
		"media.example.com",
		"127.0.0.1.example.test",
		"300.1.0.1",
		"127.1.2.3.4",
		"",
	} {
		if addr, ok := literalAddr(host); ok {
			t.Errorf("literalAddr(%q) = %s, want no literal address", host, addr)
		}
	}
}

// TestCheckOutputLiteralJudgesNumericSpellings locks the submission side of
// C-129 for the inet_aton spellings: each names the same denied address as its
// dotted-quad form, so it must be refused there too.
func TestCheckOutputLiteralJudgesNumericSpellings(t *testing.T) {
	for _, target := range []string{
		"udp://127.1:9000/stream",
		"udp://2130706433:9000/stream",
		"udp://0x7f000001:9000/stream",
		"udp://017700000001:9000/stream",
		"udp://0xa9fea9fe:9/stream",
	} {
		if err := CheckOutputLiteral(target); err == nil {
			t.Errorf("CheckOutputLiteral(%q) = nil, want a denial", target)
		}
	}
	for _, target := range []string{
		"udp://10.0.0.1:9000/stream",
		"rtmp://10.20.30.40:1935/live/x",
		"udp://media.example.test:9000/stream",
	} {
		if err := CheckOutputLiteral(target); err != nil {
			t.Errorf("CheckOutputLiteral(%q) = %v, want allowed", target, err)
		}
	}
}

// TestCheckOutputTargetFailsClosed locks the execution side of C-129: a target
// whose address cannot be judged is refused, never handed to ffmpeg on the hope
// that its resolver fails the same way.
func TestCheckOutputTargetFailsClosed(t *testing.T) {
	for _, target := range []string{
		"udp://127.1:9000/stream",
		"udp://2130706433:9000/stream",
		"udp://0xa9fea9fe:9/stream",
		"udp://no-such-host.invalid:9000/stream",
		"udp://:9000/stream",
	} {
		if err := CheckOutputTarget(context.Background(), target); err == nil {
			t.Errorf("CheckOutputTarget(%q) = nil, want a refusal", target)
		}
	}
	if err := CheckOutputTarget(context.Background(), "udp://10.20.30.40:9000/stream"); err != nil {
		t.Errorf("CheckOutputTarget(private address) = %v, want allowed", err)
	}
}
