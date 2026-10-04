package calls

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/asterisk"
	"github.com/kirikira/gsm2sip-server/internal/db"
)

type callFixture struct {
	ownerID, clientID, gatewayID, simID string
	clientEndpoint, gatewayEndpoint     string
}

func openCallsDatabase(t *testing.T) *sql.DB {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := db.Open(databaseURL)
	if err != nil {
		t.Fatal("open integration database")
	}
	if err := admin.Ping(); err != nil {
		_ = admin.Close()
		t.Fatal("ping integration database")
	}
	schema := "calls_test_" + randomHex(t, 16)
	if _, err := admin.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		_ = admin.Close()
		t.Fatal("create isolated calls schema")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		_, _ = admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`)
		_ = admin.Close()
		t.Fatal("parse integration database URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	isolated, err := db.Open(parsed.String())
	if err != nil {
		_, _ = admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`)
		_ = admin.Close()
		t.Fatal("open isolated calls schema")
	}
	if err := db.Migrate(context.Background(), isolated); err != nil {
		_ = isolated.Close()
		_, _ = admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`)
		_ = admin.Close()
		t.Fatal("migrate isolated calls schema")
	}
	t.Cleanup(func() {
		_ = isolated.Close()
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
		_ = admin.Close()
	})
	return isolated
}

func seedCallFixture(t *testing.T, database *sql.DB) callFixture {
	t.Helper()
	f := callFixture{ownerID: testUUID(t), clientID: testUUID(t), gatewayID: testUUID(t), simID: testUUID(t)}
	f.clientEndpoint = endpointForUUID(f.clientID)
	f.gatewayEndpoint = endpointForUUID(f.gatewayID)
	clientAccessHash, clientRefreshHash := randomBytes(t, 32), randomBytes(t, 32)
	gatewayAccessHash, gatewayRefreshHash := randomBytes(t, 32), randomBytes(t, 32)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO owners(id,display_name) VALUES($1,'call owner')`, []any{f.ownerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES($1,$2,'client','client')`, []any{f.clientID, f.ownerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES($1,$2,'gateway','gateway')`, []any{f.gatewayID, f.ownerID}},
		{`INSERT INTO gateways(device_id,mapping_revision,last_seen_at,sip_registered) VALUES($1,1,clock_timestamp(),true)`, []any{f.gatewayID}},
		{`INSERT INTO sim_bindings(sim_id,owner_id,gateway_id,slot_index,label,state,identity_verified,mapping_revision,service_state) VALUES($1,$2,$3,0,'Line A','active',true,1,'in_service')`, []any{f.simID, f.ownerID, f.gatewayID}},
		{`INSERT INTO sip_endpoint_bindings(device_id,endpoint_id,auth_username,aor,state) VALUES($1,$2,$2,$2,'active')`, []any{f.clientID, f.clientEndpoint}},
		{`INSERT INTO sip_endpoint_bindings(device_id,endpoint_id,auth_username,aor,state) VALUES($1,$2,$2,$2,'active')`, []any{f.gatewayID, f.gatewayEndpoint}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 day',$4,clock_timestamp()+interval '30 days')`, []any{testUUID(t), f.clientID, clientAccessHash, clientRefreshHash}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 day',$4,clock_timestamp()+interval '30 days')`, []any{testUUID(t), f.gatewayID, gatewayAccessHash, gatewayRefreshHash}},
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed call fixture: %v", err)
		}
	}
	return f
}

type fakeARI struct {
	mu             sync.Mutex
	createCalls    []map[string]any
	dialCalls      []string
	hangupCalls    []string
	destroyCalls   []string
	channels       []asterisk.Channel
	channelVars    map[string]string
	endpointState  string
	endpointStates map[string]string
	endpointChecks []string
}

func newFakeARI() *fakeARI {
	return &fakeARI{endpointState: "online", channelVars: make(map[string]string)}
}

func (f *fakeARI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/ari")
	switch {
	case path == "/asterisk/info" && r.Method == http.MethodGet:
		writeJSONForTest(w, http.StatusOK, map[string]any{"build": map[string]any{"version": "test"}})
	case strings.HasPrefix(path, "/endpoints/PJSIP/") && r.Method == http.MethodGet:
		endpointID := strings.TrimPrefix(path, "/endpoints/PJSIP/")
		f.mu.Lock()
		state := f.endpointState
		if endpointState, ok := f.endpointStates[endpointID]; ok {
			state = endpointState
		}
		f.endpointChecks = append(f.endpointChecks, endpointID)
		f.mu.Unlock()
		writeJSONForTest(w, http.StatusOK, map[string]string{"state": state})
	case path == "/channels/create" && r.Method == http.MethodPost:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.createCalls = append(f.createCalls, map[string]any{"query": r.URL.Query(), "body": body})
		id := "wrapper-test"
		if len(f.createCalls) > 1 {
			id = "wrapper-test-" + string(rune('0'+len(f.createCalls)))
		}
		f.mu.Unlock()
		writeJSONForTest(w, http.StatusOK, map[string]string{"id": id, "name": "Local/s@gsm2sip-ari-gateway-00000001;1", "state": "Up"})
	case path == "/channels" && r.Method == http.MethodGet:
		f.mu.Lock()
		channels := append([]asterisk.Channel(nil), f.channels...)
		f.mu.Unlock()
		writeJSONForTest(w, http.StatusOK, channels)
	case strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/variable") && r.Method == http.MethodGet:
		channelID := strings.TrimSuffix(strings.TrimPrefix(path, "/channels/"), "/variable")
		f.mu.Lock()
		value, ok := f.channelVars[channelID]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSONForTest(w, http.StatusOK, map[string]string{"value": value})
	case strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/dial") && r.Method == http.MethodPost:
		channelID := strings.TrimSuffix(strings.TrimPrefix(path, "/channels/"), "/dial")
		f.mu.Lock()
		f.dialCalls = append(f.dialCalls, channelID)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/progress") && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/channels/") && r.Method == http.MethodDelete:
		channelID := strings.TrimPrefix(path, "/channels/")
		f.mu.Lock()
		f.hangupCalls = append(f.hangupCalls, channelID)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/bridges/") && r.Method == http.MethodDelete:
		bridgeID := strings.TrimPrefix(path, "/bridges/")
		f.mu.Lock()
		f.destroyCalls = append(f.destroyCalls, bridgeID)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func writeJSONForTest(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func readyCallManager(t *testing.T, database *sql.DB, fake *fakeARI) (*Manager, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	manager, err := NewManager(database, Config{
		ARIURL: server.URL + "/ari", ARIUsername: "test", ARIPassword: "test-secret",
		ARIApplication: "gsm2sip", SIPRealm: "gsm2sip", TokenKey: []byte("call-test-token-key-that-is-over-32-bytes"),
	})
	if err != nil {
		server.Close()
		t.Fatalf("create call manager: %v", err)
	}
	manager.mu.Lock()
	manager.ariHealthy = true
	manager.ariReadyAt = time.Now()
	manager.mu.Unlock()
	return manager, server.Close
}

func TestCreateIntentIsIdempotentAndOnlyClientSIPStartsARIWrapper(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	fake := newFakeARI()
	manager, cleanup := readyCallManager(t, database, fake)
	defer cleanup()
	ctx := context.Background()
	request := CreateIntentRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "+15551234567"}
	key := "client-call-test-key-0001"
	intent, err := manager.CreateIntent(ctx, f.ownerID, f.clientID, request, key)
	if err != nil {
		t.Fatalf("create call intent: %v", err)
	}
	replay, err := manager.CreateIntent(ctx, f.ownerID, f.clientID, request, key)
	if err != nil || replay.IntentID != intent.IntentID || replay.CallID != intent.CallID || replay.SIPURI != intent.SIPURI {
		t.Fatalf("same-key request did not replay its intent: replay=%+v err=%v", replay, err)
	}
	changed := request
	changed.To = "+15557654321"
	if _, err := manager.CreateIntent(ctx, f.ownerID, f.clientID, changed, key); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key with a different payload returned %v, want ErrConflict", err)
	}
	fake.mu.Lock()
	createCount, dialCount := len(fake.createCalls), len(fake.dialCalls)
	fake.mu.Unlock()
	if createCount != 0 || dialCount != 0 {
		t.Fatalf("API intent creation initiated ARI work: create=%d dial=%d", createCount, dialCount)
	}

	tokenStart := strings.Index(intent.SIPURI, "call.") + len("call.")
	tokenEnd := strings.Index(intent.SIPURI[tokenStart:], "@") + tokenStart
	if tokenStart < len("call.") || tokenEnd <= tokenStart {
		t.Fatalf("unexpected one-shot SIP URI %q", intent.SIPURI)
	}
	token := intent.SIPURI[tokenStart:tokenEnd]
	clientChannelID := "client-invite-1"
	if err := manager.handleEvent(ctx, asterisk.Event{
		Type: "StasisStart", Application: "gsm2sip", Args: []string{"client-intent", token},
		Channel: asterisk.Channel{ID: clientChannelID, Name: "PJSIP/" + f.clientEndpoint + "-a1b2c3d4"},
	}); err != nil {
		t.Fatalf("consume token from trusted client PJSIP channel: %v", err)
	}
	var state, storedClientChannel, wrapperID string
	var storedTokenHash, storedKeyHash []byte
	if err := database.QueryRow(`SELECT state,client_channel_id,ari_wrapper_channel_id FROM call_sessions WHERE call_id=$1`, intent.CallID).
		Scan(&state, &storedClientChannel, &wrapperID); err != nil {
		t.Fatal("read consumed call session")
	}
	if state != "dialing" || storedClientChannel != clientChannelID || wrapperID != "wrapper-test" {
		t.Fatalf("consumed intent did not persist its channel IDs: state=%q client=%q wrapper=%q", state, storedClientChannel, wrapperID)
	}
	if err := database.QueryRow(`SELECT token_hash FROM call_intents WHERE intent_id=$1`, intent.IntentID).Scan(&storedTokenHash); err != nil {
		t.Fatal("read stored token hash")
	}
	if !equalBytes(storedTokenHash, hashToken(token)) {
		t.Fatal("call intent did not persist only the one-shot token hash")
	}
	if err := database.QueryRow(`SELECT idempotency_key_hash FROM call_intent_idempotency WHERE owner_id=$1`, f.ownerID).Scan(&storedKeyHash); err != nil {
		t.Fatal("read stored idempotency hash")
	}
	if !equalBytes(storedKeyHash, manager.idempotencyHash(f.ownerID, f.clientID, key)) {
		t.Fatal("call intent did not persist the domain-separated idempotency hash")
	}

	fake.mu.Lock()
	if len(fake.createCalls) != 1 {
		fake.mu.Unlock()
		t.Fatalf("valid client SIP invite created %d ARI wrappers, want one", len(fake.createCalls))
	}
	create := fake.createCalls[0]
	fake.mu.Unlock()
	query, ok := create["query"].(url.Values)
	if !ok || query.Get("endpoint") != "Local/s@gsm2sip-ari-gateway/n" || query.Get("appArgs") != "originate-wrapper,"+intent.CallID || query.Get("app") != "gsm2sip" {
		t.Fatalf("ARI create did not use the deployed Local/Stasis wrapper: %#v", create["query"])
	}
	variables, ok := create["body"].(map[string]any)["variables"].(map[string]any)
	if !ok || variables["__GSM2SIP_CALL_ID"] != intent.CallID || variables["__GSM2SIP_ENDPOINT"] != f.gatewayEndpoint || variables["__GSM2SIP_DESTINATION"] != request.To || variables["__GSM2SIP_SIM_ID"] != f.simID || variables["__GSM2SIP_MAPPING_REVISION"] != "1" || variables["__GSM2SIP_PROTOCOL_VERSION"] != "1" {
		t.Fatalf("ARI wrapper omitted inherited gateway call metadata: %#v", create["body"])
	}

	wrapperEvent := asterisk.Event{Type: "StasisStart", Application: "gsm2sip", Args: []string{"originate-wrapper", intent.CallID}, Channel: asterisk.Channel{ID: wrapperID}}
	if err := manager.handleEvent(ctx, wrapperEvent); err != nil {
		t.Fatalf("dial ARI wrapper: %v", err)
	}
	if err := manager.handleEvent(ctx, wrapperEvent); err != nil {
		t.Fatalf("replay wrapper event: %v", err)
	}
	fake.mu.Lock()
	dials := append([]string(nil), fake.dialCalls...)
	fake.mu.Unlock()
	if len(dials) != 1 || dials[0] != wrapperID {
		t.Fatalf("wrapper was dialed more than once or with wrong ID: %v", dials)
	}
}

func TestCreateIntentAllowsOfflineClientContactUntilAuthenticatedInvite(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	fake := newFakeARI()
	fake.endpointStates = map[string]string{f.clientEndpoint: "offline", f.gatewayEndpoint: "online"}
	manager, cleanup := readyCallManager(t, database, fake)
	defer cleanup()
	request := CreateIntentRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "+15551234567"}

	// The gateway still has to be reachable, while a background SIP client may
	// not have registered yet. Reserving its call must not originate anything.
	fake.mu.Lock()
	fake.endpointStates[f.gatewayEndpoint] = "offline"
	fake.mu.Unlock()
	if _, err := manager.CreateIntent(context.Background(), f.ownerID, f.clientID, request, "offline-gateway-call-key-01"); !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("offline gateway allowed an outgoing intent: %v", err)
	}
	fake.mu.Lock()
	fake.endpointStates[f.gatewayEndpoint] = "online"
	fake.mu.Unlock()

	intent, err := manager.CreateIntent(context.Background(), f.ownerID, f.clientID, request, "offline-client-call-key-01")
	if err != nil {
		t.Fatalf("offline client contact prevented intent reservation: %v", err)
	}
	fake.mu.Lock()
	createsBeforeInvite, dialsBeforeInvite := len(fake.createCalls), len(fake.dialCalls)
	for _, endpointID := range fake.endpointChecks {
		if endpointID == f.clientEndpoint {
			fake.mu.Unlock()
			t.Fatal("outgoing reservation queried the client's current SIP contact")
		}
	}
	fake.mu.Unlock()
	if createsBeforeInvite != 0 || dialsBeforeInvite != 0 {
		t.Fatalf("intent reservation initiated ARI channel work: create=%d dial=%d", createsBeforeInvite, dialsBeforeInvite)
	}

	tokenStart := strings.Index(intent.SIPURI, "call.") + len("call.")
	tokenEnd := strings.Index(intent.SIPURI[tokenStart:], "@") + tokenStart
	if tokenStart < len("call.") || tokenEnd <= tokenStart {
		t.Fatalf("unexpected one-shot SIP URI %q", intent.SIPURI)
	}
	if err := manager.handleEvent(context.Background(), asterisk.Event{
		Type: "StasisStart", Application: "gsm2sip", Args: []string{"client-intent", intent.SIPURI[tokenStart:tokenEnd]},
		Channel: asterisk.Channel{ID: "offline-client-now-invited", Name: "PJSIP/" + f.clientEndpoint + "-a1b2c3d4"},
	}); err != nil {
		t.Fatalf("authenticated client INVITE did not consume reserved intent: %v", err)
	}
	fake.mu.Lock()
	createsAfterInvite, dialsAfterInvite := len(fake.createCalls), len(fake.dialCalls)
	fake.mu.Unlock()
	if createsAfterInvite != 1 || dialsAfterInvite != 0 {
		t.Fatalf("authenticated client INVITE did not start exactly one gateway wrapper: create=%d dial=%d", createsAfterInvite, dialsAfterInvite)
	}
}

func TestCreateIntentConcurrentSameKeyAndGatewayLease(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	fake := newFakeARI()
	manager, cleanup := readyCallManager(t, database, fake)
	defer cleanup()
	request := CreateIntentRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "+15551234567"}
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan Intent, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := manager.CreateIntent(context.Background(), f.ownerID, f.clientID, request, "same-concurrent-key-00001")
			if err != nil {
				errs <- err
				return
			}
			results <- item
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent idempotent request failed: %v", err)
	}
	var first Intent
	for item := range results {
		if first.IntentID == "" {
			first = item
			continue
		}
		if first.IntentID != item.IntentID || first.CallID != item.CallID || first.SIPURI != item.SIPURI {
			t.Fatalf("concurrent idempotent requests returned different intents: %+v / %+v", first, item)
		}
	}
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM call_sessions WHERE gateway_id=$1 AND state='reserved'`, f.gatewayID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("gateway did not retain exactly one reserved call: count=%d err=%v", count, err)
	}
	otherKeyRequest := request
	otherKeyRequest.To = "+15557654321"
	if _, err := manager.CreateIntent(context.Background(), f.ownerID, f.clientID, otherKeyRequest, "different-concurrent-key-02"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second intent did not respect the gateway lease: %v", err)
	}
}

