package torqueclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const taskID = "CW-20261010-0001"
const otherID = "CW-20261010-0002"
const projectID = "PRJ-20261010-0001"

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, v any) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(encoded(v)))), Header: make(http.Header)}
}
func rawResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func syntheticGrant(principal string) Grant {
	return Grant{Principal: principal, Scope: "fixture-scope", ScopeEpoch: "scope-1", CredentialRef: "fixture-ref", CredentialEpoch: "version-1"}
}
func fixtureConfig(transport fakeTransport) Config {
	return Config{Endpoint: "https://torque.invalid/api/v1", Host: "127.0.0.1:8990", Integration: "fixture", Transport: transport, Verify: func(ctx context.Context, proof any, r Request) (Grant, error) {
		if ctx.Err() != nil {
			return Grant{}, ctx.Err()
		}
		p, ok := proof.(string)
		if !ok || p == "" {
			return Grant{}, Denied
		}
		return syntheticGrant(p), nil
	}, Resolve: func(ctx context.Context, ref, epoch string) (string, error) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if ref != "fixture-ref" || epoch != "version-1" {
			return "", Denied
		}
		return "synthetic-bearer", nil
	}}
}
func newClient(t *testing.T, c Config) *Client {
	t.Helper()
	client, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func fixtureTask(id string) object {
	return object{"id": id, "title": "Fixture", "description": "body never cached", "status": "doing", "priority": 2, "project_id": projectID, "epic_id": nil, "sprint_id": nil, "tags": []any{object{"slug": "ws-fixture"}}, "created_at": "2026-10-09T00:00:00Z", "updated_at": "2026-10-10T10:00:00Z", "private": "discard"}
}
func fixturePage(items []any, limit int, more bool, cursor any) object {
	return object{"items": items, "meta": object{"returned": len(items), "limit": limit, "has_more": more, "next_cursor": cursor}}
}
func commentPage(items []any, more bool, cursor any, dir string) object {
	p := fixturePage(items, 200, more, cursor)
	p["meta"].(object)["sort_by"] = "created_at"
	p["meta"].(object)["sort_dir"] = dir
	return p
}
func fixtureComment(id int, entity, created string) object {
	return object{"id": id, "entity_type": "task", "entity_id": entity, "author": "fixture", "content": "private body", "created_at": created, "updated_at": "2027-01-01T00:00:00Z"}
}
func assertFailure(t *testing.T, err error, want Failure) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v want %v", err, want)
	}
}

func TestRefusalAndFiniteValidationBeforeAccess(t *testing.T) {
	var requests, credentials, verifications atomic.Int32
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return response(200, fixtureTask(taskID)), nil
	})
	cfg.Resolve = func(context.Context, string, string) (string, error) { credentials.Add(1); return "fixture", nil }
	cfg.Verify = nil
	c := newClient(t, cfg)
	_, err := c.Read(t.Context(), "proof", "torque_task", object{"id": taskID})
	assertFailure(t, err, Denied)
	_, err = c.LatestCommentActivity(t.Context(), "proof", nil)
	assertFailure(t, err, Denied)
	if requests.Load() != 0 || credentials.Load() != 0 {
		t.Fatal("nil verifier accessed integration")
	}
	cfg.Verify = func(context.Context, any, Request) (Grant, error) {
		verifications.Add(1)
		return syntheticGrant("one"), nil
	}
	c = newClient(t, cfg)
	for _, tt := range []struct {
		name  string
		input object
	}{
		{"comment", object{}}, {"torque_task", object{"id": "../tasks"}}, {"torque_task", object{"id": taskID, "role": "owner"}},
		{"torque_titles", object{"ids": []string{taskID, "bad"}}}, {"torque_tasks", object{"limit": 201}}, {"torque_tasks", object{"limit": 1.5}},
		{"torque_tasks", object{"project_id": "private"}}, {"torque_tasks", object{"status": "doing/private"}}, {"torque_tasks", object{"tags": "bad tag"}},
		{"torque_tasks", object{"sort_by": "private"}}, {"torque_tasks", object{"cursor": "?"}}, {"torque_tasks", object{"cursor": "x", "offset": 0}},
		{"torque_tasks", object{"updated_after": "2026-99-01"}}, {"torque_tasks", object{"include_total": "true"}},
		{"torque_projects", object{"project_id": projectID}}, {"torque_epics", object{"epic_id": "EP-20261010-0001"}},
		{"torque_sprints", object{"q": "widen"}}, {"torque_facets", object{"dimensions": []string{}}}, {"torque_facets", object{"dimensions": []string{"private"}}},
	} {
		_, err = c.Read(t.Context(), "proof", tt.name, tt.input)
		assertFailure(t, err, Invalid)
	}
	_, err = c.LatestCommentActivity(t.Context(), "proof", []string{"bad"})
	assertFailure(t, err, Invalid)
	if requests.Load() != 0 || verifications.Load() != 0 {
		t.Fatal("invalid input reached authority or network")
	}
}

