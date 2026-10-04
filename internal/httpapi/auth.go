package httpapi

import (
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
)

func (s *Server) claimPairing(w http.ResponseWriter, r *http.Request) {
	limited, err := s.consumePairingLimit(r)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	if limited {
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "Pairing attempts exceeded the per-address budget.", true)
		return
	}
	var request PairingClaimRequest
	if !decodeOrError(w, r, &request, 4096) {
		return
	}
	request.PairingCode = strings.TrimSpace(request.PairingCode)
	request.DeviceName = strings.TrimSpace(request.DeviceName)
	if len(request.PairingCode) < 20 || len(request.PairingCode) > 128 || request.DeviceName == "" || len(request.DeviceName) > 100 {
		writeError(w, http.StatusBadRequest, "INVALID_PAIRING_REQUEST", "Pairing code or device name is invalid.", false)
		return
	}
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()

	var pairingID, ownerID, role string
	var expiresAt time.Time
	err = tx.QueryRowContext(r.Context(), `
		SELECT id::text, owner_id::text, role, expires_at
		FROM pairing_codes WHERE code_hash=$1 AND consumed_at IS NULL FOR UPDATE`, tokenHash(request.PairingCode)).
		Scan(&pairingID, &ownerID, &role, &expiresAt)
	if err != nil && err != sql.ErrNoRows {
		writeDBUnavailable(w)
		return
	}
	if err == sql.ErrNoRows || !expiresAt.After(s.now().UTC()) {
		writeError(w, http.StatusUnauthorized, "INVALID_PAIRING_CODE", "Pairing code is invalid or expired.", false)
		return
	}
	deviceID := newUUID()
	if _, err := tx.ExecContext(r.Context(), `UPDATE pairing_codes SET consumed_at=now() WHERE id=$1`, pairingID); err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `INSERT INTO devices(id, owner_id, role, name) VALUES ($1,$2,$3,$4)`, deviceID, ownerID, role, request.DeviceName); err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "DEVICE_CONFLICT", "This device could not be paired.", false)
			return
		}
		writeDBUnavailable(w)
		return
	}
	if role == "gateway" {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO gateways(device_id) VALUES ($1)`, deviceID); err != nil {
			writeDBUnavailable(w)
			return
		}
	}
	accessToken, refreshToken := randomToken(), randomToken()
	accessExpires, refreshExpires := s.now().UTC().Add(accessLifetime), s.now().UTC().Add(refreshLifetime)
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO sessions(id, device_id, access_hash, access_expires_at, refresh_hash, refresh_expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, newUUID(), deviceID, tokenHash(accessToken), accessExpires, tokenHash(refreshToken), refreshExpires); err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO audit_log(owner_id, actor_device_id, action, resource_id)
		VALUES ($1,$2,'device.paired',$2)`, ownerID, deviceID); err != nil {
		writeDBUnavailable(w)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	sip := s.sipConfig(Principal{DeviceID: deviceID})
	writeJSON(w, http.StatusOK, PairingClaimResponse{
		OwnerID: ownerID, DeviceID: deviceID, Role: role,
		SessionTokens: SessionTokens{
			AccessToken: accessToken, AccessExpiresAt: accessExpires,
			RefreshToken: refreshToken, RefreshExpiresAt: refreshExpires,
		},
		SIP: SIPCapability{Available: sip.Available, Reason: sip.Reason},
	})
}

func (s *Server) consumePairingLimit(r *http.Request) (bool, error) {
	ip := requestClientIP(r)
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var minuteCount, hourCount int
	err = tx.QueryRowContext(r.Context(), `
		INSERT INTO pairing_rate_limits(remote_ip,minute_start,minute_attempts,hour_start,hour_attempts)
		VALUES ($1,date_trunc('minute',now()),1,date_trunc('hour',now()),1)
		ON CONFLICT (remote_ip) DO UPDATE SET
		minute_attempts=CASE WHEN pairing_rate_limits.minute_start=date_trunc('minute',now())
			THEN pairing_rate_limits.minute_attempts+1 ELSE 1 END,
		minute_start=date_trunc('minute',now()),
		hour_attempts=CASE WHEN pairing_rate_limits.hour_start=date_trunc('hour',now())
			THEN pairing_rate_limits.hour_attempts+1 ELSE 1 END,
		hour_start=date_trunc('hour',now()),updated_at=now()
		RETURNING minute_attempts,hour_attempts`, ip).Scan(&minuteCount, &hourCount)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return minuteCount > 20 || hourCount > 100, nil
}

