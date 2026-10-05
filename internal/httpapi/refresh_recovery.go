package httpapi

import (
	"crypto/sha256"
	"strings"
)

func validRefreshIdempotencyKey(key string) bool { return visibleIdempotencyKey(key) }

func refreshRecoveryKeyHash(key string) []byte {
	hash := sha256.Sum256([]byte(key))
	return hash[:]
}

func refreshRecoveryAAD(sessionID string, oldRefreshHash, keyHash, newRefreshHash []byte) []byte {
	return []byte(strings.Join([]string{
		"gsm2sip.refresh-recovery.v1",
		sessionID,
		hexDigest(oldRefreshHash),
		hexDigest(keyHash),
		hexDigest(newRefreshHash),
	}, "\x00"))
}
