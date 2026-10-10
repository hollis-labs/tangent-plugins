package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
)

func ownedEdgeVerifier(ctx context.Context, identity json.RawMessage, name string, in map[string]any, cohort operations.EdgeCohort) (operations.Authority, error) {
	a, err := fixtureVerifier(ctx, identity, name, in)
	if err != nil {
		return a, err
	}
	grants := map[string]bool{"ID-one": true, "ID-two": true, "DEC-external": true, "https://example.test": true}
	if len(cohort.Resources) == 0 {
		return operations.Authority{}, errors.New("empty grant cohort")
	}
	for _, resource := range cohort.Resources {
		if !grants[resource.ID] {
			return operations.Authority{}, errors.New("resource grant absent")
		}
	}
	return a, nil
}

func TestEdgesHTTPDispatchAndAmbientAuthorityRefusal(t *testing.T) {
	for _, tc := range []struct{ name, body string }{{"edge_list", `{"db":"ideas","id":"ID-one"}`}, {"edge_add", `{"from":{"db":"ideas","id":"ID-one"},"to":{"db":"ideas","id":"ID-two"},"type":"informs","rev":2}`}, {"edge_remove", `{"from":{"db":"ideas","id":"ID-one"},"to":{"id":"ID-two"},"type":"related","rev":2}`}, {"edge_backlinks", `{"id":"ID-two"}`}, {"decision_gates", `{"db":"ideas","id":"ID-one"}`}} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixtureService(t)
			ambientCalls := 0
			s.AdmitEdges = func(context.Context, operations.Caller, string, map[string]any, operations.EdgeCohort) (operations.Authority, error) {
				ambientCalls++
				return operations.Authority{Principal: "principal-a", Verified: true, Allowed: true}, nil
			}
			before, _ := s.Store.Export(t.Context())
			assertError(t, response(t, New(s, fixtureVerifier), request(tc.name, tc.body), 502), "unavailable")
			if ambientCalls != 0 {
				t.Fatal("ambient authority adopted")
			}
			after, _ := s.Store.Export(t.Context())
			if !reflect.DeepEqual(before, after) {
				t.Fatal("default changed store")
			}
			response(t, NewWithEdges(s, fixtureVerifier, ownedEdgeVerifier), request(tc.name, tc.body), 200)
		})
	}
}

func TestEdgesHTTPRequestCohortDenialAndCASWithholding(t *testing.T) {
	for _, mode := range []string{"target", "backlink-source", "empty-target", "principal", "conflict-revoked", "removed-target", "detached"} {
		t.Run(mode, func(t *testing.T) {
			s := fixtureService(t)
			name, body := "edge_remove", `{"from":{"db":"ideas","id":"ID-one"},"to":{"id":"ID-two"},"type":"related","rev":2}`
			if mode == "backlink-source" {
				name, body = "edge_backlinks", `{"id":"ID-two"}`
			}
			if mode == "empty-target" {
				name, body = "edge_backlinks", `{"id":"ungranted-external"}`
			}
			if mode == "conflict-revoked" {
				body = `{"from":{"db":"ideas","id":"ID-one"},"to":{"id":"ID-two"},"type":"related","rev":0}`
			}
			calls := 0
			verify := func(ctx context.Context, identity json.RawMessage, op string, in map[string]any, cohort operations.EdgeCohort) (operations.Authority, error) {
				calls++
				a, err := ownedEdgeVerifier(ctx, identity, op, in, cohort)
				for _, resource := range cohort.Resources {
					if mode == "target" && resource.ID == "ID-two" || mode == "backlink-source" && resource.Role == "backlink_source" || mode == "removed-target" && calls >= 3 && resource.ID == "ID-two" {
						return operations.Authority{}, errors.New("exact grant denied")
					}
				}
				if mode == "principal" {
					a.Principal = "different"
				}
				if mode == "conflict-revoked" && calls >= 2 {
					return operations.Authority{}, errors.New("revoked")
				}
				if mode == "detached" {
					if string(identity) != `"fixture-a"` || cohort.Resources[0].ID != "ID-one" {
						t.Error("aliased courier/cohort")
					}
					identity[0] = 'x'
					cohort.Resources[0].ID = "forged"
					in["from"] = map[string]any{"id": "forged"}
				}
				return a, err
			}
			adapter := NewWithEdges(s, fixtureVerifier, verify)
			status := 502
			if mode == "detached" {
				status = 200
			}
			got := response(t, adapter, request(name, body), status)
			if mode != "detached" {
				assertError(t, got, "unavailable")
				if len(got.(map[string]any)["error"].(map[string]any)["details"].(map[string]any)) != 0 {
					t.Fatal("denied current details disclosed")
				}
			}
			if mode == "removed-target" {
				item := resultObject(t, New(s, fixtureVerifier), "get", `{"db":"ideas","id":"ID-one"}`, 200)
				if item["rev"] != json.Number("3") {
					t.Fatal("postcommit refusal rollback falsely implied")
				}
			}
		})
	}
}
