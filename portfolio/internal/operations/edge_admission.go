package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

// EdgeResource is a frozen identity/grant fact from the operation's State.
// DB is actual local location or empty for an external/dangling reference.
// RequestedDB preserves explicit bindings, including mismatches/missing items;
// domain validation follows cohort admission and cannot disclose them first.
type EdgeResource struct {
	ID          string
	DB          string
	RequestedDB string
	Exists      bool
	Role        string
	Access      string
	LinkKind    string
}

// EdgeCohort retains all requested, disclosed and touched references from one
// State. Removed edges and failed lookups remain for final authority checks.
type EdgeCohort struct {
	Resources []EdgeResource
	Edges     []storage.Edge
}

func (c EdgeCohort) detached() EdgeCohort {
	return EdgeCohort{Resources: append([]EdgeResource{}, c.Resources...), Edges: append([]storage.Edge{}, c.Edges...)}
}

// AdmitEdges checks current grants for the complete detached typed cohort under
// the ordinary operation's same principal. Nil refuses every edge read/write.
// This source-owned seam provides no production issuer or atomic host commit.
type AdmitEdges func(context.Context, Caller, string, map[string]any, EdgeCohort) (Authority, error)

type edgeCheck struct {
	operation string
	input     object
	cohort    EdgeCohort
}
type edgeGuard struct {
	service   *Service
	caller    Caller
	principal string
	checks    []edgeCheck
}

func detachedCaller(caller Caller) Caller {
	switch binding := caller.Binding.(type) {
	case json.RawMessage:
		caller.Binding = json.RawMessage(bytes.Clone(binding))
	case []byte:
		caller.Binding = bytes.Clone(binding)
	}
	return caller
}

func (g *edgeGuard) ordinary(ctx context.Context, name string, in object) (Authority, error) {
	if g.service.Admit != nil {
		return g.service.admission(ctx, detachedCaller(g.caller), name, in)
	}
	if g.service.Verify != nil {
		return g.service.Verify(ctx, detachedCaller(g.caller), name)
	}
	return Authority{}, failure("unavailable", "verified edge operation authority unavailable", nil)
}
func (g *edgeGuard) check(ctx context.Context, entry edgeCheck) error {
	if ctx.Err() != nil || g.service.AdmitEdges == nil {
		return failure("unavailable", "verified edge cohort authority unavailable", nil)
	}
	ordinary, err := g.ordinary(ctx, entry.operation, entry.input)
	if err != nil || !allowed(ordinary) || ordinary.Principal != g.principal {
		return failure("unavailable", "verified edge operation authority unavailable", nil)
	}
	current, err := g.service.AdmitEdges(ctx, detachedCaller(g.caller), entry.operation, clone(entry.input).(object), entry.cohort.detached())
	if err != nil || !allowed(current) || current.Principal != g.principal {
		return failure("unavailable", "verified edge cohort authority unavailable", nil)
	}
	// Typed verification can wait. Its successful result does not preserve
	// an earlier operation grant or context; recheck both before proceeding.
	if ctx.Err() != nil {
		return failure("unavailable", "verified edge cohort authority unavailable", nil)
	}
	ordinary, err = g.ordinary(ctx, entry.operation, entry.input)
	if ctx.Err() != nil || err != nil || !allowed(ordinary) || ordinary.Principal != g.principal {
		return failure("unavailable", "verified edge operation authority unavailable", nil)
	}
	return nil
}
func (g *edgeGuard) acquire(ctx context.Context, state *storage.State, name string, in object) error {
	cohort, err := edgeCohort(state, name, in)
	if err != nil {
		return serviceFailure(err)
	}
	entry := edgeCheck{operation: name, input: clone(in).(object), cohort: cohort.detached()}
	g.checks = append(g.checks, entry)
	return g.check(ctx, entry)
}
func (g *edgeGuard) checkAll(ctx context.Context) error {
	for _, entry := range g.checks {
		if err := g.check(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) callEdge(ctx context.Context, caller Caller, op Operation, in object) (out any, callErr error) {
	if s.AdmitEdges == nil {
		return nil, failure("unavailable", "verified edge cohort authority unavailable", nil)
	}
	if err := validateEdgeIDs(op.Name, in); err != nil {
		return nil, err
	}
	g := &edgeGuard{service: s, caller: caller}
	ordinary, err := g.ordinary(ctx, op.Name, in)
	if err != nil || !allowed(ordinary) {
		return nil, failure("unavailable", "verified edge operation authority unavailable", nil)
	}
	g.principal = ordinary.Principal
	defer func() {
		if guardErr := g.checkAll(ctx); guardErr != nil {
			out, callErr = nil, guardErr
		}
	}()
	if s.Store == nil {
		return nil, failure("unavailable", "shadow store unavailable", nil)
	}
	x := &execution{service: s, ctx: ctx, who: g.principal, now: s.clock().Truncate(time.Millisecond)}
	run := func(state *storage.State) error {
		x.state = state
		if grantErr := g.acquire(ctx, state, op.Name, in); grantErr != nil {
			return grantErr
		}
		result, handlerErr := op.handler(x, in)
		if handlerErr != nil {
			return handlerErr
		}
		if handlerErr = g.checkAll(ctx); handlerErr != nil {
			return handlerErr
		}
		raw, encodeErr := json.Marshal(result)
		if encodeErr != nil || len(raw) > MaxResultBytes {
			return failure("unavailable", "result exceeds output bound", nil)
		}
		out = clone(result)
		return nil
	}
	if op.Write {
		err = s.Store.Transact(ctx, run)
	} else {
		var state *storage.State
		state, err = s.Store.ReadState(ctx)
		if err == nil {
			err = run(state)
		}
	}
	if err != nil {
		return nil, serviceFailure(err)
	}
	return out, nil
}
