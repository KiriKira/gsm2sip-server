package httpapi

import (
	"context"
	"encoding/json"
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
	claim := func(role string, status int) PairingClaimResponse {
		t.Helper()
		response := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
			PairingClaimRequest{PairingCode: code, DeviceName: "join test", Role: role}, "")
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
	ready bool
	err   error
	sent  []map[string]string
	valid int
}

func (f *fakePush) Ready() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.ready }

func (f *fakePush) Send(_ context.Context, _ string, data map[string]string, validateOnly bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if validateOnly {
		f.valid++
	} else {
		f.sent = append(f.sent, data)
	}
	return nil
}

func (f *fakePush) sentCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

func TestPostgresPushWakesOnNewEventsOnlyWhereItWorks(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)

	// Without push, the node neither advertises it nor stores a token.
	plain := New(database.db, nil).Handler()
	requireStatus(t, requestJSON(t, plain, http.MethodPut, "/v1/devices/self/push-token", f.clientToken,
		map[string]string{"token": "fcm-token"}, ""), http.StatusServiceUnavailable)
	capabilities := requestJSON(t, plain, http.MethodGet, "/v1/capabilities", "", nil, "")
	if !json.Valid(capabilities.Body.Bytes()) || !containsJSON(capabilities.Body.Bytes(), `"push":false`) {
		t.Fatalf("capabilities without push: %s", capabilities.Body.String())
	}

	fake := &fakePush{ready: true}
	server := NewWithOptions(database.db, nil, Options{Push: fake})
	handler := server.Handler()
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-token", f.gatewayToken,
		map[string]string{"token": "fcm-token"}, ""), http.StatusForbidden)

	// FCM refusing the token (another project, or unknown) stores nothing.
	fake.err = push.ErrTokenGone
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-token", f.clientToken,
		map[string]string{"token": "fcm-token"}, ""), http.StatusUnprocessableEntity)
	fake.err = nil

	// Registration starts from the current events: no push for history.
	createMessage := func(text, key string) {
		t.Helper()
		requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/messages", f.clientToken,
			CreateMessageRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1,
				To: "+12025550123", Text: text, TTLSeconds: 300}, key), http.StatusAccepted)
	}
	createMessage("before registration", newUUID())
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-token", f.clientToken,
		map[string]string{"token": "fcm-token"}, ""), http.StatusOK)
	if err := server.pushPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.sentCount() != 0 {
		t.Fatalf("registration announced old events: %v", fake.sent)
	}

	// Two events before the next round are announced once, as SMS.
	createMessage("one", newUUID())
	createMessage("two", newUUID())
	if err := server.pushPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.pushPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.sentCount() != 1 || fake.sent[0]["kind"] != "sms" || fake.sent[0]["device_id"] != f.clientID {
		t.Fatalf("pushes: %v", fake.sent)
	}
	for key := range fake.sent[0] {
		if key != "type" && key != "v" && key != "kind" && key != "device_id" {
			t.Fatalf("push carries %q", key)
		}
	}

	// A token FCM forgot is dropped.
	createMessage("three", newUUID())
	fake.err = push.ErrTokenGone
	if err := server.pushPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	var tokens int
	if err := database.db.QueryRow(`SELECT count(*) FROM push_tokens`).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("tokens after UNREGISTERED: %d %v", tokens, err)
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
	handler := NewWithOptions(database.db, nil, Options{Push: &fakePush{ready: true}}).Handler()
	response := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: code, DeviceName: "main phone", Role: "client"}, "")
	requireStatus(t, response, http.StatusOK)
	var client PairingClaimResponse
	if err := json.Unmarshal(response.Body.Bytes(), &client); err != nil {
		t.Fatal("decode pairing")
	}
	requireStatus(t, requestJSON(t, handler, http.MethodPut, "/v1/devices/self/push-token", client.AccessToken,
		map[string]string{"token": "fcm-token"}, ""), http.StatusOK)
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
		RefreshRequest{RefreshToken: client.RefreshToken}, ""), http.StatusNoContent)
	var tokens int
	if err := database.db.QueryRow(`SELECT count(*) FROM push_tokens WHERE device_id=$1`, client.DeviceID).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("tokens after revoke: %d %v", tokens, err)
	}
}
