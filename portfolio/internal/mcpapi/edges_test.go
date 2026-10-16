package mcpapi

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
)

func edgeOrdinary(_ context.Context, identity json.RawMessage, operation string, _ map[string]any) (operations.Authority, error) {
	if string(identity) != `"synthetic-writer-a"` {
		return operations.Authority{}, errors.New("unknown test courier")
	}
	allow := false
	switch operation {
	case "edge_list", "edge_add", "edge_remove", "edge_backlinks", "decision_gates", "batch":
		allow = true
	}
	return operations.Authority{Principal: "fixture-a", Verified: true, Allowed: allow}, nil
}
func edgeResources(ctx context.Context, identity json.RawMessage, name string, in map[string]any, cohort operations.EdgeCohort) (operations.Authority, error) {
	a, err := edgeOrdinary(ctx, identity, name, in)
	grants := map[string]bool{"ID-one": true, "DEC-001": true, "EXT-owned": true}
	if len(cohort.Resources) == 0 {
		return operations.Authority{}, errors.New("empty cohort")
	}
	for _, resource := range cohort.Resources {
		if !grants[resource.ID] {
			return operations.Authority{}, errors.New("absent resource grant")
		}
	}
	return a, err
}
func edgeArgument(rev int, target string) map[string]any {
	return map[string]any{"from": map[string]any{"db": "ideas", "id": "ID-one"}, "to": map[string]any{"id": target}, "type": "decision_gates", "rev": rev}
}

func TestEdgesMCPDefaultReadRefusalAndTypedTranscript(t *testing.T) {
	s := shadow(t)
	ambientCalls := 0
	s.AdmitEdges = func(context.Context, operations.Caller, string, map[string]any, operations.EdgeCohort) (operations.Authority, error) {
		ambientCalls++
		return operations.Authority{Principal: "fixture-a", Verified: true, Allowed: true}, nil
	}
	for _, name := range []string{"edge_list", "edge_add", "edge_remove", "edge_backlinks", "decision_gates"} {
		input := map[string]any{"db": "ideas", "id": "ID-one"}
		if name == "edge_backlinks" {
			input = map[string]any{"id": "EXT-owned"}
		}
		if name == "edge_add" || name == "edge_remove" {
			input = edgeArgument(0, "EXT-owned")
		}
		result, _ := invoke(t, New(s, edgeOrdinary), name, "synthetic-writer-a", input)
		if !result.IsError {
			t.Fatal("nil edge verifier admitted", name)
		}
	}
	if ambientCalls != 0 {
		t.Fatal("ambient edge authority adopted")
	}
	a := NewWithEdges(s, edgeOrdinary, edgeResources)
	result, value := invoke(t, a, "edge_add", "synthetic-writer-a", edgeArgument(0, "DEC-001"))
	if result.IsError || value.(map[string]any)["item"].(map[string]any)["rev"] != json.Number("1") {
		t.Fatal(string(result.Content))
	}
	result, value = invoke(t, a, "decision_gates", "synthetic-writer-a", map[string]any{"db": "ideas", "id": "ID-one"})
	if result.IsError || !reflect.DeepEqual(value, []any{"DEC-001"}) {
		t.Fatal(value)
	}
	result, value = invoke(t, a, "edge_backlinks", "synthetic-writer-a", map[string]any{"id": "DEC-001"})
	if result.IsError || value.([]any)[0].(map[string]any)["from_id"] != "ID-one" {
		t.Fatal(value)
	}
	result, _ = invoke(t, a, "edge_remove", "synthetic-writer-a", edgeArgument(1, "DEC-001"))
	if result.IsError {
		t.Fatal(string(result.Content))
	}
	result, value = invoke(t, a, "edge_list", "synthetic-writer-a", map[string]any{"db": "ideas", "id": "ID-one"})
	if result.IsError || len(value.([]any)) != 0 {
		t.Fatal(value)
	}
	spoof := edgeArgument(2, "EXT-owned")
	spoof["cohort"] = map[string]any{"allAllowed": true}
	result, _ = invoke(t, a, "edge_add", "synthetic-writer-a", spoof)
	if !result.IsError {
		t.Fatal("body cohort spoof admitted")
	}
	unsafe := edgeArgument(2, "EXT-owned")
	unsafe["rev"] = float64(9007199254740992)
	result, value = invoke(t, a, "edge_add", "synthetic-writer-a", unsafe)
	if !result.IsError || value.(map[string]any)["error"].(map[string]any)["code"] != "invalid" {
		t.Fatal("wire numeric bound lost")
	}
}

func TestEdgeMCPBatchSerialCASAndExactDenialRollback(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(map[bool]string{true: "deny second cohort", false: "success"}[deny], func(t *testing.T) {
			s := shadow(t)
			before, _ := s.Store.Export(t.Context())
			verifierCalls := 0
			verify := func(ctx context.Context, identity json.RawMessage, name string, in map[string]any, cohort operations.EdgeCohort) (operations.Authority, error) {
				verifierCalls++
				if deny && name == "edge_remove" {
					return operations.Authority{}, errors.New("removed target no grant")
				}
				return edgeResources(ctx, identity, name, in, cohort)
			}
			a := NewWithEdges(s, edgeOrdinary, verify)
			input := map[string]any{"entries": []any{map[string]any{"operation": "edge_add", "input": edgeArgument(0, "EXT-owned")}, map[string]any{"operation": "edge_remove", "input": edgeArgument(1, "EXT-owned")}}}
			result, value := invoke(t, a, "batch", "synthetic-writer-a", input)
			if result.IsError != deny || verifierCalls == 0 {
				t.Fatal(value)
			}
			after, _ := s.Store.Export(t.Context())
			if deny {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("denied second edge did not rollback")
				}
			} else {
				results := value.(map[string]any)["results"].([]any)
				if results[0].(map[string]any)["item"].(map[string]any)["rev"] != json.Number("1") || results[1].(map[string]any)["item"].(map[string]any)["rev"] != json.Number("2") {
					t.Fatal("sequential CAS failed")
				}
			}
		})
	}
}
