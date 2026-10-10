package httpapi

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
	"github.com/hollis-labs/tangent-plugins/portfolio/torqueclient"
	"net/http"
	"sync/atomic"
	"testing"
)

type boundaryTransport func(*http.Request) (*http.Response, error)

func (f boundaryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type boundaryEvaluation func(context.Context, string, map[string]any) (any, error)

func (f boundaryEvaluation) Read(ctx context.Context, n string, in map[string]any) (any, error) {
	return f(ctx, n, in)
}

// Exercise actual adapter dispatch: ordinary test authority is allowed, but
// cannot adopt an inherited network factory. Synthetic evaluation stays usable.
func TestRequestsDoNotAdoptInheritedTorqueFactory(t *testing.T) {
	for _, evaluation := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "evaluation"}[evaluation], func(t *testing.T) {
			s := fixtureService(t)
			var factories, network, evaluations atomic.Int32
			client, err := torqueclient.New(torqueclient.Config{
				Endpoint: "https://fixture.invalid/api/v1", Host: "127.0.0.1:8990", Integration: "boundary-fixture",
				Verify: func(context.Context, any, torqueclient.Request) (torqueclient.Grant, error) {
					return torqueclient.Grant{Principal: "fixture", Scope: "test-only", ScopeEpoch: "1", CredentialRef: "test-only", CredentialEpoch: "1"}, nil
				},
				Resolve:   func(context.Context, string, string) (string, error) { return "synthetic", nil },
				Transport: boundaryTransport(func(*http.Request) (*http.Response, error) { network.Add(1); return nil, torqueclient.Unavailable }),
			})
			if err != nil {
				t.Fatal(err)
			}
			s.Admit = func(context.Context, operations.Caller, string, map[string]any) (operations.Authority, error) {
				return operations.Authority{Principal: "fixture", Verified: true, Allowed: true}, nil
			}
			s.BindTorque = func(ctx context.Context, caller operations.Caller, parent string, in map[string]any, a operations.Authority, current operations.CurrentParent) (operations.Upstream, error) {
				factories.Add(1)
				return operations.NewTorqueUpstream(client, "test-proof", current), nil
			}
			if evaluation {
				s.Torque = boundaryEvaluation(func(context.Context, string, map[string]any) (any, error) {
					evaluations.Add(1)
					return map[string]any{"items": []any{}, "meta": map[string]any{"returned": 0, "has_more": false, "next_cursor": nil}}, nil
				})
			}
			verify := func(context.Context, json.RawMessage, string, map[string]any) (operations.Authority, error) {
				return operations.Authority{Principal: "fixture", Verified: true, Allowed: true}, nil
			}
			status := 502
			if evaluation {
				status = 200
			}
			got := resultObject(t, New(s, verify), "torque_tasks", `{"limit":2}`, status)
			if evaluation {
				if _, ok := got["items"]; !ok {
					t.Fatal("evaluation result unavailable", got)
				}
			} else {
				if _, ok := got["error"]; !ok {
					t.Fatal("missing upstream should refuse")
				}
			}
			if factories.Load() != 0 || network.Load() != 0 {
				t.Fatal("request adopted inherited factory/network", factories.Load(), network.Load())
			}
			if evaluation && evaluations.Load() != 1 {
				t.Fatal("synthetic evaluation injection lost")
			}
		})
	}
}
