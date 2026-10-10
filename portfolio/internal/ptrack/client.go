package ptrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxInput = 32 * 1024
const maxOutput = 1024 * 1024
const operationPrefix = "/api/plugins/portfolio/operations/"
const requestTimeout = 15 * time.Second

// Error preserves the CLI's single JSON error envelope and sanitized failures.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

func (e *Error) Error() string { return e.Message }
func fail(code, message string) *Error {
	return &Error{Code: code, Message: message, Details: object{}}
}

// Client dispatches one operation. Implementations must honor context and must
// not retry mutations: refusal or a lost response may follow committed effects.
type Client interface {
	Call(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

type httpClient struct {
	base   string
	client *http.Client
}

func newHTTPClient(endpoint string) (Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return nil, fail("bad_request", "plugin base_url must be an explicit HTTP(S) origin without credentials, query or fragment")
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSHandshakeTimeout: requestTimeout, ResponseHeaderTimeout: requestTimeout}
	return &httpClient{base: strings.TrimRight(u.String(), "/"), client: &http.Client{Transport: transport, Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *httpClient) Call(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
	if name == "migrate" {
		return nil, fail("unsupported", "administrative migrate is only available in explicit Node mode")
	}
	if len(input) > maxInput {
		return nil, fail("bad_request", "plugin input exceeds 32 KiB")
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	// A non-replayable body and disabled keepalive avoid net/http's retry path.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+operationPrefix+name, io.NopCloser(bytes.NewReader(input)))
	if err != nil {
		return nil, fail("unavailable", "plugin request unavailable")
	}
	req.ContentLength = int64(len(input))
	req.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return nil, fail("unavailable", "plugin transport unavailable; request effects may be unknown, reconcile before retrying")
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxOutput+1))
	if err != nil || len(raw) > maxOutput {
		return nil, fail("unavailable", "plugin response unavailable or exceeds 1 MiB; reconcile effects before retrying")
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, fail("unavailable", "plugin returned an invalid response; reconcile effects before retrying")
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return raw, nil
	}
	// Only an operation's typed error is disclosed. Host HTML/URLs and arbitrary
	// upstream bodies never become a CLI error. No raw request/response logging.
	if wrapper, ok := v.(object); ok && len(wrapper) == 1 {
		if e, ok := wrapper["error"].(object); ok && len(e) == 3 {
			code, codeOK := e["code"].(string)
			message, messageOK := e["message"].(string)
			details, detailsOK := e["details"].(object)
			if codeOK && messageOK && detailsOK && knownCode(code) {
				return nil, &Error{Code: code, Message: message, Details: details}
			}
		}
	}
	return nil, fail("unavailable", "plugin refused the request; reconcile effects before retrying")
}

func knownCode(code string) bool {
	switch code {
	case "bad_request", "invalid", "not_found", "conflict", "locked", "unavailable", "unsupported":
		return true
	}
	return false
}

func decodeJSON(raw []byte) (any, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := readValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON")
	}
	return v, nil
}
func readValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("JSON nesting exceeds 64")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	var out any
	switch delim {
	case '{':
		obj := object{}
		for d.More() {
			keyToken, keyErr := d.Token()
			if keyErr != nil {
				return nil, keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid key")
			}
			if _, duplicate := obj[key]; duplicate {
				return nil, errors.New("duplicate JSON key")
			}
			value, valueErr := readValue(d, depth+1)
			if valueErr != nil {
				return nil, valueErr
			}
			obj[key] = value
		}
		out = obj
	case '[':
		array := []any{}
		for d.More() {
			value, valueErr := readValue(d, depth+1)
			if valueErr != nil {
				return nil, valueErr
			}
			array = append(array, value)
		}
		out = array
	default:
		return nil, errors.New("unexpected delimiter")
	}
	_, err = d.Token()
	return out, err
}
