package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kirikira/gsm2sip-server/internal/push"
)

func putReceptionAs(t *testing.T, api *Server, token string, revision int64, incoming, sms bool, status int) receptionConfig {
	t.Helper()
	reply := requestJSON(t, api.Handler(), "PUT", "/v1/devices/self/reception", token,
		map[string]any{"version": 2, "expected_revision": revision, "incoming_enabled": incoming, "sms_enabled": sms}, "")
	requireStatus(t, reply, status)
	var out receptionConfig
	if status == http.StatusOK {
		if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func getReceptionAs(t *testing.T, api *Server, token string) receptionConfig {
	t.Helper()
	reply := requestJSON(t, api.Handler(), "GET", "/v1/devices/self/reception", token, nil, "")
	requireStatus(t, reply, http.StatusOK)
	var out receptionConfig
	if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// offerIncoming offers an incoming call as a gateway that sends what is left
// of its ring window, and returns the reply.
func offerIncoming(t *testing.T, api *Server, f integrationFixture, remaining *int64) (rtcCreateCall, *jsonReply) {
	t.Helper()
	body := rtcCreateCall{Version: 1, OperationID: newUUID(), CallID: newUUID(), GatewayID: f.gatewayID, SIMID: f.simID,
		MappingRevision: 1, To: "15550100200", RemainingRingMS: remaining}
	reply := requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/incoming", f.gatewayToken, body, "")
	return body, &jsonReply{reply.Code, reply.Body.Bytes()}
}

type jsonReply struct {
	code int
	body []byte
}

func (r *jsonReply) snapshot(t *testing.T) rtcControlSnapshot {
	t.Helper()
	var out rtcControlSnapshot
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func int64Pointer(value int64) *int64 { return &value }

func TestPostgresReceptionV2KeepsIncomingWithoutRefreshAndSeparatesOutgoing(t *testing.T) {
	db, f, api := controlFixture(t)
	handler := api.Handler()
	if !containsJSON(requestJSON(t, handler, "GET", "/v1/capabilities", "", nil, "").Body.Bytes(), `"reception_v2":true`) {
		t.Fatal("reception v2 not advertised")
	}
	requireStatus(t, requestJSON(t, handler, "GET", "/v1/devices/self/reception", f.gatewayToken, nil, ""), http.StatusForbidden)

	// Before a write, the device reads as version 1 following its capability.
	if got := getReceptionAs(t, api, f.clientToken); got != (receptionConfig{Version: 1, IncomingEnabled: true, SMSEnabled: true}) {
		t.Fatalf("legacy view: %+v", got)
	}
	requireStatus(t, requestJSON(t, handler, "PUT", "/v1/devices/self/reception", f.clientToken,
		map[string]any{"version": 2, "expected_revision": 0, "incoming_enabled": true}, ""), http.StatusBadRequest)
	config := putReceptionAs(t, api, f.clientToken, 0, true, true, http.StatusOK)
	if config != (receptionConfig{Version: 2, IncomingEnabled: true, SMSEnabled: true, Revision: 1}) {
		t.Fatalf("first write: %+v", config)
	}
	// A write that lost its answer and is sent again conflicts; the client
	// reads the current value instead.
	putReceptionAs(t, api, f.clientToken, 0, true, true, http.StatusConflict)

	// Long past any capability refresh, a v2 client is still offered calls.
	if _, err := db.db.Exec(`DELETE FROM rtc_control_capabilities WHERE device_id=$1`, f.clientID); err != nil {
		t.Fatal(err)
	}
	body, reply := offerIncoming(t, api, f, int64Pointer(12000))
	if reply.code != http.StatusCreated {
		t.Fatalf("incoming to a v2 client: %d %s", reply.code, reply.body)
	}
	call := reply.snapshot(t)
	window := call.ExpiresAt.Sub(call.CreatedAt)
	if window < 11*time.Second || window > 12500*time.Millisecond || call.RemainingMS == nil || *call.RemainingMS > 12000 {
		t.Fatalf("ring window %v remaining %v, want what the gateway had left", window, call.RemainingMS)
	}
	// A retry with less left replays the first answer; the window does not move.
	body.RemainingRingMS = int64Pointer(9000)
	retry := requestJSON(t, handler, "POST", "/v1/rtc/control/incoming", f.gatewayToken, body, "")
	requireStatus(t, retry, http.StatusOK)
	var replayed rtcControlSnapshot
	_ = json.Unmarshal(retry.Body.Bytes(), &replayed)
	if replayed.CallID != call.CallID || !replayed.ExpiresAt.Equal(*call.ExpiresAt) {
		t.Fatal("a retry changed the call or its window")
	}
	if got := controlGet(t, api, f.clientToken, call.CallID); got.State != "ringing" || got.RemainingMS == nil {
		t.Fatalf("client view of the ringing call: %+v", got)
	}

	// Turning incoming calls off withdraws this phone from the ringing call
	// without hanging it up, and keeps SMS on.
	config = putReceptionAs(t, api, f.clientToken, 1, false, true, http.StatusOK)
	if config.IncomingEnabled || !config.SMSEnabled || config.Revision != 2 {
		t.Fatalf("incoming off: %+v", config)
	}
	if got := controlGet(t, api, f.clientToken, call.CallID); got.State != "ended" || got.Reason == nil || *got.Reason != "reception_disabled" {
		t.Fatalf("client still in the call: %+v", got)
	}
	gatewayView := controlGet(t, api, f.gatewayToken, call.CallID)
	if gatewayView.State != "ringing" || gatewayView.EndRequested {
		t.Fatalf("turning reception off hung the call up: %+v", gatewayView)
	}
	controlOp(t, api, f.clientToken, call, "accept", 409)

	// An older-protocol capability write does not turn calls back on.
	requireStatus(t, requestJSON(t, handler, "POST", "/v1/rtc/control/capability", f.clientToken, map[string]any{"version": 1, "enabled": true}, ""), 200)
	controlOp(t, api, f.gatewayToken, gatewayView, "ended", 200)
	_, reply = offerIncoming(t, api, f, int64Pointer(20000))
	if reply.code != http.StatusServiceUnavailable || !strings.Contains(string(reply.body), "NO_INCOMING_SUBSCRIBERS") {
		t.Fatalf("no subscriber, new gateway: %d %s", reply.code, reply.body)
	}
	_, reply = offerIncoming(t, api, f, nil)
	if reply.code != http.StatusServiceUnavailable || !strings.Contains(string(reply.body), "GATEWAY_UNAVAILABLE") {
		t.Fatalf("no subscriber, older gateway: %d %s", reply.code, reply.body)
	}
	_, reply = offerIncoming(t, api, f, int64Pointer(0))
	if reply.code != http.StatusBadRequest {
		t.Fatalf("an empty ring window was accepted: %d", reply.code)
	}

	// Calling out does not need incoming calls on.
	if _, err := db.db.Exec(`DELETE FROM rtc_control_capabilities WHERE device_id=$1`, f.clientID); err != nil {
		t.Fatal(err)
	}
	outgoing := controlCreate(t, f, api, false)
	if outgoing.Direction != "outgoing" {
		t.Fatalf("outgoing call: %+v", outgoing)
	}
	if got := getReceptionAs(t, api, f.clientToken); got.IncomingEnabled || got.Revision != 2 {
		t.Fatalf("calling out changed reception: %+v", got)
	}
}

func TestPostgresReceptionV2CallAcceptedFirstSurvivesTurningIncomingOff(t *testing.T) {
	_, f, api := controlFixture(t)
	putReceptionAs(t, api, f.clientToken, 0, true, true, http.StatusOK)
	_, reply := offerIncoming(t, api, f, int64Pointer(20000))
	call := reply.snapshot(t)
	call = controlOp(t, api, f.clientToken, call, "accept", 200)
	putReceptionAs(t, api, f.clientToken, 1, false, true, http.StatusOK)
	if got := controlGet(t, api, f.clientToken, call.CallID); got.State != "connecting" || got.CandidateID != f.clientID {
		t.Fatalf("an accepted call was withdrawn: %+v", got)
	}
}

func receptionPushFixture(t *testing.T) (*integrationDatabase, integrationFixture, *Server, *fakePush) {
	t.Helper()
	db := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, db.db)
	fake := &fakePush{}
	api := NewWithOptions(db.db, nil, Options{Push: fake})
	t.Cleanup(api.ShutdownWebSockets)
	requireStatus(t, requestJSON(t, api.Handler(), "POST", "/v1/rtc/control/capability", f.gatewayToken, map[string]any{"version": 1, "enabled": true}, ""), 200)
	rtcRegister(t, api, f.gatewayToken)
	reply := requestJSON(t, api.Handler(), "PUT", "/v1/devices/self/push-endpoint", f.clientToken,
		map[string]string{"endpoint": "https://relay.example/v1/w/sealed"}, "")
	requireStatus(t, reply, 200)
	if !strings.Contains(reply.Body.String(), `"endpoint_generation":1`) {
		t.Fatalf("registration: %s", reply.Body.String())
	}
	putReceptionAs(t, api, f.clientToken, 0, true, true, http.StatusOK)
	return db, f, api, fake
}

func pushOnce(t *testing.T, api *Server) {
	t.Helper()
	if err := api.pushPending(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresReceptionV2AnnouncesEachCallAndSMSApart(t *testing.T) {
	_, f, api, fake := receptionPushFixture(t)
	_, reply := offerIncoming(t, api, f, int64Pointer(20000))
	first := reply.snapshot(t)
	pushOnce(t, api)
	wakes := fake.wakesSent()
	if len(wakes) != 1 || fake.sentCount() != 0 {
		t.Fatalf("wake-ups: %+v legacy %v", wakes, fake.sent)
	}
	wake := wakes[0]
	if wake.Kind != "call" || wake.CallID != first.CallID || wake.DeviceID != f.clientID || wake.Generation != 1 ||
		wake.Urgency != "high" || wake.TTL > 20*time.Second || wake.TTL < 15*time.Second || !strings.HasPrefix(wake.WakeID, "call-event-") {
		t.Fatalf("call wake-up: %+v", wake)
	}
	pushOnce(t, api)
	if len(fake.wakesSent()) != 1 {
		t.Fatal("an announced call was announced again")
	}

	// A new SMS is announced on its own, urgently only when one arrived.
	requireStatus(t, requestJSON(t, api.Handler(), http.MethodPost, "/v1/messages", f.clientToken,
		CreateMessageRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "+12025550123", Text: "hi", TTLSeconds: 300}, newUUID()), http.StatusAccepted)
	pushOnce(t, api)
	wakes = fake.wakesSent()
	if len(wakes) != 2 || wakes[1].Kind != "sms" || wakes[1].CallID != "" || wakes[1].Urgency != "normal" || wakes[1].TTL != pushSMSTTL {
		t.Fatalf("sms wake-up: %+v", wakes)
	}

	// The cellular call ends: its end is announced while it could still ring.
	gatewayView := controlGet(t, api, f.gatewayToken, first.CallID)
	controlOp(t, api, f.gatewayToken, gatewayView, "ended", 200)
	// The next call's wake-ups fail. The end of the first call is sent once,
	// and not again while the second is retried.
	_, reply = offerIncoming(t, api, f, int64Pointer(20000))
	second := reply.snapshot(t)
	fake.mu.Lock()
	fake.wakeErr = func(w push.Wake) error {
		if w.CallID == second.CallID {
			return errors.New("relay unavailable")
		}
		return nil
	}
	fake.mu.Unlock()
	pushOnce(t, api)
	pushOnce(t, api)
	ends := 0
	for _, w := range fake.wakesSent() {
		if w.CallID == second.CallID {
			t.Fatal("a failing wake-up was recorded as sent")
		}
		if w.CallID == first.CallID && w.Revision > wake.Revision {
			ends++
		}
	}
	if ends != 1 {
		t.Fatalf("end of the first call announced %d times", ends)
	}
	var callFailures int
	if err := api.db.QueryRow(`SELECT call_failures FROM push_endpoints WHERE device_id=$1`, f.clientID).Scan(&callFailures); err != nil || callFailures == 0 {
		t.Fatalf("call failures not counted: %d %v", callFailures, err)
	}
	fake.mu.Lock()
	fake.wakeErr = nil
	fake.mu.Unlock()
	time.Sleep(pushCallRetryAfter + 100*time.Millisecond)
	pushOnce(t, api)
	got := 0
	for _, w := range fake.wakesSent() {
		if w.CallID == second.CallID {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("the retried call was announced %d times", got)
	}
}

func TestPostgresReceptionV2SMSOffHoldsSMSWakeUpsUntilBackOn(t *testing.T) {
	_, f, api, fake := receptionPushFixture(t)
	putReceptionAs(t, api, f.clientToken, 1, true, false, http.StatusOK)
	for _, text := range []string{"one", "two"} {
		requireStatus(t, requestJSON(t, api.Handler(), http.MethodPost, "/v1/messages", f.clientToken,
			CreateMessageRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "+12025550123", Text: text, TTLSeconds: 300}, newUUID()), http.StatusAccepted)
	}
	pushOnce(t, api)
	if len(fake.wakesSent()) != 0 || fake.sentCount() != 0 {
		t.Fatalf("SMS wake-ups while SMS is off: %+v", fake.wakesSent())
	}
	// Calls still ring.
	offerIncoming(t, api, f, int64Pointer(20000))
	pushOnce(t, api)
	if wakes := fake.wakesSent(); len(wakes) != 1 || wakes[0].Kind != "call" {
		t.Fatalf("call while SMS is off: %+v", wakes)
	}
	// Turned back on, the backlog is one hint.
	putReceptionAs(t, api, f.clientToken, 2, true, true, http.StatusOK)
	pushOnce(t, api)
	pushOnce(t, api)
	sms := 0
	for _, w := range fake.wakesSent() {
		if w.Kind == "sms" {
			sms++
		}
	}
	if sms != 1 {
		t.Fatalf("SMS backlog announced %d times", sms)
	}
}

func TestPostgresPushEndpointGenerationsAndLegacyDevices(t *testing.T) {
	_, f, api, fake := receptionPushFixture(t)
	handler := api.Handler()
	register := func(endpoint string) string {
		t.Helper()
		reply := requestJSON(t, handler, "PUT", "/v1/devices/self/push-endpoint", f.clientToken, map[string]string{"endpoint": endpoint}, "")
		requireStatus(t, reply, 200)
		return reply.Body.String()
	}
	if got := register("https://relay.example/v1/w/sealed"); !strings.Contains(got, `"endpoint_generation":1`) {
		t.Fatalf("same endpoint changed generation: %s", got)
	}
	if got := register("https://relay.example/v1/w/other"); !strings.Contains(got, `"endpoint_generation":2`) {
		t.Fatalf("new endpoint kept generation: %s", got)
	}
	status := requestJSON(t, handler, "GET", "/v1/devices/self/push-endpoint", f.clientToken, nil, "")
	if !containsJSON(status.Body.Bytes(), `"endpoint_generation":2`) || !strings.Contains(status.Body.String(), `"call_failures":0`) {
		t.Fatalf("status: %s", status.Body.String())
	}
	_, reply := offerIncoming(t, api, f, int64Pointer(20000))
	call := reply.snapshot(t)
	pushOnce(t, api)
	if wakes := fake.wakesSent(); len(wakes) != 1 || wakes[0].Generation != 2 {
		t.Fatalf("wake-up for the new generation: %+v", wakes)
	}
	// Back on version 1, the device gets wire 1 hints again.
	controlOp(t, api, f.gatewayToken, controlGet(t, api, f.gatewayToken, call.CallID), "ended", 200)
	requireStatus(t, requestJSON(t, handler, "PUT", "/v1/devices/self/reception", f.clientToken,
		map[string]any{"version": 1, "expected_revision": 1, "incoming_enabled": true, "sms_enabled": true}, ""), 200)
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/messages", f.clientToken,
		CreateMessageRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "+12025550123", Text: "hi", TTLSeconds: 300}, newUUID()), http.StatusAccepted)
	pushOnce(t, api)
	if fake.sentCount() != 1 || len(fake.wakesSent()) != 1 {
		t.Fatalf("version 1 device: legacy %v wire2 %d", fake.sent, len(fake.wakesSent()))
	}
}

func readWakeFrame(t *testing.T, conn *websocket.Conn, within time.Duration) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal("read wake hint:", err)
	}
	return string(message)
}