func TestReconnectFailsClosedWithoutReleasingUnknownGatewayLease(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	fake := newFakeARI()
	manager, cleanup := readyCallManager(t, database, fake)
	defer cleanup()
	callID := testUUID(t)
	clientChannel, gatewayChannel, wrapperChannel := "client-leg-1", "gateway-leg-1", "wrapper-1"
	bridgeID := "gsm2sip-" + strings.ReplaceAll(callID, "-", "")
	_, err := database.Exec(`
		INSERT INTO call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,state_revision,
		 gateway_endpoint_id,client_endpoint_id,client_channel_id,gateway_channel_id,ari_wrapper_channel_id,bridge_id,answered_at)
		VALUES($1,$2,$3,$4,$5,1,'outgoing','+15551234567','active',4,$6,$7,$8,$9,$10,$11,clock_timestamp())`,
		callID, f.ownerID, f.clientID, f.gatewayID, f.simID, f.gatewayEndpoint, f.clientEndpoint,
		clientChannel, gatewayChannel, wrapperChannel, bridgeID)
	if err != nil {
		t.Fatal("seed active call")
	}
	if _, err := database.Exec(`INSERT INTO gateway_call_slots(gateway_id,call_id) VALUES($1,$2)`, f.gatewayID, callID); err != nil {
		t.Fatal("seed call lease")
	}
	fake.channels = []asterisk.Channel{{ID: clientChannel}, {ID: gatewayChannel}, {ID: wrapperChannel}}
	if err := manager.failClosed(context.Background(), "coordinator_restarted"); err != nil {
		t.Fatalf("fail closed after coordinator restart: %v", err)
	}
	var state string
	if err := database.QueryRow(`SELECT state FROM call_sessions WHERE call_id=$1`, callID).Scan(&state); err != nil {
		t.Fatal("read interrupted call")
	}
	if state != "unknown" {
		t.Fatalf("interrupted call state is %q, want unknown", state)
	}
	var leaseCall string
	if err := database.QueryRow(`SELECT call_id::text FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&leaseCall); err != nil || leaseCall != callID {
		t.Fatalf("unknown call did not retain its gateway lease: call=%q err=%v", leaseCall, err)
	}
	fake.mu.Lock()
	hangups := append([]string(nil), fake.hangupCalls...)
	destroyed := append([]string(nil), fake.destroyCalls...)
	fake.mu.Unlock()
	if len(hangups) != 3 || len(destroyed) != 1 || destroyed[0] != bridgeID {
		t.Fatalf("reconnect cleanup was incomplete: hangups=%v bridges=%v", hangups, destroyed)
	}
}

func TestReconcileStopsCallAfterEndpointRevocationAndKeepsLeaseUntilGone(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	fake := newFakeARI()
	manager, cleanup := readyCallManager(t, database, fake)
	defer cleanup()
	callID := testUUID(t)
	clientChannel, gatewayChannel, wrapperChannel := "revoked-client-leg", "revoked-gateway-leg", "revoked-wrapper"
	bridgeID := "gsm2sip-" + strings.ReplaceAll(callID, "-", "")
	if _, err := database.Exec(`
		INSERT INTO call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,state_revision,
		 gateway_endpoint_id,client_endpoint_id,client_channel_id,gateway_channel_id,ari_wrapper_channel_id,bridge_id,answered_at)
		VALUES($1,$2,$3,$4,$5,1,'outgoing','+15551234567','active',4,$6,$7,$8,$9,$10,$11,clock_timestamp())`,
		callID, f.ownerID, f.clientID, f.gatewayID, f.simID, f.gatewayEndpoint, f.clientEndpoint,
		clientChannel, gatewayChannel, wrapperChannel, bridgeID); err != nil {
		t.Fatal("seed active call")
	}
	if _, err := database.Exec(`INSERT INTO gateway_call_slots(gateway_id,call_id) VALUES($1,$2)`, f.gatewayID, callID); err != nil {
		t.Fatal("seed call lease")
	}
	fake.channels = []asterisk.Channel{{ID: clientChannel}, {ID: gatewayChannel}, {ID: wrapperChannel}}
	if _, err := database.Exec(`DELETE FROM sessions WHERE device_id=$1`, f.clientID); err != nil {
		t.Fatal("revoke client API sessions")
	}
	if _, err := database.Exec(`UPDATE sip_endpoint_bindings SET state='revoked' WHERE device_id=$1`, f.clientID); err != nil {
		t.Fatal("revoke client SIP endpoint")
	}
	if err := manager.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile revoked active call: %v", err)
	}
	var state, reason string
	if err := database.QueryRow(`SELECT state,reason FROM call_sessions WHERE call_id=$1`, callID).Scan(&state, &reason); err != nil {
		t.Fatal("read interrupted call")
	}
	if state != "unknown" || reason != "client_endpoint_revoked" {
		t.Fatalf("revoked active call was not retained as unknown: state=%s reason=%s", state, reason)
	}
	var leaseCall string
	if err := database.QueryRow(`SELECT call_id::text FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&leaseCall); err != nil || leaseCall != callID {
		t.Fatalf("call lease was released before ARI confirmed hangup: lease=%s err=%v", leaseCall, err)
	}
	fake.mu.Lock()
	hangups := append([]string(nil), fake.hangupCalls...)
	fake.mu.Unlock()
	if len(hangups) != 3 {
		t.Fatalf("revocation did not hang up all known call channels: %v", hangups)
	}

	// ARI still reported the channels immediately after DELETE. Once a later
	// reconciliation confirms they are gone, the unknown lease can be released.
	fake.mu.Lock()
	fake.channels = nil
	fake.mu.Unlock()
	if err := manager.reconcile(context.Background()); err != nil {
		t.Fatalf("confirm revoked call teardown: %v", err)
	}
	if err := database.QueryRow(`SELECT state,reason FROM call_sessions WHERE call_id=$1`, callID).Scan(&state, &reason); err != nil {
		t.Fatal("read completed revoked call")
	}
	if state != "ended" || reason != "client_endpoint_revoked" {
		t.Fatalf("confirmed revoked call did not terminate: state=%s reason=%s", state, reason)
	}
	var leaseCount int
	if err := database.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&leaseCount); err != nil || leaseCount != 0 {
		t.Fatalf("confirmed gone call retained its gateway lease: count=%d err=%v", leaseCount, err)
	}
}

