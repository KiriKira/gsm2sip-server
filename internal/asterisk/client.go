// Package asterisk contains the small authenticated ARI client used by the
// call coordinator. It deliberately exposes only call-control operations.
package asterisk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	BaseURL  string
	Username string
	Password string
	App      string
}

type Client struct {
	baseURL *url.URL
	config  Config
	http    *http.Client
	dialer  *websocket.Dialer
}

type Channel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Caller struct {
		Name   string `json:"name"`
		Number string `json:"number"`
	} `json:"caller"`
	Connected struct {
		Name   string `json:"name"`
		Number string `json:"number"`
	} `json:"connected"`
	Dialplan struct {
		Context  string `json:"context"`
		Exten    string `json:"exten"`
		Priority int    `json:"priority"`
	} `json:"dialplan"`
	Creationtime string `json:"creationtime"`
}

type Event struct {
	Type        string   `json:"type"`
	Timestamp   string   `json:"timestamp"`
	Application string   `json:"application"`
	Args        []string `json:"args"`
	Channel     Channel  `json:"channel"`
	Cause       int      `json:"cause"`
	CauseText   string   `json:"cause_txt"`
}

type OriginateRequest struct {
	Endpoint  string
	App       string
	AppArgs   string
	CallerID  string
	ChannelID string
	Variables map[string]string
}

type Bridge struct {
	ID         string   `json:"id"`
	Type       string   `json:"bridge_type"`
	ChannelIDs []string `json:"channels"`
}

type ResponseError struct {
	StatusCode int
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("ARI request rejected with status %d", e.StatusCode)
}

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.BaseURL) == "" || strings.TrimSpace(config.Username) == "" || config.Password == "" || strings.TrimSpace(config.App) == "" {
		return nil, errors.New("ARI URL, username, password, and app are required")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("ARI URL must be an absolute private HTTP(S) URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	httpClient := &http.Client{Timeout: 8 * time.Second}
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		baseURL: parsed,
		config:  config,
		http:    httpClient,
		dialer:  &websocket.Dialer{HandshakeTimeout: 8 * time.Second},
	}, nil
}

func (c *Client) Configured() bool { return c != nil && c.baseURL != nil }

func (c *Client) Check(ctx context.Context) error {
	var info map[string]any
	return c.request(ctx, http.MethodGet, "asterisk/info", nil, nil, &info)
}

func (c *Client) Events(ctx context.Context) (*websocket.Conn, error) {
	wsURL := *c.baseURL
	if wsURL.Scheme == "https" {
		wsURL.Scheme = "wss"
	} else {
		wsURL.Scheme = "ws"
	}
	wsURL.Path = strings.TrimRight(wsURL.Path, "/") + "/events"
	query := wsURL.Query()
	query.Set("app", c.config.App)
	query.Set("subscribeAll", "true")
	wsURL.RawQuery = query.Encode()
	header := http.Header{}
	header.Set("Authorization", "Basic "+basicAuth(c.config.Username, c.config.Password))
	conn, response, err := c.dialer.DialContext(ctx, wsURL.String(), header)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connect ARI event websocket: %w", err)
	}
	return conn, nil
}

func (c *Client) ListChannels(ctx context.Context) ([]Channel, error) {
	var channels []Channel
	err := c.request(ctx, http.MethodGet, "channels", nil, nil, &channels)
	return channels, err
}

func (c *Client) Endpoint(ctx context.Context, endpointID string) (map[string]any, error) {
	var endpoint map[string]any
	err := c.request(ctx, http.MethodGet, "endpoints/PJSIP/"+url.PathEscape(endpointID), nil, nil, &endpoint)
	return endpoint, err
}

func (c *Client) EndpointOnline(ctx context.Context, endpointID string) (bool, error) {
	endpoint, err := c.Endpoint(ctx, endpointID)
	if err != nil {
		return false, err
	}
	state, _ := endpoint["state"].(string)
	return strings.EqualFold(state, "online"), nil
}

