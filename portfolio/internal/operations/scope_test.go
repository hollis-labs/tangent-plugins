package operations

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type scopeDirectory struct{}

func (scopeDirectory) ScopeKey() string { return "owned-scope-registry" }
func (scopeDirectory) Directory(context.Context) (projects.Directory, error) {
	return projects.Directory{Projects: []projects.Project{
		{URN: "msg://project/test/a", Kind: "project", ExternalIDs: []projects.ExternalID{{Substrate: "torque", ExternalID: "PRJ-20261009-0001"}}},
		{URN: "msg://project/test/b", Kind: "project", ExternalIDs: []projects.ExternalID{{Substrate: "torque", ExternalID: "PRJ-20261009-0002"}}},
	}, Evidence: projects.Evidence{Complete: true}}, nil
}
func scopedService(t *testing.T) *Service {
	t.Helper()
	files := fixtureFiles()
	files["workstreams"] = []byte(`{"schema":"portfolio/workstreams@1","items":[
 {"id":"WS-a","title":"A","status":"active","torque_project_ids":["PRJ-20261009-0001"],"torque_tag":"exact-a"},
 {"id":"WS-a2","title":"Another A","status":"active","torque_project_ids":["PRJ-20261009-0001"]},
 {"id":"WS-b","title":"B","status":"active","torque_project_ids":["PRJ-20261009-0002"]},
 {"id":"WS-only","title":"Only workstream","status":"active"}]}`)
	files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","items":[
 {"id":"ID-b","title":"Idea B","kind":"idea","status":"new","workstream_ids":["WS-b"],"order":0},
 {"id":"ID-a","title":"Idea A","kind":"idea","status":"new","workstream_ids":["WS-a"],"order":10},
 {"id":"ID-a2","title":"Idea A2","kind":"idea","status":"new","links":[{"kind":"workstream","ref":"WS-a2"}],"order":20},
 {"id":"ID-none","title":"Idea None","kind":"idea","status":"new"},
 {"id":"ID-only","title":"Idea Only","kind":"idea","status":"new","workstream_ids":["WS-only"]}]}`)
	files["roadmap"] = []byte(`{"schema":"portfolio/roadmap@1","items":[
 {"id":"RM-b","title":"B","status":"in-progress","updated":"2026-10-08","workstream_ids":["WS-b"]},
 {"id":"RM-a","title":"A","status":"in-progress","updated":"2026-10-08","workstream_ids":["WS-a"]}]}`)
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	snap, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	if err = store.SyncProjects(t.Context(), scopeDirectory{}, nil); err != nil {
		t.Fatal(err)
	}
	return &Service{Store: store, Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }, Verify: func(_ context.Context, c Caller, _ string) (Authority, error) {
		return Authority{Principal: "fixture", Verified: c.Binding == "test-binding", Allowed: true}, nil
	}}
}
func projectScope() object             { return object{"kind": "project", "id": "msg://project/test/a"} }
func workstreamScope(id string) object { return object{"kind": "workstream", "id": id} }

func TestScopeBeforeFilteringPagingTotalsAndExplicitUnscoped(t *testing.T) {
	s := scopedService(t)
	expect(t, len(call(t, s, "list", object{"db": "ideas"}).([]any)), 5)
	in := object{"db": "ideas", "scope": projectScope(), "page": object{"limit": 1}}
	first := itemCall(t, s, "list", in)
	expect(t, first["total"], 2)
	expect(t, listIDs(first["items"]), []string{"ID-a"})
	expect(t, len(first["membership"].(object)), 1)
	if jsonText(first["membership"]) == "" || first["membership"].(object)["ID-b"] != nil {
		t.Fatal("unrelated membership disclosure")
	}
	selected := itemCall(t, s, "search", object{"q": "Idea", "scope": projectScope()})
	expect(t, listIDs(selected["items"]), []string{"ID-a", "ID-a2"})
	only := itemCall(t, s, "list", object{"db": "ideas", "scope": workstreamScope("WS-only")})
	expect(t, listIDs(only["items"]), []string{"ID-only"})
	expect(t, only["unscoped"].(object)["ids"], []string{"ID-only"})
	none := itemCall(t, s, "list", object{"db": "ideas", "scope": object{"kind": "unscoped"}})
	expect(t, listIDs(none["items"]), []string{"ID-none", "ID-only"})
	counts := itemCall(t, s, "databases", object{"scope": projectScope()})
	for _, value := range counts["items"].([]any) {
		row := value.(object)
		if row["name"] == "ideas" {
			expect(t, row["count"], 2)
		}
	}
	for _, bad := range []any{nil, "a", object{}, object{"kind": "project", "id": "label"}, object{"kind": "unscoped", "id": "extra"}, object{"kind": "workstream", "id": "WS-a", "role": "owner"}} {
		errorCode(t, s, "list", object{"db": "ideas", "scope": bad}, "bad_request")
	}
	errorCode(t, s, "list", object{"db": "ideas", "scope": workstreamScope("WS-missing")}, "not_found")
	errorCode(t, s, "list", object{"db": "ideas", "scope": object{"kind": "project", "id": "msg://project/not-synced/a"}}, "not_found")
	errorCode(t, s, "get", object{"db": "ideas", "id": "ID-a", "scope": projectScope()}, "unsupported")
	for _, alias := range []string{"project_scope", "workstream_scope", "project", "project_id", "workstream", "workstream_id"} {
		errorCode(t, s, "list", object{"db": "ideas", alias: "a"}, "unsupported")
	}
}

func TestScopeContinuationObservesRelevantTransitiveProofOnly(t *testing.T) {
	s := scopedService(t)
	first := itemCall(t, s, "list", object{"db": "ideas", "scope": projectScope(), "page": object{"limit": 1}})
	next := object{"db": "ideas", "scope": projectScope(), "page": object{"limit": 1, "offset": 1, "snapshot": first["snapshot"]}}
	itemCall(t, s, "update", object{"db": "workstreams", "id": "WS-b", "patch": object{"title": "Unrelated", "torque_tag": "unrelated"}})
	itemCall(t, s, "update", object{"db": "workstreams", "id": "WS-a", "patch": object{"title": "Relevant node unrelated title"}})
	if _, err := s.Store.SetProjectOverlay(t.Context(), "msg://project/test/a", 0, storage.Overlay{Notes: "not membership authority"}); err != nil {
		t.Fatal(err)
	}
	continued := itemCall(t, s, "list", next)
	expect(t, continued["snapshot"], first["snapshot"])
	// Same selected item JSON/results, but a new alternate membership path.
	itemCall(t, s, "update", object{"db": "workstreams", "id": "WS-a", "patch": object{"workstream_ids": []any{"WS-a2"}}})
	errorCode(t, s, "list", next, "conflict")
	next["scope"] = workstreamScope("WS-a2")
	errorCode(t, s, "list", next, "conflict")
}

func TestScopedWholeCohortGrantRefusalBeforeTotalsAndCurrentDisclosure(t *testing.T) {
	s := scopedService(t)
	calls := 0
	s.Admit = func(ctx context.Context, _ Caller, name string, in object) (Authority, error) {
		calls++
		a := Authority{Principal: "owned-reader", Verified: true, Allowed: true}
		if name != "list" {
			return a, nil
		}
		state, err := s.Store.ReadState(ctx)
		if err != nil {
			return Authority{}, err
		}
		scope, err := parseScope(name, in)
		if err != nil {
			return Authority{}, err
		}
		for _, value := range state.Envelopes["ideas"]["items"].([]any) {
			row := value.(object)
			m := state.Membership.Resolve(str(row["id"]))
			if scope.matches(m) && hasString(m.Projects, "msg://project/test/b") {
				a.Allowed = false
			}
		}
		// Attempted mutation of detached input must not widen service selection.
		in["scope"] = object{"kind": "unscoped"}
		return a, nil
	}
	errorCode(t, s, "list", object{"db": "ideas", "page": object{"limit": 1}}, "unavailable")
	selected := itemCall(t, s, "list", object{"db": "ideas", "scope": projectScope(), "page": object{"limit": 1}})
	expect(t, selected["total"], 2)
	expect(t, listIDs(selected["items"]), []string{"ID-a"})
	// A mixed granted/ungranted row in the selected cohort must refuse everything,
	// even when it lies beyond the visible page.
	s.Admit = nil
	itemCall(t, s, "update", object{"db": "ideas", "id": "ID-a2", "patch": object{"workstream_ids": []any{"WS-b"}}})
	s.Admit = func(_ context.Context, _ Caller, _ string, _ object) (Authority, error) {
		calls++
		return Authority{Principal: "owned-reader", Verified: true, Allowed: calls < 2}, nil
	}
	calls = 0
	errorCode(t, s, "list", object{"db": "ideas", "scope": projectScope(), "page": object{"limit": 1}}, "unavailable")
	// Exact resource grant verifier rejects the mixed member, not silently removes it.
	s.Admit = func(ctx context.Context, _ Caller, _ string, _ object) (Authority, error) {
		state, err := s.Store.ReadState(ctx)
		if err != nil {
			return Authority{}, err
		}
		return Authority{Principal: "owned-reader", Verified: true, Allowed: !hasString(state.Membership.Resolve("ID-a2").Projects, "msg://project/test/b")}, nil
	}
	errorCode(t, s, "list", object{"db": "ideas", "scope": projectScope(), "page": object{"limit": 1}}, "unavailable")
}

type scopeUpstreamFunc func(context.Context, string, object) (any, error)

func (f scopeUpstreamFunc) Read(ctx context.Context, name string, in object) (any, error) {
	return f(ctx, name, in)
}
func taskFixture(id, pid, status string) object {
	return object{"id": id, "project_id": pid, "title": id, "status": status, "tags": []any{"exact-a"}, "updated_at": "2026-10-08T12:00:00Z"}
}
func taskPage(rows ...object) object {
	items := []any{}
	for _, row := range rows {
		items = append(items, row)
	}
	return object{"items": items, "meta": object{"has_more": false}}
}

func TestScopedUpstreamUnionIntersectionDedupBeforeCapAndBoardActivity(t *testing.T) {
	s := scopedService(t)
	requests := []object{}
	activities := []string{}
	s.Torque = scopeUpstreamFunc(func(_ context.Context, name string, in object) (any, error) {
		if name == "torque_task" {
			activities = append(activities, str(in["id"]))
			return object{"comments": []any{}}, nil
		}
		requests = append(requests, clone(in).(object))
		if in["limit"] != json.Number("200") {
			t.Fatal("scope scan after cap", in)
		}
		if str(in["project_id"]) == "PRJ-20261009-0002" {
			return taskPage(), nil
		}
		status := str(in["status"])
		if status == "" {
			status = "doing"
		}
		if status != "doing" {
			return taskPage(), nil
		}
		return taskPage(taskFixture("CW-20261009-0001", "PRJ-20261009-0001", status), taskFixture("CW-20261009-0002", "PRJ-20261009-0001", status)), nil
	})
	tasks := itemCall(t, s, "torque_tasks", object{"scope": projectScope(), "limit": 1})
	expect(t, listIDs(tasks["items"]), []string{"CW-20261009-0001"})
	expect(t, tasks["meta"].(object)["total"], 2)
	if len(requests) != 2 {
		t.Fatal("lost explicit union", requests)
	}
	requests = nil
	empty := itemCall(t, s, "torque_tasks", object{"scope": projectScope(), "project_id": "PRJ-20261009-0002"})
	expect(t, len(empty["items"].([]any)), 0)
	for _, in := range requests {
		if in["project_id"] != "PRJ-20261009-0002" {
			t.Fatal("caller filter overwritten", in)
		}
	}
	requests = nil
	board := itemCall(t, s, "board", object{"scope": projectScope(), "limit": 1})
	rows := board["sections"].(object)["in_flight"].([]any)
	if len(rows) != 1 || rows[0].(object)["id"] != "CW-20261009-0001" {
		t.Fatal(board)
	}
	for _, id := range activities {
		if id != "CW-20261009-0001" {
			t.Fatal("activity outside fetched selected doing cohort", activities)
		}
	}
	local := itemCall(t, &Service{Store: s.Store, Now: s.Now}, "board", object{"scope": workstreamScope("WS-a"), "limit": 1})
	expect(t, local["sections"].(object)["in_flight"].([]any)[0].(object)["id"], "RM-a")
	errorCode(t, s, "torque_tasks", object{"scope": projectScope(), "cursor": "one-selector"}, "unsupported")
}

func TestScopedUpstreamPartialMalformedChangingPagesAndGlobalBudget(t *testing.T) {
	for _, mode := range []string{"partial", "repeat", "mismatch", "duplicate", "metadata", "denied"} {
		t.Run(mode, func(t *testing.T) {
			s := scopedService(t)
			calls := 0
			s.Torque = scopeUpstreamFunc(func(_ context.Context, _ string, in object) (any, error) {
				calls++
				if mode == "denied" {
					return nil, failure("forbidden", "secret upstream text", object{"torque_status": 403})
				}
				row := taskFixture("CW-20261009-0001", "PRJ-20261009-0001", "doing")
				if mode == "mismatch" {
					row["project_id"] = "PRJ-20261009-0002"
					row["tags"] = []any{}
				}
				if mode == "duplicate" && calls > 1 {
					row["title"] = "changed"
				}
				out := taskPage(row)
				if mode == "metadata" {
					delete(out, "meta")
				}
				if mode == "repeat" || mode == "partial" || mode == "duplicate" {
					out["meta"] = object{"has_more": true, "next_cursor": "cursor" + strconv.Itoa(calls)}
					if mode == "repeat" {
						out["meta"].(object)["next_cursor"] = "same"
					}
				}
				return out, nil
			})
			if mode == "partial" {
				out := itemCall(t, s, "torque_tasks", object{"scope": projectScope()})
				meta := out["meta"].(object)
				expect(t, meta["partial"], true)
				if _, has := meta["total"]; has {
					t.Fatal("partial exact total")
				}
				expect(t, meta["lower_bound"], 1)
			} else {
				err := errorCode(t, s, "torque_tasks", object{"scope": projectScope()}, "unavailable")
				if err.Message == "secret upstream text" {
					t.Fatal("raw error disclosure")
				}
			}
		})
	}
	s := scopedService(t)
	state, err := s.Store.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{service: s, state: state, scope: &queryScope{Kind: "project", ID: "msg://project/test/a"}, ctx: t.Context(), budget: &scanBudget{selectors: scopeMaxSelectors}}
	reads := 0
	s.Torque = scopeUpstreamFunc(func(context.Context, string, object) (any, error) { reads++; return taskPage(), nil })
	out, err := x.scopedUpstream("torque_tasks", object{})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, out.(object)["meta"].(object)["partial"], true)
	if reads != 0 {
		t.Fatal("global exhausted budget made upstream call")
	}
}

func TestScopedBoardDenialChangedPagesAndIncompleteActivityPreserveLocalOnly(t *testing.T) {
	for _, mode := range []string{"denied-list", "changed-list", "denied-activity", "incomplete-activity", "missing-comments", "nonarray-comments", "malformed-comments", "malformed-comment-date"} {
		t.Run(mode, func(t *testing.T) {
			s := scopedService(t)
			activities := 0
			s.Torque = scopeUpstreamFunc(func(_ context.Context, name string, in object) (any, error) {
				if name == "torque_task" {
					activities++
					if mode == "denied-activity" {
						return nil, failure("unavailable", "private credential failure", object{"torque_status": json.Number("403")})
					}
					switch mode {
					case "missing-comments":
						return object{"comments_meta": object{"has_more": false}}, nil
					case "nonarray-comments":
						return object{"comments": "invalid", "comments_meta": object{"has_more": false}}, nil
					case "malformed-comments":
						return object{"comments": []any{"invalid"}, "comments_meta": object{"has_more": false}}, nil
					case "malformed-comment-date":
						return object{"comments": []any{object{"created_at": "invalid"}}, "comments_meta": object{"has_more": false}}, nil
					}
					return object{"comments": []any{}}, nil // erased completeness is not absence
				}
				status := str(in["status"])
				if status == "doing" {
					row := taskFixture("CW-20261009-0001", "PRJ-20261009-0001", status)
					row["updated_at"] = "2020-01-01T00:00:00Z"
					return taskPage(row), nil
				}
				if status == "review" && mode == "denied-list" {
					return nil, failure("forbidden", "private response body", nil)
				}
				if status == "review" && mode == "changed-list" {
					return taskPage(taskFixture("CW-20261009-0001", "PRJ-20261009-0001", status)), nil
				}
				return taskPage(), nil
			})
			out := itemCall(t, s, "board", object{"scope": projectScope()})
			rows := out["sections"].(object)["in_flight"].([]any)
			if mode == "incomplete-activity" || strings.Contains(mode, "comments") || mode == "malformed-comment-date" {
				if len(rows) != 2 || activities != 1 {
					t.Fatal("incomplete comments made selected doing task idle", out)
				}
				expect(t, out["upstream"].(object)["activity"].(object)["partial"], true)
			} else {
				if len(rows) != 1 || rows[0].(object)["id"] != "RM-a" {
					t.Fatal("protected prior rows survived invalidation", out)
				}
				if out["membership"].(object)["torque:CW-20261009-0001"] != nil {
					t.Fatal("protected proof remained")
				}
				for _, status := range boardStatuses {
					evidence := out["upstream"].(object)[status].(object)
					if evidence["meta"] != nil {
						t.Fatal("protected prior counts remained", out)
					}
				}
			}
		})
	}
}

func TestScopedBoardSharesBudgetAcrossStatusesAndActivity(t *testing.T) {
	s := scopedService(t)
	state, err := s.Store.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	s.Torque = scopeUpstreamFunc(func(context.Context, string, object) (any, error) { reads++; return taskPage(), nil })
	x := &execution{service: s, state: state, scope: &queryScope{Kind: "project", ID: "msg://project/test/a"}, ctx: t.Context(), now: s.clock(), budget: &scanBudget{selectors: scopeMaxSelectors - 2}}
	out, err := board(x, object{"limit": json.Number("1")})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 || x.budget.selectors != scopeMaxSelectors {
		t.Fatal("budget reset between board statuses", reads, x.budget)
	}
	evidence := out.(object)["upstream"].(object)
	for _, status := range []string{"review", "queued", "done"} {
		if evidence[status].(object)["meta"].(object)["partial"] != true {
			t.Fatal("exhausted scope became exact empty", out)
		}
	}
}

func TestScopedScannerRejectsReturnedQueryMismatches(t *testing.T) {
	for _, mode := range []string{"project", "tag", "status", "entity-id"} {
		t.Run(mode, func(t *testing.T) {
			s := scopedService(t)
			s.Torque = scopeUpstreamFunc(func(_ context.Context, _ string, _ object) (any, error) {
				row := taskFixture("CW-20261009-0001", "PRJ-20261009-0001", "doing")
				if mode == "entity-id" {
					row["id"] = "PRJ-20261009-0001"
				}
				return taskPage(row), nil
			})
			in := object{"scope": projectScope()}
			switch mode {
			case "project":
				in["project_id"] = "PRJ-20261009-0002"
			case "tag":
				in["tags"] = "missing-tag"
			case "status":
				in["status"] = "review"
			}
			errorCode(t, s, "torque_tasks", in, "unavailable")
		})
	}
}