func TestConstructorBoundsAndNoAmbientConfiguration(t *testing.T) {
	base := fixtureConfig(nil)
	for _, edit := range []func(*Config){
		func(c *Config) { c.Endpoint = "" }, func(c *Config) { c.Endpoint = "http://example.invalid/api/v1" }, func(c *Config) { c.Endpoint = "https://user:secret@example.invalid/api/v1" },
		func(c *Config) { c.Endpoint = "https://example.invalid/api/v1?secret=x" }, func(c *Config) { c.Endpoint = "https://example.invalid/other" },
		func(c *Config) { c.Host = "" }, func(c *Config) { c.Host = "localhost\r\nAuthorization: bad" }, func(c *Config) { c.Integration = "" },
		func(c *Config) { c.GETTimeout = 5 * time.Second }, func(c *Config) { c.OperationTimeout = 21 * time.Second }, func(c *Config) { c.MaxPages = 6 },
		func(c *Config) { c.MaxBody = 9 << 20 }, func(c *Config) { c.MaxEntries = 1001 }, func(c *Config) { c.MaxCacheBytes = 17 << 20 }, func(c *Config) { c.CacheTTL = 16 * time.Second },
	} {
		cfg := base
		edit(&cfg)
		_, err := New(cfg)
		assertFailure(t, err, Invalid)
	}
	base.Transport = nil
	c := newClient(t, base)
	if c.http.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("ambient proxy enabled")
	}
	if !errors.Is(c.http.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("redirects enabled")
	}
	if c.getTimeout != 4*time.Second || c.operationTimeout != 20*time.Second || c.ttl != 15*time.Second || cap(c.capacity) != 6 {
		t.Fatal("finite defaults")
	}
}

