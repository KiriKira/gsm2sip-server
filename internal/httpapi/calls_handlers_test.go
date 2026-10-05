package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	callsvc "github.com/kirikira/gsm2sip-server/internal/calls"
)

func TestCallHandlersRejectWrongRoleAndNonVisibleIdempotencyKey(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	ariServer := httptest.NewServer(http.NotFoundHandler())
	defer ariServer.Close()
	calls, err := callsvc.NewManager(database.db, callsvc.Config{
		ARIURL: ariServer.URL + "/ari", ARIUsername: "test", ARIPassword: "test-secret",
		ARIApplication: "gsm2sip", SIPRealm: "gsm2sip", TokenKey: []byte("api-test-token-key-that-is-at-least-32-bytes"),
	})
	if err != nil {
		t.Fatalf("create configured call manager: %v", err)
	}
	handler := NewWithOptions(database.db, nil, Options{Calls: calls}).Handler()
	body := callsvc.CreateIntentRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1, To: "+15551234567"}

	wrongRole := requestJSON(t, handler, http.MethodPost, "/v1/call-intents", f.gatewayToken, body, "visible-key-should-not-matter")
	requireStatus(t, wrongRole, http.StatusForbidden)

	invalidKey := requestJSON(t, handler, http.MethodPost, "/v1/call-intents", f.clientToken, body, "visible-key-with-tab\tbad")
	requireStatus(t, invalidKey, http.StatusBadRequest)
	if !strings.Contains(invalidKey.Body.String(), "IDEMPOTENCY_KEY_REQUIRED") {
		t.Fatalf("non-visible idempotency key returned an unexpected error: %s", invalidKey.Body.String())
	}
}
