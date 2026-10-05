package calls

import (
	"context"
	"database/sql"
	"errors"
	"github.com/kirikira/gsm2sip-server/internal/asterisk"
	"github.com/kirikira/gsm2sip-server/internal/db"
	"sync"
	"testing"
)

func secondClient(t *testing.T, db *sql.DB, owner string) (string, string) {
	t.Helper()
	id := testUUID(t)
	endpoint := endpointForUUID(id)
	for _, s := range []struct {
		q string
		a []any
	}{
		{`INSERT INTO devices(id,owner_id,role,name) VALUES($1,$2,'client','second host')`, []any{id, owner}},
		{`INSERT INTO sip_endpoint_bindings(device_id,endpoint_id,auth_username,aor,state) VALUES($1,$2,$2,$2,'active')`, []any{id, endpoint}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`, []any{testUUID(t), id, randomBytes(t, 32), randomBytes(t, 32)}},
	} {
		if _, err := db.Exec(s.q, s.a...); err != nil {
			t.Fatal(err)
		}
	}
	return id, endpoint
}

func pendingFor(t *testing.T, m *Manager, f callFixture, client, call string) Call {
	t.Helper()
	item, err := m.GetCall(context.Background(), f.ownerID, client, call)
	if err != nil || item.ClientID != client || item.State != "pending_wakeup" || item.WakeNonce == nil {
		t.Fatalf("missing own pending participant: state=%s client=%s err=%v", item.State, item.ClientID, err)
	}
	return item
}

func ringClient(t *testing.T, m *Manager, f callFixture, client, call string) string {
	t.Helper()
	item := pendingFor(t, m, f, client, call)
	if _, err := m.ClientReady(context.Background(), f.ownerID, client, client, call, *item.WakeNonce); err != nil {
		t.Fatal(err)
	}
	var wrapper string
	if err := m.db.QueryRow(`SELECT wrapper_channel_id FROM call_participants WHERE call_id=$1 AND client_device_id=$2`, call, client).Scan(&wrapper); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func clientLeg(t *testing.T, m *Manager, endpoint, id, call, state string) {
	t.Helper()
	if err := m.handleEvent(context.Background(), asterisk.Event{Type: "StasisStart", Application: "gsm2sip", Args: []string{"client-leg", call}, Channel: asterisk.Channel{ID: id, Name: "PJSIP/" + endpoint + "-00000001", State: state}}); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingMultipleClientsKeepNoncesAndReadinessIndependent(t *testing.T) {
	db := openCallsDatabase(t)
	f := seedCallFixture(t, db)
	b, bEndpoint := secondClient(t, db, f.ownerID)
	fake := newFakeARI()
	m, cleanup := readyCallManager(t, db, fake)
	defer cleanup()
	call := testUUID(t)
	if err := acceptTestIncoming(t, m, f, call, "multi-gateway"); err != nil {
		t.Fatal(err)
	}
	aPending, bPending := pendingFor(t, m, f, f.clientID, call), pendingFor(t, m, f, b, call)
	if *aPending.WakeNonce == *bPending.WakeNonce {
		t.Fatal("participants share wake nonce")
	}
	if _, err := m.ClientReady(context.Background(), f.ownerID, b, b, call, *aPending.WakeNonce); !errors.Is(err, ErrConflict) {
		t.Fatalf("other client's nonce accepted: %v", err)
	}
	if _, err := m.ClientReady(context.Background(), f.ownerID, b, f.clientID, call, *bPending.WakeNonce); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other readiness path accepted: %v", err)
	}
	late, _ := secondClient(t, db, f.ownerID)
	if _, err := m.GetCall(context.Background(), f.ownerID, late, call); !errors.Is(err, ErrForbidden) {
		t.Fatalf("late pairing joined existing call: %v", err)
	}
	fake.mu.Lock()
	fake.endpointStates = map[string]string{bEndpoint: "offline"}
	fake.mu.Unlock()
	if _, err := m.ClientReady(context.Background(), f.ownerID, b, b, call, *bPending.WakeNonce); !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("offline host ring: %v", err)
	}
	wrapper := ringClient(t, m, f, f.clientID, call)
	fake.mu.Lock()
	fake.endpointStates[bEndpoint] = "online"
	fake.mu.Unlock()
	ringClient(t, m, f, b, call)
	for i := 0; i < 2; i++ {
		if err := m.handleEvent(context.Background(), asterisk.Event{Type: "StasisStart", Application: "gsm2sip", Args: []string{"originate-wrapper", call, f.clientID}, Channel: asterisk.Channel{ID: wrapper, Name: "Local/s@gsm2sip-ari-client-00000001;1", State: "Up"}}); err != nil {
			t.Fatal(err)
		}
	}
	var winner sql.NullString
	if err := db.QueryRow(`SELECT incoming_winner_client_device_id FROM call_sessions WHERE call_id=$1`, call).Scan(&winner); err != nil || winner.Valid {
		t.Fatal("ready or Local Up selected winner")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.createCalls) != 2 || len(fake.dialCalls) != 1 {
		t.Fatalf("not independent/idempotent: creates=%d dials=%d", len(fake.createCalls), len(fake.dialCalls))
	}
}

func TestIncomingSimultaneousAnswersHaveOneWinnerAndPreserveLoserResult(t *testing.T) {
	db := openCallsDatabase(t)
	f := seedCallFixture(t, db)
	b, bEndpoint := secondClient(t, db, f.ownerID)
	fake := newFakeARI()
	fake.removeOnHangup = true
	m, cleanup := readyCallManager(t, db, fake)
	defer cleanup()
	call := testUUID(t)
	gateway := "race-gateway"
	if err := acceptTestIncoming(t, m, f, call, gateway); err != nil {
		t.Fatal(err)
	}
	aWrapper := ringClient(t, m, f, f.clientID, call)
	bWrapper := ringClient(t, m, f, b, call)
	clientLeg(t, m, f.clientEndpoint, "race-client-a", call, "Ringing")
	clientLeg(t, m, bEndpoint, "race-client-b", call, "Ringing")
	fake.mu.Lock()
	fake.channels = []asterisk.Channel{{ID: gateway, Name: "PJSIP/" + f.gatewayEndpoint + "-00000001", State: "Ring"}, {ID: "race-client-a", Name: "PJSIP/" + f.clientEndpoint + "-00000001", State: "Up"}, {ID: "race-client-b", Name: "PJSIP/" + bEndpoint + "-00000001", State: "Up"}, {ID: aWrapper}, {ID: bWrapper}}
	fake.mu.Unlock()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, v := range []struct{ id, endpoint string }{{"race-client-a", f.clientEndpoint}, {"race-client-b", bEndpoint}} {
		wg.Add(1)
		go func(id, endpoint string) {
			defer wg.Done()
			<-start
			errs <- m.handleEvent(context.Background(), asterisk.Event{Type: "ChannelStateChange", Channel: asterisk.Channel{ID: id, Name: "PJSIP/" + endpoint + "-00000001", State: "Up"}})
		}(v.id, v.endpoint)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var winner string
	if err := db.QueryRow(`SELECT incoming_winner_client_device_id::text FROM call_sessions WHERE call_id=$1`, call).Scan(&winner); err != nil {
		t.Fatal(err)
	}
	loser, loserChannel := b, "race-client-b"
	winnerChannel, winnerEndpoint := "race-client-a", f.clientEndpoint
	if winner == b {
		loser, loserChannel = f.clientID, "race-client-a"
		winnerChannel, winnerEndpoint = "race-client-b", bEndpoint
	}
	win, err := m.GetCall(context.Background(), f.ownerID, winner, call)
	if err != nil || win.State != "active" {
		t.Fatalf("winner state=%s err=%v", win.State, err)
	}
	lost, err := m.GetCall(context.Background(), f.ownerID, loser, call)
	if err != nil || lost.State != "ended" || lost.Reason == nil || *lost.Reason != "answered_elsewhere" || lost.WakeNonce != nil {
		t.Fatalf("loser state=%s err=%v", lost.State, err)
	}
	if err := m.handleEvent(context.Background(), asterisk.Event{Type: "ChannelStateChange", Channel: asterisk.Channel{ID: winnerChannel, Name: "PJSIP/" + winnerEndpoint + "-00000001", State: "Up"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.onChannelDestroyed(context.Background(), loserChannel, 16, "Normal Clearing"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	answers, bridges := len(fake.answerCalls), len(fake.bridgeCreateCalls)
	fake.mu.Unlock()
	if answers != 1 || bridges != 1 {
		t.Fatalf("duplicate bridge/answer: answers=%d bridges=%d", answers, bridges)
	}
	if err := m.onChannelDestroyed(context.Background(), gateway, 16, "Normal Clearing"); err != nil {
		t.Fatal(err)
	}
	lost, err = m.GetCall(context.Background(), f.ownerID, loser, call)
	if err != nil || lost.Reason == nil || *lost.Reason != "answered_elsewhere" {
		t.Fatal("call cleanup overwrote loser result")
	}
	var slots int
	if err := db.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&slots); err != nil || slots != 0 {
		t.Fatalf("gateway not released: slots=%d err=%v", slots, err)
	}
}

func TestIncomingRejectOrRevokeOneHostAllowsOtherToAnswer(t *testing.T) {
	for _, mode := range []string{"reject", "revoke"} {
		t.Run(mode, func(t *testing.T) {
			db := openCallsDatabase(t)
			f := seedCallFixture(t, db)
			b, bEndpoint := secondClient(t, db, f.ownerID)
			fake := newFakeARI()
			fake.removeOnHangup = true
			m, cleanup := readyCallManager(t, db, fake)
			defer cleanup()
			call := testUUID(t)
			gateway := "surviving-gateway"
			if err := acceptTestIncoming(t, m, f, call, gateway); err != nil {
				t.Fatal(err)
			}
			var primary string
			if err := db.QueryRow(`SELECT client_device_id::text FROM call_sessions WHERE call_id=$1`, call).Scan(&primary); err != nil {
				t.Fatal(err)
			}
			survivor, survivorEndpoint := b, bEndpoint
			if primary == b {
				survivor, survivorEndpoint = f.clientID, f.clientEndpoint
			}
			primaryWrapper := ringClient(t, m, f, primary, call)
			fake.mu.Lock()
			fake.channels = []asterisk.Channel{{ID: gateway, State: "Ring"}, {ID: primaryWrapper}}
			fake.mu.Unlock()
			if mode == "reject" {
				if err := m.onChannelDestroyed(context.Background(), primaryWrapper, 21, "Rejected"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.Exec(`DELETE FROM sessions WHERE device_id=$1`, primary); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE sip_endpoint_bindings SET state='revoked' WHERE device_id=$1`, primary); err != nil {
					t.Fatal(err)
				}
				if err := m.reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			pendingFor(t, m, f, survivor, call)
			ringClient(t, m, f, survivor, call)
			fake.mu.Lock()
			fake.channels = append(fake.channels, asterisk.Channel{ID: "survivor-leg", Name: "PJSIP/" + survivorEndpoint + "-00000001", State: "Up"})
			fake.mu.Unlock()
			clientLeg(t, m, survivorEndpoint, "survivor-leg", call, "Up")
			item, err := m.GetCall(context.Background(), f.ownerID, survivor, call)
			if err != nil || item.State != "active" {
				t.Fatalf("survivor cannot answer: %s %v", item.State, err)
			}
		})
	}
}

