package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/securestore"
)

func seedRefreshRecoverySession(t *testing.T, database *integrationDatabase) (string, string) {
	t.Helper()
	ownerID, deviceID, sessionID, refreshToken := newUUID(), newUUID(), newUUID(), randomToken()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO owners(id,display_name) VALUES ($1,'refresh recovery owner')`, []any{ownerID}},
		{`INSERT INTO devices(id,owner_id,role,name) VALUES ($1,$2,'client','refresh recovery client')`, []any{deviceID, ownerID}},
		{`INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at)
			VALUES ($1,$2,$3,now()+interval '1 day',$4,now()+interval '30 days')`,
			[]any{sessionID, deviceID, tokenHash(randomToken()), tokenHash(refreshToken)}},
	} {
		if _, err := database.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal("seed refresh recovery session")
		}
	}
	return sessionID, refreshToken
}

func refreshRecoveryHandler(t *testing.T, database *integrationDatabase) http.Handler {
	t.Helper()
	cipher, err := securestore.New(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal("create refresh recovery cipher")
	}
	server := New(database.db, nil)
	server.secretCipher = cipher
	return server.Handler()
}

func refreshRequest(t *testing.T, handler http.Handler, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	return requestJSON(t, handler, http.MethodPost, "/v1/auth/refresh", "",
		RefreshRequest{RefreshToken: token}, key)
}

func TestPostgresRefreshRecoveryRestoresLostResponse(t *testing.T) {
	database := openIntegrationDatabase(t)
	_, oldRefreshToken := seedRefreshRecoverySession(t, database)
	handler := refreshRecoveryHandler(t, database)
	key := "refresh-recovery-key-00000001"

	// Treat the first response as lost; retrying the same request must recover
	// byte-for-byte the exact token pair that was already committed.
	first := refreshRequest(t, handler, oldRefreshToken, key)
	requireStatus(t, first, http.StatusOK)
	var rotated SessionTokens
	if err := json.Unmarshal(first.Body.Bytes(), &rotated); err != nil {
		t.Fatal("decode first refresh response")
	}
	if rotated.AccessToken == "" || rotated.RefreshToken == "" || rotated.RefreshToken == oldRefreshToken {
		t.Fatal("refresh did not rotate credentials")
	}

	recovered := refreshRequest(t, handler, oldRefreshToken, key)
	requireStatus(t, recovered, http.StatusOK)
	if !bytes.Equal(first.Body.Bytes(), recovered.Body.Bytes()) {
		firstHash := sha256.Sum256(first.Body.Bytes())
		recoveredHash := sha256.Sum256(recovered.Body.Bytes())
		t.Fatalf("refresh recovery response differs (bytes %d/%d, SHA-256 %x/%x)",
			first.Body.Len(), recovered.Body.Len(), firstHash, recoveredHash)
	}
	var encryptedResponse []byte
	var cacheExpiresAt, refreshExpiresAt time.Time
	if err := database.db.QueryRow(`
		SELECT rr.response_ciphertext, rr.expires_at, s.refresh_expires_at
		FROM refresh_recoveries rr JOIN sessions s ON s.id=rr.session_id`).
		Scan(&encryptedResponse, &cacheExpiresAt, &refreshExpiresAt); err != nil {
		t.Fatal("read encrypted refresh recovery")
	}
	if !cacheExpiresAt.Equal(refreshExpiresAt) {
		t.Fatal("refresh recovery cache did not expire with the current refresh session")
	}
	if bytes.Contains(encryptedResponse, []byte(oldRefreshToken)) || bytes.Contains(encryptedResponse, []byte(rotated.RefreshToken)) {
		t.Fatal("refresh tokens were stored unencrypted in the recovery row")
	}

	wrongKey := refreshRequest(t, handler, oldRefreshToken, "refresh-recovery-key-00000002")
	requireStatus(t, wrongKey, http.StatusUnauthorized)
}

func TestPostgresRefreshRecoveryRejectsConcurrentDifferentKeys(t *testing.T) {
	database := openIntegrationDatabase(t)
	_, oldRefreshToken := seedRefreshRecoverySession(t, database)
	handler := refreshRecoveryHandler(t, database)
	keys := []string{"refresh-race-key-000000000001", "refresh-race-key-000000000002"}
	start := make(chan struct{})
	responses := make([]*httptest.ResponseRecorder, len(keys))
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			responses[i] = refreshRequest(t, handler, oldRefreshToken, keys[i])
		}(i)
	}
	close(start)
	wg.Wait()
	if responses[0] == nil || responses[1] == nil {
		t.Fatal("refresh requests did not finish")
	}
	statuses := []int{responses[0].Code, responses[1].Code}
	if !((statuses[0] == http.StatusOK && statuses[1] == http.StatusUnauthorized) ||
		(statuses[1] == http.StatusOK && statuses[0] == http.StatusUnauthorized)) {
		t.Fatalf("different idempotency keys must not both refresh one old token: statuses=%v", statuses)
	}
}

func TestPostgresRefreshRecoveryRevocationAndExpiryBlockReplay(t *testing.T) {
	t.Run("revoked session", func(t *testing.T) {
		database := openIntegrationDatabase(t)
		_, oldRefreshToken := seedRefreshRecoverySession(t, database)
		handler := refreshRecoveryHandler(t, database)
		key := "refresh-revoke-key-00000000001"
		first := refreshRequest(t, handler, oldRefreshToken, key)
		requireStatus(t, first, http.StatusOK)
		var rotated SessionTokens
		if err := json.Unmarshal(first.Body.Bytes(), &rotated); err != nil {
			t.Fatal("decode refresh response")
		}
		revoke := requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
			RefreshRequest{RefreshToken: rotated.RefreshToken}, "")
		requireStatus(t, revoke, http.StatusNoContent)
		replay := refreshRequest(t, handler, oldRefreshToken, key)
		requireStatus(t, replay, http.StatusUnauthorized)
	})

	t.Run("revoke with token whose response is pending", func(t *testing.T) {
		database := openIntegrationDatabase(t)
		_, oldRefreshToken := seedRefreshRecoverySession(t, database)
		handler := refreshRecoveryHandler(t, database)
		key := "refresh-pending-revoke-key-00001"
		first := refreshRequest(t, handler, oldRefreshToken, key)
		requireStatus(t, first, http.StatusOK)
		revoke := requestJSON(t, handler, http.MethodPost, "/v1/auth/revoke", "",
			RefreshRequest{RefreshToken: oldRefreshToken}, "")
		requireStatus(t, revoke, http.StatusNoContent)
		replay := refreshRequest(t, handler, oldRefreshToken, key)
		requireStatus(t, replay, http.StatusUnauthorized)
	})

	t.Run("expired recovery cache", func(t *testing.T) {
		database := openIntegrationDatabase(t)
		sessionID, oldRefreshToken := seedRefreshRecoverySession(t, database)
		handler := refreshRecoveryHandler(t, database)
		key := "refresh-expire-key-00000000001"
		first := refreshRequest(t, handler, oldRefreshToken, key)
		requireStatus(t, first, http.StatusOK)
		var rotated SessionTokens
		if err := json.Unmarshal(first.Body.Bytes(), &rotated); err != nil {
			t.Fatal("decode refresh response")
		}
		if _, err := database.db.Exec(`UPDATE refresh_recoveries SET expires_at=$1 WHERE session_id=$2`,
			time.Now().UTC().Add(-time.Second), sessionID); err != nil {
			t.Fatal("expire recovery cache")
		}
		var beforeHash []byte
		var beforeExpiry time.Time
		if err := database.db.QueryRow(`SELECT refresh_hash,refresh_expires_at FROM sessions WHERE id=$1`, sessionID).
			Scan(&beforeHash, &beforeExpiry); err != nil {
			t.Fatal("read current refresh state before replay")
		}
		replay := refreshRequest(t, handler, oldRefreshToken, key)
		requireStatus(t, replay, http.StatusUnauthorized)
		var currentHash []byte
		var currentExpiry time.Time
		if err := database.db.QueryRow(`SELECT refresh_hash,refresh_expires_at FROM sessions WHERE id=$1`, sessionID).
			Scan(&currentHash, &currentExpiry); err != nil {
			t.Fatal("read current refresh state")
		}
		if !bytes.Equal(currentHash, tokenHash(rotated.RefreshToken)) || !bytes.Equal(beforeHash, currentHash) ||
			!beforeExpiry.Equal(currentExpiry) {
			t.Fatal("expired recovery attempt changed or renewed the current refresh session")
		}
	})
}
