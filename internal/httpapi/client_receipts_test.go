package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestPostgresClientEventReceiptsAreMonotonicAndNonDestructive(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	server := New(database.db, nil)
	handler := server.Handler()

	createMessage := func(text, key string) string {
		t.Helper()
		response := requestJSON(t, handler, http.MethodPost, "/v1/messages", f.clientToken,
			CreateMessageRequest{
				GatewayID: f.gatewayID, SIMID: f.simID, MappingRevision: 1,
				To: "+12025550123", Text: text, TTLSeconds: 300,
			}, key)
		requireStatus(t, response, http.StatusAccepted)
		var accepted MessageAccepted
		if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
			t.Fatal("decode created message")
		}
		var cursor int64
		if err := database.db.QueryRow(`SELECT cursor FROM server_events WHERE owner_id=$1 AND message_id=$2`,
			f.ownerID, accepted.MessageID).Scan(&cursor); err != nil {
			t.Fatal("read created client event cursor")
		}
		return opaqueEventCursor(cursor)
	}

	firstCursor := createMessage("receipt one", "receipt-message-key-00000001")
	secondCursor := createMessage("receipt two", "receipt-message-key-00000002")
	firstValue, err := parseEventCursor(firstCursor)
	if err != nil {
		t.Fatal("parse first event cursor")
	}
	secondValue, err := parseEventCursor(secondCursor)
	if err != nil {
		t.Fatal("parse second event cursor")
	}
	if secondValue <= firstValue {
		t.Fatalf("event cursors did not increase: first=%d second=%d", firstValue, secondValue)
	}

	var eventsBefore int
	if err := database.db.QueryRow(`SELECT count(*) FROM server_events WHERE owner_id=$1`, f.ownerID).
		Scan(&eventsBefore); err != nil {
		t.Fatal("count events before receipt")
	}
	ack := func(cursor string) struct {
		DurableCursor  string    `json:"durable_cursor"`
		ConfirmedAt    time.Time `json:"confirmed_at"`
		EventsRetained bool      `json:"events_retained"`
	} {
		t.Helper()
		response := requestJSON(t, handler, http.MethodPost, "/v1/events/ack", f.clientToken,
			struct {
				DurableCursor string `json:"durable_cursor"`
			}{DurableCursor: cursor}, "")
		requireStatus(t, response, http.StatusOK)
		var result struct {
			DurableCursor  string    `json:"durable_cursor"`
			ConfirmedAt    time.Time `json:"confirmed_at"`
			EventsRetained bool      `json:"events_retained"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal("decode event receipt response")
		}
		return result
	}

	forward := ack(secondCursor)
	retained, err := parseEventCursor(forward.DurableCursor)
	if err != nil || retained != secondValue {
		t.Fatalf("receipt did not store the advanced cursor: got=%q err=%v", forward.DurableCursor, err)
	}
	backward := ack(firstCursor)
	retained, err = parseEventCursor(backward.DurableCursor)
	if err != nil || retained != secondValue {
		t.Fatalf("older receipt moved the cursor backwards: got=%q err=%v", backward.DurableCursor, err)
	}
	if !backward.ConfirmedAt.Equal(forward.ConfirmedAt) {
		t.Fatal("older receipt changed the confirmation time for a newer durable cursor")
	}
	if !forward.EventsRetained || !backward.EventsRetained {
		t.Fatal("receipt response did not report that events are retained")
	}
	var stored int64
	if err := database.db.QueryRow(`SELECT durable_cursor FROM client_event_receipts WHERE device_id=$1`, f.clientID).
		Scan(&stored); err != nil {
		t.Fatal("read persisted client receipt")
	}
	if stored != secondValue {
		t.Fatalf("persisted cursor regressed: got=%d want=%d", stored, secondValue)
	}
	var eventsAfter int
	if err := database.db.QueryRow(`SELECT count(*) FROM server_events WHERE owner_id=$1`, f.ownerID).
		Scan(&eventsAfter); err != nil {
		t.Fatal("count events after receipt")
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("receipt deleted server events: before=%d after=%d", eventsBefore, eventsAfter)
	}
}

func TestPostgresClientEventReceiptRejectsCursorAheadOfOwner(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	handler := New(database.db, nil).Handler()
	response := requestJSON(t, handler, http.MethodPost, "/v1/messages", f.otherToken,
		CreateMessageRequest{
			GatewayID: f.otherGatewayID, SIMID: f.otherSIMID, MappingRevision: 1,
			To: "+12025550123", Text: "owner B event", TTLSeconds: 300,
		}, "other-owner-receipt-event-key-01")
	requireStatus(t, response, http.StatusAccepted)
	var accepted MessageAccepted
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal("decode owner B message")
	}
	var ownerBCursor int64
	if err := database.db.QueryRow(`SELECT cursor FROM server_events WHERE owner_id=$1 AND message_id=$2`,
		f.otherOwnerID, accepted.MessageID).Scan(&ownerBCursor); err != nil {
		t.Fatal("read owner B event cursor")
	}
	var ownerACursor int64
	if err := database.db.QueryRow(`SELECT COALESCE((SELECT last_cursor FROM owner_event_cursors WHERE owner_id=$1),0)`,
		f.ownerID).Scan(&ownerACursor); err != nil {
		t.Fatal("read owner A event cursor")
	}
	if ownerBCursor <= ownerACursor {
		t.Fatalf("fixture did not create a cursor ahead of owner A: A=%d B=%d", ownerACursor, ownerBCursor)
	}

	request := requestJSON(t, handler, http.MethodPost, "/v1/events/ack", f.clientToken,
		struct {
			DurableCursor string `json:"durable_cursor"`
		}{DurableCursor: opaqueEventCursor(ownerBCursor)}, "")
	requireStatus(t, request, http.StatusConflict)
	var body errorBody
	if err := json.Unmarshal(request.Body.Bytes(), &body); err != nil {
		t.Fatal("decode cursor-ahead conflict")
	}
	if body.Error.Code != "CURSOR_AHEAD" {
		t.Fatalf("unexpected cursor-ahead error: %q", body.Error.Code)
	}
	var receiptCreated bool
	if err := database.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM client_event_receipts WHERE device_id=$1)`, f.clientID).
		Scan(&receiptCreated); err != nil {
		t.Fatal("check owner A receipt")
	}
	if receiptCreated {
		t.Fatal("out-of-owner cursor ahead created a receipt")
	}
}
