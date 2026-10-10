package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
)

// MaxBodyBytes matches the inspected Tangent plugin HTTP contract.
const MaxBodyBytes = 32 * 1024

// VerifyRequest is an isolated acceptance seam, not a host identity provider.
// The verifier must resolve current authority for this exact operation AND all
// resources its input can access. Input and identity are detached on each check.
// Query filters and labels cannot grant access. Production commit-time authority
// requires a later coupled host/transaction contract and is unavailable here.
type VerifyRequest func(context.Context, json.RawMessage, string, map[string]any) (operations.Authority, error)

// Adapter contains an immutable service configuration for an explicitly injected
// copied shadow. It retains no caller state between requests.
type Adapter struct {
	service operations.Service
	verify  VerifyRequest
	routes  map[string]Route
}

// New snapshots configuration. It never discovers, imports or opens a store.
// Missing verification refuses reads as well as writes, regardless of any
// verifier previously configured on the internal shadow service.
func New(service operations.Service, verify VerifyRequest) *Adapter {
	service.Verify = nil
	a := &Adapter{service: service, verify: verify, routes: map[string]Route{}}
	for _, route := range Routes() {
		a.routes[route.Declaration.Path] = route
	}
	return a
}

// HTTPHandle accepts only literal POST operation paths with a JSON object body.
// Origin/participant checks belong to the host: their headers are stripped by
// its public dispatcher and cannot establish a verified portfolio caller here.
func (a *Adapter) HTTPHandle(ctx context.Context, request subprocess.HTTPRequest) (subprocess.HTTPResponse, error) {
	return a.handle(ctx, request), nil
}

func (a *Adapter) handle(ctx context.Context, request subprocess.HTTPRequest) subprocess.HTTPResponse {
	route, ok := a.routes[request.Path]
	if !ok {
		return refusal(http.StatusNotFound, "not_found", "no such route")
	}
	if request.Method != route.Declaration.Method {
		return refusal(http.StatusMethodNotAllowed, "bad_request", "method not allowed")
	}
	identity := bytes.Clone(request.Identity)
	if a.verify == nil || len(bytes.TrimSpace(identity)) == 0 || bytes.Equal(bytes.TrimSpace(identity), []byte("null")) || !json.Valid(identity) {
		return authorityRefusal()
	}
	if len(request.Body) > MaxBodyBytes {
		return refusal(http.StatusRequestEntityTooLarge, "bad_request", "body exceeds 32 KiB")
	}
	if request.RawPath != "" && request.RawPath != request.Path || request.RawQuery != "" || len(request.Query) != 0 || request.SessionID != "" {
		return refusal(http.StatusBadRequest, "bad_request", "ambiguous request carrier")
	}
	contentType := ""
	contentTypeSeen := false
	for key, value := range request.Headers {
		switch strings.ToLower(key) {
		case "content-type":
			if contentTypeSeen {
				return refusal(http.StatusBadRequest, "bad_request", "ambiguous content type")
			}
			contentTypeSeen = true
			contentType = value
		case "accept", "accept-language":
		default:
			return refusal(http.StatusBadRequest, "bad_request", "unsupported request header")
		}
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return refusal(http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
	}
	input, err := decodeObject(request.Body)
	if err != nil {
		return refusal(http.StatusBadRequest, "bad_request", "body must be one unambiguous JSON object")
	}
	check := func() (operations.Authority, error) {
		if contextErr := ctx.Err(); contextErr != nil {
			return operations.Authority{}, contextErr
		}
		detached, copyErr := copyObject(input)
		if copyErr != nil {
			return operations.Authority{}, copyErr
		}
		return a.verify(ctx, bytes.Clone(identity), route.Operation.Name, detached)
	}
	authority, err := check()
	if err != nil || !admitted(authority) {
		return authorityRefusal()
	}
	service := a.service
	service.Verify = func(_ context.Context, _ operations.Caller, operation string) (operations.Authority, error) {
		current, checkErr := check()
		if operation != route.Operation.Name || !admitted(current) || current.Principal != authority.Principal {
			return operations.Authority{}, errors.New("request authority refused")
		}
		return current, checkErr
	}
	result, callErr := service.Call(ctx, operations.Caller{Binding: identity}, route.Operation.Name, input)
	// Recheck after the service's snapshot/lock/upstream waits, before disclosure
	// of either a result or an error (including CAS current-item details).
	current, checkErr := check()
	if checkErr != nil || !admitted(current) || current.Principal != authority.Principal {
		return authorityRefusal()
	}
	if callErr != nil {
		var domain *operations.Error
		if !errors.As(callErr, &domain) {
			return refusal(http.StatusBadGateway, "unavailable", "operation unavailable")
		}
		status := http.StatusBadRequest
		switch domain.Code {
		case "not_found":
			status = http.StatusNotFound
		case "conflict":
			status = http.StatusConflict
		case "locked":
			status = http.StatusLocked
		case "unavailable":
			status = http.StatusBadGateway
		}
		return jsonResponse(status, map[string]any{"error": domain})
	}
	status := http.StatusOK
	if route.Operation.Name == "create" || route.Operation.Name == "inbox_add" {
		status = http.StatusCreated
	}
	return jsonResponse(status, result)
}

func admitted(a operations.Authority) bool {
	return a.Verified && a.Allowed && strings.TrimSpace(a.Principal) != ""
}
func authorityRefusal() subprocess.HTTPResponse {
	return refusal(http.StatusServiceUnavailable, "unavailable", "verified caller authority unavailable")
}
func refusal(status int, code, message string) subprocess.HTTPResponse {
	return jsonResponse(status, map[string]any{"error": &operations.Error{Code: code, Message: message, Details: map[string]any{}}})
}
func jsonResponse(status int, value any) subprocess.HTTPResponse {
	raw, err := json.Marshal(value)
	if err != nil {
		status = http.StatusBadGateway
		raw = []byte(`{"error":{"code":"unavailable","message":"response unavailable","details":{}}}`)
	}
	return subprocess.HTTPResponse{Status: status, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store"}, Body: raw}
}

func copyObject(input map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	return decodeObject(raw)
}

func decodeObject(raw []byte) (map[string]any, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := jsonValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	return object, nil
}

// Token decoding preserves numbers/null/unknown fields while refusing duplicate
// keys at every depth. It does not copy or replace domain schema validation.
func jsonValue(d *json.Decoder, depth int) (any, error) {
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
	var value any
	switch delim {
	case '{':
		object := map[string]any{}
		for d.More() {
			keyToken, keyErr := d.Token()
			if keyErr != nil {
				return nil, keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate key")
			}
			child, childErr := jsonValue(d, depth+1)
			if childErr != nil {
				return nil, childErr
			}
			object[key] = child
		}
		value = object
	case '[':
		array := []any{}
		for d.More() {
			child, childErr := jsonValue(d, depth+1)
			if childErr != nil {
				return nil, childErr
			}
			array = append(array, child)
		}
		value = array
	default:
		return nil, errors.New("unexpected delimiter")
	}
	_, err = d.Token()
	return value, err
}