func TestSevenOperationsRoutesDTOAndHost(t *testing.T) {
	var paths []string
	c := newClient(t, fixtureConfig(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.Host != "127.0.0.1:8990" || r.Header.Get("Authorization") != "Bearer synthetic-bearer" {
			t.Fatal("request contract")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("deadline missing")
		}
		paths = append(paths, r.URL.Path)
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/v1/tasks/" + taskID:
			return response(200, fixtureTask(taskID)), nil
		case "/api/v1/tasks/" + taskID + "/comments":
			return response(200, commentPage([]any{fixtureComment(1, taskID, "2026-10-10T09:00:00Z")}, false, nil, "asc")), nil
		case "/api/v1/tasks":
			if q.Get("fields") != "summary" || q.Get("search") != "hello & world" || q.Get("updated_after") != "2026-10-09T00:00:00Z" {
				t.Fatal("query projection")
			}
			p := fixturePage([]any{fixtureTask(taskID)}, 50, false, nil)
			p["meta"].(object)["total"] = 1
			return response(200, p), nil
		case "/api/v1/projects":
			return response(200, fixturePage([]any{object{"id": projectID, "name": "Project", "status": "active", "secret": "discard"}}, 200, false, nil)), nil
		case "/api/v1/epics":
			return response(200, fixturePage([]any{object{"id": "EP-20261010-0001", "name": "Epic", "status": "active", "project_id": projectID}}, 200, false, nil)), nil
		case "/api/v1/sprints":
			return response(200, fixturePage([]any{object{"id": "SP-20261010-0001", "name": "Sprint", "status": "active", "project_id": projectID, "epic_id": nil, "goal": strings.Repeat("g", 400)}}, 200, false, nil)), nil
		case "/api/v1/tasks/facets":
			if q.Has("limit") || q.Has("fields") || q.Get("dimensions") != "status" {
				t.Fatal("facet query")
			}
			return response(200, object{"matching_count": 1, "dimensions": []any{"status"}, "facets": []any{object{"dimension": "status", "buckets": []any{object{"value": "doing", "count": 1, "private": "discard"}}, "total_distinct": 1, "returned": 1, "truncated": false}}, "private": "discard"}), nil
		}
		t.Fatalf("unexpected route %s", r.URL)
		return nil, nil
	}))
	calls := []struct {
		name  string
		input object
	}{
		{"torque_task", object{"id": taskID}}, {"torque_tasks", object{"status": "doing", "q": "hello & world", "updated_after": "2026-10-09", "include_total": true}},
		{"torque_titles", object{"ids": []string{taskID}}}, {"torque_projects", object{}}, {"torque_epics", object{"project_id": projectID}},
		{"torque_sprints", object{"project_id": projectID}}, {"torque_facets", object{"dimensions": []string{"status"}}},
	}
	for _, tt := range calls {
		out, err := c.Read(t.Context(), "one", tt.name, tt.input)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		s := string(encoded(out))
		if strings.Contains(s, "discard") || strings.Contains(s, "synthetic-bearer") {
			t.Fatal("DTO leak")
		}
		if tt.name == "torque_sprints" && len(out.([]any)[0].(object)["goal"].(string)) != 302 {
			t.Fatal("goal clipping")
		}
	}
	if len(paths) != 8 {
		t.Fatalf("finite routes %v", paths)
	}
}

func TestResourceAdmissionOnMissAndCacheDisclosure(t *testing.T) {
	var denied atomic.Bool
	var requests atomic.Int32
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return response(200, fixturePage([]any{fixtureTask(taskID)}, 50, false, nil)), nil
	})
	base := cfg.Verify
	cfg.Verify = func(ctx context.Context, proof any, r Request) (Grant, error) {
		for _, resource := range r.Resources {
			if resource.Kind == "task" && resource.ProjectID != projectID {
				return Grant{}, Denied
			}
			if denied.Load() {
				return Grant{}, Denied
			}
		}
		return base(ctx, proof, r)
	}
	c := newClient(t, cfg)
	_, err := c.Read(t.Context(), "one", "torque_tasks", object{})
	if err != nil {
		t.Fatal(err)
	}
	denied.Store(true)
	_, err = c.Read(t.Context(), "one", "torque_tasks", object{})
	assertFailure(t, err, Denied)
	if requests.Load() != 1 {
		t.Fatal("cached refusal went upstream")
	}
	foreign := fixtureTask(taskID)
	foreign["project_id"] = "PRJ-20261010-9999"
	cfg.Transport = fakeTransport(func(*http.Request) (*http.Response, error) {
		return response(200, fixturePage([]any{foreign}, 50, false, nil)), nil
	})
	denied.Store(false)
	c = newClient(t, cfg)
	_, err = c.Read(t.Context(), "one", "torque_tasks", object{"project_id": projectID})
	assertFailure(t, err, Malformed)
	_, err = c.Read(t.Context(), "one", "torque_tasks", object{})
	assertFailure(t, err, Denied)
	if len(c.cache) != 0 {
		t.Fatal("refused resource cached")
	}
}

