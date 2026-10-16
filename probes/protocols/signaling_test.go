package protocols

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kirikira/gsm2sip-server/internal/rtcsignal"
	"github.com/pion/webrtc/v4"
)

// Test-only repository/identity provider. The production PostgreSQL repository
// and authorizer boundary have separate integration tests in the root module.
type signalingRepository struct {
	mu  sync.Mutex
	leg rtcsignal.Leg
}

func (r *signalingRepository) Update(ctx context.Context, p rtcsignal.Principal, id string, update func(rtcsignal.Leg) (rtcsignal.Leg, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	binding := r.leg.Snapshot().Binding
	if id != binding.LegID || p.OwnerID != binding.OwnerID || (p.DeviceID != binding.ClientID && p.DeviceID != binding.GatewayID) {
		return rtcsignal.ErrForbidden
	}
	next, err := update(r.leg)
	if err != nil {
		return err
	}
	r.leg = next
	return nil
}

type socketMessage struct {
	Type  string                `json:"type"`
	State *rtcsignal.ReadResult `json:"state"`
	Ack   *rtcsignal.Ack        `json:"ack"`
	Code  string                `json:"code"`
}

func socketRead(t *testing.T, c *websocket.Conn, predicate func(socketMessage) bool) socketMessage {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	for {
		var value socketMessage
		if err := c.ReadJSON(&value); err != nil {
			t.Fatal("WSS signaling read failed")
		}
		if value.Type == "error" {
			t.Fatal("WSS command failed: " + value.Code)
		}
		if predicate(value) {
			return value
		}
	}
}

func TestWSSNegotiationAndICERestartKeepTheSameBusinessCall(t *testing.T) {
	now := time.Now()
	binding := rtcsignal.Binding{NodeID: "local-node", OwnerID: "owner", CallID: "stable-call", LegID: "stable-leg", AttemptID: "stable-attempt", ClientID: "client", GatewayID: "gateway", SIMID: "sim", MappingRevision: 1}
	leg, err := rtcsignal.New(binding, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	service, _ := rtcsignal.NewService(&signalingRepository{leg: leg}, time.Now)
	handler, err := rtcsignal.NewWebSocketHandler(rtcsignal.WebSocketOptions{Service: service, Authenticate: func(r *http.Request) (rtcsignal.Principal, error) {
		switch r.Header.Get("Authorization") {
		case "Bearer local-client-test":
			return rtcsignal.Principal{OwnerID: "owner", DeviceID: "client"}, nil
		case "Bearer local-gateway-test":
			return rtcsignal.Principal{OwnerID: "owner", DeviceID: "gateway"}, nil
		}
		return rtcsignal.Principal{}, errors.New("unauthorized test session")
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /rtc/v1/legs/{leg_id}/signals", handler)
	server := httptest.NewTLSServer(mux)
	t.Cleanup(func() { handler.Close(); server.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	dial := func(token string) *websocket.Conn {
		connection, _, err := dialer.Dial("wss"+strings.TrimPrefix(server.URL, "https")+"/rtc/v1/legs/stable-leg/signals", http.Header{"Authorization": []string{"Bearer " + token}})
		if err != nil {
			t.Fatal("WSS handshake failed")
		}
		t.Cleanup(func() { connection.Close() })
		return connection
	}
	c, g := dial("local-client-test"), dial("local-gateway-test")
	state := socketRead(t, c, func(m socketMessage) bool { return m.Type == "snapshot" }).State.Snapshot
	socketRead(t, g, func(m socketMessage) bool { return m.Type == "snapshot" })
	audioA, audioB := opusPackets(t, 1000), opusPackets(t, 1500)
	a := newEndpoint(t, nil, audioA, audioB)
	b := newEndpoint(t, nil, audioB, audioA)
	operation := 0
	submit := func(connection *websocket.Conn, kind rtcsignal.Kind, sdp string) rtcsignal.Ack {
		operation++
		command := rtcsignal.Command{Version: 1, CallID: binding.CallID, LegID: binding.LegID, AttemptID: binding.AttemptID, OperationID: time.Now().Format("150405000000000") + string(rune('a'+operation)), Generation: state.Generation, Revision: state.Revision, Kind: kind, SDP: sdp}
		if err := connection.WriteJSON(command); err != nil {
			t.Fatal("WSS write failed")
		}
		return *socketRead(t, connection, func(m socketMessage) bool { return m.Type == "ack" }).Ack
	}
	var firstUfrag string
	for generation := 1; generation <= 2; generation++ {
		var offer webrtc.SessionDescription
		if generation == 1 {
			offer = localDescription(t, a, false)
		} else {
			ack := submit(c, rtcsignal.Restart, "")
			state.Generation, state.Revision = ack.Generation, ack.Revision
			gathered := webrtc.GatheringCompletePromise(a.pc)
			description, err := a.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
			if err != nil {
				t.Fatal("create ICE restart offer")
			}
			if err := a.pc.SetLocalDescription(description); err != nil {
				t.Fatal("set ICE restart offer")
			}
			select {
			case <-gathered:
			case <-time.After(8 * time.Second):
				t.Fatal("restart gathering did not finish")
			}
			offer = *a.pc.LocalDescription()
		}
		ufrag := ""
		for _, line := range strings.Split(offer.SDP, "\r\n") {
			if strings.HasPrefix(line, "a=ice-ufrag:") {
				ufrag = line
			}
		}
		if ufrag == "" || generation == 2 && ufrag == firstUfrag {
			t.Fatal("ICE restart reused the old ICE credentials")
		}
		firstUfrag = ufrag
		ack := submit(c, rtcsignal.Offer, offer.SDP)
		state.Revision = ack.Revision
		remoteOffer := socketRead(t, g, func(m socketMessage) bool {
			return m.State != nil && m.State.Snapshot.Generation == generation && m.State.Snapshot.Phase == rtcsignal.WaitingAnswer
		}).State
		if len(remoteOffer.Signals) != 1 || remoteOffer.Signals[0].Kind != rtcsignal.Offer {
			t.Fatal("WSS did not deliver the authorized offer")
		}
		if err := b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: remoteOffer.Signals[0].SDP}); err != nil {
			t.Fatal("apply WSS offer")
		}
		answer := localDescription(t, b, true)
		ack = submit(g, rtcsignal.Answer, answer.SDP)
		state.Revision = ack.Revision
		remoteAnswer := socketRead(t, c, func(m socketMessage) bool {
			return m.State != nil && m.State.Snapshot.Generation == generation && m.State.Snapshot.Phase == rtcsignal.Negotiated
		}).State
		if remoteAnswer.Snapshot.Binding.CallID != binding.CallID || remoteAnswer.Snapshot.Binding.AttemptID != binding.AttemptID || len(remoteAnswer.Signals) != 1 {
			t.Fatal("ICE restart changed business identity or replayed another leg")
		}
		if err := a.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: remoteAnswer.Signals[0].SDP}); err != nil {
			t.Fatal("apply WSS answer")
		}
		a.received.Store(0)
		b.received.Store(0)
		assertMedia(t, a, b, false)
	}
}