func TestPostgresReceptionV2WebSocketSaysWhatChangedAndCallsWakeAtOnce(t *testing.T) {
	_, f, api := controlFixture(t)
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	putReceptionAs(t, api, f.clientToken, 0, true, false, http.StatusOK)
	quiet := dialWake(t, server.URL, f.clientToken)
	requireWakeHint(t, quiet)
	// With SMS wake-ups off, a new message says nothing.
	createWakeTestMessage(t, api.Handler(), f.clientToken, f.gatewayID, f.simID, newUUID())
	requireNoWake(t, quiet)
	quiet.Close()

	conn := dialWake(t, server.URL, f.clientToken)
	defer conn.Close()
	requireWakeHint(t, conn)
	createWakeTestMessage(t, api.Handler(), f.clientToken, f.gatewayID, f.simID, newUUID())
	// A call is announced as soon as it is committed, not at the next poll,
	// and without the message.
	if _, reply := offerIncoming(t, api, f, int64Pointer(20000)); reply.code != http.StatusCreated {
		t.Fatalf("incoming: %d %s", reply.code, reply.body)
	}
	if frame := readWakeFrame(t, conn, 500*time.Millisecond); frame != `{"domains":["call"],"protocol_version":1,"type":"sync_required"}` {
		t.Fatalf("call hint: %s", frame)
	}
	// SMS back on: the message held back is announced.
	putReceptionAs(t, api, f.clientToken, 1, true, true, http.StatusOK)
	if frame := readWakeFrame(t, conn, 3*time.Second); frame != `{"domains":["sms"],"protocol_version":1,"type":"sync_required"}` {
		t.Fatalf("sms hint: %s", frame)
	}
}
