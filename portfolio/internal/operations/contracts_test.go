package operations

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
	"path/filepath"
	"strings"
	"testing"
)

func grant(_ context.Context, caller Caller, _ string, _ object) (Authority, error) {
	return Authority{Principal: "fixture-principal", Verified: caller.Binding == "test-binding", Allowed: true}, nil
}

func TestDatabaseContractsNullsReferencesAndOpaqueNumbers(t *testing.T) {
	s := newService(t)
	item := itemCall(t, s, "create", object{"db": "ideas", "item": object{
		"title": "Typed", "kind": "idea", "unknown": object{"exact": json.Number("9007199254740993"), "null": nil},
	}})
	expect(t, item["unknown"], object{"exact": json.Number("9007199254740993"), "null": nil})
	errorCode(t, s, "create", object{"db": "risks", "item": object{"title": "Wrong", "kind": "risk", "severity": "critical"}}, "invalid")
	errorCode(t, s, "create", object{"db": "ideas", "item": object{"title": "Wrong", "kind": "idea", "links": []any{object{"kind": "url", "ref": 7}}}}, "invalid")
	errorCode(t, s, "create", object{"db": "decisions", "item": object{"title": "Wrong", "options": []any{object{"label": "Missing id"}}}}, "invalid")
	itemCall(t, s, "update", object{"db": "ideas", "id": item["id"], "patch": object{"unknown": nil, "notes": "remove"}})
	updated := itemCall(t, s, "update", object{"db": "ideas", "id": item["id"], "patch": object{"notes": nil}})
	if _, exists := updated["unknown"]; exists {
		t.Fatal("unknown null was not removed")
	}
	errorCode(t, s, "update", object{"db": "ideas", "id": item["id"], "patch": object{"title": nil}}, "invalid")
	expect(t, currentItem(t, s, "ideas", str(item["id"])), updated)
	itemCall(t, s, "update", object{"db": "ideas", "id": "ID-x", "patch": object{"tags": []any{"owned"}}})
	expect(t, listIDs(call(t, s, "list", object{"db": "ideas", "filters": object{"tags": "owned"}})), []string{"ID-x"})
	errorCode(t, s, "list", object{"db": "ideas", "filters": object{"tags": []any{"owned"}}}, "bad_request")
}

func TestPageContinuationOnlyObservesSelectedCohort(t *testing.T) {
	s := newService(t)
	first := itemCall(t, s, "list", object{"db": "ideas", "page": object{"limit": 1}})
	expect(t, first["total"], 2)
	expect(t, first["has_more"], true)
	continuation := object{"db": "ideas", "page": object{"limit": 1, "offset": first["next_offset"], "snapshot": first["snapshot"]}}
	itemCall(t, s, "update", object{"db": "risks", "id": "RK-001", "patch": object{"notes": "unrelated"}})
	second := itemCall(t, s, "list", continuation)
	expect(t, listIDs(second["items"]), []string{"ID-y"})
	expect(t, second["snapshot"], first["snapshot"])
	itemCall(t, s, "update", object{"db": "ideas", "id": "ID-y", "patch": object{"notes": "relevant"}})
	errorCode(t, s, "list", continuation, "conflict")
	for _, page := range []object{{"limit": 0}, {"limit": 201}, {"offset": -1}, {"offset": 100001}, {"offset": 1}, {"snapshot": "short"}, {"extra": true}} {
		errorCode(t, s, "list", object{"db": "ideas", "page": page}, "bad_request")
	}
	errorCode(t, s, "list", object{"db": "ideas", "scope": "project-x"}, "unsupported")
	search := itemCall(t, s, "search", object{"q": "idea", "db": "ideas", "page": object{"limit": 1}})
	searchNext := object{"q": "idea", "db": "ideas", "page": object{"limit": 1, "offset": 1, "snapshot": search["snapshot"]}}
	itemCall(t, s, "update", object{"db": "risks", "id": "RK-001", "patch": object{"notes": "still unrelated"}})
	itemCall(t, s, "search", searchNext)
	itemCall(t, s, "update", object{"db": "ideas", "id": "ID-y", "patch": object{}})
	errorCode(t, s, "search", searchNext, "conflict")
	errorCode(t, s, "search", object{"q": "different", "db": "ideas", "page": object{"offset": 1, "snapshot": search["snapshot"]}}, "conflict")
}

func batchEntry(name string, input object) object { return object{"operation": name, "input": input} }

func TestBatchSequentialCASDetachedResultsAndRollback(t *testing.T) {
	s := newService(t)
	s.Admit = grant
	input := object{"entries": []any{
		batchEntry("create", object{"db": "ideas", "item": object{"id": "ID-batch", "title": "Batch", "kind": "idea"}}),
		batchEntry("update", object{"db": "ideas", "id": "ID-batch", "rev": 1, "patch": object{"title": "Second", "precise": json.Number("9007199254740993")}}),
		batchEntry("comment", object{"db": "ideas", "id": "ID-batch", "text": "Third"}),
	}}
	got := itemCall(t, s, "batch", input)["results"].([]any)
	expect(t, got[0].(object)["title"], "Batch")
	expect(t, got[1].(object)["title"], "Second")
	expect(t, got[2].(object)["rev"], 3)
	expect(t, got[2].(object)["precise"], json.Number("9007199254740993"))
	expect(t, got[2].(object)["comments"].([]any)[0].(object)["author"], "fixture-principal")
	before, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, later := range []object{
		batchEntry("update", object{"db": "ideas", "id": "ID-batch", "rev": 1, "patch": object{"title": "Stale"}}),
		batchEntry("update", object{"db": "ideas", "id": "ID-batch", "patch": object{"title": nil}}),
		batchEntry("decide", object{"id": "DEC-003", "option": "absent"}),
	} {
		_, err := s.Call(t.Context(), Caller{"test-binding"}, "batch", object{"entries": []any{
			batchEntry("create", object{"db": "ideas", "item": object{"id": "ID-rollback", "title": "Rollback", "kind": "idea"}}), later,
		}})
		if err == nil {
			t.Fatal("failed entry committed")
		}
		after, exportErr := s.Store.Export(t.Context())
		if exportErr != nil {
			t.Fatal(exportErr)
		}
		expect(t, after, before)
	}
	itemCall(t, s, "batch", object{"entries": []any{batchEntry("create", object{"db": "ideas", "item": object{"id": "ID-rollback", "title": "Reclaimed after rollback", "kind": "idea"}})}})
}