func (c *Client) GetChannelVariable(ctx context.Context, channelID, variable string) (string, error) {
	query := url.Values{"variable": []string{variable}}
	var result struct {
		Value string `json:"value"`
	}
	err := c.request(ctx, http.MethodGet, "channels/"+url.PathEscape(channelID)+"/variable", query, nil, &result)
	return result.Value, err
}

func (c *Client) CreateChannel(ctx context.Context, request OriginateRequest) (Channel, error) {
	if request.Endpoint == "" || request.App == "" {
		return Channel{}, errors.New("ARI channel endpoint and app are required")
	}
	query := url.Values{}
	query.Set("endpoint", request.Endpoint)
	query.Set("app", request.App)
	if request.AppArgs != "" {
		query.Set("appArgs", request.AppArgs)
	}
	if request.CallerID != "" {
		query.Set("callerId", request.CallerID)
	}
	if request.ChannelID != "" {
		query.Set("channelId", request.ChannelID)
	}
	body := map[string]any{"variables": request.Variables}
	var channel Channel
	err := c.request(ctx, http.MethodPost, "channels/create", query, body, &channel)
	return channel, err
}

func (c *Client) Dial(ctx context.Context, channelID string, timeout time.Duration) error {
	query := url.Values{}
	if timeout > 0 {
		seconds := int(timeout.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		query.Set("timeout", fmt.Sprintf("%d", seconds))
	}
	return c.request(ctx, http.MethodPost, "channels/"+url.PathEscape(channelID)+"/dial", query, nil, nil)
}

func (c *Client) CreateBridge(ctx context.Context, bridgeID string) error {
	query := url.Values{"type": []string{"mixing"}}
	query.Set("bridgeId", bridgeID)
	return c.request(ctx, http.MethodPost, "bridges", query, nil, nil)
}

func (c *Client) GetBridge(ctx context.Context, bridgeID string) (Bridge, error) {
	var bridge Bridge
	err := c.request(ctx, http.MethodGet, "bridges/"+url.PathEscape(bridgeID), nil, nil, &bridge)
	return bridge, err
}

func (c *Client) AddChannel(ctx context.Context, bridgeID, channelID string) error {
	query := url.Values{"channel": []string{channelID}}
	return c.request(ctx, http.MethodPost, "bridges/"+url.PathEscape(bridgeID)+"/addChannel", query, nil, nil)
}

func (c *Client) Answer(ctx context.Context, channelID string) error {
	return c.request(ctx, http.MethodPost, "channels/"+url.PathEscape(channelID)+"/answer", nil, nil, nil)
}

func (c *Client) Progress(ctx context.Context, channelID string) error {
	return c.request(ctx, http.MethodPost, "channels/"+url.PathEscape(channelID)+"/progress", nil, nil, nil)
}

func (c *Client) Hangup(ctx context.Context, channelID string) error {
	return c.request(ctx, http.MethodDelete, "channels/"+url.PathEscape(channelID), nil, nil, nil)
}

func (c *Client) DestroyBridge(ctx context.Context, bridgeID string) error {
	return c.request(ctx, http.MethodDelete, "bridges/"+url.PathEscape(bridgeID), nil, nil, nil)
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any, output any) error {
	if !c.Configured() {
		return errors.New("ARI is not configured")
	}
	requestURL := *c.baseURL
	requestURL.Path = strings.TrimRight(requestURL.Path, "/") + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		requestURL.RawQuery = query.Encode()
	}
	var bodyReader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(encoded)
	}
	var req *http.Request
	var err error
	if bodyReader == nil {
		req, err = http.NewRequestWithContext(ctx, method, requestURL.String(), nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, requestURL.String(), bodyReader)
		req.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.config.Username, c.config.Password)
	response, err := c.http.Do(req)
	if err != nil {
		return errors.New("ARI request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &ResponseError{StatusCode: response.StatusCode}
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return errors.New("ARI response could not be decoded")
	}
	return nil
}

func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}