func TestIncomingAllRejectOrTimeoutRetainsLeaseUntilChannelsDisappear(t *testing.T) {
	for _, mode := range []string{"reject", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			db := openCallsDatabase(t)
			f := seedCallFixture(t, db)
			b, _ := secondClient(t, db, f.ownerID)
			fake := newFakeARI()
			m, cleanup := readyCallManager(t, db, fake)
			defer cleanup()
			call := testUUID(t)
			if err := acceptTestIncoming(t, m, f, call, "held-gateway"); err != nil {
				t.Fatal(err)
			}
			aWrapper := ringClient(t, m, f, f.clientID, call)
			bWrapper := ringClient(t, m, f, b, call)
			fake.mu.Lock()
			fake.channels = []asterisk.Channel{{ID: "held-gateway"}, {ID: aWrapper}, {ID: bWrapper}}
			fake.mu.Unlock()
			if mode == "timeout" {
				if _, err := db.Exec(`UPDATE call_sessions SET expires_at=now()-interval '1 second' WHERE call_id=$1`, call); err != nil {
					t.Fatal(err)
				}
				if err := m.expireDueCalls(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := m.onChannelDestroyed(context.Background(), aWrapper, 21, "Rejected"); err != nil {
					t.Fatal(err)
				}
				if err := m.onChannelDestroyed(context.Background(), bWrapper, 21, "Rejected"); err != nil {
					t.Fatal(err)
				}
			}
			var state string
			var slots int
			if err := db.QueryRow(`SELECT state FROM call_sessions WHERE call_id=$1`, call).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&slots); err != nil {
				t.Fatal(err)
			}
			if state != "unknown" || slots != 1 {
				t.Fatalf("released live cellular capacity: %s slots=%d", state, slots)
			}
			fake.mu.Lock()
			fake.channels = nil
			fake.mu.Unlock()
			if err := m.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&slots); err != nil || slots != 0 {
				t.Fatalf("cleaned call not released: slots=%d err=%v", slots, err)
			}
		})
	}
}

