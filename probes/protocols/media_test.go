package protocols

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/stun/v3"
	"github.com/pion/webrtc/v4"
)

type config struct {
	Address          string `json:"address"`
	UntrustedAddress string `json:"untrusted_address"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	ExpiredUsername  string `json:"expired_username"`
	ExpiredPassword  string `json:"expired_password"`
	CAFile           string `json:"ca_file"`
}

func configuration(t *testing.T) config {
	t.Helper()
	path := os.Getenv("RTC_PROTOCOL_CONFIG")
	if path == "" {
		t.Fatal("Run run.py to supply an isolated authenticated TURN fixture; no tests are skipped")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read protocol configuration")
	}
	var result config
	if json.Unmarshal(data, &result) != nil || net.ParseIP(result.Address) == nil || net.ParseIP(result.UntrustedAddress) == nil {
		t.Fatal("invalid protocol configuration")
	}
	return result
}

func opusPackets(t *testing.T, frequency int) [][]byte {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("testdata/opus-%d.json", frequency))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Frequency int      `json:"frequency_hz"`
		Rate      int      `json:"sample_rate"`
		Channels  int      `json:"channels"`
		Duration  int      `json:"duration_ms"`
		Packets   []string `json:"packets"`
	}
	if json.Unmarshal(data, &fixture) != nil || fixture.Frequency != frequency || fixture.Rate != 48000 || fixture.Channels != 1 || fixture.Duration != 20 || len(fixture.Packets) != 20 {
		t.Fatal("invalid pinned Opus fixture")
	}
	var result [][]byte
	for _, value := range fixture.Packets {
		packet, err := hex.DecodeString(value)
		if err != nil || len(packet) == 0 {
			t.Fatal("invalid encoded Opus packet")
		}
		result = append(result, packet)
	}
	return result
}

type endpoint struct {
	pc                  *webrtc.PeerConnection
	track               *webrtc.TrackLocalStaticRTP
	received            atomic.Int32
	errors              chan error
	packets             [][]byte
	fingerprintRejected *atomic.Bool
	iceEstablished      atomic.Bool
}

// Observe one controlled security outcome without retaining any library log,
// SDP, fingerprint value or credential. A terminal Close can immediately follow
// Failed, so polling the current transport enum would miss the rejection.
type outcomeLoggerFactory struct{ rejected *atomic.Bool }

func (f outcomeLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	base := &logging.DefaultLoggerFactory{DefaultLogLevel: logging.LogLevelDisabled}
	return outcomeLogger{LeveledLogger: base.NewLogger(scope), rejected: f.rejected}
}

type outcomeLogger struct {
	logging.LeveledLogger
	rejected *atomic.Bool
}

func (l outcomeLogger) Warnf(format string, args ...interface{}) {
	if strings.Contains(fmt.Sprintf(format, args...), "remote certificate does not match any fingerprint") {
		l.rejected.Store(true)
	}
}

func newEndpoint(t *testing.T, server *webrtc.ICEServer, send, receive [][]byte) *endpoint {
	t.Helper()
	m := &webrtc.MediaEngine{}
	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: capability, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	rejected := &atomic.Bool{}
	s := webrtc.SettingEngine{LoggerFactory: outcomeLoggerFactory{rejected: rejected}}
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	s.SetIncludeLoopbackCandidate(true)
	s.SetICETimeouts(2*time.Second, 3*time.Second, time.Second)
	s.SetSTUNGatherTimeout(2 * time.Second)
	c := webrtc.Configuration{}
	if server == nil {
		s.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	} else {
		c.ICEServers = []webrtc.ICEServer{*server}
		c.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(s)).NewPeerConnection(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pc.Close(); err != nil {
			t.Error("peer cleanup failed")
		}
	})
	track, err := webrtc.NewTrackLocalStaticRTP(capability, "audio", "protocol-probe")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, _, err := sender.ReadRTCP(); err != nil {
				return
			}
		}
	}()
	e := &endpoint{pc: pc, track: track, errors: make(chan error, 1), packets: send, fingerprintRejected: rejected}
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateConnected || state == webrtc.ICEConnectionStateCompleted {
			e.iceEstablished.Store(true)
		}
	})
	allowed := map[string]bool{}
	for _, packet := range receive {
		allowed[string(packet)] = true
	}
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if remote.Codec().MimeType != webrtc.MimeTypeOpus || remote.Codec().ClockRate != 48000 {
			select {
			case e.errors <- errors.New("unexpected negotiated codec"):
			default:
			}
			return
		}
		for {
			packet, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			if !allowed[string(packet.Payload)] {
				select {
				case e.errors <- errors.New("remote Opus payload was corrupted or echoed"):
				default:
				}
				return
			}
			e.received.Add(1)
		}
	})
	return e
}

func localDescription(t *testing.T, e *endpoint, answer bool) webrtc.SessionDescription {
	t.Helper()
	var d webrtc.SessionDescription
	var err error
	if answer {
		d, err = e.pc.CreateAnswer(nil)
	} else {
		d, err = e.pc.CreateOffer(nil)
	}
	if err != nil {
		t.Fatal("create description failed")
	}
	gathered := webrtc.GatheringCompletePromise(e.pc)
	if err := e.pc.SetLocalDescription(d); err != nil {
		t.Fatal("apply local description failed")
	}
	select {
	case <-gathered:
	case <-time.After(12 * time.Second):
		t.Fatal("ICE gathering did not finish")
	}
	return *e.pc.LocalDescription()
}

func negotiate(t *testing.T, a, b *endpoint, tamper bool) {
	t.Helper()
	offer := localDescription(t, a, false)
	if tamper {
		lines := strings.Split(offer.SDP, "\r\n")
		found := false
		for i, line := range lines {
			if strings.HasPrefix(line, "a=fingerprint:sha-256 ") {
				lines[i] = "a=fingerprint:sha-256 " + strings.TrimSuffix(strings.Repeat("00:", 32), ":")
				found = true
			}
		}
		if !found {
			t.Fatal("no DTLS fingerprint to test")
		}
		offer.SDP = strings.Join(lines, "\r\n")
	}
	// Reconstruct at the wire boundary. Pion descriptions carry an unexported
	// parsed cache; mutating only SDP on a local value would reuse that cache
	// and accidentally bypass the fingerprint mutation in this negative test.
	if err := b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: offer.Type, SDP: offer.SDP}); err != nil {
		t.Fatal("apply remote offer failed")
	}
	answer := localDescription(t, b, true)
	if err := a.pc.SetRemoteDescription(webrtc.SessionDescription{Type: answer.Type, SDP: answer.SDP}); err != nil {
		t.Fatal("apply remote answer failed")
	}
}

func send(t *testing.T, e *endpoint, index uint16) {
	t.Helper()
	packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: index, Timestamp: uint32(index) * 960, SSRC: 1}, Payload: e.packets[int(index)%len(e.packets)]}
	if err := e.track.WriteRTP(packet); err != nil {
		t.Fatal("send Opus RTP failed")
	}
}

func assertMedia(t *testing.T, a, b *endpoint, relay bool) {
	t.Helper()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var index uint16
	for {
		select {
		case err := <-a.errors:
			t.Fatal(err)
		case err := <-b.errors:
			t.Fatal(err)
		case <-deadline.C:
			t.Fatalf("bidirectional SRTP evidence missing: a=%d b=%d", a.received.Load(), b.received.Load())
		case <-tick.C:
			send(t, a, index)
			send(t, b, index)
			index++
			if a.received.Load() < 20 || b.received.Load() < 20 {
				continue
			}
			for _, e := range []*endpoint{a, b} {
				dtls := e.pc.SCTP().Transport()
				if dtls.State() != webrtc.DTLSTransportStateConnected || len(dtls.GetRemoteCertificate()) == 0 {
					t.Fatal("DTLS identity not established")
				}
				pair, err := dtls.ICETransport().GetSelectedCandidatePair()
				if err != nil || pair == nil {
					t.Fatal("no selected ICE pair")
				}
				if relay && (pair.Local.Typ != webrtc.ICECandidateTypeRelay || pair.Remote.Typ != webrtc.ICECandidateTypeRelay) {
					t.Fatal("relay-only policy silently fell back to host")
				}
			}
			t.Logf("verified Opus payloads over DTLS-SRTP in both directions: a=%d b=%d relay=%v", a.received.Load(), b.received.Load(), relay)
			return
		}
	}
}

func TestDirectWebRTCOpusOverDTLSSRTP(t *testing.T) {
	audioA, audioB := opusPackets(t, 1000), opusPackets(t, 1500)
	a := newEndpoint(t, nil, audioA, audioB)
	b := newEndpoint(t, nil, audioB, audioA)
	negotiate(t, a, b, false)
	assertMedia(t, a, b, false)
}

func TestTURNRelayTransports(t *testing.T) {
	c := configuration(t)
	for _, transport := range []struct{ name, scheme, port, query string }{{"UDP", "turn", "3478", "udp"}, {"TCP", "turn", "3478", "tcp"}, {"TLS", "turns", "5349", "tcp"}} {
		t.Run(transport.name, func(t *testing.T) {
			server := webrtc.ICEServer{URLs: []string{fmt.Sprintf("%s:%s:%s?transport=%s", transport.scheme, c.Address, transport.port, transport.query)}, Username: c.Username, Credential: c.Password}
			audioA, audioB := opusPackets(t, 1000), opusPackets(t, 1500)
			a := newEndpoint(t, &server, audioA, audioB)
			b := newEndpoint(t, &server, audioB, audioA)
			negotiate(t, a, b, false)
			assertMedia(t, a, b, true)
		})
	}
}

func assertNoRelay(t *testing.T, server webrtc.ICEServer) {
	t.Helper()
	audio := opusPackets(t, 1000)
	e := newEndpoint(t, &server, audio, audio)
	d := localDescription(t, e, false)
	if strings.Contains(d.SDP, " typ relay") || strings.Contains(d.SDP, " typ host") {
		t.Fatal("unauthorized relay or host fallback was admitted")
	}
}

func TestTURNRejectsInvalidAndExpiredCredentials(t *testing.T) {
	c := configuration(t)
	t.Run("wrong_password", func(t *testing.T) {
		assertNoRelay(t, webrtc.ICEServer{URLs: []string{"turn:" + c.Address + ":3478?transport=udp"}, Username: c.Username, Credential: "invalid-test-password"})
	})
	t.Run("expired_REST_credential", func(t *testing.T) {
		assertNoRelay(t, webrtc.ICEServer{URLs: []string{"turn:" + c.Address + ":3478?transport=udp"}, Username: c.ExpiredUsername, Credential: c.ExpiredPassword})
	})
}

func TestTURNRejectsUntrustedTLSCertificate(t *testing.T) {
	c := configuration(t)
	data, err := os.ReadFile(c.CAFile)
	if err != nil {
		t.Fatal("read test CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		t.Fatal("invalid test CA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dialer := tls.Dialer{Config: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(c.UntrustedAddress, "5349"))
	if connection != nil {
		connection.Close()
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatal("untrusted TURN certificate was not rejected for certificate trust")
	}
	assertNoRelay(t, webrtc.ICEServer{URLs: []string{"turns:" + c.UntrustedAddress + ":5349?transport=tcp"}, Username: c.Username, Credential: c.Password})
}

func TestSTUNBindingReturnsCorrelatedMappedAddress(t *testing.T) {
	c := configuration(t)
	client, err := stun.Dial("udp", net.JoinHostPort(c.Address, "3478"))
	if err != nil {
		t.Fatal("STUN dial failed")
	}
	defer client.Close()
	result := make(chan error, 1)
	go func() {
		err := client.Do(stun.MustBuild(stun.TransactionID, stun.BindingRequest), func(event stun.Event) {
			if event.Error != nil {
				result <- event.Error
				return
			}
			var mapped stun.XORMappedAddress
			if err := mapped.GetFrom(event.Message); err != nil {
				result <- err
				return
			}
			if mapped.IP == nil || mapped.Port < 1 || mapped.Port > 65535 {
				result <- errors.New("invalid STUN mapped address")
				return
			}
			result <- nil
		})
		if err != nil {
			select {
			case result <- err:
			default:
			}
		}
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal("STUN binding evidence missing")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("STUN transaction timed out")
	}
}

func TestTURNRejectsTLSHostnameMismatch(t *testing.T) {
	c := configuration(t)
	data, err := os.ReadFile(c.CAFile)
	if err != nil {
		t.Fatal("read test CA")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(data)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dialer := tls.Dialer{Config: &tls.Config{RootCAs: roots, ServerName: "wrong-turn.invalid", MinVersion: tls.VersionTLS12}}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(c.Address, "5349"))
	if connection != nil {
		connection.Close()
	}
	var hostname x509.HostnameError
	if !errors.As(err, &hostname) {
		t.Fatal("TURN TLS did not validate the endpoint name")
	}
}

func TestDTLSRejectsMismatchedFingerprint(t *testing.T) {
	audioA, audioB := opusPackets(t, 1000), opusPackets(t, 1500)
	a := newEndpoint(t, nil, audioA, audioB)
	b := newEndpoint(t, nil, audioB, audioA)
	negotiate(t, a, b, true)
	writes := make(chan struct{}, 2)
	for _, peer := range []*endpoint{a, b} {
		go func(e *endpoint) {
			defer func() { writes <- struct{}{} }()
			_ = e.track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: 1, Timestamp: 960, SSRC: 1}, Payload: e.packets[0]})
		}(peer)
	}
	t.Cleanup(func() {
		a.pc.Close()
		b.pc.Close()
		for i := 0; i < 2; i++ {
			select {
			case <-writes:
			case <-time.After(time.Second):
				t.Error("rejected DTLS sender did not release")
			}
		}
	})
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("DTLS did not explicitly reject the mismatched fingerprint: a=%s b=%s b_ice=%s b_peer=%s", a.pc.SCTP().Transport().State(), b.pc.SCTP().Transport().State(), b.pc.ICEConnectionState(), b.pc.ConnectionState())
		case <-tick.C:
			if !b.fingerprintRejected.Load() {
				continue
			}
			if !b.iceEstablished.Load() || len(b.pc.SCTP().Transport().GetRemoteCertificate()) == 0 {
				t.Fatal("negative DTLS test never established ICE")
			}
			if a.received.Load() != 0 || b.received.Load() != 0 {
				t.Fatal("media crossed a mismatched DTLS identity")
			}
			return
		}
	}
}
