package push

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestWakeUpCarriesOnlyKindAndDevice(t *testing.T) {
	var got map[string]any
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	if err := NewHTTP(true).Send(context.Background(), server.URL+"/v1/w/abc", "call", "dev-1"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["kind"] != "call" || got["device_id"] != "dev-1" {
		t.Fatalf("body %v", got)
	}
	if headers.Get("Urgency") != "high" || headers.Get("TTL") != "60" {
		t.Fatalf("headers %v", headers)
	}
}

func TestStatusesTellTheNodeWhatToDo(t *testing.T) {
	for status, want := range map[int]error{
		http.StatusGone: ErrEndpointGone, http.StatusNotFound: ErrEndpointGone,
		http.StatusBadRequest: ErrEndpointRefused, http.StatusFound: ErrEndpointRefused,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://169.254.169.254/")
			w.WriteHeader(status)
		}))
		err := NewHTTP(true).Send(context.Background(), server.URL, "sms", "d")
		server.Close()
		if !errors.Is(err, want) {
			t.Fatalf("HTTP %d: %v", status, err)
		}
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		err := NewHTTP(true).Send(context.Background(), server.URL, "sms", "d")
		server.Close()
		if err == nil || errors.Is(err, ErrEndpointGone) || errors.Is(err, ErrEndpointRefused) {
			t.Fatalf("HTTP %d should be retried: %v", status, err)
		}
	}
}

func TestPublicEndpointsOnlyByDefault(t *testing.T) {
	contacted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	sender := NewHTTP(false)
	if err := sender.Send(context.Background(), server.URL, "check", "d"); !errors.Is(err, ErrInvalidEndpoint) || contacted {
		t.Fatalf("loopback endpoint was contacted: %v", err)
	}
	for _, bad := range []string{"", "http://relay.example/w", "ftp://relay.example", "https://u:p@relay.example", "https://relay.example/#x"} {
		if !errors.Is(sender.Validate(bad), ErrInvalidEndpoint) {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := sender.Validate("https://kyubey.example.workers.dev/v1/w/abc"); err != nil {
		t.Fatal(err)
	}
	if NewHTTP(true).Validate("http://192.168.1.2:8080/up") != nil {
		t.Fatal("private endpoints are allowed when configured")
	}
	for addr, public := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true, "127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.100.1.1": false, "::1": false, "fd00::1": false, "0.0.0.0": false, "::ffff:127.0.0.1": false,
	} {
		if publicAddress(netip.MustParseAddr(addr)) != public {
			t.Fatalf("%s public=%v", addr, !public)
		}
	}
}