func TestCachePartitionsCloneInvalidationAndByteBounds(t *testing.T) {
	var requests atomic.Int32
	var scope, credential atomic.Int32
	scope.Store(1)
	credential.Store(1)
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return response(200, fixturePage([]any{fixtureTask(taskID)}, 50, false, nil)), nil
	})
	cfg.Verify = func(ctx context.Context, proof any, r Request) (Grant, error) {
		g := syntheticGrant(proof.(string))
		if scope.Load() == 2 {
			g.ScopeEpoch = "scope-2"
		}
		if credential.Load() == 2 {
			g.CredentialEpoch = "credential-2"
		}
		return g, nil
	}
	cfg.Resolve = func(context.Context, string, string) (string, error) { return "synthetic", nil }
	c := newClient(t, cfg)
	read := func(p string) object {
		t.Helper()
		v, err := c.Read(t.Context(), p, "torque_tasks", object{})
		if err != nil {
			t.Fatal(err)
		}
		return v.(object)
	}
	v := read("one")
	v["items"].([]any)[0].(object)["title"] = "tampered"
	if read("one")["items"].([]any)[0].(object)["title"] != "Fixture" {
		t.Fatal("cache alias")
	}
	read("two")
	scope.Store(2)
	read("one")
	credential.Store(2)
	read("one")
	if requests.Load() != 4 {
		t.Fatal("principal/scope/credential partitions")
	}
	c.ClearCache()
	read("one")
	if requests.Load() != 5 {
		t.Fatal("invalidation")
	}
	for _, entry := range c.cache {
		if strings.Contains(string(entry.value), "body never cached") || strings.Contains(string(entry.value), "synthetic") {
			t.Fatal("nonmetadata cached")
		}
	}
	cfg.MaxCacheBytes = 64
	c = newClient(t, cfg)
	read("one")
	read("one")
	if c.cacheBytes > 64 || len(c.cache) != 0 {
		t.Fatal("oversize cache retention")
	}
	// A tiny entry budget bounds distinct caller partitions.
	cfg.MaxCacheBytes = 16 << 20
	cfg.MaxEntries = 1
	c = newClient(t, cfg)
	read("one")
	read("two")
	if len(c.cache) > 1 || c.cacheBytes > c.maxCacheBytes {
		t.Fatal("entry/byte bounds")
	}
}

func TestSafeFailureBodiesAndNoErrorCaching(t *testing.T) {
	for _, tt := range []struct {
		status int
		body   string
		want   Failure
	}{
		{401, "private bearer", Denied}, {403, "private bearer", Denied}, {404, "private bearer", NotFound}, {400, "private bearer", Invalid}, {422, "private bearer", Invalid},
		{302, "private bearer", Unavailable}, {500, "private bearer", Unavailable}, {200, "invalid private bearer", Malformed}, {200, `{"items":[],"meta":{}}`, Malformed}, {200, `{} {}`, Malformed},
	} {
		t.Run(string(tt.want)+http.StatusText(tt.status), func(t *testing.T) {
			var calls int
			cfg := fixtureConfig(func(*http.Request) (*http.Response, error) { calls++; return rawResponse(tt.status, tt.body), nil })
			c := newClient(t, cfg)
			for range 2 {
				_, err := c.Read(t.Context(), "one", "torque_tasks", object{})
				assertFailure(t, err, tt.want)
				if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "torque.invalid") {
					t.Fatal("unsafe error")
				}
			}
			if calls != 2 || len(c.cache) != 0 {
				t.Fatal("failed read cached")
			}
		})
	}
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) { return rawResponse(200, strings.Repeat("x", 100)), nil })
	cfg.MaxBody = 10
	c := newClient(t, cfg)
	_, err := c.Read(t.Context(), "one", "torque_tasks", object{})
	assertFailure(t, err, TooLarge)
}

