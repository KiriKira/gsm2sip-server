package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dialWake(t *testing.T, serverURL, token string) *websocket.Conn {
	t.Helper()
	conn, response, err := dialWakeWithHeaders(t, serverURL, token, nil, "")
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatal("dial wake websocket:", err)
	}
	return conn
}

func dialWakeWithHeaders(t *testing.T, serverURL, token string, headers http.Header, rawQuery string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	websocketURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/ws" + rawQuery
	requestHeaders := make(http.Header)
	for key, values := range headers {
		requestHeaders[key] = append([]string(nil), values...)
	}
	if token != "" {
		requestHeaders.Set("Authorization", "Bearer "+token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).DialContext(ctx, websocketURL, requestHeaders)
}

func requireWakeHint(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	messageType, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal("read wake hint:", err)
	}
	if messageType != websocket.TextMessage || string(message) != `{"protocol_version":1,"type":"sync_required"}` {
		t.Fatalf("unexpected wake frame type=%d body=%q", messageType, message)
	}
}

func requireNoWake(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(1300 * time.Millisecond))
	_, _, err := conn.ReadMessage()
	var networkErr net.Error
	if !errors.As(err, &networkErr) || !networkErr.Timeout() {
		t.Fatalf("expected no wake frame, got err=%v", err)
	}
}

func requireCloseCode(t *testing.T, conn *websocket.Conn, code int) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != code {
		t.Fatalf("expected WebSocket close %d, got %v", code, err)
	}
}

func createWakeTestMessage(t *testing.T, handler http.Handler, token, gatewayID, simID, idempotencyKey string) {
	t.Helper()
	response := requestJSON(t, handler, http.MethodPost, "/v1/messages", token,
		CreateMessageRequest{GatewayID: gatewayID, SIMID: simID, MappingRevision: 1,
			To: "+8613800000000", Text: "wake test", TTLSeconds: 600},
		idempotencyKey)
	requireStatus(t, response, http.StatusAccepted)
}

