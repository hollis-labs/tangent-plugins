// Package chatbridge lets Tangent's Chat view reach Tether's chat roster
// through the plugin, because Tangent's own binary has no Tether client. It
// serves two body-bounded routes and forwards with the plugin's configured
// address and token; it keeps no state and never invents a roster.
package chatbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
)

const (
	AgentsPath = "/api/plugins/messaging/chat/agents"
	SendPath   = "/api/plugins/messaging/chat/send"

	protocolHeader = "Tether-Protocol"
	protocolValue  = "1"
	maxText        = 64 * 1024
	maxResponse    = 1 << 20
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Bridge forwards to one configured Tether endpoint.
type Bridge struct {
	base   string
	token  string
	remote bool
	client *http.Client
	close  func()
}

// New builds a bridge for a unix:/path or http(s) base address. The token may
// be empty; it is sent only as a bearer credential and never returned.
func New(address, token string, timeout time.Duration) (*Bridge, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	transport.IdleConnTimeout = time.Minute
	b := &Bridge{token: token, close: transport.CloseIdleConnections}
	if socket, ok := strings.CutPrefix(address, "unix:"); ok {
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", socket)
		}
		b.base = "http://unix"
	} else {
		u, err := url.Parse(address)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return nil, errBadAddress
		}
		b.base, b.remote = strings.TrimRight(u.String(), "/"), true
	}
	b.client = &http.Client{Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return b, nil
}

type bridgeError string

func (e bridgeError) Error() string { return string(e) }

const errBadAddress = bridgeError("chatbridge: invalid tether address")

// Close releases idle connections.
func (b *Bridge) Close() { b.close() }

// Handles reports whether the request path belongs to the bridge.
func Handles(path string) bool { return path == AgentsPath || path == SendPath }

// HTTPHandle serves both routes.
func (b *Bridge) HTTPHandle(ctx context.Context, req subprocess.HTTPRequest) (subprocess.HTTPResponse, error) {
	switch {
	case req.Method == http.MethodGet && req.Path == AgentsPath:
		if len(req.Body) != 0 || len(req.Query) != 0 {
			return reply(http.StatusBadRequest, map[string]string{"code": "invalid_request"}), nil
		}
		return b.agents(ctx), nil
	case req.Method == http.MethodPost && req.Path == SendPath:
		if len(req.Query) != 0 || len(req.Body) > maxText+4096 {
			return reply(http.StatusBadRequest, map[string]string{"code": "invalid_request"}), nil
		}
		return b.send(ctx, req.Body), nil
	}
	return reply(http.StatusNotFound, map[string]string{"code": "not_found"}), nil
}

type roster struct {
	Available bool            `json:"available"`
	Reason    string          `json:"reason,omitempty"`
	Agents    json.RawMessage `json:"agents"`
}

// agents always answers 200: an absent, forbidden or unreachable Tether is a
// state the view renders, not a failure of the route.
func (b *Bridge) agents(ctx context.Context) subprocess.HTTPResponse {
	unavailable := func(reason string) subprocess.HTTPResponse {
		return reply(http.StatusOK, roster{Reason: reason, Agents: json.RawMessage("[]")})
	}
	status, body, err := b.do(ctx, http.MethodGet, "/chat/agents", nil)
	switch {
	case err != nil:
		return unavailable("tether_unreachable")
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return unavailable("tether_endpoint_missing")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return unavailable("tether_forbidden")
	case status != http.StatusOK:
		return unavailable("tether_error")
	}
	var parsed struct {
		Agents []json.RawMessage `json:"agents"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return unavailable("tether_error")
	}
	if parsed.Agents == nil {
		parsed.Agents = []json.RawMessage{}
	}
	agents, _ := json.Marshal(parsed.Agents)
	return reply(http.StatusOK, roster{Available: true, Agents: agents})
}

type sendRequest struct {
	LaunchID       string `json:"launch_id"`
	Text           string `json:"text"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (b *Bridge) send(ctx context.Context, raw []byte) subprocess.HTTPResponse {
	var in sendRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.More() || !identifier.MatchString(in.LaunchID) ||
		strings.TrimSpace(in.Text) == "" || len(in.Text) > maxText || !utf8.ValidString(in.Text) ||
		in.IdempotencyKey == "" || len(in.IdempotencyKey) > 256 {
		return reply(http.StatusBadRequest, map[string]string{"code": "invalid_request"})
	}
	payload, _ := json.Marshal(map[string]string{"text": in.Text, "idempotency_key": in.IdempotencyKey})
	status, body, err := b.do(ctx, http.MethodPost, "/chat/agents/"+url.PathEscape(in.LaunchID)+"/send", payload)
	if err != nil {
		return reply(http.StatusBadGateway, map[string]string{"code": "tether_unreachable"})
	}
	var out struct {
		Mode      string `json:"mode"`
		SessionID string `json:"session_id"`
		Replayed  bool   `json:"replayed"`
		Error     struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		return reply(http.StatusOK, map[string]any{"ok": true, "mode": out.Mode, "session_id": out.SessionID, "replayed": out.Replayed})
	case status == http.StatusConflict && out.Error.Code == "agent_working":
		return reply(http.StatusConflict, map[string]string{"code": "agent_working"})
	case status == http.StatusNotFound:
		return reply(http.StatusNotFound, map[string]string{"code": "chat_unavailable"})
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return reply(http.StatusForbidden, map[string]string{"code": "tether_forbidden"})
	case status == http.StatusConflict && out.Error.Code == "idempotency_conflict":
		return reply(http.StatusConflict, map[string]string{"code": "idempotency_conflict"})
	}
	return reply(http.StatusBadGateway, map[string]string{"code": "tether_error"})
}

func (b *Bridge) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	if b.remote {
		req.Header.Set(protocolHeader, protocolValue)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	return resp.StatusCode, data, err
}

func reply(status int, value any) subprocess.HTTPResponse {
	body, _ := json.Marshal(value)
	return subprocess.HTTPResponse{Status: status, Headers: map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"}, Body: body}
}
