package operations

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeUpstream struct {
	read func(string, object) (any, error)
}

func (f fakeUpstream) Read(_ context.Context, name string, input object) (any, error) {
	return f.read(name, input)
}
func rowIDs(t *testing.T, b object, section string, want []string) {
	t.Helper()
	expect(t, listIDs(b["sections"].(object)[section]), want)
}
func TestBoardActivityDateWindowsOrderingAndEstimatedTotals(t *testing.T) {
	s := newService(t)
	add := func(db string, item object) { t.Helper(); call(t, s, "create", object{"db": db, "item": item}) }
	add("roadmap", object{"id": "RM-flight", "title": "Flight", "area": "process", "horizon": "now", "status": "in-progress"})
	add("roadmap", object{"id": "RM-board", "title": "Boarding", "area": "process", "horizon": "next", "status": "adopting"})
	add("roadmap", object{"id": "RM-old", "title": "Road old", "area": "process", "horizon": "now", "status": "landed"})
	call(t, s, "update", object{"db": "roadmap", "id": "RM-old", "patch": object{"updated": nil}})
	add("decisions", object{"id": "DEC-recent", "title": "Recent", "status": "decided", "date": "2026-10-07"})
	add("decisions", object{"id": "DEC-old", "title": "Old date wins", "status": "decided", "date": "2026-10-01"})
	add("risks", object{"id": "RK-high", "title": "Risk high", "kind": "risk", "severity": "high", "status": "open"})
	call(t, s, "inbox_add", object{"title": "Hold"})
	ago := func(h time.Duration) string { return iso(s.clock().Add(-h * time.Hour)) }
	task := func(id, status string, h time.Duration, priority int) object {
		return object{"id": id, "title": id, "status": status, "priority": priority, "updated_at": ago(h), "tags": []any{object{"slug": "ws-fixture"}}}
	}
	s.Torque = fakeUpstream{func(name string, in object) (any, error) {
		if name == "torque_task" {
			comments := []any{}
			if in["id"] == "CW-20261008-0002" {
				comments = append(comments, object{"created_at": ago(1)})
			}
			return object{"comments": comments}, nil
		}
		if name != "torque_tasks" {
			t.Fatalf("unexpected upstream %s", name)
		}
		status := str(in["status"])
		expect(t, in["sort_by"], "updated_at")
		expect(t, in["include_total"], true)
		switch status {
		case "doing":
			return object{"items": []any{task("CW-20261008-0001", "doing", 1, 3), task("CW-20261008-0002", "doing", 6, 3), task("CW-20261008-0003", "doing", 7, 3)}, "meta": object{"total": 10}}, nil
		case "review":
			return object{"items": []any{task("CW-20261008-0004", "review", 50, 2)}, "meta": object{"total": 1}}, nil
		case "queued":
			return object{"items": []any{task("CW-20261008-0005", "queued", 1, 3), task("CW-20261008-0006", "queued", 3, 1), task("CW-20261008-0099", "todo", 1, 1)}, "meta": object{"total": 8}}, nil
		case "done":
			expect(t, in["updated_after"], "2026-10-07T12:00:00Z")
			return object{"items": []any{task("CW-20261008-0007", "done", 1, 3), task("CW-20261008-0008", "done", 30, 3)}, "meta": object{"total": 20}}, nil
		}
		return nil, errors.New("unexpected")
	}}
	b := itemCall(t, s, "board", object{"recent_hours": 24, "active_hours": 2, "limit": 30})
	rowIDs(t, b, "in_flight", []string{"CW-20261008-0001", "CW-20261008-0002", "RM-flight", "CW-20261008-0004"})
	rowIDs(t, b, "pre_flight", []string{"CW-20261008-0003", "CW-20261008-0006", "CW-20261008-0005", "RM-board", "RM-a"})
	rowIDs(t, b, "landed", []string{"CW-20261008-0007", "RM-old", "DEC-recent"})
	rowIDs(t, b, "holds", []string{"DEC-003", "RK-high", "IN-001"})
	expect(t, b["totals"].(object)["in_flight"], 11)
	expect(t, b["totals"].(object)["pre_flight"], 11)
	expect(t, b["totals"].(object)["landed"], 21)
	expect(t, b["sections"].(object)["in_flight"].([]any)[0].(object)["workstream"], "fixture")
	expect(t, b["sections"].(object)["landed"].([]any)[2].(object)["updated"], "2026-10-07T00:00:00.000Z")
	capped := itemCall(t, s, "board", object{"recent_hours": 24, "limit": 1})
	rowIDs(t, capped, "holds", []string{"DEC-003"})
	expect(t, capped["counts"].(object)["holds"], 1)
	expect(t, capped["totals"].(object)["holds"], 3)
	for _, in := range []object{{"limit": 0}, {"limit": 201}, {"limit": 1.5}, {"recent_hours": 0}, {"recent_hours": 2161}, {"active_hours": 0}, {"active_hours": 721}, {"active_hours": nil}} {
		errorCode(t, s, "board", in, "bad_request")
	}
}
func TestBoardOutageAndCommentFallback(t *testing.T) {
	s := newService(t)
	allDown := itemCall(t, s, "board", object{})
	rowIDs(t, allDown, "holds", []string{"DEC-003"})
	expect(t, allDown["notices"], []string{"Torque is unavailable (Torque unavailable); showing tracker items only."})
	s.Torque = fakeUpstream{func(name string, in object) (any, error) {
		if name == "torque_task" {
			return nil, errors.New("secret upstream error")
		}
		if in["status"] == "review" {
			return nil, errors.New("offline")
		}
		items := []any{}
		if in["status"] == "doing" {
			items = append(items, object{"id": "CW-20261008-0001", "title": "Idle but conservatively active", "status": "doing", "updated_at": "2026-01-01T00:00:00Z"})
		}
		return object{"items": items, "meta": object{}}, nil
	}}
	b := itemCall(t, s, "board", object{})
	rowIDs(t, b, "in_flight", []string{"CW-20261008-0001"})
	expect(t, b["notices"], []string{"Torque review tasks unavailable: Torque unavailable", "Torque comments unavailable; every doing task is shown as in flight."})
}
func TestBoardPureDayEndFutureClampAndGroupOrdering(t *testing.T) {
	s := newService(t)
	state, err := s.Store.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state.Envelopes["roadmap"]["items"] = []any{
		object{"id": "RM-end", "title": "Last day", "status": "landed", "updated": "2026-10-01"},
		object{"id": "RM-expired", "title": "Expired", "status": "done", "updated": "2026-09-30"},
		object{"id": "RM-future", "title": "Future clamped", "status": "landed", "updated": "2027-01-01"},
		object{"id": "RM-next", "title": "Next", "status": "planned", "horizon": "next", "order": 1},
		object{"id": "RM-now", "title": "Now", "status": "planned", "horizon": "now", "order": 99},
		object{"id": "RM-later", "title": "Later excluded", "status": "planned", "horizon": "later"},
	}
	// Normalize the direct synthetic objects as Call would.
	v, err := normalize(state.Envelopes)
	if err != nil {
		t.Fatal(err)
	}
	for db, env := range v.(object) {
		state.Envelopes[db] = env.(object)
	}
	x := &execution{service: s, state: state, now: s.clock(), ctx: t.Context()}
	b := composeBoard(x, 72, 2, 30, nil, nil, nil, false, []any{})
	normalized, _ := normalize(b)
	b = normalized.(object)
	rowIDs(t, b, "landed", []string{"RM-future", "RM-end"})
	rowIDs(t, b, "pre_flight", []string{"RM-now", "RM-next"})
	expect(t, b["sections"].(object)["landed"].([]any)[0].(object)["updated"], "2026-10-08T12:00:00.000Z")
}
func TestUpstreamReadBoundaryValidationAndTypedErrors(t *testing.T) {
	s := newService(t)
	called := false
	s.Torque = fakeUpstream{func(name string, in object) (any, error) {
		called = true
		if name == "torque_task" {
			return nil, failure("not_found", "unknown task", object{"torque_status": 404})
		}
		if name == "torque_tasks" {
			expect(t, in["updated_after"], "2026-10-08T00:00:00Z")
			return object{"items": []any{}, "meta": object{"total": 0}}, nil
		}
		return []any{}, nil
	}}
	for _, test := range []struct {
		name string
		in   object
	}{
		{"torque_task", object{"id": "../bad"}}, {"torque_tasks", object{"secret": "unknown"}}, {"torque_tasks", object{"limit": 201}}, {"torque_tasks", object{"project_id": "bad"}}, {"torque_tasks", object{"status": "bad/status"}}, {"torque_tasks", object{"tags": "bad tag"}}, {"torque_tasks", object{"sort_dir": "up"}}, {"torque_tasks", object{"updated_after": "yesterday"}}, {"torque_tasks", object{"cursor": "?"}}, {"torque_facets", object{"dimensions": []string{}}}, {"torque_facets", object{"dimensions": []string{"unknown"}}}, {"torque_projects", object{"project_id": "PRJ-20261008-0001"}},
	} {
		called = false
		errorCode(t, s, test.name, test.in, "bad_request")
		if called {
			t.Fatal("invalid upstream input reached client")
		}
	}
	errorCode(t, s, "torque_task", object{"id": "CW-20261008-0001"}, "not_found")
	call(t, s, "torque_tasks", object{"updated_after": "2026-10-08"})
	for _, name := range []string{"torque_projects", "torque_epics", "torque_sprints", "torque_titles", "torque_facets"} {
		in := object{}
		if name == "torque_titles" {
			in["ids"] = []string{"CW-20261008-0001"}
		}
		call(t, s, name, in)
	}
	s.Torque = nil
	errorCode(t, s, "torque_task", object{"id": "CW-20261008-0001"}, "unavailable")
}

func TestPinnedEnglishBoardIDTieOrdering(t *testing.T) {
	s := newService(t)
	state, err := s.Store.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rows := []any{}
	for _, id := range []string{"IN-e", "IN-é", "IN-A", "IN-a", "IN-a-b", "IN-ab"} {
		rows = append(rows, object{"id": id, "title": id, "status": "new", "updated": "2026-10-08"})
	}
	state.Envelopes["inbox"]["items"] = rows
	x := &execution{service: s, state: state, now: s.clock(), ctx: t.Context()}
	b := composeBoard(x, 72, 2, 30, nil, nil, nil, false, []any{})
	v, _ := normalize(b)
	rowIDs(t, v.(object), "holds", []string{"DEC-003", "IN-a", "IN-A", "IN-a-b", "IN-ab", "IN-e", "IN-é"})
}
