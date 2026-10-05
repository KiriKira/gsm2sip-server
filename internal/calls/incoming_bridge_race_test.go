package calls

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/asterisk"
)

func readyCallManagerBlockingBridge(t *testing.T, database *sql.DB, fake *fakeARI) (*Manager, <-chan struct{}, func(), func()) {
	t.Helper()
	bridgeCreateStarted := make(chan struct{})
	releaseBridgeCreate := make(chan struct{})
	var blockOnce sync.Once
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ari/bridges" && r.Method == http.MethodPost {
			blockOnce.Do(func() {
				close(bridgeCreateStarted)
				<-releaseBridgeCreate
			})
		}
		fake.serveHTTP(w, r)
	}))
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
	release := func() {
		releaseOnce.Do(func() { close(releaseBridgeCreate) })
	}
	cleanup := func() {
		release()
		server.Close()
	}
	return manager, bridgeCreateStarted, release, cleanup
}

func readyCallManagerFailingHangupOnce(t *testing.T, database *sql.DB, fake *fakeARI, channelID string) (*Manager, func() int, func()) {
	t.Helper()
	var mu sync.Mutex
	failureCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ari/channels/"+channelID && r.Method == http.MethodDelete {
			mu.Lock()
			fail := failureCount == 0
			if fail {
				failureCount++
			}
			mu.Unlock()
			if fail {
				http.Error(w, "temporary ARI failure", http.StatusServiceUnavailable)
				return
			}
		}
		fake.serveHTTP(w, r)
	}))
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
	countFailures := func() int {
		mu.Lock()
		defer mu.Unlock()
		return failureCount
	}
	return manager, countFailures, server.Close
}