func requestClientIP(r *http.Request) string {
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		parts := strings.Split(forwarded, ",")
		candidate := strings.TrimSpace(parts[len(parts)-1])
		if ip := net.ParseIP(candidate); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return "127.0.0.1"
}

func (s *Server) refreshSession(w http.ResponseWriter, r *http.Request) {
	var request RefreshRequest
	if !decodeOrError(w, r, &request, 4096) {
		return
	}
	if len(request.RefreshToken) < 20 || len(request.RefreshToken) > 256 {
		writeError(w, http.StatusUnauthorized, "SESSION_REVOKED", "Refresh token is invalid or expired; pair this device again.", false)
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey != "" && !validRefreshIdempotencyKey(idempotencyKey) {
		writeError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 16 to 128 visible characters.", false)
		return
	}
	oldRefreshHash := tokenHash(request.RefreshToken)
	keyHash := refreshRecoveryKeyHash(idempotencyKey)
	now := s.now().UTC()
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()
	var sessionID, deviceID string
	var expires time.Time
	err = tx.QueryRowContext(r.Context(), `
		SELECT s.id::text, s.device_id::text, s.refresh_expires_at
		FROM sessions s JOIN devices d ON d.id=s.device_id
		WHERE s.refresh_hash=$1 AND d.state='active' FOR UPDATE OF s`, tokenHash(request.RefreshToken)).
		Scan(&sessionID, &deviceID, &expires)
	if err != nil && err != sql.ErrNoRows {
		writeDBUnavailable(w)
		return
	}
	if err == sql.ErrNoRows {
		if idempotencyKey != "" && s.secretCipher != nil {
			var recoverySessionID string
			var newRefreshHash, ciphertext []byte
			replayErr := tx.QueryRowContext(r.Context(), `
				SELECT s.id::text, rr.new_refresh_hash, rr.response_ciphertext
				FROM refresh_recoveries rr
				JOIN sessions s ON s.id=rr.session_id
				JOIN devices d ON d.id=s.device_id
				WHERE rr.old_refresh_hash=$1 AND rr.idempotency_key_hash=$2
				AND rr.expires_at > $3 AND s.refresh_hash=rr.new_refresh_hash
				AND s.access_expires_at > $3 AND s.refresh_expires_at > $3
				AND d.state='active'
				FOR UPDATE OF s, rr`, oldRefreshHash, keyHash, now).
				Scan(&recoverySessionID, &newRefreshHash, &ciphertext)
			if replayErr == nil {
				plaintext, openErr := s.secretCipher.Open(ciphertext,
					refreshRecoveryAAD(recoverySessionID, oldRefreshHash, keyHash, newRefreshHash))
				if openErr != nil || !json.Valid(plaintext) {
					writeError(w, http.StatusServiceUnavailable, "REFRESH_RECOVERY_UNAVAILABLE", "The refresh result could not be recovered.", true)
					return
				}
				if err := tx.Commit(); err != nil {
					writeDBUnavailable(w)
					return
				}
				writeRawJSON(w, http.StatusOK, plaintext)
				return
			}
			if replayErr != sql.ErrNoRows {
				writeDBUnavailable(w)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "SESSION_REVOKED", "Refresh token is invalid or expired; pair this device again.", false)
		return
	}
	if !expires.After(now) {
		writeError(w, http.StatusUnauthorized, "SESSION_REVOKED", "Refresh token is invalid or expired; pair this device again.", false)
		return
	}
	accessToken, refreshToken := randomToken(), randomToken()
	accessExpires, refreshExpires := now.Add(accessLifetime), now.Add(refreshLifetime)
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE sessions SET access_hash=$1, access_expires_at=$2,
		refresh_hash=$3, refresh_expires_at=$4, rotated_at=now() WHERE id=$5`,
		tokenHash(accessToken), accessExpires, tokenHash(refreshToken), refreshExpires, sessionID); err != nil {
		writeDBUnavailable(w)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM refresh_recoveries WHERE session_id=$1`, sessionID); err != nil {
		writeDBUnavailable(w)
		return
	}
	response := SessionTokens{
		AccessToken: accessToken, AccessExpiresAt: accessExpires,
		RefreshToken: refreshToken, RefreshExpiresAt: refreshExpires,
	}
	var responseJSON []byte
	if idempotencyKey != "" && s.secretCipher != nil {
		responseJSON, err = json.Marshal(response)
		if err != nil {
			writeDBUnavailable(w)
			return
		}
		newRefreshHash := tokenHash(refreshToken)
		ciphertext, sealErr := s.secretCipher.Seal(responseJSON,
			refreshRecoveryAAD(sessionID, oldRefreshHash, keyHash, newRefreshHash))
		if sealErr != nil {
			writeError(w, http.StatusServiceUnavailable, "REFRESH_RECOVERY_UNAVAILABLE", "The refresh result could not be stored safely.", true)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO refresh_recoveries(session_id, old_refresh_hash, idempotency_key_hash,
				new_refresh_hash, response_ciphertext, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6)`, sessionID, oldRefreshHash, keyHash,
			newRefreshHash, ciphertext, refreshExpires); err != nil {
			writeDBUnavailable(w)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	if responseJSON != nil {
		writeRawJSON(w, http.StatusOK, responseJSON)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) {
	var request RefreshRequest
	if !decodeOrError(w, r, &request, 4096) {
		return
	}
	if request.RefreshToken == "" || len(request.RefreshToken) > 256 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Refresh token is required.", false)
		return
	}
	tx, err := beginDurable(r.Context(), s.db)
	if err != nil {
		writeDBUnavailable(w)
		return
	}
	defer tx.Rollback()

	var sessionID, deviceID string
	err = tx.QueryRowContext(r.Context(), `
		SELECT s.id::text, s.device_id::text
		FROM sessions s JOIN devices d ON d.id=s.device_id
		WHERE s.refresh_hash=$1 OR EXISTS (
			SELECT 1 FROM refresh_recoveries rr
			WHERE rr.session_id=s.id AND rr.old_refresh_hash=$1 AND rr.expires_at>now()
		)
		FOR UPDATE OF d, s`, tokenHash(request.RefreshToken)).Scan(&sessionID, &deviceID)
	if err != nil && err != sql.ErrNoRows {
		writeDBUnavailable(w)
		return
	}
	if err == sql.ErrNoRows {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE id=$1`, sessionID); err != nil {
		writeDBUnavailable(w)
		return
	}
	var hasLiveSession bool
	if err := tx.QueryRowContext(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM sessions WHERE device_id=$1
			AND (access_expires_at > now() OR refresh_expires_at > now()))`, deviceID).
		Scan(&hasLiveSession); err != nil {
		writeDBUnavailable(w)
		return
	}
	if !hasLiveSession {
		var endpointID string
		bindingErr := tx.QueryRowContext(r.Context(), `
			UPDATE sip_endpoint_bindings SET state='revoked', updated_at=now()
			WHERE device_id=$1 RETURNING endpoint_id`, deviceID).Scan(&endpointID)
		if bindingErr != nil && bindingErr != sql.ErrNoRows {
			writeDBUnavailable(w)
			return
		}
		if bindingErr == sql.ErrNoRows {
			// The endpoint ID is deterministic; this also removes any orphaned
			// Asterisk realtime rows left by an interrupted older bootstrap.
			endpointID = "dev_" + strings.ReplaceAll(deviceID, "-", "")
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM device_sip_credentials WHERE device_id=$1`, deviceID); err != nil {
			writeDBUnavailable(w)
			return
		}
		for _, table := range []string{"ps_auths", "ps_endpoints", "ps_aors"} {
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM `+table+` WHERE id=$1`, endpointID); err != nil {
				writeDBUnavailable(w)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		writeDBUnavailable(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeDBUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "The request could not be committed.", true)
}

var _ = sql.ErrNoRows
