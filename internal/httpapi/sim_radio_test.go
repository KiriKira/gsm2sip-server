package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestValidSIMRadioBoundsDisplayTelemetry(t *testing.T) {
	level := func(v int) *int { return &v }
	text := func(v string) *string { return &v }
	valid := []*SIMRadio{
		nil,
		{},
		{SignalLevel: level(0), NetworkType: text("LTE"), NetworkOperator: text("中国移动")},
		{SignalLevel: level(4), NetworkType: text("NR NSA"), NetworkOperator: text("Carrier B")},
		{CountryISO: text("CN")},
	}
	for _, radio := range valid {
		if !validSIMRadio(radio) {
			t.Fatalf("valid radio rejected: %+v", radio)
		}
	}
	invalid := []*SIMRadio{
		{SignalLevel: level(-1)},
		{SignalLevel: level(5)},
		{NetworkType: text("")},
		{NetworkType: text("LTE<script>")},
		{NetworkType: text(strings.Repeat("A", 17))},
		{NetworkOperator: text("   ")},
		{NetworkOperator: text("bad\nname")},
		{NetworkOperator: text(strings.Repeat("运", 65))},
		{CountryISO: text("cn")},
		{CountryISO: text("CHN")},
		{CountryISO: text("")},
	}
	for _, radio := range invalid {
		if validSIMRadio(radio) {
			t.Fatalf("invalid radio accepted: %+v", radio)
		}
	}
}

