package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kirikira/gsm2sip-server/internal/push"
)

func TestPostgresJoinCodePairsEachRoleOnce(t *testing.T) {
	database := openIntegrationDatabase(t)
	ownerID := newUUID()
	if _, err := database.db.Exec(`INSERT INTO owners(id,display_name) VALUES ($1,'join-owner')`, ownerID); err != nil {
		t.Fatal("seed owner")
	}
	code := randomToken()
	if _, err := database.db.Exec(`INSERT INTO join_codes(id,owner_id,code_hash,expires_at) VALUES ($1,$2,$3,now()+interval '10 minutes')`,
		newUUID(), ownerID, tokenHash(code)); err != nil {
		t.Fatal("seed join code")
	}
	handler := New(database.db, nil).Handler()
	name := "join test"
	claim := func(role string, status int) PairingClaimResponse {
		t.Helper()
		response := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
			PairingClaimRequest{PairingCode: code, DeviceName: name, Role: role}, "")
		requireStatus(t, response, status)
		var result PairingClaimResponse
		if status == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal("decode pairing")
			}
		}
		return result
	}

	claim("", http.StatusBadRequest)
	// Names are limited in characters, not bytes; a bad field is named.
	name = strings.Repeat("网", 101)
	response := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: code, DeviceName: name, Role: "gateway"}, "")
	requireStatus(t, response, http.StatusBadRequest)
	if !strings.Contains(response.Body.String(), "Device name") {
		t.Fatalf("unnamed field: %s", response.Body.String())
	}
	name = strings.Repeat("网", 100)
	gateway := claim("gateway", http.StatusOK)
	if gateway.Role != "gateway" || gateway.OwnerID != ownerID {
		t.Fatalf("gateway pairing: %+v", gateway)
	}
	claim("gateway", http.StatusUnauthorized)
	client := claim("client", http.StatusOK)
	if client.Role != "client" || client.OwnerID != ownerID {
		t.Fatalf("client pairing: %+v", client)
	}
	claim("client", http.StatusUnauthorized)

	expired := randomToken()
	if _, err := database.db.Exec(`INSERT INTO join_codes(id,owner_id,code_hash,expires_at) VALUES ($1,$2,$3,now()-interval '1 second')`,
		newUUID(), ownerID, tokenHash(expired)); err != nil {
		t.Fatal("seed expired join code")
	}
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: expired, DeviceName: "late", Role: "client"}, ""), http.StatusUnauthorized)
}

func TestPostgresPairingCodeRejectsTheOtherRoleWithoutConsumingIt(t *testing.T) {
	database := openIntegrationDatabase(t)
	ownerID := newUUID()
	if _, err := database.db.Exec(`INSERT INTO owners(id,display_name) VALUES ($1,'role-owner')`, ownerID); err != nil {
		t.Fatal("seed owner")
	}
	code := randomToken()
	if _, err := database.db.Exec(`INSERT INTO pairing_codes(id,owner_id,role,code_hash,expires_at) VALUES ($1,$2,'gateway',$3,now()+interval '10 minutes')`,
		newUUID(), ownerID, tokenHash(code)); err != nil {
		t.Fatal("seed pairing code")
	}
	handler := New(database.db, nil).Handler()
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: code, DeviceName: "main phone", Role: "client"}, ""), http.StatusBadRequest)
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: code, DeviceName: "old phone", Role: "gateway"}, ""), http.StatusOK)
}

type fakePush struct {
	mu    sync.Mutex
	err   error
	sent  []string
	check int
}

func (f *fakePush) Validate(endpoint string) error {
	if !strings.HasPrefix(endpoint, "https://") {
		return push.ErrInvalidEndpoint
	}
	return nil
}

func (f *fakePush) Send(_ context.Context, _, kind, deviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if kind == "check" {
		f.check++
	} else {
		f.sent = append(f.sent, kind+":"+deviceID)
	}
	return nil
}

func (f *fakePush) sentCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