func TestIncomingCallRequiresCurrentNonceAndMappingBeforeClientRing(t *testing.T) {
	t.Run("ready nonce is listed and replays once", func(t *testing.T) {
		database := openCallsDatabase(t)
		f := seedCallFixture(t, database)
		fake := newFakeARI()
		manager, cleanup := readyCallManager(t, database, fake)
		defer cleanup()
		callID, gatewayChannel := testUUID(t), "incoming-gateway-1"
		if err := acceptTestIncoming(t, manager, f, callID, gatewayChannel); err != nil {
			t.Fatalf("accept incoming invite: %v", err)
		}
		var pending Call
		if err := database.QueryRow(`SELECT state,state_revision,EXTRACT(EPOCH FROM (expires_at-clock_timestamp())),wake_nonce FROM call_sessions WHERE call_id=$1`, callID).
			Scan(&pending.State, &pending.StateRevision, new(float64), &pending.WakeNonce); err == nil {
			// The typed query below checks the timestamp independently and avoids
			// relying on driver conversion for a nullable result.
		} else {
			t.Fatal("read incoming call")
		}
		var seconds float64
		if err := database.QueryRow(`SELECT EXTRACT(EPOCH FROM (expires_at-clock_timestamp())) FROM call_sessions WHERE call_id=$1`, callID).Scan(&seconds); err != nil {
			t.Fatal("read incoming expiry")
		}
		if pending.State != "pending_wakeup" || pending.WakeNonce == nil || seconds > 25 || seconds < 24 {
			t.Fatalf("incoming call is not a 25 second nonce-gated wake: state=%s nonce=%v expiry=%.3f", pending.State, pending.WakeNonce, seconds)
		}
		list, _, err := manager.ListCalls(context.Background(), f.ownerID, f.clientID, "", 50)
		if err != nil || len(list) != 1 || list[0].WakeNonce == nil || *list[0].WakeNonce != *pending.WakeNonce {
			t.Fatalf("client call list did not expose its pending wake nonce: %+v err=%v", list, err)
		}
		if _, err := manager.ClientReady(context.Background(), f.ownerID, f.clientID, f.clientID, callID, "stale-nonce"); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale readiness nonce returned %v, want ErrConflict", err)
		}
		ready, err := manager.ClientReady(context.Background(), f.ownerID, f.clientID, f.clientID, callID, *pending.WakeNonce)
		if err != nil || ready.State != "ringing" {
			t.Fatalf("valid readiness nonce did not start ringing the client: %+v err=%v", ready, err)
		}
		replayed, err := manager.ClientReady(context.Background(), f.ownerID, f.clientID, f.clientID, callID, *pending.WakeNonce)
		if err != nil || replayed.State != "ringing" {
			t.Fatalf("same readiness nonce did not replay the durable result: %+v err=%v", replayed, err)
		}
		fake.mu.Lock()
		creates := len(fake.createCalls)
		fake.mu.Unlock()
		if creates != 1 {
			t.Fatalf("readiness retry created %d client wrappers, want exactly one", creates)
		}
	})

	t.Run("stale mapping ends the incoming call", func(t *testing.T) {
		database := openCallsDatabase(t)
		f := seedCallFixture(t, database)
		fake := newFakeARI()
		manager, cleanup := readyCallManager(t, database, fake)
		defer cleanup()
		callID, gatewayChannel := testUUID(t), "incoming-gateway-stale"
		if err := acceptTestIncoming(t, manager, f, callID, gatewayChannel); err != nil {
			t.Fatalf("accept incoming invite: %v", err)
		}
		var wakeNonce string
		if err := database.QueryRow(`SELECT wake_nonce FROM call_sessions WHERE call_id=$1`, callID).Scan(&wakeNonce); err != nil {
			t.Fatal("read incoming nonce")
		}
		if _, err := database.Exec(`UPDATE gateways SET mapping_revision=2 WHERE device_id=$1`, f.gatewayID); err != nil {
			t.Fatal("advance gateway mapping")
		}
		if _, err := database.Exec(`UPDATE sim_bindings SET mapping_revision=2 WHERE sim_id=$1`, f.simID); err != nil {
			t.Fatal("advance SIM mapping")
		}
		if _, err := manager.ClientReady(context.Background(), f.ownerID, f.clientID, f.clientID, callID, wakeNonce); !errors.Is(err, ErrSIMUnavailable) {
			t.Fatalf("stale mapping readiness returned %v, want ErrSIMUnavailable", err)
		}
		var state, reason string
		if err := database.QueryRow(`SELECT state,reason FROM call_sessions WHERE call_id=$1`, callID).Scan(&state, &reason); err != nil {
			t.Fatal("read stale incoming result")
		}
		if state != "ended" || reason != "sim_mapping_changed" {
			t.Fatalf("stale mapping did not end the pending call safely: %s %s", state, reason)
		}
		var leaseCount int
		if err := database.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&leaseCount); err != nil || leaseCount != 0 {
			t.Fatalf("ended stale call retained a gateway lease: count=%d err=%v", leaseCount, err)
		}
		fake.mu.Lock()
		hangups := append([]string(nil), fake.hangupCalls...)
		creates := len(fake.createCalls)
		fake.mu.Unlock()
		if len(hangups) != 1 || hangups[0] != gatewayChannel || creates != 0 {
			t.Fatalf("stale incoming call was not hung up before client dialing: hangups=%v creates=%d", hangups, creates)
		}
	})

	t.Run("expired call cannot ring after its deadline", func(t *testing.T) {
		database := openCallsDatabase(t)
		f := seedCallFixture(t, database)
		fake := newFakeARI()
		manager, cleanup := readyCallManager(t, database, fake)
		defer cleanup()
		callID, gatewayChannel := testUUID(t), "incoming-gateway-expired"
		if err := acceptTestIncoming(t, manager, f, callID, gatewayChannel); err != nil {
			t.Fatalf("accept incoming invite: %v", err)
		}
		var wakeNonce string
		if err := database.QueryRow(`SELECT wake_nonce FROM call_sessions WHERE call_id=$1`, callID).Scan(&wakeNonce); err != nil {
			t.Fatal("read incoming nonce")
		}
		if _, err := database.Exec(`UPDATE call_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE call_id=$1`, callID); err != nil {
			t.Fatal("expire incoming call")
		}
		if _, err := manager.ClientReady(context.Background(), f.ownerID, f.clientID, f.clientID, callID, wakeNonce); !errors.Is(err, ErrConflict) {
			t.Fatalf("expired readiness returned %v, want ErrConflict", err)
		}
		var state string
		if err := database.QueryRow(`SELECT state FROM call_sessions WHERE call_id=$1`, callID).Scan(&state); err != nil {
			t.Fatal("read expired incoming state")
		}
		if state != "ended" {
			t.Fatalf("expired incoming call remained in %q", state)
		}
		fake.mu.Lock()
		hangups := append([]string(nil), fake.hangupCalls...)
		creates := len(fake.createCalls)
		fake.mu.Unlock()
		if len(hangups) != 1 || creates != 0 {
			t.Fatalf("expired incoming call was not canceled safely: hangups=%v creates=%d", hangups, creates)
		}
	})
}