func TestTitlesAuthorizedMissingAndConcurrentIsolation(t *testing.T) {
	var active, peak atomic.Int32
	cfg := fixtureConfig(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		if id == otherID {
			return rawResponse(404, "private"), nil
		}
		return response(200, fixtureTask(id)), nil
	})
	c := newClient(t, cfg)
	ids := []string{taskID, otherID, "CW-20261010-0003", "CW-20261010-0004", "CW-20261010-0005", "CW-20261010-0006", "CW-20261010-0007", taskID}
	var wg sync.WaitGroup
	for _, principal := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.Read(t.Context(), principal, "torque_titles", object{"ids": ids})
			if err != nil {
				t.Error(err)
				return
			}
			if v.(object)[otherID] != nil || len(v.(object)) != 7 {
				t.Error("titles dedup/missing")
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 6 || peak.Load() < 2 {
		t.Fatalf("global client capacity peak=%d", peak.Load())
	}
	base := cfg.Verify
	cfg.Verify = func(ctx context.Context, proof any, r Request) (Grant, error) {
		for _, resource := range r.Resources {
			if resource.ID == otherID {
				return Grant{}, Denied
			}
		}
		return base(ctx, proof, r)
	}
	c = newClient(t, cfg)
	_, err := c.Read(t.Context(), "one", "torque_titles", object{"ids": []string{otherID}})
	assertFailure(t, err, Denied)
}

func TestGETDeadlineCapacityAndCurrentGrantRechecks(t *testing.T) {
	var requests atomic.Int32
	var epoch atomic.Int32
	epoch.Store(1)
	cfg := fixtureConfig(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return response(200, fixtureTask(taskID)), nil
	})
	cfg.GETTimeout = 25 * time.Millisecond
	cfg.Verify = func(context.Context, any, Request) (Grant, error) {
		g := syntheticGrant("one")
		if epoch.Load() == 2 {
			g.ScopeEpoch = "changed"
		}
		return g, nil
	}
	c := newClient(t, cfg)
	for range 6 {
		c.capacity <- struct{}{}
	}
	_, err := c.Read(t.Context(), "one", "torque_task", object{"id": taskID})
	assertFailure(t, err, Unavailable)
	if requests.Load() != 0 {
		t.Fatal("capacity deadline did not bound GET")
	}
	// A grant change while waiting must refuse before credential resolution/send.
	go func() { time.Sleep(5 * time.Millisecond); epoch.Store(2); <-c.capacity }()
	_, err = c.Read(t.Context(), "one", "torque_task", object{"id": taskID})
	assertFailure(t, err, Denied)
	if requests.Load() != 0 {
		t.Fatal("stale grant sent")
	}
	for len(c.capacity) > 0 {
		<-c.capacity
	}
	epoch.Store(1)
	cfg.Resolve = func(context.Context, string, string) (string, error) { epoch.Store(2); return "synthetic", nil }
	c = newClient(t, cfg)
	_, err = c.Read(t.Context(), "one", "torque_task", object{"id": taskID})
	assertFailure(t, err, Denied)
	if requests.Load() != 0 {
		t.Fatal("credential resolution changed authority")
	}
}

type contextBody struct{ ctx context.Context }

func (b contextBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b contextBody) Close() error             { return nil }
func TestBodyAndWholeOperationDeadlines(t *testing.T) {
	cfg := fixtureConfig(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: contextBody{r.Context()}, Header: make(http.Header)}, nil
	})
	cfg.GETTimeout = 15 * time.Millisecond
	c := newClient(t, cfg)
	_, err := c.Read(t.Context(), "one", "torque_tasks", object{})
	assertFailure(t, err, Unavailable)
	cfg = fixtureConfig(func(r *http.Request) (*http.Response, error) {
		select {
		case <-time.After(15 * time.Millisecond):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return response(200, fixturePage([]any{object{"id": projectID, "name": "P", "status": "active"}}, 200, true, "next")), nil
	})
	cfg.OperationTimeout = 10 * time.Millisecond
	c = newClient(t, cfg)
	_, err = c.Read(t.Context(), "one", "torque_projects", object{})
	assertFailure(t, err, Unavailable)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.Read(ctx, "one", "torque_projects", object{})
	assertFailure(t, err, Denied)
}

