package httpapi

import (
	"crypto/md5"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SIPSettings identifies the public SIP listener, never the private ARI service.
type SIPSettings struct {
	// ServerName is the SIP host given to devices. Empty means each device is
	// told the host it used to reach this API, so one node can serve phones on
	// different networks (LAN, Tailscale) without a configured address.
	ServerName string
	Port       int
	CAPEM      string
	EnableOpus bool
	// Transport is SIPTransportTLS (default, with SDES-SRTP media) or
	// SIPTransportUDP (plain SIP and RTP, for a trusted LAN or VPN).
	Transport string
}

const (
	SIPTransportTLS = "tls"
	SIPTransportUDP = "udp"
)

// Secure reports whether signalling uses TLS; media then uses SDES-SRTP.
func (c SIPSettings) Secure() bool { return c.Transport != SIPTransportUDP }

func (c SIPSettings) transportName() string {
	if c.Secure() {
		return SIPTransportTLS
	}
	return SIPTransportUDP
}

func (c SIPSettings) mediaEncryption() string {
	if c.Secure() {
		return "sdes"
	}
	return "none"
}

type sipConfiguration struct {
	Available        bool   `json:"available"`
	Reason           string `json:"reason,omitempty"`
	EndpointID       string `json:"endpoint_id,omitempty"`
	Username         string `json:"auth_username,omitempty"`
	Realm            string `json:"auth_realm,omitempty"`
	AOR              string `json:"aor,omitempty"`
	RegistrarURI     string `json:"registrar_uri,omitempty"`
	OutboundProxyURI string `json:"outbound_proxy_uri,omitempty"`
	ServerName       string `json:"server_name,omitempty"`
	CAPEM            string `json:"ca_pem,omitempty"`
	Transport        string `json:"transport,omitempty"`
	MediaEncryption  string `json:"media_encryption,omitempty"`
	Password         string `json:"password,omitempty"`
}

func (c SIPSettings) Valid() bool {
	if c.Transport != "" && c.Transport != SIPTransportTLS && c.Transport != SIPTransportUDP {
		return false
	}
	if c.Port < 1 || c.Port > 65535 {
		return false
	}
	return c.ServerName == "" || validSIPHost(c.ServerName)
}

// requestSIPHost is the host part of the request's Host header, or "" when it
// cannot serve as a SIP host.
func requestSIPHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := strings.TrimSpace(r.Host)
	if split, _, err := net.SplitHostPort(host); err == nil {
		host = split
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if !validSIPHost(host) {
		return ""
	}
	return host
}

func validSIPHost(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	if net.ParseIP(name) != nil {
		return true
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func (s *Server) sipConfig(p Principal, r *http.Request) sipConfiguration {
	if s.secretCipher == nil || !s.sip.Valid() {
		return sipConfiguration{Reason: "sip_not_configured"}
	}
	serverName := s.sip.ServerName
	if serverName == "" {
		if serverName = requestSIPHost(r); serverName == "" {
			return sipConfiguration{Reason: "sip_not_configured"}
		}
	}
	id := "dev_" + strings.ReplaceAll(p.DeviceID, "-", "")
	host := net.JoinHostPort(serverName, strconv.Itoa(s.sip.Port))
	scheme, caPEM := "sips:", s.sip.CAPEM
	if !s.sip.Secure() {
		scheme, caPEM = "sip:", ""
	}
	return sipConfiguration{Available: true, EndpointID: id, Username: id, Realm: "gsm2sip",
		AOR: scheme + id + "@" + host, RegistrarURI: scheme + host,
		OutboundProxyURI: scheme + host + ";transport=" + s.sip.transportName() + ";lr", ServerName: serverName,
		CAPEM: caPEM, Transport: s.sip.transportName(), MediaEncryption: s.sip.mediaEncryption()}
}

func (s *Server) getSIPConfig(w http.ResponseWriter, r *http.Request, p Principal) {
	writeJSON(w, http.StatusOK, s.sipConfig(p, r))
}

func sipCredentialAAD(deviceID string, keyHash []byte) []byte {
	return []byte("gsm2sip.sip-bootstrap.v1\x00" + deviceID + "\x00" + hex.EncodeToString(keyHash))
}

func (s *Server) rotateSIPCredentials(w http.ResponseWriter, r *http.Request, p Principal) {
	config := s.sipConfig(p, r)
	if !config.Available {
		writeError(w, 503, "SIP_NOT_CONFIGURED", "SIP credentials are not configured.", false)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !validRefreshIdempotencyKey(key) {
		writeError(w, 400, "IDEMPOTENCY_KEY_INVALID", "A durable Idempotency-Key of 16 to 128 characters is required.", false)
		return
	}
	var body struct{}
	if !decodeOrError(w, r, &body, 4096) {
		return
	}
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	// Serialize provisioning, revocation and session replacement on the device.
	var state string
	if err = tx.QueryRowContext(r.Context(), `SELECT state FROM devices WHERE id=$1 AND owner_id=$2 FOR UPDATE`, p.DeviceID, p.OwnerID).Scan(&state); err != nil {
		writeDBUnavailable(w)
		return
	}
	if state != "active" {
		writeError(w, 401, "SESSION_REVOKED", "Pair this device again.", false)
		return
	}
	var activeSession bool
	if err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM sessions WHERE device_id=$1 AND access_hash=$2 AND access_expires_at>now())`, p.DeviceID, tokenHash(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))).Scan(&activeSession); err != nil {
		writeDBUnavailable(w)
		return
	}
	if !activeSession {
		writeError(w, 401, "SESSION_REVOKED", "Pair this device again.", false)
		return
	}
	keyHash := tokenHash(key)
	var oldKey, encrypted []byte
	var expires time.Time
	err = tx.QueryRowContext(r.Context(), `SELECT idempotency_hash,response_ciphertext,replay_expires_at FROM device_sip_credentials WHERE device_id=$1`, p.DeviceID).Scan(&oldKey, &encrypted, &expires)
	if err != nil && err != sql.ErrNoRows {
		writeDBUnavailable(w)
		return
	}
	if err == nil && subtle.ConstantTimeCompare(oldKey, keyHash) == 1 {
		if !expires.After(s.now().UTC()) {
			writeError(w, 409, "SIP_BOOTSTRAP_EXPIRED", "Credential replay expired; use a new key.", false)
			return
		}
		response, err := s.secretCipher.Open(encrypted, sipCredentialAAD(p.DeviceID, keyHash))
		if err != nil {
			writeDBUnavailable(w)
			return
		}
		if err = tx.Commit(); err != nil {
			writeDBUnavailable(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		_, _ = w.Write(response)
		return
	}
	var reused, busy bool
	if err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM sip_credential_used_keys WHERE device_id=$1 AND key_hash=$2)`, p.DeviceID, keyHash).Scan(&reused); err != nil {
		writeDBUnavailable(w)
		return
	}
	if reused {
		writeError(w, 409, "IDEMPOTENCY_KEY_REUSED", "This key belongs to a previous SIP credential generation.", false)
		return
	}
	if err = tx.QueryRowContext(r.Context(), `SELECT
		EXISTS(SELECT 1 FROM call_sessions WHERE (gateway_id=$1 OR client_device_id=$1) AND state<>'ended')
		OR EXISTS(SELECT 1 FROM call_participants WHERE client_device_id=$1
			AND state IN ('candidate','pending_wakeup','ringing','connecting','accepted'))`, p.DeviceID).Scan(&busy); err != nil {
		writeDBUnavailable(w)
		return
	}
	if busy {
		writeError(w, 409, "DEVICE_BUSY", "End the active call before rotating credentials.", true)
		return
	}
	config.Password = randomToken()
	digest := md5.Sum([]byte(config.Username + ":" + config.Realm + ":" + config.Password))
	response, err := json.Marshal(config)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	encrypted, err = s.secretCipher.Seal(response, sipCredentialAAD(p.DeviceID, keyHash))
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	context, codecs := "gsm-client", "alaw,ulaw,g722"
	if p.Role == "client" && s.sip.EnableOpus {
		codecs = "opus,alaw,ulaw,g722"
	}
	if p.Role == "gateway" {
		context = "gsm-gateway"
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sip_endpoint_bindings(device_id,endpoint_id,auth_username,aor,state) VALUES($1,$2,$2,$2,'active') ON CONFLICT(device_id) DO UPDATE SET state='active',updated_at=now()`, []any{p.DeviceID, config.EndpointID}},
		{`INSERT INTO ps_auths(id,auth_type,realm,username,password_digest,supported_algorithms_uas,supported_algorithms_uac) VALUES($1,'userpass','gsm2sip',$1,$2,'MD5','MD5') ON CONFLICT(id) DO UPDATE SET password_digest=EXCLUDED.password_digest`, []any{config.EndpointID, "MD5:" + hex.EncodeToString(digest[:])}},
		{`INSERT INTO ps_aors(id,max_contacts,remove_existing,qualify_frequency) VALUES($1,1,'yes',30) ON CONFLICT(id) DO NOTHING`, []any{config.EndpointID}},
		{`INSERT INTO ps_endpoints(id,transport,aors,auth,context,disallow,allow,direct_media,force_rport,rewrite_contact,rtp_symmetric,media_encryption,media_encryption_optimistic,dtmf_mode,identify_by) VALUES($1,$4,$1,$1,$2,'all',$3,'no','yes','yes','yes',$5,'no','rfc4733','auth_username,username') ON CONFLICT(id) DO UPDATE SET context=EXCLUDED.context,allow=EXCLUDED.allow,transport=EXCLUDED.transport,media_encryption=EXCLUDED.media_encryption`, []any{config.EndpointID, context, codecs, "transport-" + s.sip.transportName(), endpointMediaEncryption(s.sip)}},
		{`INSERT INTO device_sip_credentials(device_id,idempotency_hash,response_ciphertext,replay_expires_at) VALUES($1,$2,$3,$4) ON CONFLICT(device_id) DO UPDATE SET idempotency_hash=EXCLUDED.idempotency_hash,response_ciphertext=EXCLUDED.response_ciphertext,replay_expires_at=EXCLUDED.replay_expires_at,updated_at=now()`, []any{p.DeviceID, keyHash, encrypted, s.now().UTC().Add(5 * time.Minute)}},
		{`INSERT INTO sip_credential_used_keys(device_id,key_hash) VALUES($1,$2)`, []any{p.DeviceID, keyHash}},
		{`INSERT INTO audit_log(owner_id,actor_device_id,action,resource_id) VALUES($1,$2,'sip.credentials.rotated',$2)`, []any{p.OwnerID, p.DeviceID}},
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(r.Context(), statement.sql, statement.args...); err != nil {
			writeDBUnavailable(w)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(response)
}

// endpointMediaEncryption is the PJSIP media_encryption value for the mode.
func endpointMediaEncryption(c SIPSettings) string {
	if c.Secure() {
		return "sdes"
	}
	return "no"
}