func TestPostgresPushWakesOnNewEventsAtTheRegisteredEndpoint(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	const endpoint = "https://relay.example/v1/w/sealed"

	// Without push, the node neither advertises it nor stores an endpoint.
	plain := New(database.db, nil).Handler()
	requireStatus(t, requestJSON(t, plain, http.MethodPut, "/v1/devices/self/push-endpoint", f.clientToken,
		map[string]string{"endpoint": endpoint}, ""), http.StatusServiceUnavailable)
	capabilities := requestJSON(t, plain, http.MethodGet, "/v1/capabilities", "", nil, "")
	if !containsJSON(capabilities.Body.Bytes(), `"push":false`) {
		t.Fatalf("capabilities without push: %s", capabilities.Body.String())
	}

	fake := &fakePush{}
	server := NewWithOptions(database.db, nil, Options{Push: fake})
	handler := server.Handler()
	if !containsJSON(requestJSON(t, handler, http.MethodGet, "/v1/capabilities", "", nil, "").Body.Bytes(), `"push":true`) {
		t.Fatal("push not advertised")
	}
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-endpoint", f.gatewayToken,
		map[string]string{"endpoint": endpoint}, ""), http.StatusForbidden)
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-endpoint", f.clientToken,
		map[string]string{"endpoint": "http://10.0.0.1/internal"}, ""), http.StatusBadRequest)

	// An endpoint that does not reach the phone is not stored.
	fake.err = push.ErrEndpointGone
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-endpoint", f.clientToken,
		map[string]string{"endpoint": endpoint}, ""), http.StatusUnprocessableEntity)
	fake.err = nil

	// Registration starts from the current events: no wake-up for history.
	createMessage := func(text string) {
		t.Helper()
		requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/messages", f.clientToken,
			CreateMessageRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1,
				To: "+12025550123", Text: text, TTLSeconds: 300}, newUUID()), http.StatusAccepted)
	}
	createMessage("before registration")
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-endpoint", f.clientToken,
		map[string]string{"endpoint": endpoint}, ""), http.StatusOK)
	if fake.check != 1 {
		t.Fatalf("checks: %d", fake.check)
	}
	if err := server.pushPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.sentCount() != 0 {
		t.Fatalf("registration announced old events: %v", fake.sent)
	}

	// Two events before the next round are announced once, as SMS.
	createMessage("one")
	createMessage("two")
	for i := 0; i < 2; i++ {
		if err := server.pushPending(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if fake.sentCount() != 1 || fake.sent[0] != "sms:"+f.clientID {
		t.Fatalf("wake-ups: %v", fake.sent)
	}

	// An endpoint that is gone is dropped.
	createMessage("three")
	fake.err = push.ErrEndpointGone
	if err := server.pushPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	var endpoints int
	if err := database.db.QueryRow(`SELECT count(*) FROM push_endpoints`).Scan(&endpoints); err != nil || endpoints != 0 {
		t.Fatalf("endpoints after 410: %d %v", endpoints, err)
	}
}

func containsJSON(body []byte, fragment string) bool {
	return json.Valid(body) && strings.Contains(string(body), fragment)
}

func TestPostgresLeavingANodeStopsItsWakeUps(t *testing.T) {
	database := openIntegrationDatabase(t)
	ownerID := newUUID()
	if _, err := database.db.Exec(`INSERT INTO owners(id,display_name) VALUES ($1,'leave-owner')`, ownerID); err != nil {
		t.Fatal("seed owner")
	}
	code := randomToken()
	if _, err := database.db.Exec(`INSERT INTO join_codes(id,owner_id,code_hash,expires_at) VALUES ($1,$2,$3,now()+interval '10 minutes')`,
		newUUID(), ownerID, tokenHash(code)); err != nil {
		t.Fatal("seed join code")
	}
	handler := NewWithOptions(database.db, nil, Options{Push: &fakePush{}}).Handler()
	response := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: code, DeviceName: "main phone", Role: "client"}, "")
	requireStatus(t, response, http.StatusOK)
	var client PairingClaimResponse
	if err := json.Unmarshal(response.Body.Bytes(), &client); err != nil {
		t.Fatal("decode pairing")
	}
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-endpoint", client.AccessToken,
		map[string]string{"endpoint": "https://relay.example/v1/w/sealed"}, ""), http.StatusOK)
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
		RefreshRequest{RefreshToken: client.RefreshToken}, ""), http.StatusNoContent)
	var tokens int
	if err := database.db.QueryRow(`SELECT count(*) FROM push_endpoints WHERE device_id=$1`, client.DeviceID).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("tokens after revoke: %d %v", tokens, err)
	}
}