func TestStructuralPaginationNeverReturnsUnmarkedPartial(t *testing.T) {
	for _, scenario := range []string{"complete", "cap", "repeat", "malformed", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			cfg := fixtureConfig(func(r *http.Request) (*http.Response, error) {
				calls++
				id := projectID
				more := true
				var cursor any = "next"
				if calls == 2 {
					id = "PRJ-20261010-0002"
					more = false
					cursor = nil
				}
				if scenario == "repeat" {
					more = true
					cursor = "next"
				}
				if scenario == "duplicate" {
					id = projectID
				}
				p := fixturePage([]any{object{"id": id, "name": "P", "status": "active"}}, 200, more, cursor)
				if scenario == "malformed" {
					delete(p["meta"].(object), "has_more")
				}
				return response(200, p), nil
			})
			if scenario == "cap" {
				cfg.MaxPages = 1
			}
			c := newClient(t, cfg)
			v, err := c.Read(t.Context(), "one", "torque_projects", object{})
			if scenario == "complete" {
				if err != nil || len(v.([]any)) != 2 {
					t.Fatalf("complete %v %v", v, err)
				}
			} else {
				if err == nil || v != nil || len(c.cache) != 0 {
					t.Fatalf("partial escaped: %v %v", v, err)
				}
			}
		})
	}
}

func TestBatchActivityActualWireParsedTimesAndNoBodyRetention(t *testing.T) {
	var calls int
	cfg := fixtureConfig(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v1/comments" || r.URL.Query().Has("entity_id") {
			t.Fatal("batch route")
		}
		q := r.URL.Query()
		var ids []string
		if json.Unmarshal([]byte(q.Get("entity_ids")), &ids) != nil || len(ids) != 2 || q.Get("entity_type") != "task" || q.Get("sort_dir") != "desc" {
			t.Fatal("batch JSON query")
		}
		if calls == 1 {
			return response(200, commentPage([]any{fixtureComment(1, taskID, "2026-10-10T10:30:00Z")}, true, "next", "desc")), nil
		}
		if q.Get("cursor") != "next" {
			t.Fatal("cursor")
		}
		return response(200, commentPage([]any{fixtureComment(2, taskID, "2026-10-10T12:00:00+02:00")}, false, nil, "desc")), nil
	})
	c := newClient(t, cfg)
	a, err := c.LatestCommentActivity(t.Context(), "one", []string{taskID, otherID})
	if err != nil || !a.Complete {
		t.Fatal(a, err)
	}
	if a.LatestByID[taskID].Format(time.RFC3339) != "2026-10-10T10:30:00Z" {
		t.Fatal("lexical/edited timestamp used")
	}
	if _, has := a.LatestByID[otherID]; has {
		t.Fatal("absent comments invented")
	}
	if len(c.cache) != 0 {
		t.Fatal("activity body cached")
	}
}

func TestActivityIncompleteMalformedAndEmptyProof(t *testing.T) {
	for _, scenario := range []string{"cap", "error", "missingmeta", "wrongentity", "wrongtask", "badtime", "badsort", "repeat", "order", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
				calls++
				if scenario == "error" && calls == 2 {
					return rawResponse(500, "private"), nil
				}
				row := fixtureComment(calls, taskID, "2026-10-10T10:00:00Z")
				if scenario == "wrongentity" {
					row["entity_type"] = "project"
				}
				if scenario == "wrongtask" {
					row["entity_id"] = otherID
				}
				if scenario == "badtime" {
					row["created_at"] = "private"
				}
				if scenario == "order" && calls == 2 {
					row["created_at"] = "2026-10-10T11:00:00Z"
				}
				if scenario == "duplicate" {
					row["id"] = 1
				}
				more := calls == 1
				var next any = "next"
				if !more {
					next = nil
				}
				if scenario == "repeat" {
					more = true
					next = "next"
				}
				p := commentPage([]any{row}, more, next, "desc")
				if scenario == "missingmeta" {
					delete(p, "meta")
				}
				if scenario == "badsort" {
					p["meta"].(object)["sort_dir"] = "asc"
				}
				return response(200, p), nil
			})
			if scenario == "cap" {
				cfg.MaxPages = 1
			}
			c := newClient(t, cfg)
			a, err := c.LatestCommentActivity(t.Context(), "one", []string{taskID})
			if err == nil || a.Complete || len(a.LatestByID) != 0 || len(c.cache) != 0 {
				t.Fatalf("unproven quiet: %+v %v", a, err)
			}
		})
	}
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
		t.Fatal("empty cohort requested network")
		return nil, nil
	})
	c := newClient(t, cfg)
	a, err := c.LatestCommentActivity(t.Context(), "one", nil)
	if err != nil || !a.Complete || len(a.LatestByID) != 0 {
		t.Fatal(a, err)
	}
	cfg.Verify = func(context.Context, any, Request) (Grant, error) { return Grant{}, Denied }
	c = newClient(t, cfg)
	_, err = c.LatestCommentActivity(t.Context(), "one", nil)
	assertFailure(t, err, Denied)
}

