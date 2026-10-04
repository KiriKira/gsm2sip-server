package httpapi

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kirikira/gsm2sip-server/internal/securestore"
)

func sipCredentialHandler(t *testing.T, database *integrationDatabase) http.Handler {
	t.Helper()
	cipher, err := securestore.New(bytes.Repeat([]byte{0x37}, 32))
	if err != nil {
		t.Fatal("create SIP credential cipher")
	}
	return NewWithOptions(database.db, nil, Options{
		SecretCipher: cipher,
		SIP: SIPSettings{
			ServerName: "sip.example.test",
			Port:       5061,
			EnableOpus: true,
			CAPEM:      "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n",
		},
	}).Handler()
}

func TestPostgresSIPCredentialBootstrapRecoveryAndStorage(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	handler := sipCredentialHandler(t, database)
	key := "sip-bootstrap-key-000000000001"

	first := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/sip-credentials/rotate",
		f.clientToken, struct{}{}, key)
	requireStatus(t, first, http.StatusOK)
	var firstConfig sipConfiguration
	if err := json.Unmarshal(first.Body.Bytes(), &firstConfig); err != nil {
		t.Fatal("decode SIP bootstrap response")
	}
	if !firstConfig.Available || firstConfig.Password == "" || firstConfig.Username == "" ||
		firstConfig.Realm == "" || firstConfig.RegistrarURI == "" {
		t.Fatal("SIP bootstrap response omitted required fields")
	}

	// This represents a lost successful response: an exact retry with the same
	// key must return the committed response and password byte-for-byte.
	replay := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/sip-credentials/rotate",
		f.clientToken, struct{}{}, key)
	requireStatus(t, replay, http.StatusOK)
	if !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		firstHash := sha256.Sum256(first.Body.Bytes())
		replayHash := sha256.Sum256(replay.Body.Bytes())
		t.Fatalf("SIP bootstrap replay response differs (bytes %d/%d, SHA-256 %x/%x)",
			first.Body.Len(), replay.Body.Len(), firstHash, replayHash)
	}

	get := requestJSON(t, handler, http.MethodGet, "/v1/devices/self/sip-config", f.clientToken, nil, "")
	requireStatus(t, get, http.StatusOK)
	var getBody map[string]json.RawMessage
	if err := json.Unmarshal(get.Body.Bytes(), &getBody); err != nil {
		t.Fatal("decode SIP config GET")
	}
	if _, containsPassword := getBody["password"]; containsPassword {
		t.Fatal("SIP config GET exposed the credential password")
	}

	var ciphertext []byte
	if err := database.db.QueryRow(`SELECT response_ciphertext FROM device_sip_credentials WHERE device_id=$1`, f.clientID).
		Scan(&ciphertext); err != nil {
		t.Fatal("read encrypted SIP bootstrap response")
	}
	if bytes.Contains(ciphertext, []byte(firstConfig.Password)) {
		t.Fatal("SIP password was stored in plaintext in the encrypted response cache")
	}
	var digest string
	if err := database.db.QueryRow(`SELECT password_digest FROM ps_auths WHERE id=$1`, firstConfig.EndpointID).
		Scan(&digest); err != nil {
		t.Fatal("read Asterisk digest credential")
	}
	md5Digest := md5.Sum([]byte(firstConfig.Username + ":" + firstConfig.Realm + ":" + firstConfig.Password))
	if digest != "MD5:"+hex.EncodeToString(md5Digest[:]) || bytes.Contains([]byte(digest), []byte(firstConfig.Password)) {
		t.Fatal("Asterisk realtime did not store the expected Digest A1")
	}

	var rtpTimeout, holdTimeout int
	if err := database.db.QueryRow(`SELECT rtp_timeout,rtp_timeout_hold FROM ps_endpoints WHERE id=$1`, firstConfig.EndpointID).
		Scan(&rtpTimeout, &holdTimeout); err != nil {
		t.Fatal("read finite media inactivity timeouts")
	}
	if rtpTimeout != 30 || holdTimeout != 60 {
		t.Fatal("SIP endpoint must have finite active and held media timeouts")
	}

	var clientCodecs string
	if err := database.db.QueryRow(`SELECT allow FROM ps_endpoints WHERE id=$1`, firstConfig.EndpointID).
		Scan(&clientCodecs); err != nil || clientCodecs != "opus,alaw,ulaw,g722" {
		t.Fatal("approved client profile must offer Opus with gateway-compatible fallbacks")
	}
	gateway := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/sip-credentials/rotate",
		f.gatewayToken, struct{}{}, "gateway-codecs-bootstrap-key-0001")
	requireStatus(t, gateway, http.StatusOK)
	var gatewayCodecs string
	gatewayEndpoint := "dev_" + strings.ReplaceAll(f.gatewayID, "-", "")
	if err := database.db.QueryRow(`SELECT allow FROM ps_endpoints WHERE id=$1`, gatewayEndpoint).
		Scan(&gatewayCodecs); err != nil || gatewayCodecs != "alaw,ulaw,g722" {
		t.Fatal("gateway profile must retain only its verified codec set")
	}

	newKey := "sip-bootstrap-key-000000000002"
	rotated := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/sip-credentials/rotate",
		f.clientToken, struct{}{}, newKey)
	requireStatus(t, rotated, http.StatusOK)
	var nextConfig sipConfiguration
	if err := json.Unmarshal(rotated.Body.Bytes(), &nextConfig); err != nil {
		t.Fatal("decode rotated SIP bootstrap response")
	}
	if nextConfig.Password == firstConfig.Password {
		t.Fatal("a new idempotency key did not rotate the SIP password")
	}
	oldKey := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/sip-credentials/rotate",
		f.clientToken, struct{}{}, key)
	requireStatus(t, oldKey, http.StatusConflict)
}