func TestPostgresPushTestAndStatus(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	fake := &fakePush{}
	handler := NewWithOptions(database.db, nil, Options{Push: fake}).Handler()
	status := func() map[string]any {
		t.Helper()
		reply := requestJSON(t, handler, http.MethodGet, "/v1/devices/self/push-endpoint", f.clientToken, nil, "")
		requireStatus(t, reply, http.StatusOK)
		var out map[string]any
		if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	test := func(code int) map[string]any {
		t.Helper()
		reply := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/push-endpoint/test", f.clientToken, nil, "")
		requireStatus(t, reply, code)
		var out map[string]any
		_ = json.Unmarshal(reply.Body.Bytes(), &out)
		return out
	}

	if status()["registered"] != false {
		t.Fatal("status before registration")
	}
	test(http.StatusNotFound)
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/devices/self/push-endpoint/test", f.gatewayToken, nil, ""), http.StatusForbidden)
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-endpoint", f.clientToken,
		map[string]string{"endpoint": "https://relay.example/v1/w/sealed"}, ""), http.StatusOK)
	if got := status(); got["registered"] != true || got["failures"] != float64(0) || got["delivered_at"] == nil {
		t.Fatalf("status after registration: %v", got)
	}

	// A test is a delivered, silent sms hint, at most once a minute.
	if got := test(http.StatusOK); got["sent"] != true || fake.sentCount() != 1 || fake.sent[0] != "sms:"+f.clientID {
		t.Fatalf("test push: %v %v", got, fake.sent)
	}
	test(http.StatusTooManyRequests)

	// A relay that cannot be reached counts as a failure the phone can show.
	if _, err := database.db.Exec(`UPDATE push_endpoints SET tested_at=now()-interval '2 minutes', delivered_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	fake.err = errors.New("relay down")
	if got := test(http.StatusOK); got["sent"] != false {
		t.Fatalf("failed test: %v", got)
	}
	if got := status(); got["failures"] != float64(1) {
		t.Fatalf("status after a failed test: %v", got)
	}
}

func TestPostgresClientListShowsWhichPairingsStillSignIn(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	handler := New(database.db, nil).Handler()
	var owner string
	if err := database.db.QueryRow(`SELECT owner_id::text FROM devices WHERE id=$1`, f.clientID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	// An earlier pairing of the same phone whose sessions were revoked.
	if _, err := database.db.Exec(`INSERT INTO devices(id,owner_id,role,name) VALUES($1,$2,'client','same phone')`, newUUID(), owner); err != nil {
		t.Fatal(err)
	}
	reply := requestJSON(t, handler, http.MethodGet, "/v1/clients", f.clientToken, nil, "")
	requireStatus(t, reply, http.StatusOK)
	var out struct {
		Items []ClientSummary `json:"items"`
	}
	if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	live := map[bool]int{}
	for _, item := range out.Items {
		live[item.Live]++
		if item.IsSelf && (!item.Live || item.LastActiveAt == nil) {
			t.Fatalf("self: %+v", item)
		}
		if !item.Live && item.LastActiveAt != nil {
			t.Fatalf("a device that never signed in has no activity: %+v", item)
		}
	}
	if live[true] != 1 || live[false] != 1 {
		t.Fatalf("clients: %+v", out.Items)
	}
}
