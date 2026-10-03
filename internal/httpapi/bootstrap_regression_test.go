package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestPostgresFirstConnectionSMSBootstrapAndSessionRevocation(t *testing.T) {
	database := openIntegrationDatabase(t)
	ownerID := newUUID()
	if _, err := database.db.Exec(`INSERT INTO owners(id,display_name) VALUES ($1,'bootstrap-owner')`, ownerID); err != nil {
		t.Fatal("seed bootstrap owner")
	}
	gatewayCode, clientCode := randomToken(), randomToken()
	for _, seeded := range []struct {
		role string
		code string
	}{
		{role: "gateway", code: gatewayCode},
		{role: "client", code: clientCode},
	} {
		if _, err := database.db.Exec(`INSERT INTO pairing_codes(id,owner_id,role,code_hash,expires_at)
			VALUES ($1,$2,$3,$4,now()+interval '10 minutes')`, newUUID(), ownerID, seeded.role, tokenHash(seeded.code)); err != nil {
			t.Fatal("seed hashed one-time pairing code")
		}
	}

	handler := New(database.db, nil).Handler()
	spoof := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		map[string]any{"pairing_code": gatewayCode, "device_name": "new gateway", "role": "client"}, "")
	requireStatus(t, spoof, http.StatusBadRequest)

	claimGateway := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: gatewayCode, DeviceName: "new gateway"}, "")
	requireStatus(t, claimGateway, http.StatusOK)
	var gateway PairingClaimResponse
	if err := json.Unmarshal(claimGateway.Body.Bytes(), &gateway); err != nil {
		t.Fatal("decode gateway pairing response")
	}
	if gateway.OwnerID != ownerID || gateway.DeviceID == "" || gateway.Role != "gateway" ||
		gateway.AccessToken == "" || gateway.RefreshToken == "" || gateway.SIP.Available || gateway.SIP.Reason != "sip_not_configured" {
		t.Fatalf("gateway pairing returned unexpected owner, role, token, or SIP capability: %+v", gateway)
	}
	replayGatewayCode := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: gatewayCode, DeviceName: "replayed gateway"}, "")
	requireStatus(t, replayGatewayCode, http.StatusUnauthorized)

	claimClient := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
		PairingClaimRequest{PairingCode: clientCode, DeviceName: "new client"}, "")
	requireStatus(t, claimClient, http.StatusOK)
	var client PairingClaimResponse
	if err := json.Unmarshal(claimClient.Body.Bytes(), &client); err != nil {
		t.Fatal("decode client pairing response")
	}
	if client.OwnerID != ownerID || client.DeviceID == "" || client.Role != "client" || client.AccessToken == "" || client.RefreshToken == "" {
		t.Fatalf("client pairing returned unexpected owner, role, or tokens: %+v", client)
	}

	operationID := newUUID()
	proposal := map[string]any{
		"operation_id": operationID,
		"phase":        "propose",
		"mappings": []map[string]any{
			{"slot_index": 0, "label": "Line A"},
			{"slot_index": 1, "label": "Line B"},
		},
	}
	proposalResponse := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+gateway.DeviceID+"/sim-bindings", gateway.AccessToken, proposal, "")
	requireStatus(t, proposalResponse, http.StatusOK)
	var proposed SIMBindingResult
	if err := json.Unmarshal(proposalResponse.Body.Bytes(), &proposed); err != nil {
		t.Fatal("decode SIM proposal response")
	}
	if proposed.Phase != "proposed" || proposed.MappingRevision < 1 || len(proposed.Mappings) != 2 {
		t.Fatalf("first gateway SIM proposal was incomplete: %+v", proposed)
	}
	for index, mapping := range proposed.Mappings {
		if mapping.SIMID == "" || mapping.SlotIndex != index || mapping.State != "pending_local_confirmation" || mapping.MappingRevision != proposed.MappingRevision {
			t.Fatalf("SIM proposal did not return a pending server mapping: %+v", mapping)
		}
	}

	confirm := map[string]any{
		"operation_id": operationID,
		"phase":        "confirm",
		"confirmations": []map[string]any{
			{"sim_id": proposed.Mappings[0].SIMID, "slot_index": 0, "confirmed": true},
			{"sim_id": proposed.Mappings[1].SIMID, "slot_index": 1, "confirmed": true},
		},
	}
	confirmResponse := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+gateway.DeviceID+"/sim-bindings", gateway.AccessToken, confirm, "")
	requireStatus(t, confirmResponse, http.StatusOK)
	var confirmed SIMBindingResult
	if err := json.Unmarshal(confirmResponse.Body.Bytes(), &confirmed); err != nil {
		t.Fatal("decode SIM confirmation response")
	}
	if confirmed.Phase != "confirmed" || confirmed.MappingRevision != proposed.MappingRevision || len(confirmed.Mappings) != 2 {
		t.Fatalf("SIM confirmation did not match the proposal: %+v", confirmed)
	}
	for _, mapping := range confirmed.Mappings {
		if mapping.State != "active" || mapping.MappingRevision != proposed.MappingRevision {
			t.Fatalf("locally confirmed SIM was not activated: %+v", mapping)
		}
	}

	heartbeat := HeartbeatRequest{
		Sequence: 1, ProtocolVersion: 1, AppVersion: "bootstrap-test", Root: true,
		SIPRegistered: false,
		SIMStates: []SIMState{
			{SIMID: proposed.Mappings[0].SIMID, MappingRevision: proposed.MappingRevision, ServiceState: "in_service", IdentityVerified: true},
			{SIMID: proposed.Mappings[1].SIMID, MappingRevision: proposed.MappingRevision, ServiceState: "in_service", IdentityVerified: true},
		},
	}
	roleCheck := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+gateway.DeviceID+"/heartbeat", client.AccessToken, heartbeat, "")
	requireStatus(t, roleCheck, http.StatusForbidden)
	heartbeatResponse := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+gateway.DeviceID+"/heartbeat", gateway.AccessToken, heartbeat, "")
	requireStatus(t, heartbeatResponse, http.StatusOK)
	var heartbeatResult HeartbeatResponse
	if err := json.Unmarshal(heartbeatResponse.Body.Bytes(), &heartbeatResult); err != nil {
		t.Fatal("decode heartbeat response")
	}
	if heartbeatResult.MappingRevision != proposed.MappingRevision || len(heartbeatResult.InvalidatedSIMIDs) != 0 {
		t.Fatalf("valid first heartbeat unexpectedly changed the SIM binding: %+v", heartbeatResult)
	}

	gatewaysResponse := requestJSON(t, handler, http.MethodGet, "/v1/gateways", client.AccessToken, nil, "")
	requireStatus(t, gatewaysResponse, http.StatusOK)
	var gatewayPage struct {
		Items []GatewaySummary `json:"items"`
	}
	if err := json.Unmarshal(gatewaysResponse.Body.Bytes(), &gatewayPage); err != nil {
		t.Fatal("decode paired gateway list")
	}
	if len(gatewayPage.Items) != 1 || gatewayPage.Items[0].GatewayID != gateway.DeviceID || gatewayPage.Items[0].MappingRevision != proposed.MappingRevision {
		t.Fatalf("client did not see its paired gateway and current SIM revision: %+v", gatewayPage.Items)
	}

	acceptedResponse := requestJSON(t, handler, http.MethodPost, "/v1/messages", client.AccessToken,
		CreateMessageRequest{
			GatewayID: gateway.DeviceID, SIMID: proposed.Mappings[1].SIMID,
			MappingRevision: proposed.MappingRevision, To: "+12025550123", Text: "first paired SMS", TTLSeconds: 300,
		}, "bootstrap-message-key-20261003-0001")
	requireStatus(t, acceptedResponse, http.StatusAccepted)
	var accepted MessageAccepted
	if err := json.Unmarshal(acceptedResponse.Body.Bytes(), &accepted); err != nil {
		t.Fatal("decode queued SMS response")
	}
	if accepted.MessageID == "" || accepted.CommandID == "" || accepted.Status != "queued" {
		t.Fatalf("paired client did not create a queued SMS command: %+v", accepted)
	}
	command := getGatewayCommand(t, handler, gateway.DeviceID, gateway.AccessToken, accepted.CommandID)
	if command.PayloadSHA256 == "" || command.PayloadSHA256 != commandDigest(command) || command.SIMID != proposed.Mappings[1].SIMID {
		t.Fatalf("gateway command did not carry its stable SIM-scoped payload digest: %+v", command)
	}
	claimResponse := requestJSON(t, handler, http.MethodPost,
		"/v1/gateways/"+gateway.DeviceID+"/commands/"+accepted.CommandID+"/claim", gateway.AccessToken,
		commandClaimRequest{PayloadSHA256: command.PayloadSHA256}, "")
	requireStatus(t, claimResponse, http.StatusOK)
	var claimed Command
	if err := json.Unmarshal(claimResponse.Body.Bytes(), &claimed); err != nil {
		t.Fatal("decode gateway command claim")
	}
	if claimed.CommandID != accepted.CommandID || claimed.State != "accepted_by_gateway" || claimed.PayloadSHA256 != command.PayloadSHA256 {
		t.Fatalf("paired gateway failed to claim the SMS command: %+v", claimed)
	}

	refreshResponse := requestJSON(t, handler, http.MethodPost, "/v1/auth/refresh", "",
		RefreshRequest{RefreshToken: gateway.RefreshToken}, "")
	requireStatus(t, refreshResponse, http.StatusOK)
	var rotated SessionTokens
	if err := json.Unmarshal(refreshResponse.Body.Bytes(), &rotated); err != nil {
		t.Fatal("decode rotated gateway tokens")
	}
	if rotated.AccessToken == "" || rotated.RefreshToken == "" || rotated.AccessToken == gateway.AccessToken || rotated.RefreshToken == gateway.RefreshToken {
		t.Fatal("refresh did not rotate both gateway tokens")
	}
	oldRefresh := requestJSON(t, handler, http.MethodPost, "/v1/auth/refresh", "",
		RefreshRequest{RefreshToken: gateway.RefreshToken}, "")
	requireStatus(t, oldRefresh, http.StatusUnauthorized)
	oldAccess := requestJSON(t, handler, http.MethodGet, "/v1/gateways", gateway.AccessToken, nil, "")
	requireStatus(t, oldAccess, http.StatusUnauthorized)
	newAccess := requestJSON(t, handler, http.MethodGet, "/v1/gateways", rotated.AccessToken, nil, "")
	requireStatus(t, newAccess, http.StatusOK)

	revoke := requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
		RefreshRequest{RefreshToken: rotated.RefreshToken}, "")
	requireStatus(t, revoke, http.StatusNoContent)
	revokedAccess := requestJSON(t, handler, http.MethodGet, "/v1/gateways", rotated.AccessToken, nil, "")
	requireStatus(t, revokedAccess, http.StatusUnauthorized)
}
