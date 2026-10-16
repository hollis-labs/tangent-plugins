package ptrack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/httpapi"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

func TestOwnedAdapterAttributionRefusalAndScopePassThrough(t *testing.T) {
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "generated-shadow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	files := map[string][]byte{}
	for _, db := range []string{"priorities", "workstreams", "roadmap", "ideas", "decisions", "risks", "inbox"} {
		items := `[]`
		if db == "ideas" {
			items = `[{"id":"ID-owned","title":"Owned synthetic idea","kind":"idea","status":"new","rev":0,"unknown":{"n":9007199254740993,"nil":null}}]`
		}
		files[db] = []byte(`{"schema":"portfolio/` + db + `@1","updated":"2020-01-01","items":` + items + `}`)
	}
	snapshot, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Import(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	verifier := func(_ context.Context, identity json.RawMessage, name string, input map[string]any) (operations.Authority, error) {
		// Test-owned courier and exact one-item cohort. No production issuer or
		// credential loader exists; CLI input/labels cannot select this binding.
		if string(identity) != `"owned-fixture"` || input["db"] != "ideas" {
			return operations.Authority{}, errors.New("refused")
		}
		if name != "list" && (name != "comment" && name != "get" || input["id"] != "ID-owned") {
			return operations.Authority{}, errors.New("refused")
		}
		return operations.Authority{Principal: "test-principal", Verified: true, Allowed: true}, nil
	}
	adapter := httpapi.New(operations.Service{Store: store}, verifier)
	for _, carrier := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned injected carrier", false: "host missing carrier"}[carrier], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				identity := json.RawMessage(nil)
				if carrier {
					identity = json.RawMessage(`"owned-fixture"`)
				}
				if r.Header.Get("Identity") != "" || r.Header.Get("Authorization") != "" {
					t.Error("forged courier")
				}
				response, callErr := adapter.HTTPHandle(r.Context(), subprocess.HTTPRequest{Method: r.Method, Path: r.URL.Path, Headers: map[string]string{"Content-Type": r.Header.Get("Content-Type")}, Body: raw, Identity: identity})
				if callErr != nil {
					t.Error(callErr)
				}
				w.WriteHeader(response.Status)
				w.Write(response.Body)
			}))
			defer server.Close()
			client, _ := newHTTPClient(server.URL)
			result, callErr := client.Call(t.Context(), "comment", json.RawMessage(`{"db":"ideas","id":"ID-owned","text":"Synthetic","author":"operator-label"}`))
			if carrier {
				if callErr != nil || !strings.Contains(string(result), `"author":"test-principal"`) || strings.Contains(string(result), "operator-label") {
					t.Fatalf("attribution %s %v", result, callErr)
				}
				if !strings.Contains(string(result), "9007199254740993") {
					t.Fatal("output token rounded")
				}
				_, callErr = client.Call(t.Context(), "list", json.RawMessage(`{"db":"ideas","scope":{"kind":"project","id":"msg://project/test/a"}}`))
				var typed *Error
				if !errors.As(callErr, &typed) || typed.Code != "unsupported" {
					t.Fatalf("main scope broadened: %v", callErr)
				}
			} else {
				var typed *Error
				if !errors.As(callErr, &typed) || typed.Code != "unavailable" {
					t.Fatalf("missing identity admitted: %v", callErr)
				}
			}
		})
	}
	// Real registry/storage edge round trips use only a test-owned typed courier.
	for _, bound := range []bool{false, true} {
		var edgeVerifier httpapi.VerifyEdges
		if bound {
			edgeVerifier = func(_ context.Context, identity json.RawMessage, _ string, _ map[string]any, cohort operations.EdgeCohort) (operations.Authority, error) {
				if string(identity) != `"owned-fixture"` || len(cohort.Resources) == 0 {
					return operations.Authority{}, errors.New("unowned")
				}
				for _, resource := range cohort.Resources {
					if resource.ID != "ID-owned" && resource.ID != "EXT-owned" {
						return operations.Authority{}, errors.New("resource grant absent")
					}
				}
				return operations.Authority{Principal: "test-principal", Verified: true, Allowed: true}, nil
			}
		}
		ordinary := func(_ context.Context, identity json.RawMessage, name string, _ map[string]any) (operations.Authority, error) {
			if string(identity) != `"owned-fixture"` || name != "edge_add" && name != "edge_remove" && name != "edge_list" && name != "edge_backlinks" && name != "decision_gates" {
				return operations.Authority{}, errors.New("operation refused")
			}
			return operations.Authority{Principal: "test-principal", Verified: true, Allowed: true}, nil
		}
		edges := httpapi.NewWithEdges(operations.Service{Store: store}, ordinary, edgeVerifier)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Error(readErr)
				return
			}
			response, callErr := edges.HTTPHandle(r.Context(), subprocess.HTTPRequest{Method: r.Method, Path: r.URL.Path, Headers: map[string]string{"Content-Type": r.Header.Get("Content-Type")}, Body: raw, Identity: json.RawMessage(`"owned-fixture"`)})
			if callErr != nil {
				t.Error(callErr)
				return
			}
			w.WriteHeader(response.Status)
			w.Write(response.Body)
		}))
		client, clientErr := newHTTPClient(server.URL)
		if clientErr != nil {
			t.Fatal(clientErr)
		}
		for _, args := range [][]string{
			{"edge", "add", "ideas:ID-owned", "EXT-owned", "--type", "related", "--rev", "1"},
			{"edge", "list", "ideas:ID-owned"},
			{"edge", "backlinks", "EXT-owned"},
			{"edge", "decision-gates", "ideas:ID-owned"},
			{"edge", "remove", "ideas:ID-owned", "EXT-owned", "--type", "related", "--rev", "2"},
		} {
			parsed, parseErr := parse(args)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			name, input, mapErr := plan(parsed)
			if mapErr != nil {
				t.Fatal(mapErr)
			}
			raw, marshalErr := json.Marshal(input)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			result, callErr := client.Call(t.Context(), name, raw)
			if bound {
				if callErr != nil || len(result) == 0 {
					t.Fatalf("edge round trip %s: %s %v", name, result, callErr)
				}
			} else {
				var typed *Error
				if !errors.As(callErr, &typed) || typed.Code != "unavailable" {
					t.Fatalf("unbound edge admitted %s %v", result, callErr)
				}
			}
		}
		server.Close()
	}
	state, err := store.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	item := state.Envelopes["ideas"]["items"].([]any)[0].(map[string]any)
	if item["rev"] != json.Number("3") {
		t.Fatal("default-refused call changed fixture")
	}
}
