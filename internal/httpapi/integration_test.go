package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/db"
)

type integrationDatabase struct {
	db    *sql.DB
	admin *sql.DB
}

func openIntegrationDatabase(t *testing.T) *integrationDatabase {
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
	schema := "test_" + strings.ReplaceAll(newUUID(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		_ = admin.Close()
		t.Fatal("create isolated integration schema")
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
		t.Fatal("open isolated integration database")
	}
	if err := db.Migrate(context.Background(), isolated); err != nil {
		_ = isolated.Close()
		_, _ = admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`)
		_ = admin.Close()
		t.Fatal("migrate isolated integration schema")
	}
	t.Cleanup(func() {
		_ = isolated.Close()
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
		_ = admin.Close()
	})
	return &integrationDatabase{db: isolated, admin: admin}
}

type integrationFixture struct {
	ownerID, clientID, gatewayID, simID                      string
	otherOwnerID, otherClientID, otherGatewayID, otherSIMID  string
	clientToken, gatewayToken, otherToken, otherGatewayToken string
}

func seedIntegrationFixture(t *testing.T, database *sql.DB) integrationFixture {
	t.Helper()
	f := integrationFixture{
		ownerID: newUUID(), clientID: newUUID(), gatewayID: newUUID(), simID: newUUID(),
		otherOwnerID: newUUID(), otherClientID: newUUID(), otherGatewayID: newUUID(), otherSIMID: newUUID(),
		clientToken: randomToken(), gatewayToken: randomToken(), otherToken: randomToken(), otherGatewayToken: randomToken(),
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO owners(id,display_name) VALUES ($1,'owner-a')`, []any{f.ownerID}},
		{`INSERT INTO owners(id,display_name) VALUES ($1,'owner-b')`, []any{f.otherOwnerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'client','client-a')`, []any{f.clientID, f.ownerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'gateway','gateway-a')`, []any{f.gatewayID, f.ownerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'client','client-b')`, []any{f.otherClientID, f.otherOwnerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'gateway','gateway-b')`, []any{f.otherGatewayID, f.otherOwnerID}},
		{`INSERT INTO gateways(device_id,mapping_revision,last_seen_at) VALUES ($1,1,now())`, []any{f.gatewayID}},
		{`INSERT INTO gateways(device_id,mapping_revision,last_seen_at) VALUES ($1,1,now())`, []any{f.otherGatewayID}},
		{`INSERT INTO sim_bindings(sim_id,owner_id,gateway_id,slot_index,label,state,identity_verified,mapping_revision,service_state) VALUES ($1,$2,$3,0,'Line A','active',true,1,'in_service')`, []any{f.simID, f.ownerID, f.gatewayID}},
		{`INSERT INTO sim_bindings(sim_id,owner_id,gateway_id,slot_index,label,state,identity_verified,mapping_revision,service_state) VALUES ($1,$2,$3,0,'Line B','active',true,1,'in_service')`, []any{f.otherSIMID, f.otherOwnerID, f.otherGatewayID}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`, []any{newUUID(), f.clientID, tokenHash(f.clientToken), tokenHash(randomToken())}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`, []any{newUUID(), f.gatewayID, tokenHash(f.gatewayToken), tokenHash(randomToken())}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`, []any{newUUID(), f.otherClientID, tokenHash(f.otherToken), tokenHash(randomToken())}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`, []any{newUUID(), f.otherGatewayID, tokenHash(f.otherGatewayToken), tokenHash(randomToken())}},
	} {
		if _, err := database.Exec(statement.query, statement.args...); err != nil {
			t.Fatal("seed integration database")
		}
	}
	return f
}

func requestJSON(t *testing.T, handler http.Handler, method, path, token string, payload any, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func requireStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("HTTP %d, want %d: %s", response.Code, want, response.Body.String())
	}
}

func makeGatewayEvent(gatewayID string, sequence int64, eventType string, simID *string, revision *int64, payload map[string]any) GatewayEvent {
	return GatewayEvent{
		ProtocolVersion: 1, EventID: newUUID(), GatewayID: gatewayID,
		Sequence: sequence, OccurredAt: time.Now().UTC(), Type: eventType,
		SIMID: simID, MappingRevision: revision, Payload: payload,
	}
}

