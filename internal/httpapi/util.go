package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var dialStringPattern = regexp.MustCompile(`^[+0-9*#A-Da-d(). -]{1,64}$`)

func newUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

func validUUID(value string) bool { return uuidPattern.MatchString(value) }

func visibleIdempotencyKey(key string) bool {
	if len(key) < 16 || len(key) > 128 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

func tokenHash(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}

func hashJSON(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	return hash[:], nil
}

func randomToken() string {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(secret[:])
}

func hexDigest(data []byte) string { return hex.EncodeToString(data) }

func commandDigest(c Command) string {
	fields := []string{
		strings.ToLower(c.CommandID), strings.ToLower(c.MessageID), strings.ToLower(c.GatewayID),
		strings.ToLower(c.SIMID), strconv.FormatInt(c.MappingRevision, 10), c.To, c.Text,
		c.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	data := []byte(strings.Join(fields, "\x00"))
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func encodeCursor(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }

func decodeCursor(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", errors.New("invalid cursor")
	}
	return string(decoded), nil
}

func messageCursor(created time.Time, id string) string {
	return encodeCursor(created.UTC().Format(time.RFC3339Nano) + "|" + strings.ToLower(id))
}

func parseMessageCursor(value string) (time.Time, string, error) {
	decoded, err := decodeCursor(value)
	if err != nil {
		return time.Time{}, "", err
	}
	if decoded == "" {
		return time.Time{}, "", nil
	}
	parts := strings.SplitN(decoded, "|", 2)
	if len(parts) != 2 || !validUUID(parts[1]) {
		return time.Time{}, "", errors.New("invalid cursor")
	}
	timestamp, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", errors.New("invalid cursor")
	}
	return timestamp.UTC(), strings.ToLower(parts[1]), nil
}

func parseEventCursor(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	decoded, err := decodeCursor(value)
	if err != nil {
		return 0, err
	}
	cursor, err := strconv.ParseInt(decoded, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New("invalid cursor")
	}
	return cursor, nil
}

func opaqueEventCursor(cursor int64) string {
	return encodeCursor(strconv.FormatInt(cursor, 10))
}

func validText(text string) bool {
	return text != "" && utf8.ValidString(text) && len([]byte(text)) <= 16384 && !strings.ContainsRune(text, '\x00')
}

func validAddress(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && dialStringPattern.MatchString(value)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