func TestCacheTTLAndDisclosurePrincipalChange(t *testing.T) {
	var calls int
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
		calls++
		return response(200, fixturePage([]any{}, 50, false, nil)), nil
	})
	cfg.CacheTTL = time.Millisecond
	c := newClient(t, cfg)
	_, err := c.Read(t.Context(), "one", "torque_tasks", object{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	_, err = c.Read(t.Context(), "one", "torque_tasks", object{})
	if err != nil || calls != 2 {
		t.Fatal("TTL", err, calls)
	}
	cfg.Verify = func(ctx context.Context, proof any, r Request) (Grant, error) {
		if r.Phase == "disclosure" {
			return syntheticGrant("other"), nil
		}
		return syntheticGrant("one"), nil
	}
	c = newClient(t, cfg)
	_, err = c.Read(t.Context(), "one", "torque_tasks", object{})
	assertFailure(t, err, Denied)
	if len(c.cache) != 0 {
		t.Fatal("changed principal cached")
	}
}

func TestTitlesMissingNullAndEmptyCoalesceOnlyTitleMap(t *testing.T) {
	for _, mode := range []string{"missing", "null", "empty"} {
		t.Run(mode, func(t *testing.T) {
			cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
				task := fixtureTask(taskID)
				switch mode {
				case "missing":
					delete(task, "title")
				case "null":
					task["title"] = nil
				case "empty":
					task["title"] = ""
				}
				return response(200, task), nil
			})
			c := newClient(t, cfg)
			v, err := c.Read(t.Context(), "one", "torque_titles", object{"ids": []any{taskID}})
			if err != nil || v.(object)[taskID].(object)["title"] != "" {
				t.Fatal("nullable title map", v, err)
			}
			task := fixtureTask(taskID)
			if mode == "empty" {
				task["title"] = ""
			} else {
				delete(task, "title")
			}
			raw, err := decode(encoded(task))
			if err != nil {
				t.Fatal(err)
			}
			compact, _, err := compactTask(raw, false)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "empty" && compact["title"] != nil {
				t.Fatal("compact title null changed")
			}
		})
	}
}

func TestActivityEqualCreationTimeAcrossPagesUsesAscendingID(t *testing.T) {
	for _, ordered := range []bool{true, false} {
		t.Run(map[bool]string{true: "ascending", false: "descending"}[ordered], func(t *testing.T) {
			calls := 0
			cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
				calls++
				id := calls
				if !ordered {
					id = 3 - calls
				}
				var cursor any
				if calls == 1 {
					cursor = "next"
				}
				return response(200, commentPage([]any{fixtureComment(id, taskID, "2026-10-10T10:00:00Z")}, calls == 1, cursor, "desc")), nil
			})
			a, err := newClient(t, cfg).LatestCommentActivity(t.Context(), "one", []string{taskID})
			if ordered {
				if err != nil || !a.Complete {
					t.Fatal(a, err)
				}
			} else {
				if err == nil || a.Complete {
					t.Fatal("invalid equal-time ID ordering accepted")
				}
			}
		})
	}
}
