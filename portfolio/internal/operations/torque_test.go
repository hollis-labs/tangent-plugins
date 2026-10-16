package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/torqueclient"
)

type boardTransport func(*http.Request) (*http.Response, error)

func (f boardTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func boardResponse(v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: make(http.Header)}
}

type boardBinding struct {
	principal              string
	task                   string
	parent, list, comments atomic.Bool
}

func boardCaller(principal, id string) *boardBinding {
	b := &boardBinding{principal: principal, task: id}
	b.parent.Store(true)
	b.list.Store(true)
	b.comments.Store(true)
	return b
}

type boardProof struct {
	binding *boardBinding
	parent  string
}

// These grants exist only in this generated fixture: exact four board list
// queries, exact returned task resources, and a separately granted comment
// cohort. Ordinary board permission alone does not confer either child grant.
func boardFixtureService(t *testing.T, modify ...func(*http.Request, object) object) (*Service, *atomic.Int32) {
	t.Helper()
	s := newService(t)
	requests := new(atomic.Int32)
	s.Admit = func(ctx context.Context, caller Caller, name string, input object) (Authority, error) {
		b, ok := caller.Binding.(*boardBinding)
		if !ok || !b.parent.Load() || ctx.Err() != nil {
			return Authority{}, errors.New("fixture parent refused")
		}
		return Authority{Principal: b.principal, Verified: true, Allowed: true}, nil
	}
	client, err := torqueclient.New(torqueclient.Config{Endpoint: "https://fixture.invalid/api/v1", Host: "127.0.0.1:8990", Integration: "board-fixture",
		Verify: func(ctx context.Context, raw any, r torqueclient.Request) (torqueclient.Grant, error) {
			p, ok := raw.(*boardProof)
			if !ok || p.parent != "board" {
				return torqueclient.Grant{}, errors.New("fixture proof refused")
			}
			b := p.binding
			q, err := url.ParseQuery(r.Query)
			if err != nil {
				return torqueclient.Grant{}, errors.New("invalid fixture query")
			}
			switch r.Operation {
			case "torque_tasks":
				if !b.list.Load() || r.Path != "/tasks" || q.Get("limit") != "30" || q.Get("fields") != "summary" || q.Get("sort_by") != "updated_at" || q.Get("sort_dir") != "desc" || q.Get("include_total") != "true" || !strings.Contains("|queued|doing|review|done|", "|"+q.Get("status")+"|") {
					return torqueclient.Grant{}, errors.New("distinct list grant refused")
				}
			case "torque_comment_activity":
				if !b.comments.Load() || r.Path != "/comments" || q.Get("entity_type") != "task" || q.Get("entity_ids") != "[\""+b.task+"\"]" || q.Get("sort_by") != "created_at" || q.Get("sort_dir") != "desc" || q.Get("limit") != "200" {
					return torqueclient.Grant{}, errors.New("distinct comment grant refused")
				}
			default:
				return torqueclient.Grant{}, errors.New("ungranted child")
			}
			for _, resource := range r.Resources {
				if resource.ID != b.task || resource.Kind == "task" && r.Phase == "disclosure" && resource.ProjectID != "PRJ-20261010-0001" {
					return torqueclient.Grant{}, errors.New("foreign resource refused")
				}
			}
			if ctx.Err() != nil {
				return torqueclient.Grant{}, ctx.Err()
			}
			return torqueclient.Grant{Principal: b.principal, Scope: "explicit-fixture-board-queries", ScopeEpoch: "fixture-scope-1", CredentialRef: b.principal, CredentialEpoch: "fixture-version-1"}, nil
		}, Resolve: func(ctx context.Context, ref, epoch string) (string, error) { return "fixture-" + ref, nil },
		Transport: boardTransport(func(r *http.Request) (*http.Response, error) {
			requests.Add(1)
			respond := func(v object) *http.Response {
				for _, fn := range modify {
					v = fn(r, v)
				}
				return boardResponse(v)
			}
			id := "CW-20261010-0001"
			if r.Header.Get("Authorization") == "Bearer fixture-two" {
				id = "CW-20261010-0002"
			}
			switch r.URL.Path {
			case "/api/v1/tasks":
				items := []any{}
				total := 0
				if r.URL.Query().Get("status") == "doing" {
					total = 10
					items = append(items, object{"id": id, "title": id, "status": "doing", "priority": 2, "project_id": "PRJ-20261010-0001", "tags": []any{"ws-fixture"}, "updated_at": iso(s.clock().Add(-6 * time.Hour))})
				}
				return respond(object{"items": items, "meta": object{"returned": len(items), "limit": 30, "has_more": false, "next_cursor": nil, "total": total}}), nil
			case "/api/v1/comments":
				return respond(object{"items": []any{object{"id": 1, "entity_type": "task", "entity_id": id, "created_at": iso(s.clock().Add(-time.Hour)), "updated_at": iso(s.clock().Add(time.Hour)), "content": "not retained", "author": "fixture"}}, "meta": object{"returned": 1, "limit": 200, "has_more": false, "next_cursor": nil, "sort_by": "created_at", "sort_dir": "desc"}}), nil
			default:
				t.Errorf("unexpected board fanout route: %s", r.URL.Path)
				return nil, errors.New("unexpected")
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.BindTorque = func(ctx context.Context, caller Caller, parent string, input object, a Authority, current CurrentParent) (Upstream, error) {
		b, ok := caller.Binding.(*boardBinding)
		if !ok || a.Principal != b.principal {
			return nil, errors.New("fixture binding refused")
		}
		return NewTorqueUpstream(client, &boardProof{b, parent}, current), nil
	}
	return s, requests
}

func boardWithCaller(t *testing.T, s *Service, b *boardBinding) object {
	t.Helper()
	v, err := s.Call(t.Context(), Caller{Binding: b}, "board", object{})
	if err != nil {
		t.Fatal(err)
	}
	return v.(object)
}

func TestBoardBoundClientDistinctChildGrantsAndCounts(t *testing.T) {
	for _, mode := range []string{"allowed", "parent-only", "comments-denied"} {
		t.Run(mode, func(t *testing.T) {
			s, requests := boardFixtureService(t)
			b := boardCaller("one", "CW-20261010-0001")
			if mode == "parent-only" {
				b.list.Store(false)
				b.comments.Store(false)
			}
			if mode == "comments-denied" {
				b.comments.Store(false)
			}
			board := boardWithCaller(t, s, b)
			if mode == "parent-only" {
				rowIDs(t, board, "in_flight", []string{})
				if requests.Load() != 0 {
					t.Fatal("parent alone granted network")
				}
				if len(board["notices"].([]any)) != 1 {
					t.Fatal("outage degradation")
				}
			} else {
				rowIDs(t, board, "in_flight", []string{b.task})
				expect(t, board["totals"].(object)["in_flight"], 10)
				if mode == "allowed" {
					if requests.Load() != 5 {
						t.Fatal("expected four lists + one batch")
					}
					expect(t, board["notices"], []string{})
				} else {
					if requests.Load() != 4 {
						t.Fatal("comment denial fanout")
					}
					expect(t, board["notices"], []string{"Torque comments unavailable; every doing task is shown as in flight."})
				}
			}
		})
	}
}

func TestServiceFactoryRefusalCurrentParentAndNoFallback(t *testing.T) {
	for _, mode := range []string{"verify-only", "factory-error", "factory-nil", "parent-drift", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, requests := boardFixtureService(t)
			b := boardCaller("one", "CW-20261010-0001")
			var factories atomic.Int32
			s.Torque = fakeUpstream{func(string, object) (any, error) {
				t.Fatal("factory failure used evaluation fallback")
				return nil, nil
			}}
			if mode == "verify-only" {
				s.Admit = nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.BindTorque = func(factoryCtx context.Context, caller Caller, parent string, input object, a Authority, current CurrentParent) (Upstream, error) {
				factories.Add(1)
				deadline, ok := factoryCtx.Deadline()
				if !ok || time.Until(deadline) > 30*time.Second {
					t.Error("whole board deadline missing")
				}
				input["limit"] = json.Number("1") // detached from ordinary parent input
				switch mode {
				case "parent-drift":
					b.parent.Store(false)
				case "cancel":
					cancel()
				case "factory-error":
					return nil, errors.New("private factory error")
				}
				return nil, nil
			}
			v, err := s.Call(ctx, Caller{Binding: b}, "board", object{})
			if mode == "factory-error" || mode == "factory-nil" {
				if err != nil || v == nil {
					t.Fatal("factory outage did not degrade", err)
				}
			} else {
				if err == nil || v != nil {
					t.Fatal("parent refusal disclosed result")
				}
			}
			if mode == "verify-only" && factories.Load() != 0 {
				t.Fatal("Verify-only invoked factory")
			}
			if requests.Load() != 0 {
				t.Fatal("refused factory sent network")
			}
		})
	}
}

func TestServiceFactoryOrdinaryPrincipalDriftAndDetachedInput(t *testing.T) {
	s := newService(t)
	var principal atomic.Value
	principal.Store("one")
	s.Admit = func(ctx context.Context, caller Caller, name string, in object) (Authority, error) {
		if _, has := in["tampered"]; has {
			t.Error("factory changed parent input")
		}
		return Authority{Principal: principal.Load().(string), Verified: true, Allowed: true}, nil
	}
	s.BindTorque = func(ctx context.Context, caller Caller, parent string, input object, a Authority, current CurrentParent) (Upstream, error) {
		input["tampered"] = true
		principal.Store("two")
		return fakeUpstream{func(string, object) (any, error) { t.Fatal("principal drift used adapter"); return nil, nil }}, nil
	}
	v, err := s.Call(t.Context(), Caller{Binding: "actual-opaque"}, "board", object{})
	if err == nil || v != nil {
		t.Fatal("factory principal drift disclosed")
	}
	// Ordinary local operations/writes must not do factory work.
	s.BindTorque = func(context.Context, Caller, string, object, Authority, CurrentParent) (Upstream, error) {
		t.Fatal("local operation invoked factory")
		return nil, nil
	}
	call(t, s, "databases", object{})
	call(t, s, "create", object{"db": "ideas", "item": object{"title": "Local fixture", "kind": "idea"}})
}

func TestTwoServiceCallersShareCapacityButNotProofOrCache(t *testing.T) {
	s, _ := boardFixtureService(t)
	one := boardCaller("one", "CW-20261010-0001")
	two := boardCaller("two", "CW-20261010-0002")
	var wg sync.WaitGroup
	for _, b := range []*boardBinding{one, two} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2 {
				v, err := s.Call(t.Context(), Caller{Binding: b}, "board", object{})
				if err != nil {
					t.Error(err)
					return
				}
				rows := v.(object)["sections"].(object)["in_flight"].([]any)
				if len(rows) != 1 || rows[0].(object)["id"] != b.task {
					t.Error("cross-caller board proof/cache")
				}
			}
		}()
	}
	wg.Wait()
}

func TestSyntheticFallbackMalformedOrIncompleteNeverQuiet(t *testing.T) {
	for _, result := range []object{
		{}, {"comments": nil}, {"comments": "invalid"}, {"comments": []any{nil}},
		{"comments": []any{object{"created_at": "invalid"}}}, {"comments": []any{object{"updated_at": "2026-10-08T12:00:00Z"}}},
		{"comments": []any{}, "comments_meta": object{"returned": 0, "total": 1, "truncated": true}},
		{"comments": []any{}, "meta": object{"returned": 0, "has_more": true, "next_cursor": "next"}},
	} {
		s := newService(t)
		s.Torque = fakeUpstream{func(name string, in object) (any, error) {
			if name == "torque_task" {
				return result, nil
			}
			items := []any{}
			if in["status"] == "doing" {
				items = append(items, object{"id": "CW-20261010-0001", "title": "Idle", "status": "doing", "updated_at": iso(s.clock().Add(-6 * time.Hour))})
			}
			return object{"items": items, "meta": object{}}, nil
		}}
		b := itemCall(t, s, "board", object{})
		rowIDs(t, b, "in_flight", []string{"CW-20261010-0001"})
		expect(t, b["notices"], []string{"Torque comments unavailable; every doing task is shown as in flight."})
	}
	s := newService(t)
	s.Torque = fakeUpstream{func(name string, in object) (any, error) {
		if name == "torque_task" {
			return object{"comments": []any{}}, nil
		}
		items := []any{}
		if in["status"] == "doing" {
			items = append(items, object{"id": "CW-20261010-0001", "title": "Idle", "status": "doing", "updated_at": iso(s.clock().Add(-6 * time.Hour))})
		}
		return object{"items": items, "meta": object{}}, nil
	}}
	b := itemCall(t, s, "board", object{})
	rowIDs(t, b, "pre_flight", []string{"CW-20261010-0001", "RM-a"})
	expect(t, b["notices"], []string{})
}

type failingActivity struct {
	fakeUpstream
	calls *atomic.Int32
}

func (f failingActivity) LatestCommentActivity(context.Context, []string) (torqueclient.Activity, error) {
	f.calls.Add(1)
	return torqueclient.Activity{}, errors.New("fixture page incomplete")
}
func TestBoardBatchErrorDoesNotFanOut(t *testing.T) {
	s := newService(t)
	calls := new(atomic.Int32)
	s.Torque = failingActivity{fakeUpstream{func(name string, in object) (any, error) {
		if name == "torque_task" {
			t.Fatal("batch failure fanned out")
		}
		items := []any{}
		if in["status"] == "doing" {
			items = append(items, object{"id": "CW-20261010-0001", "title": "Idle", "status": "doing", "updated_at": iso(s.clock().Add(-6 * time.Hour))})
		}
		return object{"items": items, "meta": object{}}, nil
	}}, calls}
	b := itemCall(t, s, "board", object{})
	rowIDs(t, b, "in_flight", []string{"CW-20261010-0001"})
	if calls.Load() != 1 {
		t.Fatal("batch count")
	}
}

func TestBoundBoardForeignResultAndMalformedActivity(t *testing.T) {
	for _, mode := range []string{"foreign-task", "malformed-comment", "foreign-comment", "incomplete-comment"} {
		t.Run(mode, func(t *testing.T) {
			s, requests := boardFixtureService(t, func(r *http.Request, v object) object {
				items := v["items"].([]any)
				if len(items) == 0 {
					return v
				}
				if mode == "foreign-task" && r.URL.Path == "/api/v1/tasks" {
					items[0].(object)["project_id"] = "PRJ-20261010-0099"
				}
				if r.URL.Path == "/api/v1/comments" {
					switch mode {
					case "malformed-comment":
						items[0].(object)["created_at"] = "invalid"
					case "foreign-comment":
						items[0].(object)["entity_id"] = "CW-20261010-0099"
					case "incomplete-comment":
						v["meta"].(object)["has_more"] = true
						v["meta"].(object)["next_cursor"] = "next"
					}
				}
				return v
			})
			b := boardWithCaller(t, s, boardCaller("one", "CW-20261010-0001"))
			if mode == "foreign-task" {
				rowIDs(t, b, "in_flight", []string{})
				if requests.Load() != 4 {
					t.Fatal("foreign result caused comment read")
				}
			} else {
				rowIDs(t, b, "in_flight", []string{"CW-20261010-0001"})
				expect(t, b["notices"], []string{"Torque comments unavailable; every doing task is shown as in flight."})
				if requests.Load() > 6 {
					t.Fatal("batch failure caused fanout")
				}
			}
		})
	}
}
