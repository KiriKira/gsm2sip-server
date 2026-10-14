package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kirikira/gsm2sip-server/internal/securestore"
)

func TestSIPConfigurationFollowsTransport(t *testing.T) {
	cipher, err := securestore.New(bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal("create cipher")
	}
	principal := Principal{DeviceID: "11111111-2222-3333-4444-555555555555"}
	const ca = "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"

	secure := (&Server{secretCipher: cipher, sip: SIPSettings{ServerName: "sip.example.test", Port: 5061, CAPEM: ca}}).sipConfig(principal)
	if !secure.Available || secure.Transport != "tls" || secure.MediaEncryption != "sdes" || secure.CAPEM != ca ||
		secure.RegistrarURI != "sips:sip.example.test:5061" ||
		secure.OutboundProxyURI != "sips:sip.example.test:5061;transport=tls;lr" ||
		!strings.HasPrefix(secure.AOR, "sips:dev_") {
		t.Fatalf("TLS configuration changed: %+v", secure)
	}

	plain := (&Server{secretCipher: cipher, sip: SIPSettings{ServerName: "192.168.1.10", Port: 5060, CAPEM: ca, Transport: SIPTransportUDP}}).sipConfig(principal)
	if !plain.Available || plain.Transport != "udp" || plain.MediaEncryption != "none" || plain.CAPEM != "" ||
		plain.RegistrarURI != "sip:192.168.1.10:5060" ||
		plain.OutboundProxyURI != "sip:192.168.1.10:5060;transport=udp;lr" ||
		!strings.HasPrefix(plain.AOR, "sip:dev_") || !strings.HasSuffix(plain.AOR, "@192.168.1.10:5060") {
		t.Fatalf("UDP configuration is wrong: %+v", plain)
	}

	if (SIPSettings{ServerName: "sip.example.test", Port: 5060, Transport: "tcp"}).Valid() {
		t.Fatal("an unsupported transport was accepted")
	}
}

func TestPostgresSIPCredentialsProvisionPlainUDPEndpoint(t *testing.T) {
	database := openIntegrationDatabase(t)
	f := seedIntegrationFixture(t, database.db)
	cipher, err := securestore.New(bytes.Repeat([]byte{0x37}, 32))
	if err != nil {
		t.Fatal("create SIP credential cipher")
	}
	handler := NewWithOptions(database.db, nil, Options{
		SecretCipher: cipher,
		SIP:          SIPSettings{ServerName: "192.168.1.10", Port: 5060, Transport: SIPTransportUDP},
	}).Handler()

	response := requestJSON(t, handler, http.MethodPost, "/v1/devices/self/sip-credentials/rotate",
		f.clientToken, struct{}{}, "sip-bootstrap-key-udp-000000001")
	requireStatus(t, response, http.StatusOK)
	var config sipConfiguration
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		t.Fatal("decode SIP bootstrap response")
	}
	if config.Transport != "udp" || config.MediaEncryption != "none" || config.Password == "" {
		t.Fatalf("UDP bootstrap response is wrong: transport=%q media=%q", config.Transport, config.MediaEncryption)
	}
	var transport, mediaEncryption string
	if err := database.db.QueryRow(`SELECT transport,media_encryption FROM ps_endpoints WHERE id=$1`, config.EndpointID).
		Scan(&transport, &mediaEncryption); err != nil {
		t.Fatal("read provisioned endpoint")
	}
	if transport != "transport-udp" || mediaEncryption != "no" {
		t.Fatalf("UDP endpoint provisioned as %s/%s", transport, mediaEncryption)
	}
}
