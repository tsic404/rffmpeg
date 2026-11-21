package worker

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestPushTransportFor locks the protocol split: HTTP-shaped protocols keep
// the request host through the proxy, UDP-carried protocols get a datagram
// relay, and everything else is a byte stream.
func TestPushTransportFor(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   pushTransport
	}{
		{"tcp://stream.example.test:9000/live", pushTransportTCP},
		{"rtmp://stream.example.test:1935/live", pushTransportTCP},
		{"rtmps://stream.example.test:443/live", pushTransportTCP},
		{"rtsp://stream.example.test:554/live", pushTransportRTSP},
		{"rtsps://stream.example.test:322/live", pushTransportTCP},
		{"http://stream.example.test:8080/live", pushTransportTCP},
		{"https://stream.example.test:8443/live", pushTransportProxy},
		{"rtmpt://stream.example.test:8080/live", pushTransportProxy},
		{"rtmpte://stream.example.test:8080/live", pushTransportProxy},
		{"srt://stream.example.test:9000?mode=caller", pushTransportUDP},
		{"srt://stream.example.test:9000?mode=listener", pushTransportDirect},
		{"udp://stream.example.test:9000", pushTransportUDP},
		{"rtp://stream.example.test:9000", pushTransportUDP},
	} {
		if got := pushTransportFor(tc.target); got != tc.want {
			t.Errorf("pushTransportFor(%q) = %d, want %d", tc.target, got, tc.want)
		}
	}
}