func sendGatewayEvent(t *testing.T, handler http.Handler, gatewayID, gatewayToken string, event GatewayEvent) map[string]any {
	t.Helper()
	response := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+gatewayID+"/events:batch", gatewayToken,
		eventBatchRequest{Events: []GatewayEvent{event}}, "")
	requireStatus(t, response, http.StatusOK)
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func getGatewayCommand(t *testing.T, handler http.Handler, gatewayID, gatewayToken, commandID string) Command {
	t.Helper()
	response := requestJSON(t, handler, http.MethodGet,
		"/v1/gateways/"+gatewayID+"/commands?limit=100", gatewayToken, nil, "")
	requireStatus(t, response, http.StatusOK)
	var page struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, command := range page.Commands {
		if command.CommandID == commandID {
			return command
		}
	}
	t.Fatalf("command %s not present in gateway page", commandID)
	return Command{}
}

func TestPostgresSMSLifecycleScopeAndHeartbeatInvalidation(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	handler := New(database.db, nil).Handler()

	create := func(key string) MessageAccepted {
		t.Helper()
		response := requestJSON(t, handler, http.MethodPost, "/v1/messages", f.clientToken,
			CreateMessageRequest{
				GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1,
				To: "+8613800000000", Text: "two-part lifecycle 📱", TTLSeconds: 600,
			}, key)
		requireStatus(t, response, http.StatusAccepted)
		var accepted MessageAccepted
		if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
			t.Fatal(err)
		}
		return accepted
	}

	first := create("integration-message-key-0001")
	replayed := requestJSON(t, handler, http.MethodPost, "/v1/messages", f.clientToken,
		CreateMessageRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1,
			To: "+8613800000000", Text: "two-part lifecycle 📱", TTLSeconds: 600},
		"integration-message-key-0001")
	requireStatus(t, replayed, http.StatusAccepted)
	var replayedMessage MessageAccepted
	if err := json.Unmarshal(replayed.Body.Bytes(), &replayedMessage); err != nil {
		t.Fatal(err)
	}
	if replayedMessage.MessageID != first.MessageID || replayedMessage.CommandID != first.CommandID {
		t.Fatalf("idempotent replay created a new resource: first=%+v replay=%+v", first, replayedMessage)
	}
	commandList := requestJSON(t, handler, http.MethodGet,
		"/v1/gateways/"+f.gatewayID+"/commands?limit=10", f.gatewayToken, nil, "")
	requireStatus(t, commandList, http.StatusOK)
	var commandPage struct {
		Commands   []Command `json:"commands"`
		NextCursor *string   `json:"next_cursor"`
	}
	if err := json.Unmarshal(commandList.Body.Bytes(), &commandPage); err != nil {
		t.Fatal(err)
	}
	if len(commandPage.Commands) != 1 || commandPage.Commands[0].CommandID != first.CommandID {
		t.Fatalf("unexpected command page: %s", commandList.Body.String())
	}
	command := commandPage.Commands[0]
	if command.GatewayID != f.gatewayID || command.PayloadSHA256 != commandDigest(command) {
		t.Fatalf("command did not include stable server digest/scope: %+v", command)
	}
	claim := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/commands/"+command.CommandID+"/claim", f.gatewayToken,
		commandClaimRequest{PayloadSHA256: command.PayloadSHA256}, "")
	requireStatus(t, claim, http.StatusOK)
	var claimed Command
	if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.State != "accepted_by_gateway" || claimed.PayloadSHA256 != command.PayloadSHA256 {
		t.Fatalf("unexpected claim response: %+v", claimed)
	}

	detailResponse := requestJSON(t, handler, http.MethodGet, "/v1/messages/"+first.MessageID, f.clientToken, nil, "")
	requireStatus(t, detailResponse, http.StatusOK)
	var detail MessageDetail
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.CommandID == nil || *detail.CommandID != first.CommandID {
		t.Fatalf("MessageDetail did not resolve command through the join: %+v", detail)
	}
	foreignGet := requestJSON(t, handler, http.MethodGet, "/v1/messages/"+first.MessageID, f.otherToken, nil, "")
	requireStatus(t, foreignGet, http.StatusNotFound)

	revision := int64(1)
	simID := f.simID
	seq := int64(1)
	dispatch := makeGatewayEvent(f.gatewayID, seq, "sms.dispatching", &simID, &revision,
		map[string]any{"message_id": first.MessageID, "command_id": first.CommandID, "part_count": 2})
	ack := sendGatewayEvent(t, handler, f.gatewayID, f.gatewayToken, dispatch)
	acks, ok := ack["acks"].([]any)
	if !ok || len(acks) != 1 {
		t.Fatalf("event did not receive a durable ACK: %#v", ack)
	}
	var duplicateResponse map[string]any
	duplicateRecorder := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/events:batch", f.gatewayToken,
		eventBatchRequest{Events: []GatewayEvent{dispatch}}, "")
	requireStatus(t, duplicateRecorder, http.StatusOK)
	if err := json.Unmarshal(duplicateRecorder.Body.Bytes(), &duplicateResponse); err != nil {
		t.Fatal(err)
	}
	duplicateAcks := duplicateResponse["acks"].([]any)
	if len(duplicateAcks) != 1 || duplicateAcks[0].(map[string]any)["duplicate"] != true {
		t.Fatalf("replay was not acknowledged as duplicate: %#v", duplicateResponse)
	}

	seq++
	sendGatewayEvent(t, handler, f.gatewayID, f.gatewayToken, makeGatewayEvent(f.gatewayID, seq,
		"sms.part_state", &simID, &revision, map[string]any{
			"message_id": first.MessageID, "command_id": first.CommandID, "part_index": 0, "state": "failed", "error": "modem rejected part",
		}))
	var currentStatus string
	if err := database.db.QueryRow(`SELECT status FROM messages WHERE id=$1`, first.MessageID).Scan(&currentStatus); err != nil {
		t.Fatal(err)
	}
	if currentStatus != "dispatching" {
		t.Fatalf("failed part hid the still-dispatching second part: status=%s", currentStatus)
	}
	seq++
	sendGatewayEvent(t, handler, f.gatewayID, f.gatewayToken, makeGatewayEvent(f.gatewayID, seq,
		"sms.part_state", &simID, &revision, map[string]any{
			"message_id": first.MessageID, "command_id": first.CommandID, "part_index": 1, "state": "unknown", "error": "callback missing after restart",
		}))
	seq++
	sendGatewayEvent(t, handler, f.gatewayID, f.gatewayToken, makeGatewayEvent(f.gatewayID, seq,
		"sms.part_state", &simID, &revision, map[string]any{
			"message_id": first.MessageID, "command_id": first.CommandID, "part_index": 1, "state": "delivered", "result_code": 0,
		}))
	seq++
	// Enrich a terminal part with a late modem result; the message remains failed,
	// but the owner event feed must still publish the changed per-part detail.
	sendGatewayEvent(t, handler, f.gatewayID, f.gatewayToken, makeGatewayEvent(f.gatewayID, seq,
		"sms.part_state", &simID, &revision, map[string]any{
			"message_id": first.MessageID, "command_id": first.CommandID, "part_index": 0, "state": "failed", "result_code": 5,
		}))
	detailResponse = requestJSON(t, handler, http.MethodGet, "/v1/messages/"+first.MessageID, f.clientToken, nil, "")
	requireStatus(t, detailResponse, http.StatusOK)
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Status != "failed" || detail.PartCount == nil || *detail.PartCount != 2 || len(detail.Parts) != 2 ||
		detail.Parts[0].State != "failed" || detail.Parts[0].ResultCode == nil || *detail.Parts[0].ResultCode != 5 ||
		detail.Parts[1].State != "delivered" {
		t.Fatalf("late evidence was not aggregated into detail: %+v", detail)
	}
	eventPage := requestJSON(t, handler, http.MethodGet, "/v1/events?limit=100", f.clientToken, nil, "")
	requireStatus(t, eventPage, http.StatusOK)
	var eventResult struct {
		Items []struct {
			Type    string        `json:"type"`
			Message MessageDetail `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal(eventPage.Body.Bytes(), &eventResult); err != nil {
		t.Fatal(err)
	}
	if len(eventResult.Items) < 7 || eventResult.Items[0].Type != "message.created" || eventResult.Items[0].Message.Status != "failed" {
		t.Fatalf("old creation event did not contain the current message projection: %s", eventPage.Body.String())
	}
	last := eventResult.Items[len(eventResult.Items)-1]
	if last.Message.Parts[0].ResultCode == nil || *last.Message.Parts[0].ResultCode != 5 {
		t.Fatalf("same-state part enrichment was not visible in the event feed: %+v", last.Message.Parts)
	}

	second := create("integration-message-key-0002")
	secondCommand := getGatewayCommand(t, handler, f.gatewayID, f.gatewayToken, second.CommandID)
	secondClaim := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/commands/"+second.CommandID+"/claim", f.gatewayToken,
		commandClaimRequest{PayloadSHA256: secondCommand.PayloadSHA256}, "")
	requireStatus(t, secondClaim, http.StatusOK)
	if _, err := database.db.Exec(`UPDATE commands SET expires_at=now()-interval '1 second' WHERE id=$1`, second.CommandID); err != nil {
		t.Fatal(err)
	}
	if expired, err := New(database.db, nil).ExpireQueued(context.Background()); err != nil || expired != 0 {
		t.Fatalf("expiry worker changed a claimed command: expired=%d err=%v", expired, err)
	}
	if err := database.db.QueryRow(`SELECT status FROM commands WHERE id=$1`, second.CommandID).Scan(&currentStatus); err != nil {
		t.Fatal(err)
	}
	if currentStatus != "accepted_by_gateway" {
		t.Fatalf("claimed command was reassigned/expired: %s", currentStatus)
	}

	third := create("integration-message-key-0003")
	thirdCommand := getGatewayCommand(t, handler, f.gatewayID, f.gatewayToken, third.CommandID)
	heartbeat := HeartbeatRequest{Sequence: 1, ProtocolVersion: 1, AppVersion: "test", Root: true,
		SIPRegistered: false, SIMStates: []SIMState{{SIMID: f.simID, MappingRevision: 1, ServiceState: "absent", IdentityVerified: false}}}
	heartbeatResponse := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/heartbeat", f.gatewayToken, heartbeat, "")
	requireStatus(t, heartbeatResponse, http.StatusOK)
	var heartbeatResult HeartbeatResponse
	if err := json.Unmarshal(heartbeatResponse.Body.Bytes(), &heartbeatResult); err != nil {
		t.Fatal(err)
	}
	if heartbeatResult.MappingRevision != 2 || len(heartbeatResult.InvalidatedSIMIDs) != 1 || heartbeatResult.InvalidatedSIMIDs[0] != f.simID {
		t.Fatalf("heartbeat did not revoke unsafe mapping: %+v", heartbeatResult)
	}
	blockedClaim := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/commands/"+third.CommandID+"/claim", f.gatewayToken,
		commandClaimRequest{PayloadSHA256: thirdCommand.PayloadSHA256}, "")
	requireStatus(t, blockedClaim, http.StatusConflict)
	if !strings.Contains(blockedClaim.Body.String(), "SIM_MAPPING_CHANGED") {
		t.Fatalf("claim did not fail closed after heartbeat invalidation: %s", blockedClaim.Body.String())
	}
	blockedCreate := requestJSON(t, handler, http.MethodPost, "/v1/messages", f.clientToken,
		CreateMessageRequest{GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1,
			To: "+8613800000000", Text: "must remain blocked", TTLSeconds: 300},
		"integration-message-key-0004")
	requireStatus(t, blockedCreate, http.StatusConflict)
	if !strings.Contains(blockedCreate.Body.String(), "SIM_UNAVAILABLE") {
		t.Fatalf("new SMS did not fail closed after heartbeat invalidation: %s", blockedCreate.Body.String())
	}
	if err := database.db.QueryRow(`SELECT status FROM commands WHERE id=$1`, third.CommandID).Scan(&currentStatus); err != nil {
		t.Fatal(err)
	}
	if currentStatus != "failed" {
		t.Fatalf("invalidated command was not terminally rejected: %s", currentStatus)
	}

	// The first inbound event mutates a message before the second conflicts. The
	// whole batch must roll back, including the first event's message and feed row.
	messageBefore := newUUID()
	conflictingMessage := newUUID()
	invalidSIM := newUUID()
	badBatch := eventBatchRequest{Events: []GatewayEvent{
		makeGatewayEvent(f.gatewayID, seq+1, "sms.received", nil, nil, map[string]any{
			"message_id": messageBefore, "from": "+12025550100", "text": "rollback me", "parts": 1,
		}),
		makeGatewayEvent(f.gatewayID, seq+2, "sms.received", &invalidSIM, &revision, map[string]any{
			"message_id": conflictingMessage, "from": "+12025550101", "text": "invalid mapping", "parts": 1,
		}),
	}}
	badBatch.Events[0].SIMResolution = stringPointer("unknown")
	var eventsBefore int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM gateway_events WHERE gateway_id=$1`, f.gatewayID).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	var clientEventsBefore int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM server_events WHERE owner_id=$1`, f.ownerID).Scan(&clientEventsBefore); err != nil {
		t.Fatal(err)
	}
	rollbackResponse := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/events:batch", f.gatewayToken, badBatch, "")
	requireStatus(t, rollbackResponse, http.StatusConflict)
	var messageCount, eventsAfter, clientEventsAfter int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=$1`, messageBefore).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM gateway_events WHERE gateway_id=$1`, f.gatewayID).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM server_events WHERE owner_id=$1`, f.ownerID).Scan(&clientEventsAfter); err != nil {
		t.Fatal(err)
	}
	if messageCount != 0 || eventsAfter != eventsBefore || clientEventsAfter != clientEventsBefore {
		t.Fatalf("failed batch partially committed: message_count=%d gateway_events=%d/%d client_events=%d/%d", messageCount, eventsAfter, eventsBefore, clientEventsAfter, clientEventsBefore)
	}

	otherFeed := requestJSON(t, handler, http.MethodGet, "/v1/events", f.otherToken, nil, "")
	requireStatus(t, otherFeed, http.StatusOK)
	if !strings.Contains(otherFeed.Body.String(), `"items":[]`) {
		t.Fatalf("owner event feed leaked another owner's events: %s", otherFeed.Body.String())
	}
}