func TestIncomingBridgeCannotReviveCallWhileCleanupWaitsForChannels(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	fake := newFakeARI()
	manager, bridgeCreateStarted, releaseBridgeCreate, cleanup := readyCallManagerBlockingBridge(t, database, fake)
	defer cleanup()

	callID := testUUID(t)
	const gatewayChannelID = "race-cleanup-gateway"
	const clientChannelID = "race-cleanup-client"
	if err := acceptTestIncoming(t, manager, f, callID, gatewayChannelID); err != nil {
		t.Fatal(err)
	}
	wrapperID := ringClient(t, manager, f, f.clientID, callID)
	clientLeg(t, manager, f.clientEndpoint, clientChannelID, callID, "Ringing")
	fake.mu.Lock()
	fake.channels = []asterisk.Channel{
		{ID: gatewayChannelID, Name: "PJSIP/" + f.gatewayEndpoint + "-00000001", State: "Ring"},
		{ID: clientChannelID, Name: "PJSIP/" + f.clientEndpoint + "-00000001", State: "Up"},
		{ID: wrapperID, Name: "Local/s@gsm2sip-ari-client-00000001;1", State: "Up"},
	}
	fake.mu.Unlock()

	answerDone := make(chan error, 1)
	go func() {
		answerDone <- manager.acceptIncomingAnswer(context.Background(),
			deviceIdentity{deviceID: f.clientID, ownerID: f.ownerID, role: "client"},
			f.clientEndpoint, clientChannelID)
	}()
	select {
	case <-bridgeCreateStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("answer flow did not reach ARI bridge creation")
	}

	// Keep every ARI channel visible after the hangup requests so cleanup must
	// retain the gateway slot while the bridge request is still in flight.
	if err := manager.endIncomingCall(context.Background(), callID, "remote_hangup"); err != nil {
		t.Fatalf("end incoming call during bridge setup: %v", err)
	}
	var state string
	var slots int
	if err := database.QueryRow(`SELECT state FROM call_sessions WHERE call_id=$1`, callID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" || slots != 1 {
		t.Fatalf("cleanup did not retain uncertain call: state=%s slots=%d", state, slots)
	}

	releaseBridgeCreate()
	select {
	case err := <-answerDone:
		if err != nil {
			t.Fatalf("finish raced answer flow: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("answer flow did not finish after releasing ARI bridge creation")
	}

	if err := database.QueryRow(`SELECT state FROM call_sessions WHERE call_id=$1`, callID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" {
		t.Fatalf("bridge setup revived terminal cleanup state: %s", state)
	}
	if err := database.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if slots != 1 {
		t.Fatalf("uncertain call released gateway slot before ARI channels disappeared: %d", slots)
	}

	fake.mu.Lock()
	fake.channels = nil
	fake.mu.Unlock()
	if err := manager.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile channels after they disappear: %v", err)
	}
	if err := database.QueryRow(`SELECT state FROM call_sessions WHERE call_id=$1`, callID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ended" {
		t.Fatalf("confirmed channel cleanup did not finish call: %s", state)
	}
	if err := database.QueryRow(`SELECT count(*) FROM gateway_call_slots WHERE gateway_id=$1`, f.gatewayID).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if slots != 0 {
		t.Fatalf("confirmed cleanup retained gateway slot: %d", slots)
	}
}

func TestIncomingLoserHangupFailureDoesNotDropWinner(t *testing.T) {
	database := openCallsDatabase(t)
	f := seedCallFixture(t, database)
	loserID, loserEndpoint := secondClient(t, database, f.ownerID)
	const loserChannelID = "loser-hangup-retry-client"
	fake := newFakeARI()
	fake.removeOnHangup = true
	manager, countFailures, cleanup := readyCallManagerFailingHangupOnce(t, database, fake, loserChannelID)
	defer cleanup()

	callID := testUUID(t)
	const gatewayChannelID = "loser-hangup-retry-gateway"
	const winnerChannelID = "loser-hangup-retry-winner"
	if err := acceptTestIncoming(t, manager, f, callID, gatewayChannelID); err != nil {
		t.Fatal(err)
	}
	winnerWrapperID := ringClient(t, manager, f, f.clientID, callID)
	loserWrapperID := ringClient(t, manager, f, loserID, callID)
	for _, item := range []struct{ clientID, wrapperID string }{
		{clientID: f.clientID, wrapperID: winnerWrapperID},
		{clientID: loserID, wrapperID: loserWrapperID},
	} {
		if err := manager.handleEvent(context.Background(), asterisk.Event{
			Type: "StasisStart", Application: "gsm2sip",
			Args:    []string{"originate-wrapper", callID, item.clientID},
			Channel: asterisk.Channel{ID: item.wrapperID, Name: "Local/s@gsm2sip-ari-client-00000001;1", State: "Up"},
		}); err != nil {
			t.Fatalf("start participant wrapper: %v", err)
		}
	}
	clientLeg(t, manager, loserEndpoint, loserChannelID, callID, "Ringing")
	fake.mu.Lock()
	fake.channels = []asterisk.Channel{
		{ID: gatewayChannelID, Name: "PJSIP/" + f.gatewayEndpoint + "-00000001", State: "Ring"},
		{ID: winnerChannelID, Name: "PJSIP/" + f.clientEndpoint + "-00000001", State: "Up"},
		{ID: loserChannelID, Name: "PJSIP/" + loserEndpoint + "-00000001", State: "Ringing"},
		{ID: winnerWrapperID, Name: "Local/s@gsm2sip-ari-client-00000001;1", State: "Up"},
		{ID: loserWrapperID, Name: "Local/s@gsm2sip-ari-client-00000002;1", State: "Up"},
	}
	fake.mu.Unlock()

	// The first attempt to cancel the loser returns a transient 503. The Up
	// StasisStart must still finish winner selection and bridge the winner.
	clientLeg(t, manager, f.clientEndpoint, winnerChannelID, callID, "Up")
	if failures := countFailures(); failures != 1 {
		t.Fatalf("loser hangup failure path ran %d times, want one", failures)
	}
	var winner string
	if err := database.QueryRow(`SELECT incoming_winner_client_device_id::text FROM call_sessions WHERE call_id=$1`, callID).Scan(&winner); err != nil {
		t.Fatal(err)
	}
	if winner != f.clientID {
		t.Fatalf("unexpected winner after loser cleanup error: %s", winner)
	}
	item, err := manager.GetCall(context.Background(), f.ownerID, f.clientID, callID)
	if err != nil || item.State != "active" {
		t.Fatalf("winner was dropped after loser hangup error: state=%s err=%v", item.State, err)
	}
	fake.mu.Lock()
	answers, bridges := len(fake.answerCalls), len(fake.bridgeCreateCalls)
	fake.mu.Unlock()
	if answers != 1 || bridges != 1 {
		t.Fatalf("winner bridge did not complete once: answers=%d bridges=%d", answers, bridges)
	}

	if err := manager.reconcile(context.Background()); err != nil {
		t.Fatalf("retry loser hangup during reconciliation: %v", err)
	}
	fake.mu.Lock()
	stillPresent := false
	for _, channel := range fake.channels {
		if channel.ID == loserChannelID || channel.ID == loserWrapperID {
			stillPresent = true
		}
	}
	fake.mu.Unlock()
	if stillPresent {
		t.Fatal("reconciliation did not remove the ended loser's channels")
	}
	item, err = manager.GetCall(context.Background(), f.ownerID, f.clientID, callID)
	if err != nil || item.State != "active" {
		t.Fatalf("retrying loser cleanup changed winner state: state=%s err=%v", item.State, err)
	}
}
