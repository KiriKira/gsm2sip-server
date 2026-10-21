package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/rtcsignal"
)

type rtcHTTPState struct {
	State          rtcsignal.ReadResult `json:"state"`
	CellularActive bool                 `json:"cellular_active"`
}
type rtcAdmission struct {
	Binding        rtcsignal.Binding `json:"binding"`
	MediaTransport string            `json:"media_transport"`
}

func rtcFixture(t *testing.T, direction string) (*integrationDatabase, integrationFixture, *Server, string) {
	t.Helper()
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	api := NewWithOptions(database.db, nil, Options{})
	t.Cleanup(api.ShutdownWebSockets)
	callID := newUUID()
	state := "reserved"
	if direction == "incoming" {
		state = "ringing"
	}
	_, err := database.db.Exec(`INSERT INTO call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,expires_at,gateway_endpoint_id,client_endpoint_id,control_transport) VALUES($1,$2,$3,$4,$5,1,$6,'+12025550100',$7,clock_timestamp()+interval '30 seconds','','','rtc')`, callID, f.ownerID, f.clientID, f.gatewayID, f.simID, direction, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.db.Exec(`INSERT INTO rtc_control_calls(call_id) VALUES($1)`, callID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.db.Exec(`INSERT INTO gateway_call_slots(gateway_id,call_id) VALUES($1,$2)`, f.gatewayID, callID); err != nil {
		t.Fatal(err)
	}
	if direction == "incoming" {
		_, err = database.db.Exec(`INSERT INTO call_participants(call_id,owner_id,client_device_id,endpoint_id,wake_nonce,state) VALUES($1,$2,$3,'',$4,'ringing')`, callID, f.ownerID, f.clientID, newUUID())
		if err != nil {
			t.Fatal(err)
		}
		// The client accepted: it holds the bounded candidate reservation.
		if _, err = database.db.Exec(`UPDATE rtc_control_calls SET candidate_client_id=$2,candidate_until=clock_timestamp()+interval '20 seconds' WHERE call_id=$1`, callID, f.clientID); err != nil {
			t.Fatal(err)
		}
	}
	return database, f, api, callID
}
func rtcRegister(t *testing.T, api *Server, token string) {
	t.Helper()
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/devices/self/rtc-capability", token, map[string]any{"version": 1}, ""), 200)
}
func rtcSelect(t *testing.T, api *Server, token, callID string) rtcAdmission {
	t.Helper()
	reply := requestJSON(t, api.Handler(), "POST", "/v1/calls/"+callID+"/media", token, map[string]string{"media_transport": "webrtc"}, "")
	requireStatus(t, reply, 200)
	var result rtcAdmission
	if err := json.Unmarshal(reply.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.MediaTransport != "webrtc" || result.Binding.LegID == "" {
		t.Fatal("missing bound RTC admission")
	}
	return result
}
func rtcRead(t *testing.T, api *Server, token, legID string, status int) rtcHTTPState {
	t.Helper()
	reply := requestJSON(t, api.Handler(), "GET", "/v1/rtc/legs/"+legID, token, nil, "")
	requireStatus(t, reply, status)
	var result rtcHTTPState
	if status == 200 {
		if err := json.Unmarshal(reply.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
func rtcCommand(b rtcsignal.Binding, kind rtcsignal.Kind, revision int64) rtcsignal.Command {
	c := rtcsignal.Command{Version: 1, CallID: b.CallID, LegID: b.LegID, AttemptID: b.AttemptID, OperationID: newUUID(), Generation: 1, Revision: revision, Kind: kind}
	if kind == rtcsignal.Offer || kind == rtcsignal.Answer {
		c.SDP = "v=0\r\ns=contract-fixture\r\n"
	}
	return c
}
func rtcPost(t *testing.T, api *Server, token string, c rtcsignal.Command, status int) *httptest.ResponseRecorder {
	t.Helper()
	reply := requestJSON(t, api.Handler(), "POST", "/v1/rtc/legs/"+c.LegID, token, c, "")
	requireStatus(t, reply, status)
	return reply
}

func TestRTCMediaAdmissionIsExplicitBoundAndIdempotent(t *testing.T) {
	database, f, api, callID := rtcFixture(t, "outgoing")
	reply := requestJSON(t, api.Handler(), "GET", "/v1/calls/"+callID+"/media", f.gatewayToken, nil, "")
	requireStatus(t, reply, 200)
	if !strings.Contains(reply.Body.String(), `"media_transport":"pending"`) {
		t.Fatal("a call without a media leg must report pending")
	}
	// Without a gateway advertising WebRTC audio, no leg is created.
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/calls/"+callID+"/media", f.clientToken, map[string]string{"media_transport": "webrtc"}, ""), 409)
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/devices/self/rtc-capability", f.clientToken, map[string]any{"version": 1}, ""), 403)
	rtcRegister(t, api, f.gatewayToken)
	first := rtcSelect(t, api, f.clientToken, callID)
	second := rtcSelect(t, api, f.clientToken, callID)
	if first.Binding != second.Binding || first.Binding.CallID != callID || first.Binding.ClientID != f.clientID || first.Binding.GatewayID != f.gatewayID {
		t.Fatal("admission changed identity across retry")
	}
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/calls/"+callID+"/media", f.otherToken, map[string]string{"media_transport": "webrtc"}, ""), 403)
	if _, err := database.db.Exec(`UPDATE call_sessions SET state='dialing' WHERE call_id=$1`, callID); err != nil {
		t.Fatal(err)
	}
	if later := rtcSelect(t, api, f.clientToken, callID); later.Binding != first.Binding {
		t.Fatal("retry replaced a live media leg")
	}
}
func TestRTCMediaCannotSelectANewEngineAfterDial(t *testing.T) {
	database, f, api, callID := rtcFixture(t, "outgoing")
	rtcRegister(t, api, f.gatewayToken)
	if _, err := database.db.Exec(`UPDATE call_sessions SET state='dialing' WHERE call_id=$1`, callID); err != nil {
		t.Fatal(err)
	}
	reply := requestJSON(t, api.Handler(), "POST", "/v1/calls/"+callID+"/media", f.clientToken, map[string]string{"media_transport": "webrtc"}, "")
	requireStatus(t, reply, 409)
	if !strings.Contains(reply.Body.String(), "RTC_MODE_FROZEN") {
		t.Fatal(reply.Body.String())
	}
	var count int
	if err := database.db.QueryRow(`SELECT count(*) FROM rtc_media_sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("mid-call engine selection persisted")
	}
}
func TestRTCMediaRelaysCommittedSDPAndSeparatesCellularActive(t *testing.T) {
	database, f, api, callID := rtcFixture(t, "outgoing")
	rtcRegister(t, api, f.gatewayToken)
	admission := rtcSelect(t, api, f.clientToken, callID)
	b := admission.Binding
	host := rtcRead(t, api, f.clientToken, b.LegID, 200)
	offer := rtcCommand(b, rtcsignal.Offer, host.State.Snapshot.Revision)
	rtcPost(t, api, f.clientToken, offer, 200)
	duplicate := rtcPost(t, api, f.clientToken, offer, 200)
	if !strings.Contains(duplicate.Body.String(), `"duplicate":true`) {
		t.Fatal("network retry was not deduplicated")
	}
	gateway := rtcRead(t, api, f.gatewayToken, b.LegID, 200)
	if len(gateway.State.Signals) != 1 || gateway.State.Signals[0].Kind != rtcsignal.Offer || gateway.CellularActive {
		t.Fatal("SDP publication was treated as cellular ACTIVE")
	}
	rtcRead(t, api, f.otherGatewayToken, b.LegID, 403)
	answer := rtcCommand(b, rtcsignal.Answer, gateway.State.Snapshot.Revision)
	rtcPost(t, api, f.gatewayToken, answer, 200)
	host = rtcRead(t, api, f.clientToken, b.LegID, 200)
	if len(host.State.Signals) != 1 || host.State.Signals[0].Kind != rtcsignal.Answer || host.CellularActive {
		t.Fatal("negotiation was treated as cellular ACTIVE")
	}
	activePath := "/v1/calls/" + callID + "/media/active"
	requireStatus(t, requestJSON(t, api.Handler(), "POST", activePath, f.clientToken, map[string]string{"leg_id": b.LegID}, ""), 403)
	requireStatus(t, requestJSON(t, api.Handler(), "POST", activePath, f.gatewayToken, map[string]string{"leg_id": b.LegID}, ""), 409)
	if _, err := database.db.Exec(`UPDATE call_sessions SET state='dialing' WHERE call_id=$1`, callID); err != nil {
		t.Fatal(err)
	}
	// The gateway has not recorded its cellular dispatch yet.
	requireStatus(t, requestJSON(t, api.Handler(), "POST", activePath, f.gatewayToken, map[string]string{"leg_id": b.LegID}, ""), 409)
	if _, err := database.db.Exec(`UPDATE rtc_control_calls SET dispatched=true WHERE call_id=$1`, callID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, api.Handler(), "POST", activePath, f.gatewayToken, map[string]string{"leg_id": b.LegID}, ""), 200)
	if !rtcRead(t, api, f.clientToken, b.LegID, 200).CellularActive {
		t.Fatal("gateway cellular report was not persisted")
	}
	closeCommand := rtcCommand(b, rtcsignal.Close, 0)
	rtcPost(t, api, f.clientToken, closeCommand, 200)
	rtcPost(t, api, f.gatewayToken, rtcCommand(b, rtcsignal.Answer, host.State.Snapshot.Revision), 410)
	requireStatus(t, requestJSON(t, api.Handler(), "GET", "/v1/calls/"+callID+"/media", f.gatewayToken, nil, ""), 410)
}
func TestRTCMediaAuthorityIsRecheckedForEverySnapshot(t *testing.T) {
	for _, change := range []string{"slot", "sim", "gateway_device", "session"} {
		t.Run(change, func(t *testing.T) {
			database, f, api, callID := rtcFixture(t, "outgoing")
			rtcRegister(t, api, f.gatewayToken)
			b := rtcSelect(t, api, f.clientToken, callID).Binding
			var err error
			status := 403
			switch change {
			case "slot":
				_, err = database.db.Exec(`DELETE FROM gateway_call_slots WHERE call_id=$1`, callID)
			case "sim":
				_, err = database.db.Exec(`UPDATE sim_bindings SET mapping_revision=2 WHERE sim_id=$1`, f.simID)
			case "gateway_device":
				_, err = database.db.Exec(`UPDATE devices SET state='revoked' WHERE id=$1`, f.gatewayID)
			case "session":
				_, err = database.db.Exec(`DELETE FROM sessions WHERE device_id=$1`, f.clientID)
				status = 401
			}
			if err != nil {
				t.Fatal(err)
			}
			rtcRead(t, api, f.clientToken, b.LegID, status)
		})
	}
}
func TestRTCMediaIncomingWinnerCannotLeakToAnotherCandidate(t *testing.T) {
	database, f, api, callID := rtcFixture(t, "incoming")
	rtcRegister(t, api, f.gatewayToken)
	clientB, tokenB := newUUID(), randomToken()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO devices(id,owner_id,role,name) VALUES($1,$2,'client','other-host')`, []any{clientB, f.ownerID}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`, []any{newUUID(), clientB, tokenHash(tokenB), tokenHash(randomToken())}},
		{`INSERT INTO call_participants(call_id,owner_id,client_device_id,endpoint_id,wake_nonce,state) VALUES($1,$2,$3,'other-host','other-wake','ringing')`, []any{callID, f.ownerID, clientB}},
	} {
		if _, err := database.db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	a := rtcSelect(t, api, f.clientToken, callID).Binding
	// B accepts after A released its reservation.
	if _, err := database.db.Exec(`UPDATE rtc_control_calls SET candidate_client_id=$2 WHERE call_id=$1`, callID, clientB); err != nil {
		t.Fatal(err)
	}
	b := rtcSelect(t, api, tokenB, callID).Binding
	rtcRead(t, api, f.gatewayToken, a.LegID, 403)
	if _, err := database.db.Exec(`UPDATE call_sessions SET incoming_winner_client_device_id=$2,state='connecting' WHERE call_id=$1`, callID, clientB); err != nil {
		t.Fatal(err)
	}
	rtcRead(t, api, f.gatewayToken, a.LegID, 403)
	rtcRead(t, api, f.clientToken, a.LegID, 403)
	response := requestJSON(t, api.Handler(), "GET", "/v1/calls/"+callID+"/media", f.gatewayToken, nil, "")
	requireStatus(t, response, 200)
	var winner rtcAdmission
	if err := json.Unmarshal(response.Body.Bytes(), &winner); err != nil {
		t.Fatal(err)
	}
	if winner.Binding != b {
		t.Fatal("gateway received a losing candidate's media leg")
	}
	requireStatus(t, requestJSON(t, api.Handler(), http.MethodGet, "/v1/calls/"+callID+"/media", f.clientToken, nil, ""), 403)
}

// A moving real clock previously made the two renewal timestamps exceed the
// reducer's exact 60s bound by nanoseconds, ending every call at its first renew.
func TestRTCMediaRenewalUsesOneClockInstant(t *testing.T) {
	_, f, api, callID := rtcFixture(t, "outgoing")
	rtcRegister(t, api, f.gatewayToken)
	b := rtcSelect(t, api, f.clientToken, callID).Binding
	first := rtcRead(t, api, f.clientToken, b.LegID, 200)
	rtcPost(t, api, f.clientToken, rtcCommand(b, rtcsignal.Offer, first.State.Snapshot.Revision), 200)
	offer := rtcRead(t, api, f.gatewayToken, b.LegID, 200)
	rtcPost(t, api, f.gatewayToken, rtcCommand(b, rtcsignal.Answer, offer.State.Snapshot.Revision), 200)
	before := rtcRead(t, api, f.clientToken, b.LegID, 200)
	base := time.Now().Add(31 * time.Second)
	var ticks atomic.Int64
	api.now = func() time.Time { return base.Add(time.Duration(ticks.Add(1)) * time.Microsecond) }
	renewed := rtcRead(t, api, f.clientToken, b.LegID, 200)
	if !renewed.State.Snapshot.AuthorizedUntil.After(before.State.Snapshot.AuthorizedUntil) ||
		renewed.State.Snapshot.Phase != rtcsignal.Negotiated {
		t.Fatal("active native leg did not retain its renewed grant")
	}
	rtcRead(t, api, f.gatewayToken, b.LegID, 200)
}

// SIP releases left their calls in the shared tables. They get no media, and
// the server ends them so their cellular slot does not block new calls.
func TestLegacySIPCallsGetNoMediaAndAreEnded(t *testing.T) {
	database, f, api, rtcCall := rtcFixture(t, "outgoing")
	rtcRegister(t, api, f.gatewayToken)
	if _, err := database.db.Exec(`DELETE FROM gateway_call_slots WHERE call_id=$1`, rtcCall); err != nil {
		t.Fatal(err)
	}
	legacy := newUUID()
	if _, err := database.db.Exec(`INSERT INTO call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,reason,gateway_endpoint_id,client_endpoint_id) VALUES($1,$2,$3,$4,$5,1,'incoming','+12025550100','unknown','ari_reconciliation_uncertain','gw','host')`, legacy, f.ownerID, f.clientID, f.gatewayID, f.simID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO call_participants(call_id,owner_id,client_device_id,endpoint_id,wake_nonce,state,reason,accepted_at) VALUES($1,$2,$3,'host','legacy-wake','accepted','legacy_participant_reason',clock_timestamp())`, legacy, f.ownerID, f.clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO gateway_call_slots(gateway_id,call_id) VALUES($1,$2)`, f.gatewayID, legacy); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/calls/"+legacy+"/media", f.clientToken, map[string]string{"media_transport": "webrtc"}, ""), 403)
	requireStatus(t, requestJSON(t, api.Handler(), "GET", "/v1/calls/"+legacy+"/media", f.gatewayToken, nil, ""), 403)
	count, err := api.EndLegacySIPCalls(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("ended %d legacy calls: %v", count, err)
	}
	var state, reason, participant, participantReason string
	var slots int
	if err := database.db.QueryRow(`SELECT c.state,c.reason,p.state,p.reason,(SELECT count(*) FROM gateway_call_slots) FROM call_sessions c JOIN call_participants p ON p.call_id=c.call_id WHERE c.call_id=$1`, legacy).Scan(&state, &reason, &participant, &participantReason, &slots); err != nil {
		t.Fatal(err)
	}
	if state != "ended" || reason != "sip_removed" || participant != "ended" || participantReason != "sip_removed" || slots != 0 {
		t.Fatalf("legacy call left state=%s reason=%s participant=%s participant_reason=%s slots=%d", state, reason, participant, participantReason, slots)
	}
	if count, err = api.EndLegacySIPCalls(context.Background()); err != nil || count != 0 {
		t.Fatal("a second pass changed ended calls")
	}
	reply := requestJSON(t, api.Handler(), "GET", "/v1/calls/"+legacy, f.clientToken, nil, "")
	requireStatus(t, reply, 200)
	if !strings.Contains(reply.Body.String(), `"state":"ended"`) {
		t.Fatal("legacy call history is no longer readable")
	}
}