func TestPostgresPairingRefreshAndTwoPhaseSIMBinding(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	handler := New(database.db, nil).Handler()

	operationID := newUUID()
	proposal := simBindingEnvelope{OperationID: operationID, Phase: "propose", Mappings: []SIMProposal{
		{SlotIndex: 1, Label: "Moved primary", ExistingSIMID: stringPointer(f.simID), SameSIMVerified: true},
		{SlotIndex: 0, Label: "Second line"},
	}}
	proposalResponse := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/sim-bindings", f.gatewayToken, proposal, "")
	requireStatus(t, proposalResponse, http.StatusOK)
	var proposed SIMBindingResult
	if err := json.Unmarshal(proposalResponse.Body.Bytes(), &proposed); err != nil {
		t.Fatal(err)
	}
	if proposed.Phase != "proposed" || proposed.MappingRevision != 2 || len(proposed.Mappings) != 2 || proposed.Mappings[0].SIMID != f.simID {
		t.Fatalf("SIM proposal did not preserve verified same-card identity: %+v", proposed)
	}
	proposalReplay := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/sim-bindings", f.gatewayToken, proposal, "")
	requireStatus(t, proposalReplay, http.StatusOK)
	var replayedProposal SIMBindingResult
	if err := json.Unmarshal(proposalReplay.Body.Bytes(), &replayedProposal); err != nil {
		t.Fatal(err)
	}
	if replayedProposal.MappingRevision != proposed.MappingRevision || replayedProposal.Mappings[1].SIMID != proposed.Mappings[1].SIMID {
		t.Fatalf("proposal replay issued different SIM identities: first=%+v replay=%+v", proposed, replayedProposal)
	}
	confirm := simBindingEnvelope{OperationID: operationID, Phase: "confirm", Confirmations: []SIMConfirmation{
		{SIMID: proposed.Mappings[0].SIMID, SlotIndex: 1, Confirmed: true},
		{SIMID: proposed.Mappings[1].SIMID, SlotIndex: 0, Confirmed: false},
	}}
	confirmResponse := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/sim-bindings", f.gatewayToken, confirm, "")
	requireStatus(t, confirmResponse, http.StatusOK)
	var confirmed SIMBindingResult
	if err := json.Unmarshal(confirmResponse.Body.Bytes(), &confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed.Phase != "confirmed" || len(confirmed.Mappings) != 2 || confirmed.Mappings[0].State != "active" || confirmed.Mappings[1].State != "unverified" {
		t.Fatalf("local confirmation states were not enforced: %+v", confirmed)
	}
	confirmReplay := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+f.gatewayID+"/sim-bindings", f.gatewayToken, confirm, "")
	requireStatus(t, confirmReplay, http.StatusOK)
	var replayedConfirmation SIMBindingResult
	if err := json.Unmarshal(confirmReplay.Body.Bytes(), &replayedConfirmation); err != nil {
		t.Fatal(err)
	}
	if replayedConfirmation.Mappings[1].State != "unverified" {
		t.Fatalf("confirmation replay changed result: %+v", replayedConfirmation)
	}
	simList := requestJSON(t, handler, http.MethodGet, "/v1/gateways/"+f.gatewayID+"/sims", f.clientToken, nil, "")
	requireStatus(t, simList, http.StatusOK)
	var listed struct {
		MappingRevision int64               `json:"mapping_revision"`
		SIMs            []SIMBindingSummary `json:"sims"`
	}
	if err := json.Unmarshal(simList.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.MappingRevision != 2 || len(listed.SIMs) != 2 {
		t.Fatalf("unexpected SIM list after two-phase mapping: %s", simList.Body.String())
	}

	pairingCode := randomToken()
	if _, err := database.db.Exec(`INSERT INTO pairing_codes(id,owner_id,role,code_hash,expires_at) VALUES ($1,$2,'gateway',$3,now()+interval '10 minutes')`, newUUID(), f.ownerID, tokenHash(pairingCode)); err != nil {
		t.Fatal("seed one-time pairing code")
	}
	pairedResponse := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: pairingCode, DeviceName: "paired gateway"}, "")
	requireStatus(t, pairedResponse, http.StatusOK)
	var paired PairingClaimResponse
	if err := json.Unmarshal(pairedResponse.Body.Bytes(), &paired); err != nil {
		t.Fatal(err)
	}
	if paired.Role != "gateway" || paired.DeviceID == "" || paired.SIP.Available || paired.SIP.Reason != "sip_not_configured" {
		t.Fatalf("pairing response claimed unavailable SIP capability: %+v", paired)
	}
	var gatewayExists bool
	if err := database.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM gateways WHERE device_id=$1)`, paired.DeviceID).Scan(&gatewayExists); err != nil || !gatewayExists {
		t.Fatalf("paired gateway identity did not initialize: exists=%t err=%v", gatewayExists, err)
	}
	refreshResponse := requestJSON(t, handler, http.MethodPost, "/v1/auth/refresh", "",
		RefreshRequest{RefreshToken: paired.RefreshToken}, "")
	requireStatus(t, refreshResponse, http.StatusOK)
	var rotated SessionTokens
	if err := json.Unmarshal(refreshResponse.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.AccessToken == paired.AccessToken || rotated.RefreshToken == paired.RefreshToken {
		t.Fatal("refresh did not rotate both token secrets")
	}
	replayOldRefresh := requestJSON(t, handler, http.MethodPost, "/v1/auth/refresh", "",
		RefreshRequest{RefreshToken: paired.RefreshToken}, "")
	requireStatus(t, replayOldRefresh, http.StatusUnauthorized)
	revoke := requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
		RefreshRequest{RefreshToken: rotated.RefreshToken}, "")
	requireStatus(t, revoke, http.StatusNoContent)
	useRevokedAccess := requestJSON(t, handler, http.MethodGet, "/v1/gateways", rotated.AccessToken, nil, "")
	requireStatus(t, useRevokedAccess, http.StatusUnauthorized)
}

func stringPointer(value string) *string { return &value }
