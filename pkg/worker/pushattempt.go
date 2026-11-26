package worker

import (
	"fmt"
	"net/netip"
)

// pushAttempt owns the relay or proxy one ffmpeg execution talks to for a
// single judged address. close is idempotent and releases the listeners once
// the address is abandoned; collect finalizes the attempt's connection list.
type pushAttempt struct {
	target  string
	collect func()
	close   func()
	// skipExecution marks an attempt whose address is already known to need a
	// destination outside the judged set: ffmpeg must not run, and the recorded
	// blocked element is what makes the address unusable (C-140).
	skipExecution bool
	// rewriteArgs, when set, rewrites the execution's argument list. It carries
	// the request-named proxy's relay address into the request (C-140).
	rewriteArgs func([]string) []string
}

// openPushAttempt starts the transport that keeps an execution on addr. The
// first pass and an encoder-fallback retry both call it: a retry that reused
// the first pass's target would dial a listener that pass already released.
func (w *Worker) openPushAttempt(plannedOutput jobOutput, pushMode pushTransport, addr netip.Addr, pushProxy *judgedHTTPProxy, ledger *pushLedger, attemptArgs []string) (*pushAttempt, error) {
	noop := func() {}
	// C-140: a proxy the request named is an outbound destination of its own.
	// Inside the judged set the session goes through it; outside, the element
	// is recorded as blocked and ffmpeg never runs for this address.
	if proxy, ok := plannedOutput.requestProxyFor(attemptArgs, addr); ok {
		sessionKind, _ := pushSessionKind(pushTargetScheme(plannedOutput.target))
		if !proxy.inside {
			blocked := proxy.target
			return &pushAttempt{
				target: plannedOutput.target,
				collect: func() {
					ledger.recordBlocked(sessionKind, blocked, pushOriginRequest)
				},
				close:         noop,
				skipExecution: true,
			}, nil
		}
		// The relay is a proxy-to-proxy tunnel: ffmpeg sends CONNECT to it, the
		// relay forwards the request to the request's own proxy. Counting the
		// dial would record a proxy that accepted TCP and then refused as
		// connected, so the forwarded bytes decide the element's result
		// (C-140, C-142).
		relay, err := startJudgedTCPRelayWithObserver(proxy.target, nil, nil,
			newProxyTunnelObserver(pushSchemeUsesTLS(pushTargetScheme(plannedOutput.target))))
		if err != nil {
			return nil, fmt.Errorf("failed to start request proxy relay: %w", err)
		}
		return &pushAttempt{
			target: plannedOutput.target,
			collect: func() {
				attempted, established := relay.ObservedWithChannel()
				ledger.recordSessionOrigin(sessionKind, proxy.target, attempted, established, pushOriginRequest)
			},
			close: relay.Close,
			rewriteArgs: func(args []string) []string {
				return replaceFFmpegOptionValue(args, "-http_proxy", "http://"+relay.Addr())
			},
		}, nil
	}
	switch pushMode {
	case pushTransportProxy:
		judged, err := plannedOutput.judgedHostPort(addr)
		if err != nil {
			return nil, err
		}
		pushProxy.BeginAttempt()
		pushProxy.SetAttemptTarget(judged)
		// A TLS family's handshake happens inside the tunnel, so the attempt
		// has to wait for it before calling the receiver connected (C-140).
		pushProxy.SetAttemptTLS(pushSchemeUsesTLS(pushTargetScheme(plannedOutput.target)))
		sessionKind, _ := pushSessionKind(pushTargetScheme(plannedOutput.target))
		return &pushAttempt{
			target: plannedOutput.target,
			collect: func() {
				attempted, established := pushProxy.Observed()
				ledger.recordSession(sessionKind, judged, attempted, established)
			},
			close: noop,
		}, nil
	case pushTransportUDP:
		if plannedOutput.isRTP() {
			rtpPort, rtcpPort, err := plannedOutput.rtpPorts()
			if err != nil {
				return nil, err
			}
			relays, err := startJudgedRTPRelays(addr.String(), rtpPort, rtcpPort, plannedOutput.fecEnabled(attemptArgs))
			if err != nil {
				return nil, fmt.Errorf("failed to start RTP/RTCP relays: %w", err)
			}
			target, err := plannedOutput.rtpTargetAtHost(relays.rtp.Addr(), relays.rtcp.Addr())
			if err != nil {
				relays.Close()
				return nil, err
			}
			return &pushAttempt{
				target: target,
				collect: func() {
					ledger.recordDatagrams(relays.flows())
				},
				close: relays.Close,
			}, nil
		}
		judged, err := plannedOutput.judgedHostPort(addr)
		if err != nil {
			return nil, err
		}
		relay, err := startJudgedUDPRelay(judged, plannedOutput.srtHandshake())
		if err != nil {
			return nil, fmt.Errorf("failed to start push relay: %w", err)
		}
		target, err := plannedOutput.targetAtHost(relay.Addr())
		if err != nil {
			relay.Close()
			return nil, err
		}
		streamKind, isStream := pushStreamKind(pushTargetScheme(plannedOutput.target))
		sessionKind, _ := pushSessionKind(pushTargetScheme(plannedOutput.target))
		return &pushAttempt{
			target: target,
			collect: func() {
				if isStream {
					// udp:// is a datagram stream: sent/failed, never a
					// session connection.
					flow, attempted := relay.flow(streamKind.family, streamKind.purpose)
					if attempted {
						ledger.recordDatagrams([]pushDatagramFlow{flow})
					}
					return
				}
				// srt:// is a session: the SRT handshake decides connected.
				attempted, established := relay.Observed()
				ledger.recordSession(sessionKind, judged, attempted, established)
			},
			close: relay.Close,
		}, nil
	case pushTransportRTSP:
		judged, err := plannedOutput.judgedHostPort(addr)
		if err != nil {
			return nil, err
		}
		media := &rtspMediaRelays{judged: addr, ledger: ledger}
		relay, err := startJudgedRTSPRelay(judged, addr, media)
		if err != nil {
			return nil, fmt.Errorf("failed to start RTSP relay: %w", err)
		}
		target, err := plannedOutput.targetAtHost(relay.Addr())
		if err != nil {
			relay.Close()
			return nil, err
		}
		sessionKind, _ := pushSessionKind(pushTargetScheme(plannedOutput.target))
		return &pushAttempt{
			target: target,
			collect: func() {
				attempted, established := relay.Observed()
				ledger.recordSession(sessionKind, judged, attempted, established)
				ledger.recordDatagrams(media.flows())
			},
			close: func() {
				relay.Close()
				media.close()
			},
		}, nil
	case pushTransportFTP:
		judged, err := plannedOutput.judgedHostPort(addr)
		if err != nil {
			return nil, err
		}
		data := &ftpDataRelays{judged: addr, ledger: ledger}
		rewriter := newFTPReplyRewriter(addr, data, ledger)
		relay, err := startJudgedTCPRelayWith(judged, rewriter, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to start FTP control relay: %w", err)
		}
		target, err := plannedOutput.targetAtHost(relay.Addr())
		if err != nil {
			relay.Close()
			return nil, err
		}
		sessionKind, _ := pushSessionKind("ftp")
		return &pushAttempt{
			target: target,
			collect: func() {
				attempted, established := relay.Observed()
				ledger.recordSession(sessionKind, judged, attempted, established)
				data.collect()
			},
			close: func() {
				relay.Close()
				data.close()
			},
		}, nil
	case pushTransportDirect:
		target, err := plannedOutput.targetAt(addr)
		if err != nil {
			return nil, err
		}
		return &pushAttempt{target: target, collect: nil, close: noop}, nil
	default:
		judged, err := plannedOutput.judgedHostPort(addr)
		if err != nil {
			return nil, err
		}
		relay, err := startJudgedTCPRelay(judged)
		if err != nil {
			return nil, fmt.Errorf("failed to start push relay: %w", err)
		}
		target, err := plannedOutput.targetAtHost(relay.Addr())
		if err != nil {
			relay.Close()
			return nil, err
		}
		sessionKind, _ := pushSessionKind(pushTargetScheme(plannedOutput.target))
		return &pushAttempt{
			target: target,
			collect: func() {
				attempted, established := relay.Observed()
				ledger.recordSession(sessionKind, judged, attempted, established)
			},
			close: relay.Close,
		}, nil
	}
}