func TestMultiClientMigrationPreservesLegacyIncomingCalls(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	// Reconstruct the call schema immediately before 0009, without modifying
	// shared databases or mocking the migration's SQL execution.
	if _, err := database.Exec(`DROP TABLE call_participants;
 ALTER TABLE call_sessions DROP COLUMN incoming_winner_client_device_id;
 ALTER TABLE call_events DROP CONSTRAINT call_events_call_client_revision_key;
 ALTER TABLE call_events ADD CONSTRAINT call_events_call_id_state_revision_key UNIQUE(call_id,state_revision);
 DELETE FROM schema_migrations WHERE name='0009_multi_client_calls.sql'`); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, state := range []string{"pending_wakeup", "ringing", "connecting", "active", "unknown", "ended"} {
		id := testUUID(t)
		ids[state] = id
		if _, err := database.Exec(`INSERT INTO call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,state_revision,expires_at,wake_nonce,gateway_endpoint_id,client_endpoint_id,gateway_channel_id,client_channel_id,ari_wrapper_channel_id,answered_at,ended_at)
  VALUES($1,$2,$3,$4,$5,1,'incoming','123',$6,5,now()+interval '25 seconds','legacy-nonce',$7,$8,$9,
  CASE WHEN $6 IN ('connecting','active') THEN $10 END,
  CASE WHEN $6 IN ('ringing','connecting','active') THEN $11 END,
  CASE WHEN $6='active' THEN now() END,CASE WHEN $6='ended' THEN now() END)`,
			id, f.ownerID, f.clientID, f.gatewayID, f.simID, state, f.gatewayEndpoint, f.clientEndpoint, "legacy-gateway-"+state, "legacy-client-"+state, "legacy-wrapper-"+state); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate existing incoming calls: %v", err)
	}
	fake := newFakeARI()
	m, cleanup := readyCallManager(t, database, fake)
	defer cleanup()
	pending := pendingFor(t, m, f, f.clientID, ids["pending_wakeup"])
	if *pending.WakeNonce != "legacy-nonce" {
		t.Fatal("migration discarded legacy nonce")
	}
	active, err := m.GetCall(context.Background(), f.ownerID, f.clientID, ids["active"])
	if err != nil || active.State != "active" {
		t.Fatal("migration lost active participant")
	}
	var winner string
	if err := database.QueryRow(`SELECT incoming_winner_client_device_id::text FROM call_sessions WHERE call_id=$1`, ids["active"]).Scan(&winner); err != nil || winner != f.clientID {
		t.Fatal("legacy active call lost its winner")
	}
	for i := 0; i < 2; i++ {
		if err := m.dialWrapper(context.Background(), "legacy-wrapper-ringing", ids["ringing"], ""); err != nil {
			t.Fatalf("legacy two-argument wrapper failed: %v", err)
		}
	}
	fake.mu.Lock()
	dials := len(fake.dialCalls)
	fake.mu.Unlock()
	if dials != 1 {
		t.Fatalf("legacy wrapper redialed %d times", dials)
	}
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM call_participants`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("migration missed legacy participants: %d %v", count, err)
	}
	// A connecting legacy leg may already have answered before coordinator restart.
	fake.mu.Lock()
	fake.channels = []asterisk.Channel{{ID: "legacy-client-connecting", Name: "PJSIP/" + f.clientEndpoint + "-00000001", State: "Up"}, {ID: "legacy-gateway-connecting", Name: "PJSIP/" + f.gatewayEndpoint + "-00000001", State: "Ring"}}
	fake.mu.Unlock()
	if err := m.reconcileIncomingParticipants(context.Background(), fake.channels); err != nil {
		t.Fatal(err)
	}
	connected, err := m.GetCall(context.Background(), f.ownerID, f.clientID, ids["connecting"])
	if err != nil || connected.State != "active" {
		t.Fatalf("legacy answered connecting call not recovered: %s %v", connected.State, err)
	}
}