func acceptTestIncoming(t *testing.T, manager *Manager, f callFixture, callID, channelID string) error {
	t.Helper()
	return manager.handleEvent(context.Background(), asterisk.Event{
		Type: "StasisStart", Application: "gsm2sip",
		Args: []string{"gateway-inbound", callID, f.simID, "1", "1"},
		Channel: asterisk.Channel{
			ID: channelID, Name: "PJSIP/" + f.gatewayEndpoint + "-1234abcd",
			Caller: struct {
				Name   string `json:"name"`
				Number string `json:"number"`
			}{Number: "+15551234567"},
			Connected: struct {
				Name   string `json:"name"`
				Number string `json:"number"`
			}{Number: "+15557654321"},
		},
	})
}

func TestGatewayInboundRequiresSupportedProtocolVersion(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	fake := newFakeARI()
	manager, cleanup := readyCallManager(t, database, fake)
	defer cleanup()
	for _, version := range []string{"", "2"} {
		channelID := "unsupported-protocol-" + version
		args := []string{"gateway-inbound", testUUID(t), f.simID, "1"}
		if version != "" {
			args = append(args, version)
		}
		err := manager.handleEvent(context.Background(), asterisk.Event{
			Type: "StasisStart", Application: "gsm2sip", Args: args,
			Channel: asterisk.Channel{ID: channelID, Name: "PJSIP/" + f.gatewayEndpoint + "-1234abcd"},
		})
		if err != nil {
			t.Fatalf("reject unsupported gateway protocol %q: %v", version, err)
		}
	}
	var callsCreated int
	if err := database.QueryRow(`SELECT count(*) FROM call_sessions`).Scan(&callsCreated); err != nil {
		t.Fatal("count call sessions")
	}
	if callsCreated != 0 {
		t.Fatalf("unsupported gateway protocol created %d calls", callsCreated)
	}
	fake.mu.Lock()
	hangups := append([]string(nil), fake.hangupCalls...)
	fake.mu.Unlock()
	if len(hangups) != 2 {
		t.Fatalf("unsupported protocol calls were not rejected with hangup: %v", hangups)
	}
}

func randomHex(t *testing.T, count int) string {
	t.Helper()
	data := make([]byte, count)
	if _, err := rand.Read(data); err != nil {
		t.Fatal("generate random test schema name")
	}
	return hex.EncodeToString(data)
}

func randomBytes(t *testing.T, count int) []byte {
	t.Helper()
	data := make([]byte, count)
	if _, err := rand.Read(data); err != nil {
		t.Fatal("generate random test secret hash")
	}
	return data
}

func testUUID(t *testing.T) string {
	t.Helper()
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		t.Fatal("generate test UUID")
	}
	data[6] = data[6]&0x0f | 0x40
	data[8] = data[8]&0x3f | 0x80
	return hex.EncodeToString(data[0:4]) + "-" + hex.EncodeToString(data[4:6]) + "-" + hex.EncodeToString(data[6:8]) + "-" + hex.EncodeToString(data[8:10]) + "-" + hex.EncodeToString(data[10:16])
}

func endpointForUUID(value string) string { return "dev_" + strings.ReplaceAll(value, "-", "") }
