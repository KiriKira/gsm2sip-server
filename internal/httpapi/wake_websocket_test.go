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