// TestJudgedTCPRelayPublishesEstablishedBeforeObserved closes the ordering
// window: a successful dial must be visible before the in-flight count drops.
// The old order let Observed report attempted-but-not-established while the
// upstream socket was already accepted, which started a duplicate push.
func TestJudgedTCPRelayPublishesEstablishedBeforeObserved(t *testing.T) {
	receiver, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := receiver.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	dialed := make(chan struct{})
	release := make(chan struct{})
	relay, err := startJudgedTCPRelayWith(receiver.Addr().String(), nil, func() {
		close(dialed)
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	client, err := net.Dial("tcp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	select {
	case <-dialed:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never dialed the judged receiver")
	}

	type observation struct{ attempted, established bool }
	observed := make(chan observation, 1)
	go func() {
		attempted, established := relay.Observed()
		observed <- observation{attempted, established}
	}()

	select {
	case got := <-observed:
		t.Fatalf("Observed returned (%v, %v) before the successful dial was published",
			got.attempted, got.established)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case got := <-observed:
		if !got.attempted || !got.established {
			t.Fatalf("Observed = (%v, %v), want an established dial", got.attempted, got.established)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Observed did not return after the dial result was published")
	}
	select {
	case upstream := <-accepted:
		upstream.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the judged receiver never accepted the relay dial")
	}
}

// TestJudgedTCPRelayCloseDuringDial proves Close and a completed dial can
// interleave: the established fact is still published, Close returns, and no
// observer can turn the connection into a fallback.
func TestJudgedTCPRelayCloseDuringDial(t *testing.T) {
	receiver, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := receiver.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	dialed := make(chan struct{})
	release := make(chan struct{})
	relay, err := startJudgedTCPRelayWith(receiver.Addr().String(), nil, func() {
		close(dialed)
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}

	client, err := net.Dial("tcp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-dialed:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never dialed the judged receiver")
	}

	closed := make(chan struct{})
	go func() {
		relay.Close()
		close(closed)
	}()
	close(release)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a dial completed")
	}
	attempted, established := relay.Observed()
	if !attempted || !established {
		t.Fatalf("Observed after Close = (%v, %v), want the established dial", attempted, established)
	}
	select {
	case upstream := <-accepted:
		upstream.Close()
	default:
	}
}

// TestJudgedTCPRelayObservesFailedDial proves the relay itself is the witness:
// a refused judged dial is visible as attempted-but-not-established without any
// ffmpeg evidence.
func TestJudgedTCPRelayObservesFailedDial(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	judged := closed.Addr().String()
	closed.Close()

	relay, err := startJudgedTCPRelay(judged)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relay.BeginAttempt()

	client, err := net.Dial("tcp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := client.Read(buf); err == nil {
		t.Fatal("a refused judged dial left the local connection open")
	}

	attempted, established := relay.Observed()
	if !attempted || established {
		t.Fatalf("Observed = (%v, %v), want a dial that did not establish", attempted, established)
	}
	if !shouldRetryPush(relay) {
		t.Error("a failed judged dial must allow the next judged address")
	}
}

// TestJudgedTCPRelayForwardsAfterEstablishedDial proves real bytes cross the
// relay and an established dial is never retried, even before media is muxed.
func TestJudgedTCPRelayForwardsAfterEstablishedDial(t *testing.T) {
	receiver, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := receiver.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	relay, err := startJudgedTCPRelay(receiver.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relay.BeginAttempt()

	client, err := net.Dial("tcp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var upstream net.Conn
	select {
	case upstream = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the receiver got no connection from the relay")
	}
	defer upstream.Close()

	if _, err := client.Write([]byte("push-bytes")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("push-bytes"))
	if _, err := io.ReadFull(upstream, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "push-bytes" {
		t.Fatalf("receiver read %q", buf)
	}

	attempted, established := relay.Observed()
	if !attempted || !established {
		t.Fatalf("Observed = (%v, %v), want an established dial", attempted, established)
	}
	if shouldRetryPush(relay) {
		t.Error("an established judged dial must not move the push to another address")
	}
}

// TestJudgedUDPRelayObservesSRTHandshake proves the datagram relay treats an
// upstream response as establishment and a silent destination as retryable.
func TestJudgedUDPRelayObservesSRTHandshake(t *testing.T) {
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()

	relay, err := startJudgedUDPRelay(echo.LocalAddr().String(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relay.BeginAttempt()

	client, err := net.Dial("udp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("handshake")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	if _, err := client.Read(buf); err != nil {
		t.Fatalf("no response came back through the relay: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		attempted, established := relay.Observed()
		if attempted && established {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Observed = (%v, %v), want an established handshake", attempted, established)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if shouldRetryPush(relay) {
		t.Error("an answered SRT handshake must not move the push to another address")
	}
}

func TestJudgedUDPRelaySilentDestinationIsRetryable(t *testing.T) {
	dead, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	judged := dead.LocalAddr().String()
	dead.Close()

	relay, err := startJudgedUDPRelay(judged, true)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relay.BeginAttempt()

	client, err := net.Dial("udp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("handshake")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		attempted, established := relay.Observed()
		if attempted && !established {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Observed = (%v, %v), want a silent destination", attempted, established)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !shouldRetryPush(relay) {
		t.Error("a silent SRT destination must allow the next judged address")
	}
}

// TestJudgedUDPRelayOneWayUsesFirstDatagram locks the plain-UDP boundary: a
// one-way protocol has no handshake, so any sent datagram consumes the address
// and is never duplicated to the next one.
func TestJudgedUDPRelayOneWayUsesFirstDatagram(t *testing.T) {
	dead, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	judged := dead.LocalAddr().String()
	dead.Close()

	relay, err := startJudgedUDPRelay(judged, false)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relay.BeginAttempt()

	client, err := net.Dial("udp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("media")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		attempted, established := relay.Observed()
		if attempted && established {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Observed = (%v, %v), want the first datagram to be authoritative", attempted, established)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if shouldRetryPush(relay) {
		t.Error("a one-way UDP send must not be duplicated to another address")
	}
}

// TestJudgedUDPRelayObservedCountsQueuedDatagrams closes the ordering gap
// between ffmpeg writing a short stream into the loopback socket and the relay
// reading it: the attempt must be reported as used before the address decision,
// or the same media would be sent to the next judged address.
func TestJudgedUDPRelayObservedCountsQueuedDatagrams(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	relay, err := startJudgedUDPRelay(receiver.LocalAddr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relay.BeginAttempt()

	client, err := net.Dial("udp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("short-stream")); err != nil {
		t.Fatal(err)
	}

	attempted, established := relay.Observed()
	if !attempted || !established {
		t.Fatalf("Observed = (%v, %v), want the queued datagram counted", attempted, established)
	}
	if shouldRetryPush(relay) {
		t.Error("a datagram already written by ffmpeg must consume the address")
	}
}

// TestJudgedUDPRelayCloseDrainsQueuedDatagrams proves the relay does not
// discard datagrams the sender already wrote into the loopback socket when
// the push process exits: Close has to drain before it tears the socket down.
func TestJudgedUDPRelayCloseDrainsQueuedDatagrams(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	const datagrams = 200
	const payload = 200
	received := make(chan int, 1)
	go func() {
		buf := make([]byte, 65535)
		total := 0
		for total < datagrams*payload {
			n, _, err := receiver.ReadFromUDP(buf)
			if err != nil {
				received <- total
				return
			}
			total += n
		}
		received <- total
	}()

	relay, err := startJudgedUDPRelay(receiver.LocalAddr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	relay.BeginAttempt()

	client, err := net.Dial("udp", relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, payload)
	for i := 0; i < datagrams; i++ {
		if _, err := client.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	_ = client.Close()
	relay.Close()

	select {
	case got := <-received:
		if got != datagrams*payload {
			t.Fatalf("receiver got %d bytes, want %d: Close discarded queued datagrams", got, datagrams*payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not collect the relayed datagrams")
	}
}
