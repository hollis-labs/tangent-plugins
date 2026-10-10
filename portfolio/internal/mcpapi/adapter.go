// Package mcpapi declares the source-only portfolio MCP surface. Production
// request authority and the lossless data-bearing wire contract remain absent.
package mcpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
)

const Prefix = "tangent.portfolio_"

// VerifyRequest is test-owned injection only. It must verify current explicit
// read/write/decide and every resource grant; labels and plugin credentials
// supply no caller authority. Checks receive detached request-local values.
type VerifyRequest func(context.Context, json.RawMessage, string, map[string]any) (operations.Authority, error)

type Adapter struct {
	service operations.Service
	verify  VerifyRequest
	tools   map[string]operations.Operation
}

// New never opens a store or adopts authority from the supplied service.
func New(service operations.Service, verify VerifyRequest) *Adapter {
	service.Verify, service.Admit = nil, nil
	a := &Adapter{service: service, verify: verify, tools: map[string]operations.Operation{}}
	for _, op := range operations.Registry() {
		if op.Name != "migrate" {
			a.tools[Prefix+op.Name] = op
		}
	}
	return a
}

// Tools derives reviewed discovery metadata from the shared domain registry.
// Hints and effects are descriptions, not resource or caller grants.
func Tools() []sdkmanifest.Tool {
	var tools []sdkmanifest.Tool
	for _, op := range operations.Registry() {
		if op.Name == "migrate" {
			continue
		}
		raw, err := json.Marshal(op.Input)
		if err != nil {
			panic(err)
		}
		readOnly, destructive, idempotent := !op.Write, op.Write, !op.Write
		openWorld := strings.HasPrefix(op.Name, "torque_") || op.Name == "board"
		effect := "read"
		if op.Write {
			effect = "write"
		}
		tools = append(tools, sdkmanifest.Tool{Name: Prefix + op.Name, InputSchema: raw, Effect: effect,
			Description: op.Description + " Source-only: requires injected current caller/resource verification; production authority unavailable. Write numbers must have decoded absolute value below 2^53; decimal/exponent tokens are not lossless.",
			Annotations: &sdkmanifest.ToolAnnotations{ReadOnlyHint: &readOnly, DestructiveHint: &destructive, IdempotentHint: &idempotent, OpenWorldHint: &openWorld},
		})
	}
	return tools
}

func (a *Adapter) MCPCallTool(ctx context.Context, request subprocess.MCPCallRequest) (subprocess.MCPCallResult, error) {
	return a.handle(ctx, request), nil
}

func (a *Adapter) handle(ctx context.Context, request subprocess.MCPCallRequest) subprocess.MCPCallResult {
	op, exists := a.tools[request.ToolName]
	if !exists {
		return refused("not_found", "no such tool")
	}
	name := op.Name
	identity := bytes.Clone(request.Identity)
	if a.verify == nil || !json.Valid(identity) || bytes.Equal(bytes.TrimSpace(identity), []byte("null")) {
		return refused("unavailable", "verified caller authority unavailable")
	}
	if request.SessionID != "" || request.Context != nil {
		return refused("bad_request", "unsupported caller carrier")
	}
	raw, err := json.Marshal(request.Arguments)
	if err != nil || len(raw) > operations.MaxInputBytes {
		return refused("bad_request", "arguments exceed input bound or are not JSON")
	}
	decode := func() (map[string]any, error) {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var in map[string]any
		decodeErr := d.Decode(&in)
		return in, decodeErr
	}
	in, err := decode()
	if err != nil || in == nil {
		return refused("bad_request", "arguments must be an object")
	}
	check := func(operation string, input map[string]any) (operations.Authority, error) {
		if contextErr := ctx.Err(); contextErr != nil {
			return operations.Authority{}, contextErr
		}
		copyRaw, copyErr := json.Marshal(input)
		if copyErr != nil {
			return operations.Authority{}, copyErr
		}
		d := json.NewDecoder(bytes.NewReader(copyRaw))
		d.UseNumber()
		var detached map[string]any
		if copyErr = d.Decode(&detached); copyErr != nil {
			return operations.Authority{}, copyErr
		}
		return a.verify(ctx, bytes.Clone(identity), operation, detached)
	}
	authority, err := check(name, in)
	if err != nil || !admitted(authority) {
		return refused("unavailable", "verified caller authority unavailable")
	}
	// This boundary is inclusive. An authored unsafe odd integer can already
	// round down to exactly 2^53 in the SDK/host map decoder. The guard cannot
	// recover numeric tokens and does not promise decimal/exponent losslessness.
	// PM01a1265f-2739 / CW-20261009-0104 decision15968 accepts this bounded
	// contract; raw-number host/SDK follow-up is CW-20261010-0197.
	if op.Write {
		if path := unsafeNumber(in, "$"); path != "" {
			current, checkErr := check(name, in)
			if checkErr != nil || !admitted(current) || current.Principal != authority.Principal {
				return refused("unavailable", "verified caller authority unavailable")
			}
			return encoded(map[string]any{"error": &operations.Error{Code: "invalid", Message: "write numbers must have decoded absolute value below 2^53", Details: map[string]any{"path": path}}}, true)
		}
	}
	service := a.service
	service.Admit = func(_ context.Context, _ operations.Caller, operation string, input map[string]any) (operations.Authority, error) {
		current, checkErr := check(operation, input)
		if name != "batch" && operation != name || !admitted(current) || current.Principal != authority.Principal {
			return operations.Authority{}, errors.New("request authority refused")
		}
		return current, checkErr
	}
	result, callErr := service.Call(ctx, operations.Caller{Binding: bytes.Clone(identity)}, name, in)
	current, err := check(name, in)
	if err != nil || !admitted(current) || current.Principal != authority.Principal {
		return refused("unavailable", "verified caller authority unavailable")
	}
	if callErr != nil {
		var domain *operations.Error
		if !errors.As(callErr, &domain) {
			return refused("unavailable", "operation unavailable")
		}
		return encoded(map[string]any{"error": domain}, true)
	}
	return encoded(result, false)
}

func unsafeNumber(value any, path string) string {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Float64()
		if err != nil || math.Abs(n) >= 9007199254740992 {
			return path
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if found := unsafeNumber(v[key], path+"."+key); found != "" {
				return found
			}
		}
	case []any:
		for i, child := range v {
			if found := unsafeNumber(child, fmt.Sprintf("%s[%d]", path, i)); found != "" {
				return found
			}
		}
	}
	return ""
}

func admitted(a operations.Authority) bool {
	return a.Verified && a.Allowed && strings.TrimSpace(a.Principal) != ""
}
func refused(code, message string) subprocess.MCPCallResult {
	return encoded(map[string]any{"error": &operations.Error{Code: code, Message: message, Details: map[string]any{}}}, true)
}
func encoded(value any, isError bool) subprocess.MCPCallResult {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > operations.MaxResultBytes {
		return subprocess.MCPCallResult{IsError: true, Content: json.RawMessage(`{"error":{"code":"unavailable","message":"response unavailable","details":{}}}`)}
	}
	return subprocess.MCPCallResult{Content: raw, IsError: isError}
}
