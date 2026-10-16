package rtcsignal

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type socketFixture struct {
	server     *httptest.Server
	handler    *WebSocketHandler
	dialer     websocket.Dialer
	repository *testRepository
	revoked    atomic.Bool
}

func sockets(t *testing.T, perDevice int) *socketFixture {
	t.Helper()
	f := &socketFixture{repository: &testRepository{leg: newLeg(t)}}
	service, _ := NewService(f.repository, func() time.Time { return epoch })
	handler, err := NewWebSocketHandler(WebSocketOptions{Service: service, MaxConnections: 3, MaxConnectionsPerDevice: perDevice, AuthCheckInterval: 20 * time.Millisecond,
		Authenticate: func(r *http.Request) (Principal, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer client-test-session":
				if !f.revoked.Load() {
					return client, nil
				}
			case "Bearer gateway-test-session":
				return gateway, nil
			case "Bearer foreign-test-session":
				return Principal{"other-owner", "client"}, nil
			}
			return Principal{}, ErrForbidden
		}})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = handler
	mux := http.NewServeMux()
	mux.Handle("GET /rtc/v1/legs/{leg_id}/signals", handler)
	mux.HandleFunc("GET /control-health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	f.server = httptest.NewUnstartedServer(mux)
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.StartTLS()
	roots := x509.NewCertPool()
	roots.AddCert(f.server.Certificate())
	f.dialer = websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, HandshakeTimeout: time.Second}
	t.Cleanup(func() { handler.Close(); f.server.Close() })
	return f
}

func (f *socketFixture) dial(token string, origin string) (*websocket.Conn, *http.Response, error) {
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return f.dialer.Dial("wss"+strings.TrimPrefix(f.server.URL, "https")+"/rtc/v1/legs/leg/signals", header)
}

func readSocket(t *testing.T, connection *websocket.Conn, kind string) signalMessage {
	t.Helper()
	connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var value signalMessage
		if err := connection.ReadJSON(&value); err != nil {
			t.Fatal("read RTC frame", err)
		}
		if value.Type == kind {
			return value
		}
	}
}

