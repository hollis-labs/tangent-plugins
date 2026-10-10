package operations

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

func edgeInput(target, typ string, rev any) object {
	in := object{"from": object{"db": "ideas", "id": "ID-x"}, "to": object{"id": target}, "type": typ}
	if rev != nil {
		in["rev"] = rev
	}
	return in
}

func ownedEdgeGrant(_ context.Context, caller Caller, _ string, _ map[string]any, cohort EdgeCohort) (Authority, error) {
	grants := map[string]bool{"ID-x": true, "ID-y": true, "DEC-external": true, "DEC-003": true, "WS-one": true, "EXT-owned": true, "EXT-two": true, "https://example.test": true, "ID-created": true}
	if caller.Binding != "test-binding" || len(cohort.Resources) == 0 {
		return Authority{}, errors.New("not owned")
	}
	for _, resource := range cohort.Resources {
		if !grants[resource.ID] || resource.Access != "read" && resource.Access != "write" && resource.Access != "reference" {
			return Authority{}, errors.New("grant absent")
		}
	}
	return Authority{Principal: "verified-test", Verified: true, Allowed: true}, nil
}
func edgeService(t *testing.T) *Service {
	t.Helper()
	s := newService(t)
	s.AdmitEdges = ownedEdgeGrant
	return s
}
func exported(t *testing.T, s *Service) map[string][]byte {
	t.Helper()
	files, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestEdgeDefaultRefusalStrictInputsAndMissingFacts(t *testing.T) {
	s := newService(t)
	for _, name := range []string{"edge_list", "edge_backlinks", "decision_gates", "edge_add", "edge_remove"} {
		in := object{"db": "ideas", "id": "ID-x"}
		if name == "edge_backlinks" {
			in = object{"id": "ID-x"}
		}
		if name == "edge_add" || name == "edge_remove" {
			in = edgeInput("ID-y", "related", nil)
		}
		errorCode(t, s, name, in, "unavailable")
	}
	s.AdmitEdges = ownedEdgeGrant
	s.Verify = nil
	errorCode(t, s, "edge_list", object{"db": "ideas", "id": "ID-x"}, "unavailable")
	s = edgeService(t)
	for _, typ := range []string{"bogus", "", "RELATED"} {
		errorCode(t, s, "edge_add", edgeInput("ID-y", typ, nil), "bad_request")
	}
	for _, in := range []object{edgeInput(" ID-y", "related", nil), edgeInput("ID-y", "related", -1), edgeInput("ID-y", "related", 1.5), {"from": object{"db": "wrong", "id": "ID-x"}, "to": object{"id": "ID-y"}, "type": "related"}, {"from": object{"db": "ideas", "id": "ID-x"}, "to": object{"id": "ID-y", "authority": true}, "type": "related"}} {
		errorCode(t, s, "edge_add", in, "bad_request")
	}
	spoof := edgeInput("ID-y", "related", nil)
	spoof["cohort"] = object{"allAllowed": true}
	errorCode(t, s, "edge_add", spoof, "bad_request")
	var missing EdgeResource
	s.AdmitEdges = func(_ context.Context, _ Caller, _ string, _ object, cohort EdgeCohort) (Authority, error) {
		missing = cohort.Resources[0]
		return Authority{}, errors.New("missing resource denied")
	}
	errorCode(t, s, "edge_list", object{"db": "ideas", "id": "ID-missing"}, "unavailable")
	if missing.ID != "ID-missing" || missing.Exists || missing.RequestedDB != "ideas" {
		t.Fatalf("missing fact%+v", missing)
	}
	errorCode(t, s, "edge_backlinks", object{"id": "EXT-missing"}, "unavailable")
}

func TestEdgeNoopCASPreservationAndExplicitTarget(t *testing.T) {
	s := edgeService(t)
	before := exported(t, s)
	got := itemCall(t, s, "edge_add", edgeInput("DEC-external", "informs", 0))
	if got["changed"] != false {
		t.Fatal("existing representation changed")
	}
	if !reflect.DeepEqual(before, exported(t, s)) {
		t.Fatal("noop modified import/envelopes")
	}
	snap, err := storage.ParseSnapshot(before)
	if err != nil {
		t.Fatal(err)
	}
	if changed, importErr := s.Store.Import(t.Context(), snap); importErr != nil || changed {
		t.Fatal("noop marked receipt mutated", importErr)
	}
	got = itemCall(t, s, "edge_add", edgeInput("EXT-owned", "depends_on", 0))
	item := got["item"].(object)
	if got["changed"] != true || item["rev"] != json.Number("1") || item["updated"] != "2026-10-08" {
		t.Fatal(got)
	}
	before = exported(t, s)
	errorCode(t, s, "edge_add", edgeInput("EXT-owned", "depends_on", 0), "conflict")
	if !reflect.DeepEqual(before, exported(t, s)) {
		t.Fatal("stale noop changed state")
	}
	got = itemCall(t, s, "edge_remove", edgeInput("EXT-two", "related", 1))
	if got["changed"] != false || !reflect.DeepEqual(before, exported(t, s)) {
		t.Fatal("absent removal changed state")
	}
	explicit := edgeInput("EXT-two", "related", 1)
	explicit["to"].(object)["db"] = "ideas"
	errorCode(t, s, "edge_add", explicit, "not_found")
	explicit = edgeInput("ID-y", "related", 1)
	explicit["to"].(object)["db"] = "decisions"
	errorCode(t, s, "edge_add", explicit, "not_found")
	explicit["to"].(object)["db"] = "ideas"
	got = itemCall(t, s, "edge_add", explicit)
	if got["item"].(object)["rev"] != json.Number("2") {
		t.Fatal(got)
	}
	files := exported(t, s)
	after, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Store.Verify(t.Context(), after); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeRemoveAllRepresentationsAndOrdering(t *testing.T) {
	s := edgeService(t)
	if err := s.Store.Transact(t.Context(), func(state *storage.State) error {
		item := state.Envelopes["ideas"]["items"].([]any)[0].(object)
		item["workstream_ids"] = []any{"WS-one", "WS-one"}
		item["links"] = []any{object{"kind": "workstream", "ref": "WS-one", "unknown": true}, object{"kind": "url", "ref": "https://example.test"}}
		item["supersedes"] = "ID-y"
		metadata := item["_portfolio_relationships"].(object)
		metadata["extra"] = object{"preserve": nil}
		metadata["edges"] = append(metadata["edges"].([]any), object{"type": "belongs_to", "target": "WS-one", "custom": nil}, object{"type": "decision_gates", "target": "DEC-003"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := currentItem(t, s, "ideas", "ID-x")
	oldUnknown := clone(before["unknown"])
	oldMetadata := clone(before["_portfolio_relationships"].(object)["extra"])
	got := itemCall(t, s, "edge_remove", edgeInput("WS-one", "belongs_to", 0))
	item := got["item"].(object)
	if item["rev"] != json.Number("1") || len(item["workstream_ids"].([]any)) != 0 || len(item["links"].([]any)) != 1 || !reflect.DeepEqual(item["unknown"], oldUnknown) || !reflect.DeepEqual(item["_portfolio_relationships"].(object)["extra"], oldMetadata) || item["supersedes"] != "ID-y" {
		t.Fatal(item)
	}
	state, err := s.Store.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	outgoing, err := state.Relationships("ID-x", false, "")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := s.Store.ListEdges(t.Context(), "ID-x", "")
	if err != nil || !reflect.DeepEqual(outgoing, persisted) {
		t.Fatal("outgoing diverged", err)
	}
	back, err := state.Relationships("ID-y", true, "")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.Store.Backlinks(t.Context(), "ID-y", "")
	if err != nil || !reflect.DeepEqual(back, stored) {
		t.Fatal("backlinks diverged", err)
	}
	gateIDs := call(t, s, "decision_gates", object{"db": "ideas", "id": "ID-x"})
	expect(t, gateIDs, []any{"DEC-003"})
	got = itemCall(t, s, "edge_remove", edgeInput("ID-y", "supersedes", 1))
	if _, exists := got["item"].(object)["supersedes"]; exists {
		t.Fatal("scalar supersedes survived removal")
	}
}

func TestEdgeExactCohortGrantsDetachedCallbacksAndRevocation(t *testing.T) {
	for _, kind := range []string{"target", "source", "existing-reference", "backlink-source", "principal", "read-revoked", "conflict-revoked", "postcommit", "detached"} {
		t.Run(kind, func(t *testing.T) {
			s := edgeService(t)
			input := edgeInput("ID-y", "related", 0)
			name := "edge_add"
			if kind == "read-revoked" {
				name = "edge_list"
				input = object{"db": "ideas", "id": "ID-x"}
			}
			if kind == "conflict-revoked" {
				input["rev"] = 2
			}
			if kind == "backlink-source" {
				name = "edge_backlinks"
				input = object{"id": "DEC-external"}
			}
			calls := 0
			observed := []EdgeCohort{}
			s.AdmitEdges = func(ctx context.Context, caller Caller, op string, in object, cohort EdgeCohort) (Authority, error) {
				calls++
				observed = append(observed, cohort.detached())
				a, err := ownedEdgeGrant(ctx, caller, op, in, cohort)
				for _, resource := range cohort.Resources {
					if kind == "target" && resource.ID == "ID-y" || kind == "source" && resource.Role == "source" || kind == "existing-reference" && resource.ID == "DEC-external" || kind == "backlink-source" && resource.Role == "backlink_source" {
						return Authority{}, errors.New("denied exact resource")
					}
				}
				if kind == "principal" {
					a.Principal = "other"
				}
				if (kind == "read-revoked" || kind == "conflict-revoked") && calls >= 2 || kind == "postcommit" && calls >= 3 {
					return Authority{}, errors.New("revoked")
				}
				if kind == "detached" {
					cohort.Resources[0].ID = "forged"
					if len(cohort.Edges) > 0 {
						cohort.Edges[0].ToID = "forged"
					}
					in["from"] = object{"db": "ideas", "id": "forged"}
				}
				return a, err
			}
			if kind == "detached" {
				itemCall(t, s, name, input)
				for _, cohort := range observed {
					if cohort.Resources[0].ID == "forged" {
						t.Fatal("cohort alias")
					}
				}
				return
			}
			err := errorCode(t, s, name, input, "unavailable")
			if len(err.Details) != 0 {
				t.Fatal("revocation disclosed CAS/item")
			}
			current := currentItem(t, s, "ideas", "ID-x")
			want := json.Number("0")
			if kind == "postcommit" {
				want = json.Number("1")
			}
			rev, _ := current["rev"].(json.Number)
			if rev == "" {
				rev = "0"
			}
			if rev != want {
				t.Fatal("wrong effect semantics", rev, want)
			}
		})
	}
}

func TestEdgeBatchEvolvingFactsRollbackAndRetainedRemoval(t *testing.T) {
	s := edgeService(t)
	s.Admit = func(_ context.Context, caller Caller, _ string, _ object) (Authority, error) {
		return Authority{Principal: "verified-test", Verified: caller.Binding == "test-binding", Allowed: true}, nil
	}
	observedLocal := false
	s.AdmitEdges = func(ctx context.Context, caller Caller, name string, in object, cohort EdgeCohort) (Authority, error) {
		for _, r := range cohort.Resources {
			if r.ID == "ID-created" && r.DB == "ideas" && r.Exists {
				observedLocal = true
			}
		}
		return ownedEdgeGrant(ctx, caller, name, in, cohort)
	}
	result := itemCall(t, s, "batch", object{"entries": []any{batchEntry("create", object{"db": "ideas", "item": object{"id": "ID-created", "title": "Created in same TX", "kind": "idea"}}), batchEntry("edge_add", edgeInput("ID-created", "related", 0)), batchEntry("edge_remove", edgeInput("ID-created", "related", 1))}})
	if !observedLocal || result["results"].([]any)[1].(object)["item"].(object)["rev"] != json.Number("1") || currentItem(t, s, "ideas", "ID-x")["rev"] != json.Number("2") {
		t.Fatal("batch did not use evolving State/CAS")
	}
	before := exported(t, s)
	errorCode(t, s, "batch", object{"entries": []any{batchEntry("edge_add", edgeInput("EXT-owned", "related", 2)), batchEntry("edge_remove", edgeInput("EXT-owned", "related", 2))}}, "conflict")
	if !reflect.DeepEqual(before, exported(t, s)) {
		t.Fatal("batch CAS did not rollback")
	}
	// Added then removed: authority must still cover the removed target at
	// final disclosure. Revoke only when a captured pre-removal edge is checked.
	calls := 0
	s.AdmitEdges = func(ctx context.Context, caller Caller, name string, in object, cohort EdgeCohort) (Authority, error) {
		calls++
		if calls >= 3 {
			for _, r := range cohort.Resources {
				if r.ID == "EXT-owned" {
					return Authority{}, errors.New("removed target grant revoked")
				}
			}
		}
		return ownedEdgeGrant(ctx, caller, name, in, cohort)
	}
	errorCode(t, s, "batch", object{"entries": []any{batchEntry("edge_add", edgeInput("EXT-owned", "related", 2)), batchEntry("edge_remove", edgeInput("EXT-owned", "related", 3))}}, "unavailable")
	if !reflect.DeepEqual(before, exported(t, s)) {
		t.Fatal("retained cohort refusal did not rollback")
	}
}

func TestEdgeOutputBoundRollsBack(t *testing.T) {
	s := edgeService(t)
	if err := s.Store.Transact(t.Context(), func(state *storage.State) error {
		state.Envelopes["ideas"]["items"].([]any)[0].(object)["unknown-large"] = strings.Repeat("x", MaxResultBytes)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := exported(t, s)
	errorCode(t, s, "edge_add", edgeInput("EXT-owned", "related", 0), "unavailable")
	if !reflect.DeepEqual(before, exported(t, s)) {
		t.Fatal("oversized result committed edge")
	}
}

func TestEdgeAuthorityAndCohortResolveAfterWriterWait(t *testing.T) {
	s := edgeService(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	locked, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- s.Store.Transact(ctx, func(state *storage.State) error {
			// A relationship added while the edge request waits must appear in
			// its actual cohort, not an earlier independent read.
			item := state.Envelopes["ideas"]["items"].([]any)[0].(object)
			item["related_ids"] = []any{"EXT-two"}
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-locked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	preflight := make(chan struct{})
	var checks atomic.Int32
	original := s.Verify
	s.Verify = func(ctx context.Context, caller Caller, name string) (Authority, error) {
		a, err := original(ctx, caller, name)
		if checks.Add(1) == 1 {
			close(preflight)
		}
		return a, err
	}
	var sawAfterWait atomic.Bool
	s.AdmitEdges = func(ctx context.Context, caller Caller, name string, in object, cohort EdgeCohort) (Authority, error) {
		for _, r := range cohort.Resources {
			if r.ID == "EXT-two" {
				sawAfterWait.Store(true)
				return Authority{}, errors.New("new waited-for reference not granted")
			}
		}
		return ownedEdgeGrant(ctx, caller, name, in, cohort)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := s.Call(ctx, Caller{"test-binding"}, "edge_add", edgeInput("ID-y", "related", 0))
		finished <- err
	}()
	select {
	case <-preflight:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if sawAfterWait.Load() {
		t.Fatal("cohort used uncommitted/independent snapshot")
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	var domain *Error
	if err := <-finished; !errors.As(err, &domain) || domain.Code != "unavailable" {
		t.Fatal(err)
	}
	if !sawAfterWait.Load() {
		t.Fatal("did not admit actual post-wait State")
	}
	item := currentItem(t, s, "ideas", "ID-x")
	if _, bumped := item["rev"]; bumped {
		t.Fatal("refused edge committed")
	}
}

func TestEdgeTypedWaitRechecksOrdinaryAndCancellation(t *testing.T) {
	for _, change := range []string{"revoke", "cancel", "principal"} {
		for _, mode := range []string{"write", "read", "stale", "batch"} {
			t.Run(change+"/"+mode, func(t *testing.T) {
				s := edgeService(t)
				before := exported(t, s)
				var grant, calls atomic.Int32
				s.Verify = func(context.Context, Caller, string) (Authority, error) {
					a := Authority{Principal: "verified-test", Verified: true, Allowed: true}
					if grant.Load() == 1 {
						a.Allowed = false
					}
					if grant.Load() == 2 {
						a.Principal = "changed"
					}
					return a, nil
				}
				s.Admit = func(ctx context.Context, caller Caller, name string, _ object) (Authority, error) {
					return s.Verify(ctx, caller, name)
				}
				entered, release := make(chan struct{}), make(chan struct{})
				blockAt := int32(1)
				if mode == "batch" {
					blockAt = 2
				}
				s.AdmitEdges = func(ctx context.Context, c Caller, n string, in object, co EdgeCohort) (Authority, error) {
					if calls.Add(1) == blockAt {
						close(entered)
						<-release
					}
					return ownedEdgeGrant(ctx, c, n, in, co)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				name, in := "edge_add", edgeInput("ID-y", "related", 0)
				switch mode {
				case "read":
					name, in = "edge_list", object{"db": "ideas", "id": "ID-x"}
				case "stale":
					in = edgeInput("ID-y", "related", 99)
				case "batch":
					name, in = "batch", object{"entries": []any{batchEntry("edge_add", edgeInput("ID-y", "related", 0)), batchEntry("edge_remove", edgeInput("ID-y", "related", 1))}}
				}
				type outcome struct {
					result any
					err    error
				}
				done := make(chan outcome, 1)
				go func() { result, err := s.Call(ctx, Caller{"test-binding"}, name, in); done <- outcome{result, err} }()
				select {
				case <-entered:
				case <-ctx.Done():
					close(release)
					t.Fatal("verifier not reached")
				}
				switch change {
				case "revoke":
					grant.Store(1)
				case "principal":
					grant.Store(2)
				case "cancel":
					cancel()
				}
				close(release)
				select {
				case got := <-done:
					var typed *Error
					if got.result != nil || !errors.As(got.err, &typed) || typed.Code != "unavailable" {
						t.Fatalf("disclosure %#v %v", got.result, got.err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("call blocked")
				}
				if !reflect.DeepEqual(before, exported(t, s)) {
					t.Fatal("refusal committed")
				}
			})
		}
	}
}
