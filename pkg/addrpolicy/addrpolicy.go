// Package addrpolicy bounds the addresses a job may reach: remote inputs are
// fetched only from addresses that can be global unicast destinations, and a
// protocol-address output target is judged by the same classes before ffmpeg
// ever connects to it (C-128 / C-129).
//
// Both the submission side and the execution side call into this package, so
// the deny list, the resolution rules and the refusals cannot drift apart.
package addrpolicy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DeniedError reports a target whose address is in a class that can never
// serve as a global unicast destination. The connection is never attempted, so
// the target receives no request.
type DeniedError struct {
	Target string
	Addr   string
	Class  string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("target %s is not permitted: it resolves to %s (%s)",
		e.Target, e.Addr, e.Class)
}

// deniedPrefixes enumerates the address classes a job may never reach. RFC1918
// and IPv6 unique local addresses are deliberately absent: a worker deployment
// in the operator's own network treats intranet media sources as an in-scope
// use, and the contract puts private space on the allowed side. Anything not
// listed here is allowed by default.
var deniedPrefixes = []struct {
	prefix netip.Prefix
	class  string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "this-network"},
	{netip.MustParsePrefix("100.64.0.0/10"), "shared address space"},
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback"},
	{netip.MustParsePrefix("169.254.0.0/16"), "link-local"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignment"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation"},
	{netip.MustParsePrefix("192.88.99.0/24"), "6to4 relay anycast"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation"},
	{netip.MustParsePrefix("224.0.0.0/4"), "multicast"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},
	{netip.MustParsePrefix("::/128"), "unspecified"},
	{netip.MustParsePrefix("::1/128"), "loopback"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "NAT64"},
	{netip.MustParsePrefix("100::/64"), "discard-only"},
	{netip.MustParsePrefix("2001::/32"), "Teredo"},
	{netip.MustParsePrefix("2001:2::/48"), "benchmarking"},
	{netip.MustParsePrefix("2001:10::/28"), "ORCHID"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation"},
	{netip.MustParsePrefix("2002::/16"), "6to4"},
	{netip.MustParsePrefix("fe80::/10"), "link-local"},
	{netip.MustParsePrefix("ff00::/8"), "multicast"},
}

// DeniedClass returns the denied class of addr, or "" when the address is
// allowed. An IPv4-mapped IPv6 address is judged as its IPv4 form, so
// ::ffff:127.0.0.1 cannot bypass the loopback rule.
func DeniedClass(addr netip.Addr) string {
	if addr.Zone() != "" {
		addr = addr.WithZone("")
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	for _, denied := range deniedPrefixes {
		if denied.prefix.Contains(addr) {
			return denied.class
		}
	}
	return ""
}

// literalAddr parses a host as a literal address: the canonical spellings
// netip understands, plus the inet_aton numeric forms a C library resolves
// (127.1, 2130706433, 0x7f000001, 017700000001). ffmpeg resolves its targets
// through the C library, so an address the Go parser rejects can still connect;
// the judgement has to see the same address that the connection will use.
func literalAddr(host string) (netip.Addr, bool) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr, true
	}
	return parseNumericIPv4(host)
}

// parseNumericIPv4 parses the inet_aton numeric spellings: one to four
// dot-separated parts, each decimal, octal (leading zero) or hexadecimal (0x).
// With fewer than four parts the last part fills the remaining bytes.
func parseNumericIPv4(host string) (netip.Addr, bool) {
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	values := make([]uint64, len(parts))
	for i, part := range parts {
		value, ok := parseNumericPart(part)
		if !ok {
			return netip.Addr{}, false
		}
		values[i] = value
	}
	var addr uint64
	switch len(values) {
	case 1:
		if values[0] > 0xffffffff {
			return netip.Addr{}, false
		}
		addr = values[0]
	case 2:
		if values[0] > 0xff || values[1] > 0xffffff {
			return netip.Addr{}, false
		}
		addr = values[0]<<24 | values[1]
	case 3:
		if values[0] > 0xff || values[1] > 0xff || values[2] > 0xffff {
			return netip.Addr{}, false
		}
		addr = values[0]<<24 | values[1]<<16 | values[2]
	default:
		for _, value := range values {
			if value > 0xff {
				return netip.Addr{}, false
			}
		}
		addr = values[0]<<24 | values[1]<<16 | values[2]<<8 | values[3]
	}
	return netip.AddrFrom4([4]byte{byte(addr >> 24), byte(addr >> 16), byte(addr >> 8), byte(addr)}), true
}

