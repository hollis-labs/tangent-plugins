package projects

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func testClient(t *testing.T, f fakeTransport) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: "http://127.0.0.1:8123", SecretRef: "test-owned", Epoch: "fixture-epoch", Resolve: func(context.Context, string) (string, error) { return "private-fixture-bearer", nil }, Transport: f})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRegistryAllowlistAndExplicitMappings(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/registry/projects" || r.URL.RawQuery != "include=external_ids" || r.Header.Get("Authorization") != "Bearer private-fixture-bearer" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("missing deadline")
		}
		return response(200, `{"projects":[{"urn":"msg://project/demo/a","kind":"project","display_name":"Demo","status":"active","external_ids":[{"substrate":"torque","external_id":"PRJ-20261009-0001","attached_at":"2026-10-09"},{"substrate":"torque","external_id":"PRJ-20261009-0002"},{"substrate":"torque","external_id":"bad"}],"callback":{"target":"private"},"Props":{"secret":"private"}}]}`), nil
	})
	dir, err := c.Directory(t.Context())
	if err != nil || !dir.Evidence.Complete || len(dir.Projects) != 1 {
		t.Fatalf("directory: %+v %v", dir, err)
	}
	p := dir.Projects[0]
	if len(p.TorqueIDs()) != 2 || len(p.ExternalIDs) != 3 {
		t.Fatal("collapsed unresolved/ambiguous mapping")
	}
}
func TestRefusalAndSafeFailures(t *testing.T) {
	for _, endpoint := range []string{"http://example.invalid", "https://user:secret@example.invalid", "https://example.invalid/?secret=x", "file:///private", "https://example.invalid/path"} {
		if _, err := New(Config{Endpoint: endpoint, SecretRef: "fixture", Epoch: "fixture-epoch", Resolve: func(context.Context, string) (string, error) { return "x", nil }}); !errors.Is(err, Invalid) {
			t.Fatalf("accepted %s: %v", endpoint, err)
		}
	}
	if _, err := New(Config{}); !errors.Is(err, Missing) {
		t.Fatal(err)
	}
	for _, test := range []struct {
		code int
		body string
		want error
	}{{401, "secret upstream", Denied}, {403, "private", Denied}, {404, "private", NotFound}, {500, "secret", Unavailable}, {200, `{"projects":null}`, Malformed}, {200, `{"projects":[]} {}`, Malformed}, {200, `{"projects":[{"urn":"msg://project/a","kind":"agent"}]}`, Malformed}, {200, `{"projects":[{"urn":"msg://project/a","kind":"project"},{"urn":"msg://project/a","kind":"project"}]}`, Malformed}, {302, "", Unavailable}} {
		calls := 0
		c := testClient(t, func(*http.Request) (*http.Response, error) {
			calls++
			r := response(test.code, test.body)
			r.Header.Set("Location", "https://other.invalid/steal")
			return r, nil
		})
		_, err := c.Directory(t.Context())
		if !errors.Is(err, test.want) || calls != 1 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("failure %v calls=%d", err, calls)
		}
	}
	c := testClient(t, func(*http.Request) (*http.Response, error) { return response(200, strings.Repeat("x", 100)), nil })
	c.maxBody = 10
	if _, err := c.Directory(t.Context()); !errors.Is(err, TooLarge) {
		t.Fatal(err)
	}
	c.resolve = func(context.Context, string) (string, error) { return "", errors.New("private-token-file") }
	if _, err := c.Directory(t.Context()); !errors.Is(err, Missing) {
		t.Fatal(err)
	}
}
func TestTasksScopePaginationAndBudget(t *testing.T) {
	calls := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if q.Get("project_id") != "PRJ-20261009-0001" || q.Get("tag") != "team & work" || q.Get("limit") != "200" {
			t.Fatalf("unscoped request %s", r.URL)
		}
		if calls == 1 {
			return response(200, `{"items":[{"id":"CW-20261009-0001","project_id":"PRJ-20261009-0001","tags":[{"slug":"team & work"}]}],"meta":{"has_more":true,"next_cursor":"a+/=","total":3}}`), nil
		}
		if q.Get("cursor") != "a+/=" {
			t.Fatal("cursor encoding")
		}
		return response(200, `{"items":[{"id":"CW-20261009-0002","project_id":"PRJ-20261009-0001","tags":[{"slug":"team & work"}]}],"meta":{"has_more":false,"total":3}}`), nil
	})
	page, err := c.Tasks(t.Context(), "PRJ-20261009-0001", "team & work")
	if err != nil || !page.Evidence.Complete || len(page.Tasks) != 2 || *page.Evidence.Total != 3 {
		t.Fatalf("page %+v %v", page, err)
	}
	calls = 0
	c.maxPages = 1
	page, err = c.Tasks(t.Context(), "PRJ-20261009-0001", "team & work")
	if err != nil || !page.Evidence.Partial || page.Evidence.NextCursor != "a+/=" {
		t.Fatal("budget claims complete")
	}
	if _, err = c.Tasks(t.Context(), "", ""); !errors.Is(err, Invalid) {
		t.Fatal("unscoped query accepted")
	}
	c = testClient(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"items":[{"id":"CW-20261009-0001","project_id":"PRJ-20261009-0002"}],"meta":{"has_more":false}}`), nil
	})
	if _, err = c.Tasks(t.Context(), "PRJ-20261009-0001", ""); !errors.Is(err, Malformed) {
		t.Fatal("cross-scope result accepted")
	}
}
func TestCancellationAndProjectIDValidation(t *testing.T) {
	calls := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(time.Second):
			t.Fatal("deadline not bound")
		}
		return nil, nil
	})
	c.timeout = time.Millisecond
	if _, err := c.Directory(t.Context()); !errors.Is(err, Unavailable) {
		t.Fatal(err)
	}
	if _, err := c.Project(t.Context(), "../token"); !errors.Is(err, Invalid) || calls != 1 {
		t.Fatal("invalid ID called transport")
	}
}

func TestEncodedRegistryURNAndPartialEvidence(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.EscapedPath() != "/registry/projects/msg:%2F%2Fproject%2Fexample%2Fa" || r.URL.RawQuery != "include=external_ids" {
			t.Fatalf("URN path %s", r.URL)
		}
		return response(200, `{"urn":"msg://project/example/a","kind":"project"}`), nil
	})
	if _, err := c.RegistryProject(t.Context(), "msg://project/example/a"); err != nil {
		t.Fatal(err)
	}
	c = testClient(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"projects":[],"meta":{"partial":true,"has_more":true,"next_cursor":"retained","total":42}}`), nil
	})
	d, err := c.Directory(t.Context())
	if err != nil || d.Evidence.Complete || !d.Evidence.Partial || d.Evidence.NextCursor != "retained" || *d.Evidence.Total != 42 {
		t.Fatalf("lost partial evidence %+v %v", d, err)
	}
}
func TestEndpointReferenceEpochPartitions(t *testing.T) {
	c := testClient(t, func(*http.Request) (*http.Response, error) { return response(200, `{"projects":[]}`), nil })
	d, err := New(Config{Endpoint: "http://127.0.0.1:8123", SecretRef: "test-owned", Epoch: "replaced", Resolve: func(context.Context, string) (string, error) { return "fixture", nil }})
	if err != nil || c.ScopeKey() == d.ScopeKey() || strings.Contains(c.ScopeKey(), "test-owned") {
		t.Fatal("credential epoch not isolated", err)
	}
}