func TestPostgresSIPCredentialRotationBlockedDuringCall(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	callID := newUUID()
	if _, err := database.db.Exec(`
		INSERT INTO call_sessions(call_id, owner_id, client_device_id, gateway_id, sim_id,
			mapping_revision, direction, to_address, state, gateway_endpoint_id, client_endpoint_id)
		VALUES ($1,$2,$3,$4,$5,1,'outgoing','+12025550123','active',$6,$7)`,
		callID, f.ownerID, f.clientID, f.gatewayID, f.simID,
		"dev_"+strings.ReplaceAll(f.gatewayID, "-", ""),
		"dev_"+strings.ReplaceAll(f.clientID, "-", "")); err != nil {
		t.Fatal("seed active call")
	}
	handler := sipCredentialHandler(t, database)
	response := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/sip-credentials/rotate",
		f.clientToken, struct{}{}, "sip-call-block-key-0000000001")
	requireStatus(t, response, http.StatusConflict)
	var body errorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal("decode active call conflict")
	}
	if body.Error.Code != "DEVICE_BUSY" {
		t.Fatalf("rotation conflict had unexpected code: %s", body.Error.Code)
	}
	var created bool
	if err := database.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM device_sip_credentials WHERE device_id=$1)`, f.clientID).
		Scan(&created); err != nil {
		t.Fatal("check credential provisioning after blocked rotation")
	}
	if created {
		t.Fatal("blocked rotation persisted SIP credentials")
	}
}

func TestPostgresFinalSessionRevokeRevokesSIPRealtimeCredentials(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	firstRefresh, secondRefresh, secondAccess := randomToken(), randomToken(), randomToken()
	if _, err := database.db.Exec(`UPDATE sessions SET refresh_hash=$1 WHERE device_id=$2 AND access_hash=$3`,
		tokenHash(firstRefresh), f.clientID, tokenHash(f.clientToken)); err != nil {
		t.Fatal("set first refresh token")
	}
	if _, err := database.db.Exec(`
		INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at)
		VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`,
		newUUID(), f.clientID, tokenHash(secondAccess), tokenHash(secondRefresh)); err != nil {
		t.Fatal("seed second active session")
	}
	handler := sipCredentialHandler(t, database)
	rotated := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/sip-credentials/rotate",
		f.clientToken, struct{}{}, "sip-revoke-bootstrap-key-0001")
	requireStatus(t, rotated, http.StatusOK)
	var config sipConfiguration
	if err := json.Unmarshal(rotated.Body.Bytes(), &config); err != nil {
		t.Fatal("decode SIP bootstrap response")
	}

	firstRevoke := requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
		RefreshRequest{RefreshToken: firstRefresh}, "")
	requireStatus(t, firstRevoke, http.StatusNoContent)
	var stillPresent bool
	if err := database.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM device_sip_credentials WHERE device_id=$1)`, f.clientID).
		Scan(&stillPresent); err != nil {
		t.Fatal("check credentials after non-final session revoke")
	}
	if !stillPresent {
		t.Fatal("revoking one session removed credentials while another live session remained")
	}

	lastRevoke := requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
		RefreshRequest{RefreshToken: secondRefresh}, "")
	requireStatus(t, lastRevoke, http.StatusNoContent)
	var bindingState string
	if err := database.db.QueryRow(`SELECT state FROM sip_endpoint_bindings WHERE device_id=$1`, f.clientID).
		Scan(&bindingState); err != nil {
		t.Fatal("read revoked SIP endpoint binding")
	}
	if bindingState != "revoked" {
		t.Fatalf("last session revoke left endpoint binding %q", bindingState)
	}
	for _, table := range []string{"device_sip_credentials", "ps_auths", "ps_endpoints", "ps_aors"} {
		query := `SELECT EXISTS(SELECT 1 FROM ` + table + ` WHERE device_id=$1)`
		args := []any{f.clientID}
		if table != "device_sip_credentials" {
			query = `SELECT EXISTS(SELECT 1 FROM ` + table + ` WHERE id=$1)`
			args = []any{config.EndpointID}
		}
		var remains bool
		if err := database.db.QueryRow(query, args...).Scan(&remains); err != nil {
			t.Fatalf("check cleaned SIP table %s", table)
		}
		if remains {
			t.Fatalf("last session revoke left a row in %s", table)
		}
	}
}
