package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kirikira/gsm2sip-server/internal/rtcsignal"
)

func controlFixture(t *testing.T) (*integrationDatabase, integrationFixture, *Server) {
	t.Helper()
	db := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, db.db)
	api := NewWithOptions(db.db, nil, Options{})
	t.Cleanup(api.ShutdownWebSockets)
	for _, token := range []string{f.clientToken, f.gatewayToken} {
		requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/capability", token, map[string]any{"version": 1, "enabled": true}, ""), 200)
	}
	rtcRegister(t, api, f.gatewayToken)
	return db, f, api
}
func controlCreate(t *testing.T, f integrationFixture, api *Server, incoming bool) rtcControlSnapshot {
	t.Helper()
	path := "/v1/rtc/control/calls"
	token := f.clientToken
	if incoming {
		path = "/v1/rtc/control/incoming"
		token = f.gatewayToken
	}
	body := rtcCreateCall{Version: 1, OperationID: newUUID(), CallID: newUUID(), GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "15550100200"}
	reply := requestJSON(t, api.Handler(), "POST", path, token, body, "")
	requireStatus(t, reply, 201)
	var out rtcControlSnapshot
	if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	retry := requestJSON(t, api.Handler(), "POST", path, token, body, "")
	requireStatus(t, retry, 200)
	if !strings.Contains(retry.Body.String(), out.CallID) {
		t.Fatal("retry changed call identity")
	}
	body.To = "15550100300"
	requireStatus(t, requestJSON(t, api.Handler(), "POST", path, token, body, ""), 409)
	return out
}
func controlOp(t *testing.T, api *Server, token string, out rtcControlSnapshot, kind string, status int) rtcControlSnapshot {
	t.Helper()
	body := rtcCallOperation{Version: 1, OperationID: newUUID(), Revision: out.StateRevision, Kind: kind, CandidateID: out.CandidateID}
	path := "/v1/rtc/control/calls/" + out.CallID + "/operations"
	reply := requestJSON(t, api.Handler(), "POST", path, token, body, "")
	requireStatus(t, reply, status)
	if status != 200 {
		return out
	}
	var next rtcControlSnapshot
	if err := json.Unmarshal(reply.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	retry := requestJSON(t, api.Handler(), "POST", path, token, body, "")
	requireStatus(t, retry, 200)
	var duplicate rtcControlSnapshot
	_ = json.Unmarshal(retry.Body.Bytes(), &duplicate)
	if duplicate.StateRevision != next.StateRevision {
		t.Fatal("retry performed another transition")
	}
	return next
}
func controlGet(t *testing.T, api *Server, token, id string) rtcControlSnapshot {
	t.Helper()
	reply := requestJSON(t, api.Handler(), "GET", "/v1/rtc/control/calls/"+id, token, nil, "")
	requireStatus(t, reply, 200)
	var out rtcControlSnapshot
	if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func negotiateControl(t *testing.T, api *Server, f integrationFixture, id string) rtcAdmission {
	t.Helper()
	admit := rtcSelect(t, api, f.clientToken, id)
	rtcPost(t, api, f.clientToken, rtcCommand(admit.Binding, rtcsignal.Offer, 1), 200)
	rtcPost(t, api, f.gatewayToken, rtcCommand(admit.Binding, rtcsignal.Answer, 2), 200)
	return admit
}
func TestIndependentRTCOutgoingWithoutSIPCredentialsOrARI(t *testing.T) {
	db, f, api := controlFixture(t)
	var sip int
	_ = db.db.QueryRow(`SELECT count(*) FROM sip_endpoint_bindings`).Scan(&sip)
	if sip != 0 {
		t.Fatal("test requires no SIP bindings")
	}
	out := controlCreate(t, f, api, false)
	if out.ControlTransport != "rtc" || out.Dispatched || out.State != "reserved" {
		t.Fatal("reservation dispatched cellular work")
	}
	// Neither a client nor an unprepared gateway can authorize dispatch.
	controlOp(t, api, f.clientToken, out, "dispatch", 409)
	out = controlOp(t, api, f.clientToken, out, "start", 200)
	controlOp(t, api, f.gatewayToken, out, "dispatch", 409)
	admission := negotiateControl(t, api, f, out.CallID)
	out = controlOp(t, api, f.gatewayToken, out, "dispatch", 200)
	if out.State != "dialing" || !out.Dispatched {
		t.Fatal("missing durable dispatch")
	}
	controlOp(t, api, f.gatewayToken, out, "active", 409)
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/calls/"+out.CallID+"/media/active", f.gatewayToken, map[string]string{"leg_id": admission.Binding.LegID}, ""), 200)
	out = controlOp(t, api, f.gatewayToken, out, "active", 200)
	if out.State != "active" || out.AnsweredAt == nil {
		t.Fatal("actual cellular active not recorded")
	}
	rtcRead(t, api, f.clientToken, admission.Binding.LegID, 200) // no SIP lease / ARI needed
	controlOp(t, api, f.otherToken, out, "hangup", 404)
	out = controlOp(t, api, f.clientToken, out, "hangup", 200)
	var slots int
	_ = db.db.QueryRow(`SELECT count(*) FROM gateway_call_slots`).Scan(&slots)
	if !out.EndRequested || slots != 1 {
		t.Fatal("unconfirmed modem end released occupancy")
	}
	rtcRead(t, api, f.clientToken, admission.Binding.LegID, 403)
	out = controlOp(t, api, f.gatewayToken, out, "ended", 200)
	_ = db.db.QueryRow(`SELECT count(*) FROM gateway_call_slots`).Scan(&slots)
	if out.State != "ended" || slots != 0 {
		t.Fatal("confirmed cellular end did not release occupancy")
	}
	controlOp(t, api, f.gatewayToken, out, "active", 409)
}
func TestIndependentRTCIncomingReservationAndGatewayWinner(t *testing.T) {
	_, f, api := controlFixture(t)
	out := controlCreate(t, f, api, true)
	if out.State != "ringing" || out.WinnerID != "" {
		t.Fatal("invitation answered before user action")
	}
	out = controlOp(t, api, f.clientToken, out, "accept", 200)
	if out.WinnerID != "" || out.CandidateID != f.clientID {
		t.Fatal("server reservation claimed a global gateway winner")
	}
	controlOp(t, api, f.gatewayToken, out, "winner", 409)
	admission := negotiateControl(t, api, f, out.CallID)
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/calls/"+out.CallID+"/media/active", f.gatewayToken, map[string]string{"leg_id": admission.Binding.LegID}, ""), 409)
	out = controlOp(t, api, f.gatewayToken, out, "winner", 200)
	if out.WinnerID != f.clientID {
		t.Fatal("gateway winner not committed")
	}
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/calls/"+out.CallID+"/media/active", f.gatewayToken, map[string]string{"leg_id": admission.Binding.LegID}, ""), 200)
	out = controlOp(t, api, f.gatewayToken, out, "active", 200)
	controlOp(t, api, f.clientToken, out, "reject", 409)
	controlOp(t, api, f.gatewayToken, out, "ended", 200)
}
func TestIndependentRTCExpiryRetainsUncertainCellularSlot(t *testing.T) {
	db, f, api := controlFixture(t)
	out := controlCreate(t, f, api, false)
	out = controlOp(t, api, f.clientToken, out, "start", 200)
	negotiateControl(t, api, f, out.CallID)
	out = controlOp(t, api, f.gatewayToken, out, "dispatch", 200)
	_, err := db.db.Exec(`UPDATE rtc_control_calls SET gateway_seen_at=clock_timestamp()-interval '30 seconds' WHERE call_id=$1`, out.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if err = api.sweepRTCControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	out = controlGet(t, api, f.clientToken, out.CallID)
	var slots int
	_ = db.db.QueryRow(`SELECT count(*) FROM gateway_call_slots`).Scan(&slots)
	if out.State != "unknown" || !out.EndRequested || slots != 1 {
		t.Fatal("control loss allowed possible duplicate cellular dispatch")
	}
	controlOp(t, api, f.gatewayToken, out, "dispatch", 409)
	controlOp(t, api, f.gatewayToken, out, "ended", 200)
}
func TestIndependentRTCRejectAndIncomingWithoutReadyClients(t *testing.T) {
	db, f, api := controlFixture(t)
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/capability", f.clientToken, map[string]any{"version": 1, "enabled": false}, ""), 200)
	body := rtcCreateCall{Version: 1, OperationID: newUUID(), CallID: newUUID(), GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "15550100200"}
	reply := requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/incoming", f.gatewayToken, body, "")
	requireStatus(t, reply, 503)
	if !strings.Contains(reply.Body.String(), "GATEWAY_UNAVAILABLE") {
		t.Fatal("an incoming call without a ready client was not refused")
	}
	var count int
	_ = db.db.QueryRow(`SELECT count(*) FROM call_sessions`).Scan(&count)
	if count != 0 {
		t.Fatal("a refused incoming call left a call record")
	}
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/capability", f.clientToken, map[string]any{"version": 1, "enabled": true}, ""), 200)
	out := controlCreate(t, f, api, true)
	out = controlOp(t, api, f.clientToken, out, "reject", 200)
	out = controlGet(t, api, f.gatewayToken, out.CallID)
	if !out.EndRequested {
		t.Fatal("last rejection failed to cancel cellular ringing")
	}
	controlOp(t, api, f.gatewayToken, out, "ended", 200)
}

func TestIndependentRTCReplaySurvivesRestartAndConcurrentRetries(t *testing.T) {
	db, f, api := controlFixture(t)
	body := rtcCreateCall{Version: 1, OperationID: strings.ToUpper(newUUID()), CallID: strings.ToUpper(newUUID()), GatewayID: strings.ToUpper(f.gatewayID), SIMID: strings.ToUpper(f.simID), MappingRevision: 1, To: "15550100200"}
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/calls", f.clientToken, body, ""), 201)
	// A different API process has no in-memory replay state.
	restarted := NewWithOptions(db.db, nil, Options{})
	t.Cleanup(restarted.ShutdownWebSockets)
	requireStatus(t, requestJSON(t, restarted.Handler(), "POST", "/v1/rtc/control/calls", f.clientToken, body, ""), 200)
	op := rtcCallOperation{Version: 1, OperationID: strings.ToUpper(newUUID()), Revision: 1, Kind: "start"}
	path := "/v1/rtc/control/calls/" + body.CallID + "/operations"
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- requestJSON(t, restarted.Handler(), "POST", path, f.clientToken, op, "")
		}()
	}
	wg.Wait()
	close(results)
	for reply := range results {
		requireStatus(t, reply, 200)
		var out rtcControlSnapshot
		if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil || out.StateRevision != 2 || out.State != "connecting" {
			t.Fatal("concurrent retry performed a second transition")
		}
	}
	var ops int
	if err := db.db.QueryRow(`SELECT count(*) FROM rtc_control_operations WHERE call_id=$1`, body.CallID).Scan(&ops); err != nil || ops != 2 {
		t.Fatal("replay did not retain exactly create and start")
	}
	// Revocation must also deny a previously committed operation's ACK.
	if _, err := db.db.Exec(`UPDATE devices SET state='revoked' WHERE id=$1`, f.clientID); err != nil {
		t.Fatal(err)
	}
	reply := requestJSON(t, restarted.Handler(), "POST", path, f.clientToken, op, "")
	if reply.Code != 401 && reply.Code != 403 {
		t.Fatalf("revoked retry returned %d", reply.Code)
	}
}

func TestIndependentRTCRejectsAmbiguousCapabilityAndForgedCaller(t *testing.T) {
	_, f, api := controlFixture(t)
	for _, body := range []map[string]any{{"version": 1}, {"version": 1, "enabled": nil}, {"version": 2, "enabled": true}} {
		requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/capability", f.clientToken, body, ""), 400)
	}
	body := rtcCreateCall{Version: 1, OperationID: newUUID(), CallID: newUUID(), GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "15550100200", From: "forged-caller"}
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/calls", f.clientToken, body, ""), 400)
	body.From = "caller\nforged"
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/incoming", f.gatewayToken, body, ""), 400)
}

func TestIndependentRTCConcurrentAcceptReservesOneCandidate(t *testing.T) {
	db, f, api := controlFixture(t)
	secondID, secondToken := newUUID(), randomToken()
	if _, err := db.db.Exec(`INSERT INTO devices(id,owner_id,role,name) VALUES($1,$2,'client','second RTC client')`, secondID, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`, newUUID(), secondID, tokenHash(secondToken), tokenHash(randomToken())); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/capability", secondToken, map[string]any{"version": 1, "enabled": true}, ""), 200)
	out := controlCreate(t, f, api, true)
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, token := range []string{f.clientToken, secondToken} {
		go func(token string) {
			<-start
			results <- requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/calls/"+out.CallID+"/operations", token, rtcCallOperation{Version: 1, OperationID: newUUID(), Revision: out.StateRevision, Kind: "accept"}, "")
		}(token)
	}
	close(start)
	codes := map[int]int{}
	for i := 0; i < 2; i++ {
		reply := <-results
		codes[reply.Code]++
	}
	if codes[200] != 1 || codes[409] != 1 {
		t.Fatalf("competing accepts: %v", codes)
	}
	out = controlGet(t, api, f.gatewayToken, out.CallID)
	if out.CandidateID == "" || out.WinnerID != "" || out.State != "connecting" {
		t.Fatal("candidate became an unconfirmed winner")
	}
	loser := f.clientToken
	if out.CandidateID == f.clientID {
		loser = secondToken
	}
	controlOp(t, api, loser, out, "reject", 200)
	out = controlGet(t, api, f.gatewayToken, out.CallID)
	if out.EndRequested || out.CandidateID == "" {
		t.Fatal("loser's rejection canceled the candidate's call")
	}
	controlOp(t, api, f.gatewayToken, out, "ended", 200)
}
