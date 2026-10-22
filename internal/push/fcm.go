// Package push wakes client phones through Firebase Cloud Messaging.
//
// Messages carry no content: only a request to synchronise, after which the
// phone reads its events over HTTPS as usual. Without a service account the
// package is never constructed and the server behaves as before.
package push

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	messagingScope  = "https://www.googleapis.com/auth/firebase.messaging"
	defaultTokenURL = "https://oauth2.googleapis.com/token"
	defaultSendBase = "https://fcm.googleapis.com/v1/projects/"
)

var (
	// ErrTokenGone means this project cannot deliver to the device token:
	// FCM no longer knows it, or it belongs to another Firebase project.
	ErrTokenGone = errors.New("push token is not deliverable")
	// ErrMisconfigured means the server's credentials cannot send to this
	// app: a wrong, revoked or foreign-project service account.
	ErrMisconfigured = errors.New("push credentials are not valid for this app")
)

// Sender delivers one wake request to a device token.
type Sender interface {
	// Send wakes the device with data, which must not carry message
	// content. validateOnly asks FCM to check the request without
	// delivering it.
	Send(ctx context.Context, token string, data map[string]string, validateOnly bool) error
	// Ready reports whether the credentials currently work.
	Ready() bool
}

type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// FCM sends through the HTTP v1 API with a service-account key.
type FCM struct {
	project  string
	email    string
	key      *rsa.PrivateKey
	tokenURL string
	sendBase string
	client   *http.Client
	now      func() time.Time

	mu          sync.Mutex
	accessToken string
	accessUntil time.Time
	ready       bool
}

// LoadFCM reads a Firebase service-account JSON file.
func LoadFCM(path string) (*FCM, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read service account: %w", err)
	}
	return NewFCM(data)
}

// NewFCM parses a Firebase service-account JSON document.
func NewFCM(document []byte) (*FCM, error) {
	var account serviceAccount
	if err := json.Unmarshal(document, &account); err != nil {
		return nil, errors.New("service account is not valid JSON")
	}
	if account.Type != "service_account" || account.ProjectID == "" || account.ClientEmail == "" || account.PrivateKey == "" {
		return nil, errors.New("service account must have type service_account, project_id, client_email and private_key")
	}
	block, _ := pem.Decode([]byte(account.PrivateKey))
	if block == nil {
		return nil, errors.New("service account private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("service account private_key is not PKCS#8")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("service account private_key is not RSA")
	}
	tokenURL := account.TokenURI
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	return &FCM{
		project: account.ProjectID, email: account.ClientEmail, key: key,
		tokenURL: tokenURL, sendBase: defaultSendBase,
		client: &http.Client{Timeout: 10 * time.Second}, now: time.Now,
	}, nil
}

// Project is the Firebase project the credentials belong to.
func (f *FCM) Project() string { return f.project }

// Check obtains an access token, proving the key is accepted by Google.
func (f *FCM) Check(ctx context.Context) error {
	_, err := f.token(ctx)
	return err
}

func (f *FCM) Ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *FCM) setReady(ready bool) {
	f.mu.Lock()
	f.ready = ready
	if !ready {
		f.accessToken = ""
	}
	f.mu.Unlock()
}

func (f *FCM) Send(ctx context.Context, token string, data map[string]string, validateOnly bool) error {
	access, err := f.token(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"validate_only": validateOnly,
		"message": map[string]any{
			"token": token,
			"data":  data,
			"android": map[string]any{
				"priority": "HIGH",
				"ttl":      "60s",
			},
		},
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.sendBase+url.PathEscape(f.project)+"/messages:send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := f.client.Do(request)
	if err != nil {
		return fmt.Errorf("send push: %w", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
	if response.StatusCode == http.StatusOK {
		return nil
	}
	return f.classify(response.StatusCode, payload)
}

// classify maps an FCM error to what the caller should do with it.
func (f *FCM) classify(status int, payload []byte) error {
	var envelope struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &envelope)
	code := envelope.Error.Status
	for _, detail := range envelope.Error.Details {
		if detail.ErrorCode != "" {
			code = detail.ErrorCode
		}
	}
	switch {
	case code == "UNREGISTERED" || (status == http.StatusNotFound && code == "NOT_FOUND"):
		return ErrTokenGone
	case code == "INVALID_ARGUMENT" && status == http.StatusBadRequest:
		// A malformed or foreign token; the message itself is fixed.
		return ErrTokenGone
	case code == "SENDER_ID_MISMATCH":
		// The token was issued for another Firebase project. With a server key
		// from the wrong project every token fails this way, so none register.
		return ErrTokenGone
	case code == "THIRD_PARTY_AUTH_ERROR" || status == http.StatusUnauthorized || status == http.StatusForbidden:
		f.setReady(false)
		return ErrMisconfigured
	default:
		return fmt.Errorf("push rejected: HTTP %d %s", status, code)
	}
}

func (f *FCM) token(ctx context.Context) (string, error) {
	f.mu.Lock()
	if f.accessToken != "" && f.now().Before(f.accessUntil) {
		token := f.accessToken
		f.mu.Unlock()
		return token, nil
	}
	f.mu.Unlock()

	assertion, err := f.assertion()
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := f.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch access token: %w", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
	if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		f.setReady(false)
		return "", ErrMisconfigured
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch access token: HTTP %d", response.StatusCode)
	}
	var granted struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(payload, &granted); err != nil || granted.AccessToken == "" {
		return "", errors.New("fetch access token: malformed response")
	}
	lifetime := time.Duration(granted.ExpiresIn) * time.Second
	if lifetime <= 0 || lifetime > time.Hour {
		lifetime = time.Hour
	}
	f.mu.Lock()
	f.accessToken = granted.AccessToken
	f.accessUntil = f.now().Add(lifetime - 5*time.Minute)
	f.ready = true
	f.mu.Unlock()
	return granted.AccessToken, nil
}

func (f *FCM) assertion() (string, error) {
	now := f.now().Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": f.email, "scope": messagingScope, "aud": f.tokenURL,
		"iat": now, "exp": now + 3600,
	})
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(nil, f.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("sign access token request")
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
