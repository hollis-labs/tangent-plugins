// Package operations ports the pinned tracker domain registry for disposable
// shadow evaluation. It registers no transport and has no live writer binding.
package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/textcompat"
	"modernc.org/sqlite"
)

type object = map[string]any

// Error is the transport-independent service failure. Current on conflicts is
// the detached current item, including unknown fields and historical provenance.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details object `json:"details"`
}

func (e *Error) Error() string { return e.Message }
func failure(code, message string, details object) error {
	if details == nil {
		details = object{}
	}
	return &Error{code, message, details}
}

// Caller is an opaque request-local courier. Body labels are never authority.
type Caller struct{ Binding any }

// Authority is supplied only by the explicitly trusted verifier. The verifier
// must resolve current assurance/revocation and an operation-specific grant on
// every call. A role name or plugin process identity is insufficient.
type Authority struct {
	Principal string
	Verified  bool
	Allowed   bool
}

// VerifyCaller is an injection seam, not an implemented host identity provider.
type VerifyCaller func(context.Context, Caller, string) (Authority, error)

// Upstream is a read-only injection seam. No endpoint/token/default client exists.
type Upstream interface {
	Read(context.Context, string, object) (any, error)
}

// Service evaluates local domain operations on one explicitly selected shadow.
type Service struct {
	Store      *storage.Store
	Verify     VerifyCaller
	Admit      AdmitRequest
	AdmitEdges AdmitEdges
	Torque     Upstream
	Now        func() time.Time
}

// Operation is the single domain declaration used for validation and dispatch.
type Operation struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Input       object `json:"input"`
	Write       bool   `json:"write"`
	handler     func(*execution, object) (any, error)
	baseInput   object
}
type execution struct {
	service *Service
	state   *storage.State
	who     string
	now     time.Time
	ctx     context.Context
}

func (s *Service) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out any
	err = d.Decode(&out)
	return out, err
}
func clone(v any) any { out, _ := normalize(v); return out }

// Call validates shared schemas and dispatches once. When Admit is configured,
// it checks current request/resource authority before access, after writer waits
// and before result/error disclosure. Internal shadow reads without Admit remain
// unauthenticated evaluation only; both transport adapters require verification.
func (s *Service) Call(ctx context.Context, caller Caller, name string, input any) (out any, callErr error) {
	op, exists := registry[name]
	if !exists {
		return nil, failure("bad_request", "unknown operation: "+name, object{"operations": operationNames()})
	}
	v, err := normalize(input)
	if err != nil {
		return nil, failure("bad_request", "input must be JSON", nil)
	}
	in, ok := v.(object)
	if !ok {
		return nil, failure("bad_request", "input must be an object", nil)
	}
	if raw, marshalErr := json.Marshal(in); marshalErr != nil || len(raw) > MaxInputBytes {
		return nil, failure("bad_request", "input exceeds 32 KiB", nil)
	}
	principal := ""
	if s.Admit != nil && name != "batch" {
		a, admissionErr := s.admission(ctx, caller, name, in)
		if admissionErr != nil || !allowed(a) {
			return nil, failure("unavailable", "verified caller authority unavailable", nil)
		}
		principal = a.Principal
		defer func() {
			current, disclosureErr := s.admission(ctx, caller, name, in)
			if disclosureErr != nil || !allowed(current) || current.Principal != principal {
				out, callErr = nil, failure("unavailable", "verified caller authority unavailable", nil)
			}
		}()
	}
	for _, key := range []string{"scope", "project_scope", "workstream_scope"} {
		if _, requested := in[key]; requested {
			return nil, failure("unsupported", "query scope unavailable", nil)
		}
	}
	if name == "list" || name == "search" || name == "board" {
		for _, key := range []string{"project", "project_id", "workstream", "workstream_id"} {
			if _, requested := in[key]; requested {
				return nil, failure("unsupported", "query scope unavailable", nil)
			}
		}
	}
	if validationErr := validateInput(op, in); validationErr != nil {
		return nil, validationErr
	}
	if name == "batch" {
		return s.callBatch(ctx, caller, in)
	}
	if edgeOperation(name) {
		return s.callEdge(ctx, caller, op, in)
	}
	x := &execution{service: s, ctx: ctx, now: s.clock().Truncate(time.Millisecond)}
	if op.Write && s.Verify == nil && s.Admit == nil {
		return nil, failure("unavailable", "verified caller authority unavailable", nil)
	}
	var result any
	run := func(state *storage.State) error {
		x.state = state
		// Resolve current authority after acquiring the writer transaction and
		// verifying its snapshot, immediately before mutation dispatch. This is
		// not atomic commit-time authority or a production host verifier.
		if op.Write {
			var a Authority
			var verifyErr error
			if s.Admit != nil {
				a, verifyErr = s.admission(ctx, caller, name, in)
			} else {
				a, verifyErr = s.Verify(ctx, caller, name)
			}
			if verifyErr != nil || !a.Verified || !a.Allowed || a.Principal == "" {
				return failure("unavailable", "verified caller authority refused", nil)
			}
			if principal != "" && a.Principal != principal {
				return failure("unavailable", "verified caller changed", nil)
			}
			x.who = a.Principal
		}
		var callErr error
		result, callErr = op.handler(x, in)
		if callErr == nil && op.Write {
			raw, encodeErr := json.Marshal(result)
			if encodeErr != nil || len(raw) > MaxResultBytes {
				return failure("unavailable", "result exceeds output bound", nil)
			}
		}
		return callErr
	}
	if op.Write {
		if s.Store == nil {
			return nil, failure("unavailable", "shadow store unavailable", nil)
		}
		err = s.Store.Transact(ctx, run)
	} else if localOperation(name) {
		if s.Store == nil {
			return nil, failure("unavailable", "shadow store unavailable", nil)
		}
		x.state, err = s.Store.ReadState(ctx)
		if err == nil {
			result, err = op.handler(x, in)
		}
	} else {
		result, err = op.handler(x, in)
	}
	if err != nil {
		return nil, serviceFailure(err)
	}
	if name == "list" || name == "search" {
		result, err = pageResult(name, in, x.state, result)
		if err != nil {
			return nil, err
		}
	}
	if raw, encodeErr := json.Marshal(result); encodeErr != nil || len(raw) > MaxResultBytes {
		return nil, failure("unavailable", "result exceeds output bound", nil)
	}
	return clone(result), nil
}

func serviceFailure(err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	var dbErr *sqlite.Error
	if errors.As(err, &dbErr) && (dbErr.Code()&255 == 5 || dbErr.Code()&255 == 6) {
		return failure("locked", "shadow database is locked by another writer", nil)
	}
	return failure("unavailable", "shadow operation unavailable", nil)
}
func localOperation(name string) bool {
	return name == "databases" || name == "list" || name == "get" || name == "search" || name == "board"
}
func (x *execution) day() string { return x.now.Format("2006-01-02") }
func (x *execution) upstream(name string, input object) (any, error) {
	validated, err := upstreamInput(name, input)
	if err != nil {
		return nil, err
	}
	if x.service.Torque == nil {
		return nil, failure("unavailable", "Torque unavailable", nil)
	}
	result, err := x.service.Torque.Read(x.ctx, name, validated)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return nil, err
		}
		return nil, failure("unavailable", "Torque unavailable", nil)
	}
	return normalize(result)
}
func str(v any) string { s, _ := v.(string); return s }
func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}
func integer(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	return textcompat.Integer(n)
}
func jsString(v any) string { return textcompat.String(v) }