func TestWSSSignalingRelaysOnlyCommittedAuthorizedSignalsAndCancellation(t *testing.T) {
	f := sockets(t, 2)
	c, _, err := f.dial("client-test-session", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	g, _, err := f.dial("gateway-test-session", "")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if state := readSocket(t, c, "snapshot").State; state == nil || state.Snapshot.Phase != WaitingOffer || !state.ReplayComplete {
		t.Fatal("missing initial authoritative snapshot")
	}
	readSocket(t, g, "snapshot")
	request := command(newLeg(t), Offer, "offer")
	if err := c.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	ack := readSocket(t, c, "ack")
	if ack.Ack == nil || ack.Ack.Revision != 2 || ack.Duplicate {
		t.Fatal("invalid committed ACK")
	}
	for {
		state := readSocket(t, g, "snapshot").State
		if state.Snapshot.Phase != WaitingAnswer {
			continue
		}
		if len(state.Signals) != 1 || state.Signals[0].Kind != Offer || state.Signals[0].RecipientID != "gateway" {
			t.Fatal("offer delivered outside recipient scope")
		}
		break
	}
	if err := c.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	if ack := readSocket(t, c, "ack"); !ack.Duplicate || ack.Ack.Revision != 2 {
		t.Fatal("retry did not retain historical ACK")
	}
	close := command(newLeg(t), Close, "close")
	if err := c.WriteJSON(close); err != nil {
		t.Fatal(err)
	}
	readSocket(t, c, "ack")
	for {
		state := readSocket(t, g, "snapshot").State
		if state.Snapshot.Phase != Closed {
			continue
		}
		if len(state.Signals) != 0 {
			t.Fatal("terminal snapshot retained SDP")
		}
		break
	}
}

func TestWSSRejectsUnauthenticatedForeignAndCrossOriginUpgrades(t *testing.T) {
	f := sockets(t, 2)
	for _, test := range []struct {
		token, origin string
		status        int
	}{{"", "", 401}, {"foreign-test-session", "", 403}, {"client-test-session", "https://another.example", 403}} {
		connection, response, err := f.dial(test.token, test.origin)
		if connection != nil {
			connection.Close()
		}
		if response != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != test.status {
			t.Fatalf("unexpected upgrade admission: status=%v err=%v", response, err)
		}
	}
	connection, response, err := websocket.DefaultDialer.Dial("wss"+strings.TrimPrefix(f.server.URL, "https")+"/rtc/v1/legs/leg/signals", http.Header{"Authorization": []string{"Bearer client-test-session"}})
	if connection != nil {
		connection.Close()
	}
	if response != nil {
		response.Body.Close()
	}
	var certificate *tls.CertificateVerificationError
	if !errors.As(err, &certificate) {
		t.Fatal("WSS accepted an untrusted certificate", err)
	}
}

func TestWSSConnectionBudgetAndRevocationLeaveControlServiceAvailable(t *testing.T) {
	f := sockets(t, 1)
	c, _, err := f.dial("client-test-session", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readSocket(t, c, "snapshot")
	extra, response, err := f.dial("client-test-session", "")
	if extra != nil {
		extra.Close()
	}
	if response != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatal("voice connection budget exceeded")
	}
	g, _, err := f.dial("gateway-test-session", "")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	readSocket(t, g, "snapshot")
	f.revoked.Store(true)
	c.SetReadDeadline(time.Now().Add(time.Second))
	for {
		_, _, err = c.ReadMessage()
		if err == nil {
			continue
		}
		var closed *websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != websocket.ClosePolicyViolation {
			t.Fatal("revoked session did not close its RTC socket", err)
		}
		break
	}
	if err := g.WriteJSON(command(newLeg(t), Candidate, "gateway-ice")); err != nil {
		t.Fatal(err)
	}
	readSocket(t, g, "ack")
	f.handler.Close()
	response, err = f.server.Client().Get(f.server.URL + "/control-health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("closing RTC transport stopped control HTTP")
	}
}

func TestWSSRejectsForgedFieldsWithoutApplyingTheOperation(t *testing.T) {
	f := sockets(t, 2)
	c, _, err := f.dial("client-test-session", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readSocket(t, c, "snapshot")
	data, _ := json.Marshal(command(newLeg(t), Offer, "offer"))
	forged := strings.Replace(string(data), `"version":1`, `"version":1,"sender_id":"gateway"`, 1)
	if err := c.WriteMessage(websocket.TextMessage, []byte(forged)); err != nil {
		t.Fatal(err)
	}
	if result := readSocket(t, c, "error"); result.Code != "INVALID_SIGNAL" {
		t.Fatal("forged sender accepted")
	}
	if err := c.WriteJSON(command(newLeg(t), Offer, "offer")); err != nil {
		t.Fatal(err)
	}
	if ack := readSocket(t, c, "ack"); ack.Ack.Revision != 2 {
		t.Fatal("invalid frame consumed revision")
	}
}

func TestVoiceOutputQueueOverflowFailsClosedWithoutBlocking(t *testing.T) {
	handler := &WebSocketHandler{connections: map[*signalConnection]struct{}{}}
	c := &signalConnection{owner: handler, queue: make(chan any, 1), done: make(chan struct{})}
	c.send(signalMessage{Type: "snapshot"})
	c.send(signalMessage{Type: "ack"})
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("saturated voice queue blocked instead of closing")
	}
	if handler.closing {
		t.Fatal("single slow consumer shut down the voice handler")
	}
}

func TestWSSDoesNotAcknowledgeFailedStorageOrConsumeTheOperation(t *testing.T) {
	f := sockets(t, 2)
	f.handler.options.AuthCheckInterval = 2 * time.Second
	c, _, err := f.dial("client-test-session", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readSocket(t, c, "snapshot")
	f.repository.mu.Lock()
	f.repository.commitError = errors.New("test durable store unavailable")
	f.repository.mu.Unlock()
	request := command(newLeg(t), Offer, "offer")
	if err := c.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	if result := readSocket(t, c, "error"); result.Code != "SIGNAL_UNAVAILABLE" {
		t.Fatal("failed storage was acknowledged")
	}
	f.repository.mu.Lock()
	f.repository.commitError = nil
	f.repository.mu.Unlock()
	if err := c.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	if result := readSocket(t, c, "ack"); result.Duplicate || result.Ack.Revision != 2 {
		t.Fatal("failed write consumed the operation")
	}
}

func TestWSSRejectsBinaryAndOversizedFrames(t *testing.T) {
	for _, test := range []struct {
		name string
		kind int
		data []byte
		code int
	}{
		{"binary", websocket.BinaryMessage, []byte("not-json"), websocket.CloseUnsupportedData},
		{"oversized", websocket.TextMessage, []byte(strings.Repeat("x", MaxFrameBytes+1)), websocket.CloseMessageTooBig},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := sockets(t, 2)
			c, _, err := f.dial("client-test-session", "")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			readSocket(t, c, "snapshot")
			if err := c.WriteMessage(test.kind, test.data); err != nil {
				t.Fatal(err)
			}
			c.SetReadDeadline(time.Now().Add(time.Second))
			for {
				_, _, err := c.ReadMessage()
				if err == nil {
					continue
				}
				var closed *websocket.CloseError
				if !errors.As(err, &closed) || closed.Code != test.code {
					t.Fatal("invalid frame was not closed with the expected reason", err)
				}
				break
			}
		})
	}
}

func TestWSSAdmissionBudgetIncludesPendingAuthentication(t *testing.T) {
	f := sockets(t, 2)
	f.handler.options.MaxConnections = 1
	started, release := make(chan struct{}, 1), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.handler.options.Authenticate = func(r *http.Request) (Principal, error) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return client, nil
		case <-r.Context().Done():
			return Principal{}, r.Context().Err()
		}
	}
	first := make(chan int, 1)
	go func() {
		c, response, _ := f.dial("client-test-session", "")
		if c != nil {
			c.Close()
		}
		status := 0
		if response != nil {
			status = response.StatusCode
			response.Body.Close()
		}
		first <- status
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("authentication did not start")
	}
	c, response, err := f.dial("client-test-session", "")
	if c != nil {
		c.Close()
	}
	if response != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != 429 || calls.Load() != 1 {
		t.Fatal("pending voice admissions bypassed the budget")
	}
	response, err = f.server.Client().Get(f.server.URL + "/control-health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("blocked voice authentication blocked other HTTP")
	}
	f.handler.Close()
	unblock()
	select {
	case status := <-first:
		if status != 429 {
			t.Fatal("handler shutdown admitted a pending upgrade")
		}
	case <-time.After(time.Second):
		t.Fatal("pending handshake did not drain")
	}
}