// parseNumericPart parses one part of an inet_aton address: 0x-prefixed hex, a
// leading-zero octal, or decimal.
func parseNumericPart(part string) (uint64, bool) {
	if part == "" {
		return 0, false
	}
	base := 10
	digits := part
	switch {
	case len(part) > 2 && (strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X")):
		base, digits = 16, part[2:]
	case len(part) > 1 && part[0] == '0':
		base, digits = 8, part[1:]
	}
	for i := 0; i < len(digits); i++ {
		if !validDigit(digits[i], base) {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func validDigit(c byte, base int) bool {
	switch {
	case c >= '0' && c <= '9':
		return int(c-'0') < base
	case base == 16 && c >= 'a' && c <= 'f':
		return true
	case base == 16 && c >= 'A' && c <= 'F':
		return true
	default:
		return false
	}
}

// Resolve resolves a target host and rejects the whole target when any resolved
// address is denied: a literal address is judged as itself, a host name by its
// resolution, and one denied address among several is enough to refuse. It
// returns the addresses so a caller that dials itself never resolves a second
// time (no rebinding window).
func Resolve(ctx context.Context, target, host string) ([]netip.Addr, error) {
	if addr, ok := literalAddr(host); ok {
		if class := DeniedClass(addr); class != "" {
			return nil, &DeniedError{Target: target, Addr: addr.String(), Class: class}
		}
		return []netip.Addr{addr}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("target host %q resolved to no address", host)
	}
	for _, addr := range addrs {
		if class := DeniedClass(addr); class != "" {
			return nil, &DeniedError{Target: target, Addr: addr.String(), Class: class}
		}
	}
	return addrs, nil
}

// CheckInputURL judges a remote input URL before any request is made: only
// http/https with a host is a remote input, and the address it reaches must be
// allowed (C-128).
func CheckInputURL(ctx context.Context, rawURL string) error {
	target, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid remote input URL %q: %w", rawURL, err)
	}
	switch strings.ToLower(target.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("remote input URL %q is not http or https", rawURL)
	}
	if target.Hostname() == "" {
		return fmt.Errorf("remote input URL %q carries no host", rawURL)
	}
	_, err = Resolve(ctx, rawURL, target.Hostname())
	return err
}

// CheckOutputTarget judges a protocol-address output target before ffmpeg
// starts (C-129). A target that cannot be parsed carries no host of its own and
// is left to the caller; a denied address class is reported so the job fails
// without the target receiving a connection.
func CheckOutputTarget(ctx context.Context, rawURL string) error {
	target, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid output target %q: %w", rawURL, err)
	}
	if target.Hostname() == "" {
		return fmt.Errorf("output target %q carries no host", rawURL)
	}
	_, err = Resolve(ctx, rawURL, target.Hostname())
	return err
}

// CheckOutputLiteral judges the submission side of a protocol-address output
// target: only a literal address can be judged without resolving a host name,
// and a host name is left to the execution side (C-129). It returns a
// DeniedError for a literal address in a denied class, and nil otherwise.
func CheckOutputLiteral(rawURL string) error {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := target.Hostname()
	if host == "" {
		return nil
	}
	addr, ok := literalAddr(host)
	if !ok {
		return nil
	}
	if class := DeniedClass(addr); class != "" {
		return &DeniedError{Target: rawURL, Addr: addr.String(), Class: class}
	}
	return nil
}

// NewRemoteInputClient builds the HTTP client used only for user-provided
// remote input URLs. Its dialer judges every resolved address itself and pins
// the connection to a judged address, and its redirect hook re-judges each hop,
// so neither a redirect nor a second resolved address can reach a denied target
// (C-128).
func NewRemoteInputClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addrs, err := Resolve(ctx, address, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, addr := range addrs {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		Proxy: nil,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("remote input redirect limit reached")
			}
			return CheckInputURL(req.Context(), req.URL.String())
		},
	}
}
