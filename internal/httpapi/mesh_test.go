package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

type meshFixture struct {
	handler   http.Handler
	ownerID   string
	gateway   PairingClaimResponse
	client    PairingClaimResponse
	simIDs    []string
	revision  int64
	sequence  int64
	installID string
}

// pairMeshFixture pairs one gateway and one client under a fresh owner and,
// when proposedSIMs is non-empty, binds one SIM per proposed ID.
func pairMeshFixture(t *testing.T, database *integrationDatabase, proposedSIMs ...string) *meshFixture {
	t.Helper()
	f := &meshFixture{ownerID: newUUID(), installID: newUUID()}
	if _, err := database.db.Exec(`INSERT INTO owners(id,display_name) VALUES ($1,'mesh-owner')`, f.ownerID); err != nil {
		t.Fatal("seed owner")
	}
	gatewayCode, clientCode := randomToken(), randomToken()
	for role, code := range map[string]string{"gateway": gatewayCode, "client": clientCode} {
		if _, err := database.db.Exec(`INSERT INTO pairing_codes(id,owner_id,role,code_hash,expires_at)
			VALUES ($1,$2,$3,$4,now()+interval '10 minutes')`, newUUID(), f.ownerID, role, tokenHash(code)); err != nil {
			t.Fatal("seed pairing code")
		}
	}
	f.handler = New(database.db, nil).Handler()
	claim := func(code string) PairingClaimResponse {
		response := requestJSON(t, f.handler, http.MethodPost, "/v1/pairings/claim", "",
			PairingClaimRequest{PairingCode: code, DeviceName: "mesh test"}, "")
		requireStatus(t, response, http.StatusOK)
		var result PairingClaimResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal("decode pairing")
		}
		return result
	}
	f.gateway, f.client = claim(gatewayCode), claim(clientCode)
	if len(proposedSIMs) == 0 {
		return f
	}
	operationID := newUUID()
	mappings := make([]map[string]any, 0, len(proposedSIMs))
	for index, id := range proposedSIMs {
		mappings = append(mappings, map[string]any{"slot_index": index, "label": "SIM", "proposed_sim_id": id})
	}
	proposal := f.proposeSIMs(t, operationID, mappings, http.StatusOK)
	confirmations := make([]map[string]any, 0, len(proposal.Mappings))
	for _, mapping := range proposal.Mappings {
		confirmations = append(confirmations, map[string]any{"sim_id": mapping.SIMID, "slot_index": mapping.SlotIndex, "confirmed": true})
		f.simIDs = append(f.simIDs, mapping.SIMID)
	}
	requireStatus(t, requestJSON(t, f.handler, http.MethodPost, "/v1/gateways/"+f.gateway.DeviceID+"/sim-bindings",
		f.gateway.AccessToken, map[string]any{"operation_id": operationID, "phase": "confirm", "confirmations": confirmations}, ""), http.StatusOK)
	f.revision = proposal.MappingRevision
	return f
}

func (f *meshFixture) proposeSIMs(t *testing.T, operationID string, mappings []map[string]any, status int) SIMBindingResult {
	t.Helper()
	response := requestJSON(t, f.handler, http.MethodPost, "/v1/gateways/"+f.gateway.DeviceID+"/sim-bindings",
		f.gateway.AccessToken, map[string]any{"operation_id": operationID, "phase": "propose", "mappings": mappings}, "")
	requireStatus(t, response, status)
	var result SIMBindingResult
	if status == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal("decode proposal")
		}
	}
	return result
}

func (f *meshFixture) heartbeat(t *testing.T, installID *string) HeartbeatResponse {
	t.Helper()
	f.sequence++
	states := make([]SIMState, 0, len(f.simIDs))
	for _, id := range f.simIDs {
		states = append(states, SIMState{SIMID: id, MappingRevision: f.revision, ServiceState: "in_service", IdentityVerified: true})
	}
	response := requestJSON(t, f.handler, http.MethodPost, "/v1/gateways/"+f.gateway.DeviceID+"/heartbeat", f.gateway.AccessToken,
		HeartbeatRequest{Sequence: f.sequence, ProtocolVersion: 1, AppVersion: "mesh", SIMStates: states, InstallID: installID}, "")
	requireStatus(t, response, http.StatusOK)
	var result HeartbeatResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal("decode heartbeat")
	}
	return result
}

