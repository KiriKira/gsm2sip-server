package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/calls"
)

func TestPostgresCallControllerKeepsPrincipalAndLegacyIntentContract(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	controller := &stubCallController{configured: true}
	controller.intent = calls.Intent{
		IntentID:  "11111111-1111-4111-8111-111111111111",
		CallID:    "22222222-2222-4222-8222-222222222222",
		SIPURI:    "sips:call.authorization@example.test",
		ExpiresAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
	}
	handler := NewWithOptions(database.db, nil, Options{Calls: controller}).Handler()
	body := calls.CreateIntentRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1,
		To: "+15551234567", CallID: controller.intent.CallID}
	key := "controller-intent-test-0001"

	response := requestJSON(t, handler, http.MethodPost, "/v1/call-intents", f.clientToken, body, key)
	requireStatus(t, response, http.StatusAccepted)
	var intent calls.Intent
	if err := json.Unmarshal(response.Body.Bytes(), &intent); err != nil {
		t.Fatal(err)
	}
	if intent != controller.intent || controller.ownerID != f.ownerID || controller.clientID != f.clientID ||
		controller.request != body || controller.key != key || controller.creates != 1 {
		t.Fatal("controller lost authenticated principal, request identity or the existing intent response")
	}

	wrongRole := requestJSON(t, handler, http.MethodPost, "/v1/call-intents", f.gatewayToken, body, key)
	requireStatus(t, wrongRole, http.StatusForbidden)
	unauthenticated := requestJSON(t, handler, http.MethodPost, "/v1/call-intents", "", body, key)
	requireStatus(t, unauthenticated, http.StatusUnauthorized)
	if controller.creates != 1 {
		t.Fatal("an unauthorized request reached the call controller")
	}
}

func TestPostgresUnavailableVoiceControllerDoesNotDisableGatewaySyncOrHistory(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	controller := &stubCallController{configured: false}
	handler := NewWithOptions(database.db, nil, Options{Calls: controller}).Handler()
	body := calls.CreateIntentRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "+15551234567"}
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/call-intents", f.clientToken, body,
		"disabled-controller-0001"), http.StatusServiceUnavailable)
	if controller.creates != 0 {
		t.Fatal("an unconfigured voice controller accepted a new call")
	}
	requireStatus(t, requestJSON(t, handler, http.MethodGet, "/v1/gateways", f.clientToken, nil, ""), http.StatusOK)
	requireStatus(t, requestJSON(t, handler, http.MethodGet, "/v1/calls", f.clientToken, nil, ""), http.StatusOK)
	if controller.ownerID != f.ownerID || controller.clientID != f.clientID {
		t.Fatal("history lost the authenticated client scope while voice was unavailable")
	}
}

func TestNilLegacyCallControllerRemainsUnconfigured(t *testing.T) {
	var manager *calls.Manager
	api := NewWithOptions(nil, nil, Options{Calls: manager})
	if api.calls != nil {
		t.Fatal("a nil legacy manager became a non-nil controller")
	}
	for _, handler := range []func(http.ResponseWriter, *http.Request, Principal){
		api.getCall, api.listCalls, api.cancelCallIntent,
	} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/", nil), Principal{Role: "client"})
		requireStatus(t, response, http.StatusServiceUnavailable)
	}
}

// A controller without ARI, sockets or a media implementation. The handlers still
// enforce authentication and keep the published v1 wire contract around this port.
type stubCallController struct {
	configured             bool
	creates                int
	ownerID, clientID, key string
	request                calls.CreateIntentRequest
	intent                 calls.Intent
}

func (s *stubCallController) Configured() bool { return s.configured }
func (s *stubCallController) CreateIntent(_ context.Context, ownerID, clientID string, request calls.CreateIntentRequest, key string) (calls.Intent, error) {
	s.creates++
	s.ownerID, s.clientID, s.request, s.key = ownerID, clientID, request, key
	return s.intent, nil
}
func (s *stubCallController) ListCalls(_ context.Context, ownerID, clientID, _ string, _ int) ([]calls.Call, string, error) {
	s.ownerID, s.clientID = ownerID, clientID
	return []calls.Call{}, "", nil
}
func (*stubCallController) CancelIntent(context.Context, string, string, string) error {
	return calls.ErrNotFound
}
func (*stubCallController) GetCall(context.Context, string, string, string) (calls.Call, error) {
	return calls.Call{}, calls.ErrNotFound
}
func (*stubCallController) ClientReady(context.Context, string, string, string, string, string) (calls.Call, error) {
	return calls.Call{}, calls.ErrNotReady
}