func TestPostgresWakeWebSocketOwnerScopedChangesAndReauthentication(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	api := New(database.db, nil)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		api.ShutdownWebSockets()
		server.Close()
	})

	clientA := dialWake(t, server.URL, f.clientToken)
	gatewayA := dialWake(t, server.URL, f.gatewayToken)
	requireWakeHint(t, clientA)
	requireWakeHint(t, gatewayA)

	// A different owner's durable event and command must not wake either device.
	createWakeTestMessage(t, api.Handler(), f.otherToken, f.otherGatewayID, f.otherSIMID, "wake-test-other-owner-0001")
	requireNoWake(t, clientA)
	requireNoWake(t, gatewayA)
	_ = clientA.Close()
	_ = gatewayA.Close()

	// A command for another gateway of the same owner remains device-scoped.
	siblingGatewayID, siblingSIMID := newUUID(), newUUID()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'gateway','gateway-a-sibling')`, []any{siblingGatewayID, f.ownerID}},
		{`INSERT INTO gateways(device_id,mapping_revision,last_seen_at) VALUES ($1,1,now())`, []any{siblingGatewayID}},
		{`INSERT INTO sim_bindings(sim_id,owner_id,gateway_id,slot_index,label,state,identity_verified,mapping_revision,service_state) VALUES ($1,$2,$3,0,'Line A sibling','active',true,1,'in_service')`, []any{siblingSIMID, f.ownerID, siblingGatewayID}},
	} {
		if _, err := database.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal("seed sibling gateway:", err)
		}
	}
	gatewayA = dialWake(t, server.URL, f.gatewayToken)
	requireWakeHint(t, gatewayA)
	createWakeTestMessage(t, api.Handler(), f.clientToken, siblingGatewayID, siblingSIMID, "wake-test-sibling-gw-0001")
	requireNoWake(t, gatewayA)
	_ = gatewayA.Close()

	clientA = dialWake(t, server.URL, f.clientToken)
	gatewayA = dialWake(t, server.URL, f.gatewayToken)
	requireWakeHint(t, clientA)
	requireWakeHint(t, gatewayA)
	createWakeTestMessage(t, api.Handler(), f.clientToken, f.gatewayID, f.simID, "wake-test-own-gateway-0001")
	requireWakeHint(t, clientA)
	requireWakeHint(t, gatewayA)
	_ = clientA.Close()
	_ = gatewayA.Close()

	// The server reauthenticates the same access token and closes a revoked session.
	clientA = dialWake(t, server.URL, f.clientToken)
	requireWakeHint(t, clientA)
	if _, err := database.db.Exec(`DELETE FROM sessions WHERE device_id=$1`, f.clientID); err != nil {
		t.Fatal("revoke integration session:", err)
	}
	requireCloseCode(t, clientA, websocket.ClosePolicyViolation)
	_ = clientA.Close()
}

func TestPostgresWakeWebSocketCallEventsArePerClientAndSMSEventsAreShared(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	clientBID, clientBToken := newUUID(), randomToken()
	if _, err := database.db.Exec(`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'client','client-a-sibling')`, clientBID, f.ownerID); err != nil {
		t.Fatal("seed sibling client")
	}
	if _, err := database.db.Exec(`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at)
		VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`,
		newUUID(), clientBID, tokenHash(clientBToken), tokenHash(randomToken())); err != nil {
		t.Fatal("seed sibling client session")
	}
	api := New(database.db, nil)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		api.ShutdownWebSockets()
		server.Close()
	})

	var connections []*websocket.Conn
	dialPair := func() (*websocket.Conn, *websocket.Conn) {
		t.Helper()
		clientA := dialWake(t, server.URL, f.clientToken)
		clientB := dialWake(t, server.URL, clientBToken)
		connections = append(connections, clientA, clientB)
		requireWakeHint(t, clientA)
		requireWakeHint(t, clientB)
		return clientA, clientB
	}
	closePair := func(clientA, clientB *websocket.Conn) {
		t.Helper()
		_ = clientA.Close()
		_ = clientB.Close()
	}
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	clientA, clientB := dialPair()

	callID := newUUID()
	if _, err := database.db.Exec(`INSERT INTO call_sessions(
		call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,
		expires_at,wake_nonce,gateway_endpoint_id,client_endpoint_id)
		VALUES ($1,$2,$3,$4,$5,1,'incoming','+12025550123','pending_wakeup',now()+interval '30 seconds',
		'call-wake-nonce','gateway-endpoint','client-endpoint')`,
		callID, f.ownerID, f.clientID, f.gatewayID, f.simID); err != nil {
		t.Fatal("seed incoming call for participant wake test")
	}
	for _, clientID := range []string{f.clientID, clientBID} {
		if _, err := database.db.Exec(`INSERT INTO call_participants(call_id,owner_id,client_device_id,endpoint_id,wake_nonce,state)
			VALUES ($1,$2,$3,$4,$5,'candidate')`, callID, f.ownerID, clientID, "endpoint-"+clientID, "nonce-"+clientID); err != nil {
			t.Fatal("seed incoming call participant")
		}
	}
	appendCallEvent := func(clientID, eventType string) {
		t.Helper()
		if _, err := database.db.Exec(`UPDATE call_participants SET state='pending_wakeup',state_revision=state_revision+1
			WHERE call_id=$1 AND client_device_id=$2`, callID, clientID); err != nil {
			t.Fatal("advance participant state")
		}
		var revision int64
		if err := database.db.QueryRow(`SELECT state_revision FROM call_participants WHERE call_id=$1 AND client_device_id=$2`, callID, clientID).Scan(&revision); err != nil {
			t.Fatal("read participant revision")
		}
		if _, err := database.db.Exec(`INSERT INTO call_events(call_id,owner_id,client_device_id,state_revision,event_type,event_json)
			VALUES ($1,$2,$3,$4,$5,'{}'::jsonb)`, callID, f.ownerID, clientID, revision, eventType); err != nil {
			t.Fatal("append participant call event")
		}
	}

	appendCallEvent(clientBID, "call.pending_wakeup")
	requireWakeHint(t, clientB)
	requireNoWake(t, clientA)
	closePair(clientA, clientB)

	clientA, clientB = dialPair()
	appendCallEvent(f.clientID, "call.pending_wakeup")
	requireWakeHint(t, clientA)
	requireNoWake(t, clientB)
	closePair(clientA, clientB)

	// SMS remains an owner-level feed, so either host's new SMS wakes both.
	clientA, clientB = dialPair()
	createWakeTestMessage(t, api.Handler(), f.clientToken, f.gatewayID, f.simID, "wake-sms-shared-between-hosts-01")
	requireWakeHint(t, clientA)
	requireWakeHint(t, clientB)
}

func TestPostgresWakeWebSocketAuthenticationOriginBudgetAndFrames(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	api := New(database.db, nil)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		api.ShutdownWebSockets()
		server.Close()
	})

	conn, response, err := dialWakeWithHeaders(t, server.URL, "", nil, "")
	if err == nil {
		_ = conn.Close()
		t.Fatal("unauthenticated websocket upgrade unexpectedly succeeded")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upgrade status = %v, want 401", response)
	}
	_ = response.Body.Close()

	conn, response, err = dialWakeWithHeaders(t, server.URL, f.clientToken, nil, "?access_token=must-not-be-accepted")
	if err == nil {
		_ = conn.Close()
		t.Fatal("query token unexpectedly upgraded")
	}
	if response == nil || response.StatusCode != http.StatusBadRequest || strings.Contains(response.Header.Get("X-Request-ID"), f.clientToken) {
		t.Fatalf("query token was not rejected safely: response=%v", response)
	}
	_ = response.Body.Close()

	origin := make(http.Header)
	origin.Set("Origin", "https://foreign.invalid")
	conn, response, err = dialWakeWithHeaders(t, server.URL, f.clientToken, origin, "")
	if err == nil {
		_ = conn.Close()
		t.Fatal("cross-origin websocket unexpectedly upgraded")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %v, want 403", response)
	}
	_ = response.Body.Close()

	first := dialWake(t, server.URL, f.clientToken)
	second := dialWake(t, server.URL, f.clientToken)
	requireWakeHint(t, first)
	requireWakeHint(t, second)
	conn, response, err = dialWakeWithHeaders(t, server.URL, f.clientToken, nil, "")
	if err == nil {
		_ = conn.Close()
		t.Fatal("third connection for one device unexpectedly upgraded")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("connection budget status = %v, want 429", response)
	}
	_ = response.Body.Close()
	_ = first.Close()
	_ = second.Close()
	waitForWakeBudgetRelease(t, api, f.clientID)

	messageConn := dialWake(t, server.URL, f.clientToken)
	requireWakeHint(t, messageConn)
	if err := messageConn.WriteMessage(websocket.TextMessage, []byte(`{"action":"anything"}`)); err != nil {
		t.Fatal("write client frame:", err)
	}
	requireCloseCode(t, messageConn, websocket.ClosePolicyViolation)
	_ = messageConn.Close()

	largeConn := dialWake(t, server.URL, f.clientToken)
	requireWakeHint(t, largeConn)
	if err := largeConn.WriteMessage(websocket.BinaryMessage, make([]byte, wakeReadLimit+1)); err != nil {
		t.Fatal("write oversized client frame:", err)
	}
	requireCloseCode(t, largeConn, websocket.CloseMessageTooBig)
	_ = largeConn.Close()
}

func TestWakeWebSocketShutdownClosesHijackedConnectionAndReleasesBudget(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	api := New(database.db, nil)
	server := httptest.NewServer(api.Handler())
	conn := dialWake(t, server.URL, f.clientToken)
	requireWakeHint(t, conn)

	api.ShutdownWebSockets()
	requireCloseCode(t, conn, websocket.CloseGoingAway)
	_ = conn.Close()
	server.Close()
	waitForWakeBudgetRelease(t, api, f.clientID)
}

func waitForWakeBudgetRelease(t *testing.T, api *Server, deviceID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		api.wsMu.Lock()
		count := api.wsByDevice[deviceID]
		connections := len(api.wsConnections)
		api.wsMu.Unlock()
		if count == 0 && connections == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("websocket resources did not release for device %s", deviceID)
}
