package operations

import (
	"context"
	"errors"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/torqueclient"
)

// CurrentParent rechecks the actual caller's exact ordinary parent request.
// Its booleans/principal cannot manufacture child scope or credential grants.
type CurrentParent func(context.Context) (Authority, error)

// BindUpstream is an optional trusted request-local factory, not a production
// provider. It receives detached parent input after resource admission succeeds.
// It must honor context and bind real client adapters through CurrentParent.
type BindUpstream func(context.Context, Caller, string, map[string]any, Authority, CurrentParent) (Upstream, error)

// ActivityUpstream is optional complete-only metadata, not a public operation.
type ActivityUpstream interface {
	LatestCommentActivity(context.Context, []string) (torqueclient.Activity, error)
}

type torqueUpstream struct{ bound *torqueclient.Bound }

// NewTorqueUpstream enforces the ordinary parent guard inside every real client
// admission, including cached reads and child-verifier waits. Proof must come
// from an explicitly trusted factory; no production proof provider exists here.
func NewTorqueUpstream(client *torqueclient.Client, proof any, current CurrentParent) Upstream {
	if client == nil || current == nil {
		return nil
	}
	return &torqueUpstream{bound: client.Bind(proof, func(ctx context.Context) (string, error) {
		a, err := current(ctx)
		if err != nil || !allowed(a) || ctx.Err() != nil {
			return "", errors.New("parent authority unavailable")
		}
		return a.Principal, nil
	})}
}

func (u *torqueUpstream) Read(ctx context.Context, name string, input object) (any, error) {
	v, err := u.bound.Read(ctx, name, input)
	return v, torqueFailure(err)
}
func (u *torqueUpstream) LatestCommentActivity(ctx context.Context, ids []string) (torqueclient.Activity, error) {
	a, err := u.bound.LatestCommentActivity(ctx, ids)
	if err != nil || !a.Complete {
		return torqueclient.Activity{}, failure("unavailable", "Torque comments unavailable", nil)
	}
	return a, nil
}
func torqueFailure(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, torqueclient.Invalid):
		return failure("bad_request", "Torque request invalid", nil)
	case errors.Is(err, torqueclient.NotFound):
		return failure("not_found", "not found in Torque", object{"torque_status": 404})
	default:
		return failure("unavailable", "Torque unavailable", nil)
	}
}

func usesTorque(name string) bool {
	switch name {
	case "board", "torque_task", "torque_tasks", "torque_titles", "torque_projects", "torque_epics", "torque_sprints", "torque_facets":
		return true
	}
	return false
}

func (s *Service) bindTorque(ctx context.Context, caller Caller, name string, in object, parent Authority) (Upstream, error) {
	if s.BindTorque == nil {
		return s.Torque, nil
	}
	if s.Admit == nil || !allowed(parent) {
		return nil, failure("unavailable", "verified caller authority unavailable", nil)
	}
	parentInput := clone(in).(object)
	current := func(currentCtx context.Context) (Authority, error) {
		a, err := s.admission(currentCtx, caller, name, parentInput)
		if err != nil || currentCtx.Err() != nil || !allowed(a) || a.Principal != parent.Principal {
			return Authority{}, failure("unavailable", "verified caller authority unavailable", nil)
		}
		return a, nil
	}
	fresh, checkErr := current(ctx)
	if checkErr != nil {
		return nil, checkErr
	}
	upstream, err := s.BindTorque(ctx, caller, name, clone(parentInput).(object), fresh, current)
	if err != nil {
		upstream = nil
	}
	// A factory may wait. Recheck ordinary parent and context before its adapter
	// is used. Factory failures never fall back to an ambient/evaluation client.
	if _, checkErr := current(ctx); checkErr != nil {
		return nil, checkErr
	}
	return upstream, nil
}

func syntheticActivity(result object) (time.Time, bool) {
	comments, ok := result["comments"].([]any)
	if !ok {
		return time.Time{}, false
	}
	// Some evaluation providers include the underlying comment-page envelope.
	// A declared page must prove exhaustion; absent paging is the historical
	// explicit synthetic full-array convention, never a live transport proof.
	if raw, has := result["meta"]; has {
		meta, ok := raw.(object)
		if !ok {
			return time.Time{}, false
		}
		more, ok := meta["has_more"].(bool)
		if !ok || more {
			return time.Time{}, false
		}
		if cursor, has := meta["next_cursor"]; !has || cursor != nil {
			return time.Time{}, false
		}
		returned, ok := integer(meta["returned"])
		if !ok || returned != int64(len(comments)) {
			return time.Time{}, false
		}
	}
	if raw, has := result["comments_meta"]; has {
		meta, ok := raw.(object)
		if !ok {
			return time.Time{}, false
		}
		truncated, ok := meta["truncated"].(bool)
		if !ok || truncated {
			return time.Time{}, false
		}
		returned, ok := integer(meta["returned"])
		if !ok || returned != int64(len(comments)) {
			return time.Time{}, false
		}
		total, ok := integer(meta["total"])
		if !ok || total != returned {
			return time.Time{}, false
		}
		if omitted, has := meta["omitted"]; has {
			n, ok := integer(omitted)
			if !ok || n != 0 {
				return time.Time{}, false
			}
		}
	}
	var latest time.Time
	for _, raw := range comments {
		comment, ok := raw.(object)
		if !ok {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, str(comment["created_at"]))
		if err != nil || t.IsZero() {
			return time.Time{}, false
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest, true
}