func TestCapabilitiesAreAdvertisedWithoutAuthentication(t *testing.T) {
	handler := New(nil, nil).Handler()
	response := requestJSON(t, handler, http.MethodGet, "/v1/capabilities", "", nil, "")
	requireStatus(t, response, http.StatusOK)
	var body struct {
		ProtocolVersion int      `json:"protocol_version"`
		Extensions      []string `json:"extensions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.ProtocolVersion != 1 {
		t.Fatalf("capabilities response is malformed: %s", response.Body.String())
	}
	found := map[string]bool{}
	for _, name := range body.Extensions {
		found[name] = true
	}
	if !found[HeartbeatExtensionMesh] || !found[HeartbeatExtensionSIMRadio] {
		t.Fatalf("capabilities omit mesh or sim_radio: %v", body.Extensions)
	}
}

func TestPostgresGatewayProposedSIMIDsAreKeptAndNeverTakenOver(t *testing.T) {
	database := openIntegrationDatabase(t)
	simA, simB := newUUID(), newUUID()
	node := pairMeshFixture(t, database, simA, simB)
	if len(node.simIDs) != 2 || node.simIDs[0] != simA || node.simIDs[1] != simB {
		t.Fatalf("server did not keep gateway-proposed SIM IDs: %v", node.simIDs)
	}

	// Re-proposing the same IDs on the same gateway reuses them.
	again := node.proposeSIMs(t, newUUID(), []map[string]any{
		{"slot_index": 0, "label": "SIM", "proposed_sim_id": simA},
	}, http.StatusOK)
	if len(again.Mappings) != 1 || again.Mappings[0].SIMID != simA {
		t.Fatalf("re-proposal did not reuse the gateway's own SIM ID: %+v", again)
	}

	// Another gateway (a different owner on the same node) cannot claim them.
	other := pairMeshFixture(t, database)
	other.proposeSIMs(t, newUUID(), []map[string]any{{"slot_index": 0, "label": "SIM", "proposed_sim_id": simB}}, http.StatusConflict)

	// Malformed or ambiguous proposals are rejected before any write.
	node.proposeSIMs(t, newUUID(), []map[string]any{{"slot_index": 0, "label": "SIM", "proposed_sim_id": "not-a-uuid"}}, http.StatusBadRequest)
	node.proposeSIMs(t, newUUID(), []map[string]any{
		{"slot_index": 0, "label": "SIM", "proposed_sim_id": simA},
		{"slot_index": 1, "label": "SIM", "proposed_sim_id": simA},
	}, http.StatusBadRequest)
	node.proposeSIMs(t, newUUID(), []map[string]any{{"slot_index": 0, "label": "SIM", "proposed_sim_id": simA,
		"existing_sim_id": simA, "same_sim_verified": true}}, http.StatusBadRequest)
}

func TestPostgresClientMintedMessageIDIsKeptAndConflictsAreRejected(t *testing.T) {
	database := openIntegrationDatabase(t)
	node := pairMeshFixture(t, database, newUUID())
	node.heartbeat(t, nil)
	messageID := newUUID()
	body := map[string]any{"message_id": messageID, "gateway_id": node.gateway.DeviceID, "sim_id": node.simIDs[0],
		"mapping_revision": node.revision, "to": "+15550000001", "text": "mesh"}
	first := requestJSON(t, node.handler, http.MethodPost, "/v1/messages", node.client.AccessToken, body, "mesh-key-0000000001")
	requireStatus(t, first, http.StatusAccepted)
	var accepted MessageAccepted
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil || accepted.MessageID != messageID {
		t.Fatalf("server did not keep the client-minted message ID: %s", first.Body.String())
	}
	retry := requestJSON(t, node.handler, http.MethodPost, "/v1/messages", node.client.AccessToken, body, "mesh-key-0000000001")
	requireStatus(t, retry, http.StatusAccepted)
	var replayed MessageAccepted
	if err := json.Unmarshal(retry.Body.Bytes(), &replayed); err != nil || replayed.MessageID != accepted.MessageID || replayed.CommandID != accepted.CommandID {
		t.Fatalf("exact retry did not replay the original task: %s", retry.Body.String())
	}
	// Same message ID under a new idempotency key is another task: refuse it.
	requireStatus(t, requestJSON(t, node.handler, http.MethodPost, "/v1/messages", node.client.AccessToken, body, "mesh-key-0000000002"),
		http.StatusConflict)
	invalid := map[string]any{"message_id": "nope", "gateway_id": node.gateway.DeviceID, "sim_id": node.simIDs[0],
		"mapping_revision": node.revision, "to": "+15550000001", "text": "mesh"}
	requireStatus(t, requestJSON(t, node.handler, http.MethodPost, "/v1/messages", node.client.AccessToken, invalid, "mesh-key-0000000003"),
		http.StatusBadRequest)

	commands := requestJSON(t, node.handler, http.MethodGet, "/v1/gateways/"+node.gateway.DeviceID+"/commands", node.gateway.AccessToken, nil, "")
	requireStatus(t, commands, http.StatusOK)
	var page struct {
		Commands []struct {
			MessageID string `json:"message_id"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(commands.Body.Bytes(), &page); err != nil || len(page.Commands) != 1 || page.Commands[0].MessageID != messageID {
		t.Fatalf("gateway command does not carry the client-minted message ID: %s", commands.Body.String())
	}
}

func TestPostgresHeartbeatInstallIDIsStoredAndListed(t *testing.T) {
	database := openIntegrationDatabase(t)
	node := pairMeshFixture(t, database, newUUID())
	result := node.heartbeat(t, &node.installID)
	found := false
	for _, name := range result.Extensions {
		found = found || name == HeartbeatExtensionMesh
	}
	if !found {
		t.Fatalf("heartbeat did not advertise mesh: %+v", result)
	}
	// A later heartbeat without the field keeps the recorded install.
	node.heartbeat(t, nil)
	response := requestJSON(t, node.handler, http.MethodGet, "/v1/gateways", node.client.AccessToken, nil, "")
	requireStatus(t, response, http.StatusOK)
	var page struct {
		Items []GatewaySummary `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Items) != 1 ||
		page.Items[0].InstallID == nil || *page.Items[0].InstallID != node.installID {
		t.Fatalf("gateway list does not carry the install ID: %s", response.Body.String())
	}
	bad := "not-a-uuid"
	node.sequence++
	requireStatus(t, requestJSON(t, node.handler, http.MethodPost, "/v1/gateways/"+node.gateway.DeviceID+"/heartbeat", node.gateway.AccessToken,
		HeartbeatRequest{Sequence: node.sequence, ProtocolVersion: 1, AppVersion: "mesh", InstallID: &bad}, ""), http.StatusBadRequest)
}
