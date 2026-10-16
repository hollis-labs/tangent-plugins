package operations

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

const MaxInputBytes = 32 * 1024
const MaxResultBytes = 1024 * 1024

// AdmitRequest resolves exact current operation/resource grants. It is an
// injected shadow seam, not a host carrier or commit-time authority contract.
type AdmitRequest func(context.Context, Caller, string, map[string]any) (Authority, error)

func batchDeclaration() Operation {
	choices := []any{}
	for _, name := range declarationOrder {
		op := registry[name]
		if !op.Write || name == "migrate" {
			continue
		}
		input := clone(op.Input).(object)
		// Move operation-local definitions under the enclosing batch root.
		rebaseRefs(input, "#/$defs/"+name+"/$defs/")
		choices = append(choices, object{"type": "object", "additionalProperties": false,
			"required": []any{"operation", "input"}, "properties": object{
				"operation": object{"const": name}, "input": input,
			}})
	}
	defs := object{}
	for _, name := range declarationOrder {
		if d, ok := registry[name].Input["$defs"].(object); ok {
			copyDefs := clone(d).(object)
			rebaseRefs(copyDefs, "#/$defs/"+name+"/$defs/")
			defs[name] = object{"$defs": copyDefs}
		}
	}
	input := object{"type": "object", "additionalProperties": false, "required": []any{"entries"}, "$defs": defs,
		"properties": object{"entries": object{"type": "array", "minItems": 1, "maxItems": 20, "items": object{"oneOf": choices}}}}
	return Operation{Name: "batch", Write: true, Input: clone(input).(object), Description: "Apply bounded local mutations sequentially in one shadow transaction. Precommit failures roll back all entries. Update rev compares against earlier entries in this batch. No result references, idempotency or safe automatic retry; authority refusal can follow committed effects."}
}

func (s *Service) admission(ctx context.Context, caller Caller, name string, input object) (Authority, error) {
	if err := ctx.Err(); err != nil {
		return Authority{}, err
	}
	if s.Admit != nil {
		return s.Admit(ctx, caller, name, clone(input).(object))
	}
	return Authority{}, errors.New("resource admission unavailable")
}

func allowed(a Authority) bool { return a.Verified && a.Allowed && a.Principal != "" }

// callBatch never nests independently committing Service.Call invocations.
func (s *Service) callBatch(ctx context.Context, caller Caller, input object) (any, error) {
	entries := input["entries"].([]any)
	principal := ""
	check := func(name string, in object) error {
		a, err := s.admission(ctx, caller, name, in)
		if err != nil || !allowed(a) || principal != "" && principal != a.Principal {
			return failure("unavailable", "verified batch authority unavailable", nil)
		}
		principal = a.Principal
		return nil
	}
	checkAll := func() error {
		if err := check("batch", input); err != nil {
			return err
		}
		for _, value := range entries {
			entry := value.(object)
			if err := check(str(entry["operation"]), entry["input"].(object)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkAll(); err != nil {
		return nil, err
	}
	if s.Store == nil {
		return nil, failure("unavailable", "shadow store unavailable", nil)
	}
	var result any
	edges := &edgeGuard{service: s, caller: caller, principal: principal}
	err := s.Store.Transact(ctx, func(state *storage.State) error {
		if err := checkAll(); err != nil {
			return err
		}
		x := &execution{service: s, state: state, who: principal, ctx: ctx, now: s.clock().Truncate(time.Millisecond)}
		results := []any{}
		for i, value := range entries {
			entry := value.(object)
			name, in := str(entry["operation"]), entry["input"].(object)
			if err := check(name, in); err != nil {
				return err
			}
			if edgeOperation(name) {
				if err := validateEdgeIDs(name, in); err != nil {
					return err
				}
				// Resolve each entry against evolving transaction State, not a
				// second read or the original batch inputs. Retain every cohort.
				if err := edges.acquire(ctx, state, name, in); err != nil {
					return err
				}
			}
			out, err := registry[name].handler(x, in)
			if err != nil {
				var domain *Error
				if errors.As(err, &domain) {
					return failure(domain.Code, "batch entry failed", object{"index": i, "error": domain})
				}
				return err
			}
			// Detach now: a later entry must not alter an earlier result.
			results = append(results, clone(out))
		}
		if err := checkAll(); err != nil {
			return err
		}
		if err := edges.checkAll(ctx); err != nil {
			return err
		}
		result = object{"results": results}
		raw, err := json.Marshal(result)
		if err != nil || len(raw) > MaxResultBytes {
			return failure("unavailable", "batch result exceeds output bound", nil)
		}
		return nil
	})
	// This check may refuse after a committed effect. It is not a rollback or
	// a retry assurance, and it withholds error details as well as successes.
	if admissionErr := checkAll(); admissionErr != nil {
		return nil, admissionErr
	}
	if edgeErr := edges.checkAll(ctx); edgeErr != nil {
		return nil, edgeErr
	}
	if err != nil {
		return nil, serviceFailure(err)
	}
	return result, nil
}
