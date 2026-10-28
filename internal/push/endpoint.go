// Package push wakes client phones by posting to the push endpoint each phone
// registered: a Kyubey relay URL, or any compatible (UnifiedPush-style)
// endpoint. The server holds no push credentials. A wake-up carries only
// what changed and identifiers on this node; the phone then reads its events
// over HTTPS.
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
	"strconv"
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
	// Wake posts a wire 2 wake-up, which only phones that switched to
	// reception v2 understand.
	Wake(ctx context.Context, endpoint string, wake Wake) error
}

// Wake is a wire 2 wake-up: which domain changed and, for a call, which call,
// so the phone fetches that one call. TTL and Urgency travel as the standard
// HTTP headers.
type Wake struct {
	Kind       string // "call" or "sms"
	DeviceID   string
	WakeID     string
	CallID     string // calls only
	Revision   int64
	Generation int64
	TTL        time.Duration
	Urgency    string // "high", "normal", "low" or "very-low"
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
	body, _ := json.Marshal(map[string]string{"kind": kind, "device_id": deviceID})
	// WebPush hints: deliver soon, drop if not delivered within a minute.
	return h.post(ctx, endpoint, body, 60, "high")
}

func (h *HTTP) Wake(ctx context.Context, endpoint string, wake Wake) error {
	body := map[string]any{"v": 2, "kind": wake.Kind, "device_id": wake.DeviceID, "wake_id": wake.WakeID,
		"revision": wake.Revision, "endpoint_generation": wake.Generation}
	if wake.CallID != "" {
		body["call_id"] = wake.CallID
	}
	encoded, _ := json.Marshal(body)
	return h.post(ctx, endpoint, encoded, int64(max(wake.TTL, 0)/time.Second), wake.Urgency)
}

func (h *HTTP) post(ctx context.Context, endpoint string, body []byte, ttlSeconds int64, urgency string) error {
	if err := h.Validate(endpoint); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrInvalidEndpoint
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("TTL", strconv.FormatInt(ttlSeconds, 10))
	request.Header.Set("Urgency", urgency)
	response, err := h.client.Do(request)
	if err != nil {
		if errors.Is(err, ErrInvalidEndpoint) {
			return ErrInvalidEndpoint
		}
		return fmt.Errorf("post wake-up: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.Header.Get("Cf-Mitigated") != "" {
		// A Cloudflare challenge in front of the relay, not the relay's
		// answer: the endpoint may still work once the zone stops
		// challenging this node, so retry instead of forgetting it.
		return fmt.Errorf("push endpoint challenged (HTTP %d)", response.StatusCode)
	}
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