func TestPostgresHeartbeatRadioIsAdvertisedStoredAndCleared(t *testing.T) {
	database := openIntegrationDatabase(t)
	ownerID := newUUID()
	if _, err := database.db.Exec(`INSERT INTO owners(id,display_name) VALUES ($1,'radio-owner')`, ownerID); err != nil {
		t.Fatal("seed owner")
	}
	gatewayCode, clientCode := randomToken(), randomToken()
	for role, code := range map[string]string{"gateway": gatewayCode, "client": clientCode} {
		if _, err := database.db.Exec(`INSERT INTO pairing_codes(id,owner_id,role,code_hash,expires_at)
			VALUES ($1,$2,$3,$4,now()+interval '10 minutes')`, newUUID(), ownerID, role, tokenHash(code)); err != nil {
			t.Fatal("seed pairing code")
		}
	}
	handler := New(database.db, nil).Handler()
	claim := func(code string) PairingClaimResponse {
		response := requestJSON(t, handler, http.MethodPost, "/v1/pairings/claim", "",
			PairingClaimRequest{PairingCode: code, DeviceName: "radio test"}, "")
		requireStatus(t, response, http.StatusOK)
		var result PairingClaimResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal("decode pairing")
		}
		return result
	}
	gateway, client := claim(gatewayCode), claim(clientCode)

	operationID := newUUID()
	proposalResponse := requestJSON(t, handler, http.MethodPost, "/v1/gateways/"+gateway.DeviceID+"/sim-bindings",
		gateway.AccessToken, map[string]any{"operation_id": operationID, "phase": "propose",
			"mappings": []map[string]any{{"slot_index": 0, "label": "A"}, {"slot_index": 1, "label": "B"}}}, "")
	requireStatus(t, proposalResponse, http.StatusOK)
	var proposed SIMBindingResult
	if err := json.Unmarshal(proposalResponse.Body.Bytes(), &proposed); err != nil {
		t.Fatal("decode proposal")
	}
	confirmResponse := requestJSON(t, handler, http.MethodPost, "/v1/gateways/"+gateway.DeviceID+"/sim-bindings",
		gateway.AccessToken, map[string]any{"operation_id": operationID, "phase": "confirm",
			"confirmations": []map[string]any{
				{"sim_id": proposed.Mappings[0].SIMID, "slot_index": 0, "confirmed": true},
				{"sim_id": proposed.Mappings[1].SIMID, "slot_index": 1, "confirmed": true},
			}}, "")
	requireStatus(t, confirmResponse, http.StatusOK)
	simA, simB, revision := proposed.Mappings[0].SIMID, proposed.Mappings[1].SIMID, proposed.MappingRevision

	// A legacy heartbeat without radio still learns that the server accepts it.
	legacy := HeartbeatRequest{Sequence: 1, ProtocolVersion: 1, AppVersion: "radio", Root: true,
		SIMStates: []SIMState{
			{SIMID: simA, MappingRevision: revision, ServiceState: "in_service", IdentityVerified: true},
			{SIMID: simB, MappingRevision: revision, ServiceState: "in_service", IdentityVerified: true},
		}}
	legacyResponse := requestJSON(t, handler, http.MethodPost, "/v1/gateways/"+gateway.DeviceID+"/heartbeat",
		gateway.AccessToken, legacy, "")
	requireStatus(t, legacyResponse, http.StatusOK)
	var legacyResult HeartbeatResponse
	if err := json.Unmarshal(legacyResponse.Body.Bytes(), &legacyResult); err != nil {
		t.Fatal("decode heartbeat")
	}
	advertised, countryAdvertised := false, false
	for _, name := range legacyResult.Extensions {
		advertised = advertised || name == HeartbeatExtensionSIMRadio
		countryAdvertised = countryAdvertised || name == HeartbeatExtensionSIMCountry
	}
	if !advertised || !countryAdvertised {
		t.Fatalf("heartbeat did not advertise radio telemetry: %+v", legacyResult)
	}

	level, networkType, operator, roaming, country := 3, "LTE", "  Carrier A  ", false, "CN"
	withRadio := legacy
	withRadio.Sequence = 2
	withRadio.SIMStates = []SIMState{
		{SIMID: simA, MappingRevision: revision, ServiceState: "in_service", IdentityVerified: true,
			Radio: &SIMRadio{SignalLevel: &level, NetworkType: &networkType, NetworkOperator: &operator, Roaming: &roaming, CountryISO: &country}},
		{SIMID: simB, MappingRevision: revision, ServiceState: "out_of_service", IdentityVerified: true,
			Radio: &SIMRadio{}},
	}
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/gateways/"+gateway.DeviceID+"/heartbeat",
		gateway.AccessToken, withRadio, ""), http.StatusOK)

	bad := withRadio
	bad.Sequence = 3
	tooStrong := 9
	bad.SIMStates = []SIMState{{SIMID: simA, MappingRevision: revision, ServiceState: "in_service", IdentityVerified: true,
		Radio: &SIMRadio{SignalLevel: &tooStrong}}}
	requireStatus(t, requestJSON(t, handler, http.MethodPost, "/v1/gateways/"+gateway.DeviceID+"/heartbeat",
		gateway.AccessToken, bad, ""), http.StatusBadRequest)

	sims := listSIMSummaries(t, handler, gateway.DeviceID, client.AccessToken)
	if len(sims) != 2 || sims[0].Radio == nil || sims[1].Radio == nil {
		t.Fatalf("client did not receive radio telemetry: %+v", sims)
	}
	first := sims[0].Radio
	if first.SignalLevel == nil || *first.SignalLevel != 3 || first.NetworkType == nil || *first.NetworkType != "LTE" ||
		first.NetworkOperator == nil || *first.NetworkOperator != "Carrier A" || first.Roaming == nil || *first.Roaming ||
		first.CountryISO == nil || *first.CountryISO != "CN" || first.UpdatedAt.IsZero() {
		t.Fatalf("radio telemetry was not stored verbatim and trimmed: %+v", first)
	}
	if second := sims[1].Radio; second.SignalLevel != nil || second.NetworkType != nil || second.NetworkOperator != nil ||
		second.Roaming != nil || second.CountryISO != nil {
		t.Fatalf("empty radio observation should keep unknown fields null: %+v", second)
	}

	// Omitting a SIM revokes it and clears its stale radio observation.
	omitted := legacy
	omitted.Sequence = 4
	omitted.SIMStates = []SIMState{withRadio.SIMStates[0]}
	omittedResponse := requestJSON(t, handler, http.MethodPost, "/v1/gateways/"+gateway.DeviceID+"/heartbeat",
		gateway.AccessToken, omitted, "")
	requireStatus(t, omittedResponse, http.StatusOK)
	sims = listSIMSummaries(t, handler, gateway.DeviceID, client.AccessToken)
	if sims[0].Radio == nil || sims[1].Radio != nil || sims[1].State != "unverified" {
		t.Fatalf("omitted SIM kept a stale radio observation: %+v", sims)
	}
}

func listSIMSummaries(t *testing.T, handler http.Handler, gatewayID, token string) []SIMSummary {
	t.Helper()
	response := requestJSON(t, handler, http.MethodGet, "/v1/gateways/"+gatewayID+"/sims", token, nil, "")
	requireStatus(t, response, http.StatusOK)
	var page struct {
		SIMs []SIMSummary `json:"sims"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal("decode SIM list")
	}
	return page.SIMs
}