func TestBatchGrantsPrincipalsAndBoundsBeforeEffects(t *testing.T) {
	for _, mode := range []string{"missing", "deny-later", "change-principal", "revoke-under-lock"} {
		t.Run(mode, func(t *testing.T) {
			s := newService(t)
			checks := 0
			if mode != "missing" {
				s.Admit = func(ctx context.Context, caller Caller, name string, in object) (Authority, error) {
					a, err := grant(ctx, caller, name, in)
					if name == "batch" {
						checks++
					}
					if mode == "deny-later" && name == "decide" || mode == "revoke-under-lock" && checks > 1 {
						a.Allowed = false
					}
					if mode == "change-principal" && name == "decide" {
						a.Principal = "other"
					}
					return a, err
				}
			}
			errorCode(t, s, "batch", object{"entries": []any{
				batchEntry("comment", object{"db": "ideas", "id": "ID-x", "text": "Must not commit"}),
				batchEntry("decide", object{"id": "DEC-003", "option": "a"}),
			}}, "unavailable")
			s.Admit = nil
			expect(t, len(currentItem(t, s, "ideas", "ID-x")["comments"].([]any)), 1)
			expect(t, currentItem(t, s, "decisions", "DEC-003")["status"], "needs-decision")
		})
	}
	s := newService(t)
	s.Admit = grant
	for _, input := range []object{
		{"entries": []any{}},
		{"entries": []any{batchEntry("batch", object{})}},
		{"entries": []any{batchEntry("migrate", object{})}},
		{"entries": []any{batchEntry("torque_task", object{"id": "CW-20261010-0001"})}},
		{"entries": []any{batchEntry("comment", object{"db": "ideas", "id": "ID-x", "text": strings.Repeat("x", MaxInputBytes)})}},
	} {
		errorCode(t, s, "batch", input, "bad_request")
	}
	entries := []any{}
	for range 21 {
		entries = append(entries, batchEntry("reopen", object{"id": "DEC-003"}))
	}
	errorCode(t, s, "batch", object{"entries": entries}, "bad_request")
}

func TestAdmissionWithholdsConflictAndPostCommitResults(t *testing.T) {
	s := newService(t)
	checks := 0
	s.Admit = func(ctx context.Context, caller Caller, name string, in object) (Authority, error) {
		a, err := grant(ctx, caller, name, in)
		if name == "update" {
			checks++
			if checks > 2 {
				return Authority{}, errors.New("revoked")
			}
		}
		return a, err
	}
	got, err := s.Call(t.Context(), Caller{"test-binding"}, "update", object{"db": "ideas", "id": "ID-y", "patch": object{"title": "Committed"}})
	if got != nil || err == nil {
		t.Fatal("revoked result disclosed", got, err)
	}
	s.Admit = nil
	expect(t, currentItem(t, s, "ideas", "ID-y")["title"], "Committed")
}

func TestBatchAuthorityLossBeforeCommitAndAfterCommit(t *testing.T) {
	for _, boundary := range []string{"before-commit", "after-commit"} {
		t.Run(boundary, func(t *testing.T) {
			s := newService(t)
			checks := 0
			s.Admit = func(ctx context.Context, caller Caller, name string, in object) (Authority, error) {
				a, err := grant(ctx, caller, name, in)
				if name == "batch" {
					checks++
					if boundary == "before-commit" && checks >= 3 || boundary == "after-commit" && checks >= 4 {
						a.Allowed = false
					}
				}
				return a, err
			}
			errorCode(t, s, "batch", object{"entries": []any{batchEntry("comment", object{"db": "ideas", "id": "ID-x", "text": "Effect"})}}, "unavailable")
			s.Admit = nil
			comments := currentItem(t, s, "ideas", "ID-x")["comments"].([]any)
			want := 1
			if boundary == "after-commit" {
				want = 2
			}
			if len(comments) != want {
				t.Fatal("incorrect authority boundary effect", comments)
			}
		})
	}
}

func TestBatchOutputBoundRollsBackBeforePublication(t *testing.T) {
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "large-shadow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	files := fixtureFiles()
	files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","items":[{"id":"ID-large","title":"Large","kind":"idea","status":"new","opaque":"` + strings.Repeat("x", 600000) + `"}]}`)
	snap, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, Admit: grant}
	errorCode(t, s, "batch", object{"entries": []any{
		batchEntry("comment", object{"db": "ideas", "id": "ID-large", "text": "one"}),
		batchEntry("comment", object{"db": "ideas", "id": "ID-large", "text": "two"}),
	}}, "unavailable")
	s.Admit = nil
	item := currentItem(t, s, "ideas", "ID-large")
	if _, has := item["comments"]; has {
		t.Fatal("overflow committed child writes")
	}
	if _, has := item["rev"]; has {
		t.Fatal("overflow committed revision")
	}
}
