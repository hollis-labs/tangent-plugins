package operations

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

func fixtureFiles() map[string][]byte {
	files := map[string][]byte{}
	items := map[string]string{
		"priorities":  `[{"id":"PR-009","title":"Priority","rank":1,"status":"active"}]`,
		"workstreams": `[{"id":"WS-one","title":"One","status":"active"}]`,
		"roadmap":     `[{"id":"RM-a","title":"Alpha","area":"hub","horizon":"now","status":"planned"}]`,
		"ideas":       `[{"id":"ID-x","title":"Idea X","kind":"idea","status":"new","unknown":{"nil":null,"list":[1,true]},"comments":[{"id":"c-9","author":"historical","text":"a.b/token partialword","created":"2020-01-01T00:00:00Z","extra":null}],"_portfolio_relationships":{"version":1,"edges":[{"type":"informs","target":"DEC-external","custom":null}]}},{"id":"ID-y","title":"Idea Y","kind":"question","status":"new"}]`,
		"decisions":   `[{"id":"DEC-001","title":"Old","status":"active","decision":"historical"},{"id":"DEC-003","title":"Pick","status":"needs-decision","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}]`,
		"risks":       `[{"id":"RK-001","title":"Risk","kind":"risk","severity":"low","status":"open"}]`,
		"inbox":       `[]`,
	}
	for _, db := range dbNames {
		files[db] = []byte(`{"schema":"portfolio/` + db + `@1","updated":"2020-01-01","custom":null,"items":` + items[db] + `}`)
	}
	return files
}
func newService(t *testing.T) *Service {
	t.Helper()
	s, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "shadow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	snap, err := storage.ParseSnapshot(fixtureFiles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	return &Service{Store: s, Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }, Verify: func(_ context.Context, caller Caller, _ string) (Authority, error) {
		return Authority{Principal: "verified-test", Verified: caller.Binding == "test-binding", Allowed: true}, nil
	}}
}
func call(t *testing.T, s *Service, name string, in any) any {
	t.Helper()
	out, err := s.Call(t.Context(), Caller{"test-binding"}, name, in)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}
