package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kirikira/gsm2sip-server/internal/securestore"
)

func TestValidPlatformIdentifierAcceptsFutureLowercaseValues(t *testing.T) {
	for value, valid := range map[string]bool{
		"android": true, "windows": true, "unknown": true, "future_client-2": true,
		"": false, "Windows": false, "linux desktop": false,
		"1android": false, strings.Repeat("a", 33): false,
	} {
		if got := validPlatform(value); got != valid {
			t.Errorf("validPlatform(%q) = %t, want %t", value, got, valid)
		}
	}
}

func TestPostgresMultipleClientsHaveIndependentSessionsSMSAndReceipts(t *testing.T) {
	database := openIntegrationDatabase(t)
	ownerID, gatewayID, simID := newUUID(), newUUID(), newUUID()
	gatewayToken := randomToken()
	otherOwnerID, otherClientID, otherAccessToken := newUUID(), newUUID(), randomToken()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO owners(id,display_name) VALUES ($1,'multi-client owner')`, []any{ownerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'gateway','shared gateway')`, []any{gatewayID, ownerID}},
		{`INSERT INTO gateways(device_id,mapping_revision) VALUES ($1,1)`, []any{gatewayID}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at)
			VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`,
			[]any{newUUID(), gatewayID, tokenHash(gatewayToken), tokenHash(randomToken())}},
		{`INSERT INTO sim_bindings(sim_id,owner_id,gateway_id,slot_index,label,state,identity_verified,mapping_revision,service_state)
			VALUES ($1,$2,$3,0,'Line A','active',true,1,'in_service')`, []any{simID, ownerID, gatewayID}},
		{`INSERT INTO owners(id,display_name) VALUES ($1,'other owner')`, []any{otherOwnerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'client','other owner client')`, []any{otherClientID, otherOwnerID}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at)
			VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`,
			[]any{newUUID(), otherClientID, tokenHash(otherAccessToken), tokenHash(randomToken())}},
	} {
		if _, err := database.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed multi-client database: %v", err)
		}
	}

	pairingCodes := []string{randomToken(), randomToken()}
	for _, code := range pairingCodes {
		if _, err := database.db.Exec(`INSERT INTO pairing_codes(id,owner_id,role,code_hash,expires_at)
			VALUES ($1,$2,'client',$3,now()+interval '10 minutes')`, newUUID(), ownerID, tokenHash(code)); err != nil {
			t.Fatal("seed one-time client pairing code")
		}
	}

	cipher, err := securestore.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal("create secret cipher")
	}
	handler := NewWithOptions(database.db, nil, Options{SecretCipher: cipher}).Handler()
	claim := func(code, name, platform string) PairingClaimResponse {
		t.Helper()
		payload := map[string]string{"pairing_code": code, "device_name": name}
		if platform != "" {
			payload["platform"] = platform
		}
		response := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "", payload, "")
		requireStatus(t, response, http.StatusOK)
		var result PairingClaimResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal("decode client pairing response")
		}
		if result.OwnerID != ownerID || result.Role != "client" || result.DeviceID == "" {
			t.Fatalf("unexpected multi-client pairing response: %+v", result)
		}
		return result
	}
	// A legacy CLI request without platform remains valid and is labeled unknown.
	clientA := claim(pairingCodes[0], "host A", "")
	clientB := claim(pairingCodes[1], "Windows host", "windows")
	if clientA.DeviceID == clientB.DeviceID || clientA.AccessToken == clientB.AccessToken || clientA.RefreshToken == clientB.RefreshToken {
		t.Fatal("separate pairing codes did not create independent client identities and credentials")
	}
	replayedCode := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		map[string]string{"pairing_code": pairingCodes[0], "device_name": "replayed host"}, "")
	requireStatus(t, replayedCode, http.StatusUnauthorized)

	listClients := func(token string) []map[string]json.RawMessage {
		t.Helper()
		response := requestJSON(t, handler, http.MethodGet, "/v1/clients", token, nil, "")
		requireStatus(t, response, http.StatusOK)
		var body struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal("decode paired client list")
		}
		return body.Items
	}
	assertClientList := func(token, selfID string, wantOtherPlatform string) {
		t.Helper()
		items := listClients(token)
		if len(items) != 2 {
			t.Fatalf("paired client list has %d items, want both owner clients", len(items))
		}
		selfCount := 0
		foundPlatforms := map[string]string{}
		for _, item := range items {
			allowed := map[string]bool{"id": true, "name": true, "platform": true, "state": true, "is_self": true, "live": true, "last_active_at": true}
			for field := range item {
				if !allowed[field] {
					t.Fatalf("client list exposed %q beyond id/name/platform/state/is_self/live/last_active_at: %v", field, item)
				}
			}
			if len(item) < 6 {
				t.Fatalf("client list omitted required fields: %v", item)
			}
			for _, forbidden := range []string{"access_token", "refresh_token", "password", "sip", "auth_username"} {
				if _, ok := item[forbidden]; ok {
					t.Fatalf("client list exposed sensitive field %q", forbidden)
				}
			}
			var summary ClientSummary
			encoded, _ := json.Marshal(item)
			if err := json.Unmarshal(encoded, &summary); err != nil {
				t.Fatal("decode client list item")
			}
			if summary.Name == "" || summary.State != "active" {
				t.Fatalf("client list omitted safe metadata: %+v", summary)
			}
			foundPlatforms[summary.ID] = summary.Platform
			if summary.IsSelf {
				selfCount++
				if summary.ID != selfID {
					t.Fatalf("is_self marked another device: got=%s want=%s", summary.ID, selfID)
				}
			}
		}
		if selfCount != 1 || foundPlatforms[clientA.DeviceID] != "unknown" || foundPlatforms[clientB.DeviceID] != wantOtherPlatform {
			t.Fatalf("client list has incorrect self/platform metadata: %+v", foundPlatforms)
		}
	}
	assertClientList(clientA.AccessToken, clientA.DeviceID, "windows")
	assertClientList(clientB.AccessToken, clientB.DeviceID, "windows")
	wrongRole := requestJSON(t, handler, http.MethodGet, "/v1/clients", gatewayToken, nil, "")
	requireStatus(t, wrongRole, http.StatusForbidden)

	// Same gateway and same idempotency key are independent because the host
	// device is part of the key namespace.
	messageRequest := CreateMessageRequest{
		GatewayID: gatewayID, SIMID: simID, MappingRevision: 1,
		To: "+12025550123", Text: "same-key from two hosts", TTLSeconds: 300,
	}
	const sharedMessageKey = "same-message-key-from-hosts-0001"
	createMessage := func(token string) MessageAccepted {
		t.Helper()
		response := requestJSON(t, handler, http.MethodPost, "/v1/messages", token, messageRequest, sharedMessageKey)
		requireStatus(t, response, http.StatusAccepted)
		var accepted MessageAccepted
		if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
			t.Fatal("decode accepted SMS task")
		}
		return accepted
	}
	messageA := createMessage(clientA.AccessToken)
	messageB := createMessage(clientB.AccessToken)
	if messageA.MessageID == messageB.MessageID || messageA.CommandID == messageB.CommandID {
		t.Fatal("same key on separate clients replayed one another's SMS task")
	}
	replayA := createMessage(clientA.AccessToken)
	if replayA.MessageID != messageA.MessageID || replayA.CommandID != messageA.CommandID {
		t.Fatal("same client retry did not replay its original SMS task")
	}
	var idempotencyRecordCount int
	if err := database.db.QueryRow(`SELECT count(*) FROM idempotency_records WHERE owner_id=$1 AND operation='messages.create' AND idempotency_key=$2`,
		ownerID, sharedMessageKey).Scan(&idempotencyRecordCount); err != nil || idempotencyRecordCount != 2 {
		t.Fatalf("expected separate idempotency rows for both clients: count=%d err=%v", idempotencyRecordCount, err)
	}

	// SIP logins provisioned by releases before 0.4 stay in the database until
	// their device is revoked.
	for _, device := range []string{clientA.DeviceID, clientB.DeviceID} {
		endpoint := "dev_" + strings.ReplaceAll(device, "-", "")
		for _, statement := range []string{
			`INSERT INTO sip_endpoint_bindings(device_id,endpoint_id,auth_username,aor) SELECT $1::uuid,$2,$2,$2`,
			`INSERT INTO ps_endpoints(id) SELECT $2 WHERE $1::uuid IS NOT NULL`,
			`INSERT INTO ps_auths(id,username,password_digest) SELECT $2,$2,'MD5:'||md5('legacy') WHERE $1::uuid IS NOT NULL`,
			`INSERT INTO ps_aors(id) SELECT $2 WHERE $1::uuid IS NOT NULL`,
		} {
			if _, err := database.db.Exec(statement, device, endpoint); err != nil {
				t.Fatalf("seed legacy SIP login: %v", err)
			}
		}
	}

	refresh := func(token, key string) (SessionTokens, *bytes.Buffer) {
		t.Helper()
		response := requestJSON(t, handler, http.MethodPost, "/v1/auth/refresh", "", RefreshRequest{RefreshToken: token}, key)
		requireStatus(t, response, http.StatusOK)
		var result SessionTokens
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal("decode rotated session")
		}
		return result, bytes.NewBuffer(response.Body.Bytes())
	}
	const sharedRefreshKey = "same-refresh-key-on-two-hosts-0001"
	rotatedA, firstRefreshA := refresh(clientA.RefreshToken, sharedRefreshKey)
	rotatedB, firstRefreshB := refresh(clientB.RefreshToken, sharedRefreshKey)
	if rotatedA.AccessToken == rotatedB.AccessToken || rotatedA.RefreshToken == rotatedB.RefreshToken {
		t.Fatal("separate hosts received the same rotated session credentials")
	}
	recoveredA, _ := refresh(clientA.RefreshToken, sharedRefreshKey)
	recoveredB, _ := refresh(clientB.RefreshToken, sharedRefreshKey)
	if recoveredA != rotatedA || recoveredB != rotatedB || firstRefreshA.String() == firstRefreshB.String() {
		t.Fatal("refresh recovery crossed device boundaries")
	}

	// Each host reads with its own local cursor. A receipt on one host neither
	// changes the other host's cursor nor deletes the owner's retained events.
	getEvents := func(token, cursor string) []map[string]json.RawMessage {
		t.Helper()
		path := "/v1/events"
		if cursor != "" {
			path += "?cursor=" + cursor
		}
		response := requestJSON(t, handler, http.MethodGet, path, token, nil, "")
		requireStatus(t, response, http.StatusOK)
		var body struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal("decode owner event feed")
		}
		return body.Items
	}
	itemsA := getEvents(rotatedA.AccessToken, "")
	itemsB := getEvents(rotatedB.AccessToken, "")
	if len(itemsA) != 2 || len(itemsB) != 2 {
		t.Fatalf("both clients should independently read both owner events: A=%d B=%d", len(itemsA), len(itemsB))
	}
	var firstCursor, lastCursor string
	if err := json.Unmarshal(itemsA[0]["cursor"], &firstCursor); err != nil {
		t.Fatal("decode first event cursor")
	}
	if err := json.Unmarshal(itemsA[len(itemsA)-1]["cursor"], &lastCursor); err != nil {
		t.Fatal("decode last event cursor")
	}
	if firstCursor == "" || lastCursor == "" {
		t.Fatal("event feed omitted opaque per-client cursor values")
	}
	ack := func(token, cursor string) {
		t.Helper()
		response := requestJSON(t, handler, http.MethodPost, "/v1/events/ack", token,
			struct {
				DurableCursor string `json:"durable_cursor"`
			}{DurableCursor: cursor}, "")
		requireStatus(t, response, http.StatusOK)
	}
	ack(rotatedA.AccessToken, lastCursor)
	var receiptAExists, receiptBExists bool
	if err := database.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM client_event_receipts WHERE device_id=$1),
		EXISTS(SELECT 1 FROM client_event_receipts WHERE device_id=$2)`, clientA.DeviceID, clientB.DeviceID).
		Scan(&receiptAExists, &receiptBExists); err != nil || !receiptAExists || receiptBExists {
		t.Fatalf("host A receipt was not isolated: A=%t B=%t err=%v", receiptAExists, receiptBExists, err)
	}
	ack(rotatedB.AccessToken, firstCursor)
	var receiptCursorA, receiptCursorB string
	if err := database.db.QueryRow(`SELECT a.durable_cursor::text,b.durable_cursor::text
		FROM client_event_receipts a JOIN client_event_receipts b ON a.owner_id=b.owner_id
		WHERE a.device_id=$1 AND b.device_id=$2`, clientA.DeviceID, clientB.DeviceID).
		Scan(&receiptCursorA, &receiptCursorB); err != nil {
		t.Fatal("read per-host event receipts")
	}
	if receiptCursorA == receiptCursorB {
		t.Fatal("one host's receipt overwrote the other host's durable cursor")
	}
	if len(getEvents(rotatedA.AccessToken, lastCursor)) != 0 || len(getEvents(rotatedB.AccessToken, firstCursor)) != 1 || len(getEvents(rotatedB.AccessToken, "")) != 2 {
		t.Fatal("one host's feed cursor or receipt affected the other host's read position")
	}
	var retainedEvents int
	if err := database.db.QueryRow(`SELECT count(*) FROM server_events WHERE owner_id=$1`, ownerID).Scan(&retainedEvents); err != nil || retainedEvents != 2 {
		t.Fatalf("client receipts must retain both owner events: count=%d err=%v", retainedEvents, err)
	}

	otherClientList := listClients(otherAccessToken)
	if len(otherClientList) != 1 {
		t.Fatalf("other owner saw this owner's clients: %+v", otherClientList)
	}
	var ownOtherClient ClientSummary
	encodedOther, _ := json.Marshal(otherClientList[0])
	if err := json.Unmarshal(encodedOther, &ownOtherClient); err != nil || ownOtherClient.ID != otherClientID || !ownOtherClient.IsSelf {
		t.Fatalf("other owner client listing crossed owner boundary: %+v err=%v", ownOtherClient, err)
	}
	foreignMessage := requestJSON(t, handler, http.MethodGet, "/v1/messages/"+messageA.MessageID, otherAccessToken, nil, "")
	requireStatus(t, foreignMessage, http.StatusNotFound)
	foreignSend := requestJSON(t, handler, http.MethodPost, "/v1/messages", otherAccessToken, messageRequest, "foreign-owner-message-key-00001")
	requireStatus(t, foreignSend, http.StatusNotFound)
	foreignReceipt := requestJSON(t, handler, http.MethodPost, "/v1/events/ack", otherAccessToken,
		struct {
			DurableCursor string `json:"durable_cursor"`
		}{DurableCursor: lastCursor}, "")
	requireStatus(t, foreignReceipt, http.StatusConflict)
	if !strings.Contains(foreignReceipt.Body.String(), "CURSOR_AHEAD") {
		t.Fatalf("cross-owner cursor was not rejected: %s", foreignReceipt.Body.String())
	}

	// Revoking A removes only A's session and legacy SIP login. B can keep
	// reading and sending through the same gateway, and existing SMS remain.
	revokeA := requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
		RefreshRequest{RefreshToken: rotatedA.RefreshToken}, "")
	requireStatus(t, revokeA, http.StatusNoContent)
	requireStatus(t, requestJSON(t, handler, http.MethodGet, "/v1/clients", rotatedA.AccessToken, nil, ""), http.StatusUnauthorized)
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/auth/refresh", "",
		RefreshRequest{RefreshToken: rotatedA.RefreshToken}, sharedRefreshKey), http.StatusUnauthorized)
	if len(listClients(rotatedB.AccessToken)) != 2 {
		t.Fatal("revoking one host affected the other host's session or client list")
	}
	rotatedBAfterRevoke, _ := refresh(rotatedB.RefreshToken, "refresh-host-b-after-revoke-0001")
	if rotatedBAfterRevoke.AccessToken == "" {
		t.Fatal("the remaining host could not independently refresh")
	}
	var bindingA, bindingB string
	if err := database.db.QueryRow(`SELECT a.state,b.state FROM sip_endpoint_bindings a
		JOIN sip_endpoint_bindings b ON a.device_id<>b.device_id
		WHERE a.device_id=$1 AND b.device_id=$2`, clientA.DeviceID, clientB.DeviceID).Scan(&bindingA, &bindingB); err != nil {
		t.Fatal("read per-device SIP bindings after revocation")
	}
	var loginsA, loginsB int
	if err := database.db.QueryRow(`SELECT count(*) FILTER (WHERE id=$1),count(*) FILTER (WHERE id=$2) FROM ps_auths`,
		"dev_"+strings.ReplaceAll(clientA.DeviceID, "-", ""), "dev_"+strings.ReplaceAll(clientB.DeviceID, "-", "")).Scan(&loginsA, &loginsB); err != nil {
		t.Fatal("read legacy SIP logins after revocation")
	}
	if bindingA != "revoked" || bindingB != "active" || loginsA != 0 || loginsB != 1 {
		t.Fatalf("revocation crossed SIP device boundary: A=%q/%d B=%q/%d", bindingA, loginsA, bindingB, loginsB)
	}
	var messagesAfterRevoke int
	if err := database.db.QueryRow(`SELECT count(*) FROM messages WHERE owner_id=$1`, ownerID).Scan(&messagesAfterRevoke); err != nil || messagesAfterRevoke != 2 {
		t.Fatalf("host revocation deleted owner SMS history: count=%d err=%v", messagesAfterRevoke, err)
	}
	listedMessages := requestJSON(t, handler, http.MethodGet, "/v1/messages?limit=100", rotatedBAfterRevoke.AccessToken, nil, "")
	requireStatus(t, listedMessages, http.StatusOK)
	if !strings.Contains(listedMessages.Body.String(), messageA.MessageID) || !strings.Contains(listedMessages.Body.String(), messageB.MessageID) {
		t.Fatal("remaining host cannot read retained SMS tasks")
	}
}
