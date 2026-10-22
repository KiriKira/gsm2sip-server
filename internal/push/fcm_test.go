package push

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type fcmStub struct {
	key        *rsa.PrivateKey
	sendStatus int
	sendBody   string
	tokenFails bool
	sends      atomic.Int32
	lastSend   map[string]any
}

func newStub(t *testing.T) (*fcmStub, *httptest.Server, *FCM) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	stub := &fcmStub{key: key, sendStatus: http.StatusOK, sendBody: `{"name":"projects/p/messages/1"}`}
	server := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(server.Close)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	document, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "mahogsm-test", "client_email": "push@mahogsm-test.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   server.URL + "/token",
	})
	fcm, err := NewFCM(document)
	if err != nil {
		t.Fatal(err)
	}
	fcm.sendBase = server.URL + "/v1/projects/"
	return stub, server, fcm
}

func (s *fcmStub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/token":
		form, _ := url.ParseQuery(string(body))
		parts := strings.Split(form.Get("assertion"), ".")
		if s.tokenFails || len(parts) != 3 || form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if rsa.VerifyPKCS1v15(&s.key.PublicKey, crypto.SHA256, digest[:], signature) != nil ||
			!strings.Contains(string(claims), `"scope":"https://www.googleapis.com/auth/firebase.messaging"`) {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"access-1","expires_in":3599,"token_type":"Bearer"}`)
	case r.URL.Path == "/v1/projects/mahogsm-test/messages:send":
		if r.Header.Get("Authorization") != "Bearer access-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.sends.Add(1)
		_ = json.Unmarshal(body, &s.lastSend)
		w.WriteHeader(s.sendStatus)
		_, _ = io.WriteString(w, s.sendBody)
	default:
		http.NotFound(w, r)
	}
}

func TestFCMSendsHighPriorityDataOnly(t *testing.T) {
	stub, _, fcm := newStub(t)
	if fcm.Ready() {
		t.Fatal("ready before any token exchange")
	}
	if err := fcm.Check(context.Background()); err != nil || !fcm.Ready() {
		t.Fatalf("check: %v ready=%v", err, fcm.Ready())
	}
	if err := fcm.Send(context.Background(), "device-token", map[string]string{"type": "sync", "kind": "call"}, true); err != nil {
		t.Fatal(err)
	}
	if stub.lastSend["validate_only"] != true {
		t.Fatalf("validate_only not sent: %v", stub.lastSend)
	}
	message := stub.lastSend["message"].(map[string]any)
	android := message["android"].(map[string]any)
	if message["token"] != "device-token" || android["priority"] != "HIGH" || message["notification"] != nil {
		t.Fatalf("message: %v", message)
	}
	if message["data"].(map[string]any)["kind"] != "call" {
		t.Fatalf("data: %v", message["data"])
	}
}

func TestFCMClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
		ready  bool
	}{
		{"unregistered", 404, `{"error":{"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`, ErrTokenGone, true},
		{"foreign project token", 403, `{"error":{"status":"PERMISSION_DENIED","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`, ErrTokenGone, true},
		{"credentials refused", 403, `{"error":{"status":"PERMISSION_DENIED"}}`, ErrMisconfigured, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, _, fcm := newStub(t)
			stub.sendStatus, stub.sendBody = tc.status, tc.body
			err := fcm.Send(context.Background(), "device-token", map[string]string{"type": "sync"}, false)
			if !errors.Is(err, tc.want) || fcm.Ready() != tc.ready {
				t.Fatalf("err=%v ready=%v", err, fcm.Ready())
			}
		})
	}
	t.Run("transient", func(t *testing.T) {
		stub, _, fcm := newStub(t)
		stub.sendStatus, stub.sendBody = 503, `{"error":{"status":"UNAVAILABLE"}}`
		err := fcm.Send(context.Background(), "device-token", nil, false)
		if err == nil || errors.Is(err, ErrTokenGone) || errors.Is(err, ErrMisconfigured) || !fcm.Ready() {
			t.Fatalf("err=%v ready=%v", err, fcm.Ready())
		}
	})
}

func TestFCMRefusedKeyStaysUnready(t *testing.T) {
	stub, _, fcm := newStub(t)
	stub.tokenFails = true
	if err := fcm.Check(context.Background()); !errors.Is(err, ErrMisconfigured) || fcm.Ready() {
		t.Fatalf("err=%v ready=%v", err, fcm.Ready())
	}
	if stub.sends.Load() != 0 {
		t.Fatal("sent without an access token")
	}
}

func TestNewFCMRejectsUnusableDocuments(t *testing.T) {
	for _, document := range []string{`{}`, `not json`, `{"type":"service_account","project_id":"p","client_email":"e","private_key":"nope"}`} {
		if _, err := NewFCM([]byte(document)); err == nil {
			t.Fatalf("accepted %s", document)
		}
	}
}