func itemCall(t *testing.T, s *Service, name string, in object) object {
	t.Helper()
	return call(t, s, name, in).(object)
}
func errorCode(t *testing.T, s *Service, name string, in any, want string) *Error {
	t.Helper()
	_, err := s.Call(t.Context(), Caller{"test-binding"}, name, in)
	var e *Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("%s error: %v, want %s", name, err, want)
	}
	return e
}
func expect(t *testing.T, got, want any) {
	t.Helper()
	if jsonText(got) != jsonText(want) {
		t.Fatalf("got %s, want %s", jsonText(got), jsonText(want))
	}
}
func listIDs(v any) []string {
	out := []string{}
	for _, v := range v.([]any) {
		out = append(out, str(v.(object)["id"]))
	}
	return out
}
func currentItem(t *testing.T, s *Service, db, id string) object {
	t.Helper()
	return itemCall(t, s, "get", object{"db": db, "id": id})
}
func TestInputSchemasAndDetachedMetadata(t *testing.T) {
	s := newService(t)
	for _, in := range []any{nil, []any{}, "nope"} {
		errorCode(t, s, "get", in, "bad_request")
	}
	errorCode(t, s, "missing", object{}, "bad_request")
	errorCode(t, s, "get", object{"db": "ideas"}, "bad_request")
	errorCode(t, s, "get", object{"db": "ideas", "id": nil}, "bad_request")
	errorCode(t, s, "get", object{"db": "ideas", "id": 1}, "bad_request")
	errorCode(t, s, "get", object{"db": "nope", "id": "ID-x"}, "not_found")
	expect(t, itemCall(t, s, "get", object{"db": "ideas", "id": "ID-y", "extra": nil})["title"], "Idea Y")
	risk := itemCall(t, s, "schema", object{"db": "risks"})
	expect(t, risk["required"], []string{"id", "title", "kind", "severity", "status"})
	expect(t, risk["enums"].(object)["severity"], []string{"low", "medium", "high"})
	expect(t, risk["fields"].(object)["priority"].(object)["maximum"], 5)
	for _, op := range Registry() {
		if op.Name == "update" {
			op.Input["type"] = "array"
		}
		if op.Name == "get" && op.Write {
			t.Fatal("get writes")
		}
		if op.Name == "inbox_promote" && !op.Write {
			t.Fatal("promotion isn't classified as write")
		}
	}
	itemCall(t, s, "update", object{"db": "ideas", "id": "ID-y", "patch": object{}})
}
func TestVerifiedRequestAuthorityOwnsEveryAssignedPrincipal(t *testing.T) {
	s := newService(t)
	snap, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, verify := range []VerifyCaller{nil, func(context.Context, Caller, string) (Authority, error) {
		return Authority{"claimed-role", false, true}, nil
	}, func(context.Context, Caller, string) (Authority, error) {
		return Authority{"verified", true, false}, nil
	}, func(context.Context, Caller, string) (Authority, error) { return Authority{"", true, true}, nil }} {
		s.Verify = verify
		errorCode(t, s, "create", object{"db": "ideas", "item": object{"title": "Forged", "kind": "idea", "author": "owner"}}, "unavailable")
	}
	after, err := s.Store.Export(t.Context())
	if err != nil || !reflect.DeepEqual(after, snap) {
		t.Fatal("denied authority changed store")
	}
	s.Verify = func(_ context.Context, c Caller, op string) (Authority, error) {
		return Authority{"verified-test", c.Binding == "test-binding", op != "migrate"}, nil
	}
	errorCode(t, s, "migrate", object{}, "unavailable")
	created := itemCall(t, s, "create", object{"db": "ideas", "item": object{"title": "Auth", "kind": "idea", "author": "forged", "added_by": "forged", "decided_by": "forged"}})
	for _, k := range []string{"author", "added_by", "decided_by"} {
		expect(t, created[k], "verified-test")
	}
	updated := itemCall(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{"author": nil, "added_by": "forged", "decided_by": "forged"}})
	for _, k := range []string{"author", "added_by", "decided_by"} {
		expect(t, updated[k], "verified-test")
	}
	expect(t, updated["comments"].([]any)[0].(object)["author"], "historical")
	c := itemCall(t, s, "comment", object{"db": "ideas", "id": "ID-x", "text": "new", "author": "forged"})
	expect(t, c["comments"].([]any)[1].(object)["author"], "verified-test")
	d := itemCall(t, s, "decide", object{"id": "DEC-003", "option": "a", "author": "forged", "comment": "choice"})
	expect(t, d["decided_by"], "verified-test")
	expect(t, d["comments"].([]any)[0].(object)["author"], "verified-test")
	in := itemCall(t, s, "inbox_add", object{"title": "Promotion", "added_by": "forged"})
	expect(t, in["added_by"], "verified-test")
	promoted := itemCall(t, s, "inbox_promote", object{"id": in["id"], "to_db": "ideas", "fields": object{"author": "forged", "added_by": "forged", "decided_by": "forged"}})["item"].(object)
	for _, k := range []string{"author", "added_by", "decided_by"} {
		expect(t, promoted[k], "verified-test")
	}
	s.Verify = func(_ context.Context, c Caller, _ string) (Authority, error) {
		return Authority{"another", c.Binding == "another-binding", true}, nil
	}
	if _, err = s.Call(t.Context(), Caller{"test-binding"}, "comment", object{"db": "ideas", "id": "ID-x", "text": "revoked"}); err == nil {
		t.Fatal("reused stale caller")
	}
}
func TestCreateAllocatorsDefaultsAndPermanentDroppedIDs(t *testing.T) {
	s := newService(t)
	for _, test := range []struct {
		db   string
		item object
		id   string
	}{
		{"priorities", object{"title": "Next", "rank": 2}, "PR-010"}, {"decisions", object{"title": "Next"}, "DEC-004"}, {"risks", object{"title": "Next", "kind": "gap", "severity": "low"}, "RK-002"}, {"inbox", object{"title": "Next"}, "IN-001"},
		{"workstreams", object{"title": "Hello, World!"}, "WS-hello-world"}, {"roadmap", object{"title": "Hello World", "area": "process", "horizon": "next"}, "RM-hello-world"}, {"ideas", object{"title": "Hello World", "kind": "idea", "rev": 999, "comments": []any{object{"id": "c-100"}}}, "ID-hello-world"},
	} {
		item := itemCall(t, s, "create", object{"db": test.db, "item": test.item})
		expect(t, item["id"], test.id)
		expect(t, item["rev"], 1)
		expect(t, item["created"], "2026-10-08")
		if _, has := item["comments"]; has {
			t.Fatal("create accepted comments")
		}
	}
	itemCall(t, s, "update", object{"db": "ideas", "id": "ID-hello-world", "patch": object{"status": "dropped"}})
	expect(t, itemCall(t, s, "create", object{"db": "ideas", "item": object{"title": "Hello World", "kind": "idea"}})["id"], "ID-hello-world-2")
	errorCode(t, s, "create", object{"db": "ideas", "item": object{"id": "PR-009", "title": "Duplicate", "kind": "idea"}}, "conflict")
	expect(t, itemCall(t, s, "create", object{"db": "ideas", "item": object{"title": "...", "kind": "idea"}})["id"], "ID-item")
	errorCode(t, s, "create", object{"db": "ideas", "item": object{"title": "Bad", "kind": "idea", "status": nil}}, "invalid")
	bad := errorCode(t, s, "create", object{"db": "risks", "item": object{"title": "Bad", "severity": "extreme"}}, "invalid")
	if bad.Details["errors"] == nil {
		t.Fatal("missing validation detail")
	}
	errorCode(t, s, "create", object{"db": "ideas", "item": object{}}, "invalid")
}
func TestUpdateCASLosslessPatchAndSearch(t *testing.T) {
	s := newService(t)
	original := currentItem(t, s, "ideas", "ID-x")
	first := itemCall(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{"summary": "first", "tags": []any{"old"}, "new_unknown": nil}, "rev": 0})
	expect(t, first["rev"], 1)
	expect(t, first["unknown"], original["unknown"])
	if _, has := first["new_unknown"]; has {
		t.Fatal("null patch was materialized")
	}
	stale := errorCode(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{"summary": "stale"}, "rev": 0}, "conflict")
	expect(t, stale.Details["current"], first)
	second := itemCall(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{"summary": nil, "tags": []any{"new"}}})
	expect(t, second["rev"], 2)
	expect(t, second["tags"], []string{"new"})
	if _, has := second["summary"]; has {
		t.Fatal("null failed to remove")
	}
	noop := itemCall(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{}})
	expect(t, noop["rev"], 3)
	for _, key := range []string{"id", "created", "comments", "rev"} {
		errorCode(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{key: nil}}, "bad_request")
	}
	errorCode(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{"status": "invalid"}}, "invalid")
	expect(t, currentItem(t, s, "ideas", "ID-x"), noop)
	for _, q := range []string{"PARTIAL a.b/token", "a.b", "x", "rtialword"} {
		expect(t, listIDs(call(t, s, "search", object{"q": q, "db": "ideas"})), []string{"ID-x"})
	}
	expect(t, listIDs(call(t, s, "search", object{"q": "old", "db": "ideas"})), []string{})
	errorCode(t, s, "search", object{"q": "   "}, "bad_request")
	files, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Store.Verify(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	originalSnap, err := storage.ParseSnapshot(fixtureFiles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Import(t.Context(), originalSnap); err == nil {
		t.Fatal("mutation allowed original replay")
	}
	edges, err := s.Store.ListEdges(t.Context(), "ID-x", "informs")
	if err != nil {
		t.Fatal(err)
	}
	expect(t, edges, []storage.Edge{{FromID: "ID-x", ToID: "DEC-external", Type: "informs", Field: "_portfolio_relationships"}})
}
func TestCommentPerItemSequenceAndLinkNoOps(t *testing.T) {
	s := newService(t)
	a := itemCall(t, s, "comment", object{"db": "ideas", "id": "ID-x", "text": "first", "kind": "decision"})
	expect(t, a["comments"].([]any)[1].(object)["id"], "c-10")
	expect(t, a["rev"], 1)
	b := itemCall(t, s, "comment", object{"db": "ideas", "id": "ID-y", "text": "first"})
	expect(t, b["comments"].([]any)[0].(object)["id"], "c-1")
	errorCode(t, s, "comment", object{"db": "ideas", "id": "ID-x", "text": " "}, "invalid")
	in := object{"from": object{"db": "ideas", "id": "ID-x"}, "to": object{"db": "risks", "id": "RK-001"}, "field": "risk_ids"}
	linked := itemCall(t, s, "link", in)
	expect(t, linked["risk_ids"], []string{"RK-001"})
	expect(t, linked["rev"], 2)
	expect(t, itemCall(t, s, "link", in), linked)
	unlinked := itemCall(t, s, "unlink", in)
	expect(t, unlinked["risk_ids"], []string{})
	expect(t, unlinked["rev"], 3)
	expect(t, itemCall(t, s, "unlink", in), unlinked)
	errorCode(t, s, "link", object{"from": in["from"], "to": object{"db": "risks", "id": "RK-nope"}}, "not_found")
	errorCode(t, s, "link", object{"from": in["from"], "to": in["to"], "field": "title"}, "bad_request")
}
func TestExternalLinksAndReorderWriteRules(t *testing.T) {
	s := newService(t)
	add := object{"db": "ideas", "id": "ID-y", "link": object{"kind": "file", "ref": " /never/read ", "label": " notes "}}
	a := itemCall(t, s, "link_add", add)
	expect(t, a["links"], []any{object{"kind": "file", "ref": "/never/read", "label": "notes"}})
	errorCode(t, s, "link_add", add, "conflict")
	for _, link := range []object{{"kind": "unknown", "ref": "x"}, {"kind": "url", "ref": "javascript:alert(1)"}, {"kind": "torque", "ref": "bad"}, {"kind": "file", "ref": "x", "extra": 1}, {"kind": "file", "ref": " "}} {
		errorCode(t, s, "link_add", object{"db": "ideas", "id": "ID-y", "link": link}, "invalid")
	}
	removed := itemCall(t, s, "link_remove", object{"db": "ideas", "id": "ID-y", "kind": "file", "ref": "/never/read"})
	expect(t, removed["rev"], 2)
	expect(t, removed["links"], []any{})
	errorCode(t, s, "link_remove", object{"db": "ideas", "id": "ID-y", "kind": "file", "ref": "/never/read"}, "not_found")
	call(t, s, "reorder", object{"db": "ideas", "ids": []string{"ID-x", "ID-y"}})
	expect(t, listIDs(call(t, s, "list", object{"db": "ideas"})), []string{"ID-x", "ID-y"})
	first := currentItem(t, s, "ideas", "ID-x")
	call(t, s, "reorder", object{"db": "ideas", "ids": []string{"ID-x"}})
	expect(t, currentItem(t, s, "ideas", "ID-x"), first)
	call(t, s, "reorder", object{"db": "ideas", "ids": []string{"ID-y"}})
	expect(t, currentItem(t, s, "ideas", "ID-y")["order"], 10)
	expect(t, currentItem(t, s, "ideas", "ID-x")["order"], 20)
	expect(t, itemCall(t, s, "create", object{"db": "ideas", "item": object{"title": "Late", "kind": "idea"}})["order"], 30)
	errorCode(t, s, "reorder", object{"db": "ideas", "ids": []string{"ID-x", "ID-x"}}, "bad_request")
	errorCode(t, s, "reorder", object{"db": "ideas", "ids": []string{"ID-nope"}}, "not_found")
	expect(t, listIDs(call(t, s, "list", object{"db": "ideas", "filters": object{"related_ids": "absent"}})), []string{})
	expect(t, listIDs(call(t, s, "list", object{"db": "ideas", "sort": "-title"})), []string{"ID-late", "ID-y", "ID-x"})
}
func TestDecisionTransitionsAndInboxDefaults(t *testing.T) {
	s := newService(t)
	errorCode(t, s, "decide", object{"id": "DEC-003", "option": "unknown"}, "invalid")
	decided := itemCall(t, s, "decide", object{"id": "DEC-003", "option": "b", "comment": "why"})
	expect(t, decided["decision"], "B")
	expect(t, decided["rev"], 1)
	// The source permits deciding an already decided item; description is stricter.
	decided = itemCall(t, s, "decide", object{"id": "DEC-003", "option": "a"})
	expect(t, decided["selected_option"], "a")
	expect(t, decided["decision"], "B")
	expect(t, decided["rev"], 2)
	reopened := itemCall(t, s, "reopen", object{"id": "DEC-003"})
	expect(t, reopened["decided_by"], "")
	if _, has := reopened["selected_option"]; has {
		t.Fatal("reopen retained choice")
	}
	deferred := itemCall(t, s, "defer", object{"id": "DEC-003", "note": "later"})
	expect(t, deferred["status"], "deferred")
	expect(t, deferred["comments"].([]any)[1].(object)["kind"], "comment")
	for _, test := range []struct {
		db   string
		want object
	}{
		{"ideas", object{"kind": "idea", "status": "new"}}, {"risks", object{"kind": "risk", "severity": "medium", "status": "open"}}, {"roadmap", object{"area": "process", "horizon": "someday", "status": "idea"}}, {"priorities", object{"rank": 2, "status": "queued"}},
	} {
		in := itemCall(t, s, "inbox_add", object{"title": "Promote " + test.db, "body": "notes", "path": "/never/read"})
		result := itemCall(t, s, "inbox_promote", object{"id": in["id"], "to_db": test.db})
		item := result["item"].(object)
		src := result["inbox"].(object)
		for k, v := range test.want {
			expect(t, item[k], v)
		}
		expect(t, item["notes"], "notes")
		expect(t, item["links"], []any{object{"kind": "file", "ref": "/never/read"}})
		expect(t, item["related_ids"], []any{in["id"]})
		expect(t, src["related_ids"], []any{item["id"]})
		expect(t, src["status"], "promoted")
		expect(t, src["rev"], 2)
		errorCode(t, s, "inbox_promote", object{"id": in["id"], "to_db": test.db}, "conflict")
	}
	in := itemCall(t, s, "inbox_add", object{"title": "Dismiss"})
	d := itemCall(t, s, "inbox_dismiss", object{"id": in["id"], "note": "later"})
	expect(t, d["status"], "dismissed")
	expect(t, d["comments"].([]any)[0].(object)["text"], "later")
	errorCode(t, s, "inbox_promote", object{"id": in["id"], "to_db": "ideas"}, "conflict")
	errorCode(t, s, "inbox_promote", object{"id": in["id"], "to_db": "inbox"}, "bad_request")
}
func TestMigrateExplicitAndIdempotent(t *testing.T) {
	s := newService(t)
	first := itemCall(t, s, "migrate", object{})
	expect(t, first["seeded"], []string{"DEC-017", "DEC-018", "DEC-019"})
	expect(t, currentItem(t, s, "decisions", "DEC-001")["status"], "decided")
	expect(t, currentItem(t, s, "ideas", "ID-x")["rev"], 1)
	expect(t, itemCall(t, s, "migrate", object{}), object{"changed": []any{}, "seeded": []any{}})
	expect(t, listIDs(call(t, s, "list", object{"db": "inbox"})), []string{})
}
func TestTwoHandleCASReservationCommentsAndDisjointWrites(t *testing.T) {
	a := newService(t)
	files, err := a.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shared.db")
	sa, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer sa.Close()
	sb, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if _, err = sa.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	left, right := *a, *a
	left.Store = sa
	right.Store = sb
	both := func(name string, input func(int) object) []error {
		var wg sync.WaitGroup
		result := make([]error, 2)
		services := []*Service{&left, &right}
		for i, s := range services {
			wg.Go(func() { _, result[i] = s.Call(context.Background(), Caller{"test-binding"}, name, input(i)) })
		}
		wg.Wait()
		return result
	}
	errs := both("update", func(int) object {
		return object{"db": "ideas", "id": "ID-y", "rev": 0, "patch": object{"summary": "CAS"}}
	})
	success, conflict := 0, 0
	for _, err := range errs {
		if err == nil {
			success++
		} else {
			var e *Error
			if errors.As(err, &e) && e.Code == "conflict" {
				conflict++
			} else {
				t.Fatal(err)
			}
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("CAS did not choose one winner: %v", errs)
	}
	errs = both("create", func(int) object {
		return object{"db": "ideas", "item": object{"id": "ID-reserved", "title": "Explicit", "kind": "idea"}}
	})
	success = 0
	for _, err := range errs {
		if err == nil {
			success++
		} else {
			var e *Error
			if !errors.As(err, &e) || e.Code != "conflict" {
				t.Fatal(err)
			}
		}
	}
	if success != 1 {
		t.Fatal("reservation lacked single winner")
	}
	for _, err := range both("create", func(int) object {
		return object{"db": "risks", "item": object{"title": "Sequential", "kind": "risk", "severity": "low"}}
	}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	expect(t, listIDs(call(t, &left, "list", object{"db": "risks"})), []string{"RK-001", "RK-002", "RK-003"})
	for _, err := range both("comment", func(int) object { return object{"db": "ideas", "id": "ID-x", "text": "concurrent"} }) {
		if err != nil {
			t.Fatal(err)
		}
	}
	comments := currentItem(t, &left, "ideas", "ID-x")["comments"].([]any)
	expect(t, comments[1].(object)["id"], "c-10")
	expect(t, comments[2].(object)["id"], "c-11")
	for _, err := range both("update", func(i int) object {
		return object{"db": "ideas", "id": []string{"ID-x", "ID-y"}[i], "patch": object{"summary": []string{"disjoint-x", "disjoint-y"}[i]}}
	}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	expect(t, currentItem(t, &right, "ideas", "ID-x")["summary"], "disjoint-x")
	expect(t, currentItem(t, &left, "ideas", "ID-y")["summary"], "disjoint-y")
	exported, err := sa.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	current, err := storage.ParseSnapshot(exported)
	if err != nil {
		t.Fatal(err)
	}
	if err = sb.Verify(t.Context(), current); err != nil {
		t.Fatal(err)
	}
}

func TestDecimalExponentCASAndExhaustedRevision(t *testing.T) {
	s := newService(t)
	call(t, s, "update", object{"db": "ideas", "id": "ID-y", "rev": json.Number("0e0"), "patch": object{}})
	out := itemCall(t, s, "update", object{"db": "ideas", "id": "ID-y", "rev": json.Number("1.0"), "patch": object{}})
	expect(t, out["rev"], 2)
	for _, rev := range []string{"1.00000000000000000001", "9007199254740992.1", "9223372036854775808"} {
		errorCode(t, s, "update", object{"db": "ideas", "id": "ID-y", "rev": json.Number(rev), "patch": object{}}, "bad_request")
	}
	files, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Store.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state.Envelopes["ideas"]["items"].([]any)[0].(object)["rev"] = json.Number("9007199254740991")
	for db, env := range state.Envelopes {
		files[db] = []byte(jsonText(env))
	}
	snap, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "exhausted.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	s.Store = store
	errorCode(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{}}, "invalid")
	if err = store.Verify(t.Context(), snap); err != nil {
		t.Fatal("exhausted write changed state")
	}
}
