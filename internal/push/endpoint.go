// Package push wakes client phones by posting to the push endpoint each phone
// registered: a Kyubey relay URL, or any compatible (UnifiedPush-style)
// endpoint. The server holds no push credentials. A wake-up carries only
// what changed and the phone's device ID on this node; the phone then reads
// its events over HTTPS.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

const (
	MaxEndpointLength = 8192
	sendTimeout       = 10 * time.Second
)

var (
	// ErrEndpointGone means the endpoint no longer reaches the phone; forget it.
	ErrEndpointGone = errors.New("push endpoint is gone")
	// ErrEndpointRefused means the endpoint will not take this node's
	// wake-ups; retrying would not help.
	ErrEndpointRefused = errors.New("push endpoint refused the request")
	// ErrInvalidEndpoint means the URL itself is not acceptable.
	ErrInvalidEndpoint = errors.New("push endpoint is not an allowed URL")
)

// Sender delivers one wake-up to an endpoint.
type Sender interface {
	// Validate checks the URL before anything is sent to it.
	Validate(endpoint string) error
	// Send posts kind ("sms", "call", or "check" to test the endpoint
	// without waking the phone) for deviceID.
	Send(ctx context.Context, endpoint, kind, deviceID string) error
}

// HTTP posts wake-ups. Unless private endpoints are allowed, it refuses
// plain HTTP and every loopback, private, link-local or otherwise
// non-public address, checked at connect time so DNS cannot redirect it.
type HTTP struct {
	allowPrivate bool
	client       *http.Client
}

func NewHTTP(allowPrivate bool) *HTTP {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return ErrInvalidEndpoint
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !publicAddress(ip) {
			return ErrInvalidEndpoint
		}
		return nil
	}}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: sendTimeout,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	}
	return &HTTP{allowPrivate: allowPrivate, client: &http.Client{
		Transport: transport, Timeout: sendTimeout,
		// A redirect could lead anywhere; endpoints answer directly.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var (
	sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")
	uniqueLocal        = netip.MustParsePrefix("fc00::/7")
)

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !sharedAddressSpace.Contains(ip) && !uniqueLocal.Contains(ip)
}

func (h *HTTP) Validate(endpoint string) error {
	if endpoint == "" || len(endpoint) > MaxEndpointLength {
		return ErrInvalidEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return ErrInvalidEndpoint
	}
	if parsed.Scheme != "https" && !(h.allowPrivate && parsed.Scheme == "http") {
		return ErrInvalidEndpoint
	}
	return nil
}

func (h *HTTP) Send(ctx context.Context, endpoint, kind, deviceID string) error {
	if err := h.Validate(endpoint); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"kind": kind, "device_id": deviceID})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrInvalidEndpoint
	}
	request.Header.Set("Content-Type", "application/json")
	// WebPush hints: deliver soon, drop if not delivered within a minute.
	request.Header.Set("TTL", "60")
	request.Header.Set("Urgency", "high")
	response, err := h.client.Do(request)
	if err != nil {
		if errors.Is(err, ErrInvalidEndpoint) {
			return ErrInvalidEndpoint
		}
		return fmt.Errorf("post wake-up: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	return classify(response.StatusCode)
}

func classify(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound || status == http.StatusGone:
		return ErrEndpointGone
	case status == http.StatusTooManyRequests || status >= 500:
		return fmt.Errorf("push endpoint HTTP %d", status)
	default:
		return ErrEndpointRefused
	}
}
