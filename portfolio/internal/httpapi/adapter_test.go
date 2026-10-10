package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

// This fixture is generated test data, never a copied operator corpus.
func fixtureService(t *testing.T) operations.Service {
	t.Helper()
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "shadow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	files := map[string][]byte{}
	items := map[string]string{
		"priorities": `[]`, "workstreams": `[]`, "roadmap": `[]`, "risks": `[]`,
		"ideas":     `[{"id":"ID-one","title":"One","kind":"idea","status":"new","rev":2,"unknown":{"null":null,"number":9007199254740993,"array":[true,4]},"comments":[{"id":"c-9","author":"historical","text":"needle","created":"2020-01-01T00:00:00Z"}],"links":[{"kind":"url","ref":"https://example.test"}],"related_ids":["ID-two"],"_portfolio_relationships":{"version":1,"edges":[{"type":"informs","target":"DEC-external"}]}},{"id":"ID-two","title":"Two","kind":"idea","status":"new"}]`,
		"decisions": `[{"id":"DEC-001","title":"Choice","status":"needs-decision","options":[{"id":"a","label":"A"}]}]`,
		"inbox":     `[{"id":"IN-001","title":"Inbox","kind":"topic","status":"new","body":"inbox body","added_by":"historical"}]`,
	}
	for db, data := range items {
		files[db] = []byte(`{"schema":"portfolio/` + db + `@1","updated":"2020-01-01","items":` + data + `}`)
	}
	snapshot, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Import(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	return operations.Service{Store: store, Now: func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }}
}

func fixtureVerifier(_ context.Context, identity json.RawMessage, _ string, _ map[string]any) (operations.Authority, error) {
	// Opaque test handles are known ONLY to this injected fixture verifier.
	switch string(identity) {
	case `"fixture-a"`:
		return operations.Authority{Principal: "principal-a", Verified: true, Allowed: true}, nil
	case `"fixture-b"`:
		return operations.Authority{Principal: "principal-b", Verified: true, Allowed: true}, nil
	default:
		return operations.Authority{}, errors.New("private verifier failure")
	}
}
func request(name, body string) subprocess.HTTPRequest {
	return subprocess.HTTPRequest{Method: http.MethodPost, Path: Prefix + name, Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(body), Identity: json.RawMessage(`"fixture-a"`)}
}
func response(t *testing.T, adapter *Adapter, req subprocess.HTTPRequest, status int) any {
	t.Helper()
	got, err := adapter.HTTPHandle(t.Context(), req)
	if err != nil || got.Status != status {
		t.Fatalf("%s: status %d want %d: %s, err=%v", req.Path, got.Status, status, got.Body, err)
	}
	if got.Headers["Cache-Control"] != "no-store" || !strings.HasPrefix(got.Headers["Content-Type"], "application/json") {
		t.Fatal("unsafe response headers", got.Headers)
	}
	d := json.NewDecoder(bytes.NewReader(got.Body))
	d.UseNumber()
	var value any
	if err = d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
func resultObject(t *testing.T, adapter *Adapter, name, body string, status int) map[string]any {
	t.Helper()
	return response(t, adapter, request(name, body), status).(map[string]any)
}
func assertError(t *testing.T, value any, code string) {
	t.Helper()
	if value.(map[string]any)["error"].(map[string]any)["code"] != code {
		t.Fatal(value)
	}
}

type upstreamFunc func(context.Context, string, map[string]any) (any, error)

func (f upstreamFunc) Read(ctx context.Context, name string, input map[string]any) (any, error) {
	return f(ctx, name, input)
}

func TestEachRouteDispatchesItsDomainContract(t *testing.T) {
	cases := []struct {
		name, input, key string
		want             any
		status           int
	}{
		{"databases", `{}`, "", nil, 200},
		{"list", `{"db":"ideas","filters":{"id":"ID-two"}}`, "", nil, 200},
		{"get", `{"db":"ideas","id":"ID-one"}`, "title", "One", 200},
		{"search", `{"q":"needle"}`, "", nil, 200},
		{"schema", `{"db":"ideas"}`, "required", nil, 200},
		{"contract", `{}`, "", nil, 200},
		{"create", `{"db":"ideas","item":{"title":"Created","kind":"idea","author":"forged"}}`, "author", "principal-a", 201},
		{"update", `{"db":"ideas","id":"ID-one","rev":2,"patch":{"title":"Changed"}}`, "title", "Changed", 200},
		{"comment", `{"db":"ideas","id":"ID-one","text":"New","author":"forged"}`, "rev", json.Number("3"), 200},
		{"link", `{"from":{"db":"ideas","id":"ID-two"},"to":{"db":"ideas","id":"ID-one"}}`, "rev", json.Number("1"), 200},
		{"unlink", `{"from":{"db":"ideas","id":"ID-one"},"to":{"db":"ideas","id":"ID-two"}}`, "rev", json.Number("3"), 200},
		{"link_add", `{"db":"ideas","id":"ID-one","link":{"kind":"file","ref":"/unread/pointer"}}`, "rev", json.Number("3"), 200},
		{"link_remove", `{"db":"ideas","id":"ID-one","kind":"url","ref":"https://example.test"}`, "rev", json.Number("3"), 200},
		{"reorder", `{"db":"ideas","ids":["ID-two","ID-one"]}`, "", nil, 200},
		{"decide", `{"id":"DEC-001","option":"a","author":"forged"}`, "decided_by", "principal-a", 200},
		{"defer", `{"id":"DEC-001","note":"Later","author":"forged"}`, "status", "deferred", 200},
		{"reopen", `{"id":"DEC-001"}`, "status", "needs-decision", 200},
		{"inbox_add", `{"title":"New inbox","added_by":"forged"}`, "added_by", "principal-a", 201},
		{"inbox_promote", `{"id":"IN-001","to_db":"ideas","fields":{"kind":"idea","author":"forged"}}`, "item", nil, 200},
		{"inbox_dismiss", `{"id":"IN-001","note":"No","author":"forged"}`, "status", "dismissed", 200},
		{"board", `{"limit":2,"active_hours":2,"recent_hours":72}`, "notices", nil, 200},
		{"torque_task", `{"id":"CW-20261010-0001"}`, "operation", "torque_task", 200},
		{"torque_tasks", `{"limit":2,"include_total":true,"cursor":"next"}`, "operation", "torque_tasks", 200},
		{"torque_titles", `{"ids":["CW-20261010-0001"]}`, "operation", "torque_titles", 200},
		{"torque_projects", `{"status":"active"}`, "operation", "torque_projects", 200},
		{"torque_epics", `{"project_id":"PRJ-20261010-0001"}`, "operation", "torque_epics", 200},
		{"torque_sprints", `{"epic_id":"EP-20261010-0001"}`, "operation", "torque_sprints", 200},
		{"torque_facets", `{"dimensions":["status"]}`, "operation", "torque_facets", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fixtureService(t)
			if strings.HasPrefix(tc.name, "torque_") {
				s.Torque = upstreamFunc(func(_ context.Context, name string, in map[string]any) (any, error) {
					return map[string]any{"operation": name, "input": in}, nil
				})
			}
			a := New(s, fixtureVerifier)
			got := response(t, a, request(tc.name, tc.input), tc.status)
			if tc.key != "" {
				value, exists := got.(map[string]any)[tc.key]
				if !exists || tc.want != nil && !reflect.DeepEqual(value, tc.want) {
					t.Fatal(got)
				}
			} else if _, ok := got.([]any); !ok {
				t.Fatalf("expected array: %v", got)
			}
			// An invalid JSON root is rejected on every transport route, including
			// no-input discovery, before it reaches a domain handler.
			assertError(t, response(t, a, request(tc.name, `[]`), 400), "bad_request")
			assertError(t, response(t, New(s, nil), request(tc.name, tc.input), 503), "unavailable")
		})
	}
}

func TestPerRouteSchemasAndMethodCapabilityAdmission(t *testing.T) {
	a := New(fixtureService(t), fixtureVerifier)
	for _, route := range Routes() {
		t.Run(route.Operation.Name, func(t *testing.T) {
			if route.Declaration.Method != "POST" || route.Declaration.Path != Prefix+route.Operation.Name {
				t.Fatal(route)
			}
			want := "view"
			if route.Operation.Write {
				want = "draft"
			}
			if route.Operation.Name == "decide" || route.Operation.Name == "defer" || route.Operation.Name == "reopen" {
				want = "resolve"
			}
			if route.Declaration.Capability != want {
				t.Fatal(route)
			}
			for _, method := range []string{"GET", "HEAD", "PATCH", "DELETE", "PUT"} {
				req := request(route.Operation.Name, `{}`)
				req.Method = method
				assertError(t, response(t, a, req, 405), "bad_request")
			}
			// Required field types and omissions are validated by the registry.
			props := route.Operation.Input["properties"].(map[string]any)
			if required := route.Operation.Input["required"].([]any); len(required) > 0 {
				key := required[0].(string)
				if props[key] == nil {
					t.Fatal("required field has no schema", key)
				}
				assertError(t, response(t, a, request(route.Operation.Name, `{}`), 400), "bad_request")
			}
		})
	}
	for _, path := range []string{Prefix + "migrate", Prefix + "unknown", Prefix + "get/ID-one", "/api/plugins/other/operations/get"} {
		req := request("get", `{}`)
		req.Path = path
		assertError(t, response(t, a, req, 404), "not_found")
	}
}

func TestBodyCarrierAndContentTypeLimits(t *testing.T) {
	a := New(fixtureService(t), fixtureVerifier)
	for _, body := range []string{``, `null`, `"text"`, `true`, `[]`, `{} {}`, `{"db":"ideas","db":"risks"}`, `{"db":"ideas","filters":{"id":"ID-one","id":"ID-two"}}`, strings.Repeat("[", 66) + strings.Repeat("]", 66)} {
		assertError(t, response(t, a, request("list", body), 400), "bad_request")
	}
	for _, contentType := range []string{"", "text/plain", "application/jsonp", "application/json; broken"} {
		req := request("get", `{"db":"ideas","id":"ID-one"}`)
		req.Headers["Content-Type"] = contentType
		assertError(t, response(t, a, req, 415), "bad_request")
	}
	req := request("get", `{"db":"ideas","id":"ID-one"}`)
	req.Headers["Content-Type"] = "application/json; charset=utf-8"
	response(t, a, req, 200)
	for _, change := range []func(*subprocess.HTTPRequest){
		func(r *subprocess.HTTPRequest) { r.Query = map[string]string{"id": "ID-two"} },
		func(r *subprocess.HTTPRequest) { r.RawQuery = "id=ID-one&id=ID-two" },
		func(r *subprocess.HTTPRequest) { r.RawPath = Prefix + "%67et" },
		func(r *subprocess.HTTPRequest) { r.SessionID = "claimed" },
		func(r *subprocess.HTTPRequest) { r.Headers["content-type"] = "application/json" },
	} {
		req = request("get", `{"db":"ideas","id":"ID-one"}`)
		change(&req)
		assertError(t, response(t, a, req, 400), "bad_request")
	}
	base := `{"db":"ideas","id":"ID-one"}`
	req = request("get", base+strings.Repeat(" ", MaxBodyBytes-len(base)))
	response(t, a, req, 200)
	req.Body = append(req.Body, ' ')
	assertError(t, response(t, a, req, 413), "bad_request")
	for _, headers := range []map[string]string{
		{"Content-Type": "", "content-type": "application/json"},
		{"Content-Type": "application/json", "content-type": ""},
	} {
		req = request("get", base)
		req.Headers = headers
		assertError(t, response(t, a, req, 400), "bad_request")
	}
}

func TestCallerGrantsAreExactAndLabelsNeverAuthorize(t *testing.T) {
	s := fixtureService(t)
	before, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	verify := func(ctx context.Context, identity json.RawMessage, op string, in map[string]any) (operations.Authority, error) {
		a, verifyErr := fixtureVerifier(ctx, identity, op, in)
		a.Allowed = op == "get" && in["db"] == "ideas" && in["id"] == "ID-one"
		return a, verifyErr
	}
	a := New(s, verify)
	response(t, a, request("get", `{"db":"ideas","id":"ID-one"}`), 200)
	for _, test := range []struct{ op, input string }{
		{"get", `{"db":"ideas","id":"ID-two","scope":"allowed"}`},
		{"list", `{"db":"ideas","filters":{"id":"ID-one"}}`},
		{"search", `{"q":"needle"}`},
		{"board", `{}`},
		{"contract", `{}`},
		{"comment", `{"db":"ideas","id":"ID-one","text":"forged","author":"principal-a"}`},
		{"decide", `{"id":"DEC-001","option":"a","author":"Architect"}`},
	} {
		assertError(t, response(t, a, request(test.op, test.input), 503), "unavailable")
	}
	for _, identity := range []string{"", `null`, `{bad}`, `"Architect"`, `{"principal":"principal-a","verified":true,"allowed":true}`, `"synthetic-upstream-credential"`} {
		req := request("get", `{"db":"ideas","id":"ID-one","author":"Architect"}`)
		req.Identity = json.RawMessage(identity)
		got := response(t, a, req, 503)
		assertError(t, got, "unavailable")
		if strings.Contains(fmt.Sprint(got), "private verifier") {
			t.Fatal("raw verifier error leaked")
		}
	}
	for _, header := range []string{"X-Client", "Authorization", "Cookie", "X-Caller", "Origin", "Host"} {
		req := request("get", `{"db":"ideas","id":"ID-one"}`)
		req.Headers[header] = "Architect"
		assertError(t, response(t, a, req, 400), "bad_request")
		req.Identity = nil
		assertError(t, response(t, a, req, 503), "unavailable")
	}
	after, err := s.Store.Export(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("denied request mutated shadow", err)
	}
	// A write operation grant does not confer a decision operation grant.
	a = New(s, func(ctx context.Context, id json.RawMessage, op string, in map[string]any) (operations.Authority, error) {
		auth, err := fixtureVerifier(ctx, id, op, in)
		auth.Allowed = op == "comment"
		return auth, err
	})
	resultObject(t, a, "comment", `{"db":"ideas","id":"ID-one","text":"allowed"}`, 200)
	assertError(t, response(t, a, request("decide", `{"id":"DEC-001","option":"a"}`), 503), "unavailable")
}

func TestAttributionCASAndLosslessNoop(t *testing.T) {
	s := fixtureService(t)
	a := New(s, fixtureVerifier)
	created := resultObject(t, a, "create", `{"db":"ideas","item":{"title":"New","kind":"idea","author":"forged","added_by":"forged","decided_by":"forged"}}`, 201)
	for _, key := range []string{"author", "added_by", "decided_by"} {
		if created[key] != "principal-a" {
			t.Fatal(created)
		}
	}
	current := resultObject(t, a, "get", `{"db":"ideas","id":"ID-one"}`, 200)
	conflict := resultObject(t, a, "update", `{"db":"ideas","id":"ID-one","rev":1,"patch":{"title":"forged"}}`, 409)
	assertError(t, conflict, "conflict")
	if !reflect.DeepEqual(conflict["error"].(map[string]any)["details"].(map[string]any)["current"], current) {
		t.Fatal("CAS omitted complete current item", conflict)
	}
	before, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	linked := resultObject(t, a, "link", `{"from":{"db":"ideas","id":"ID-one"},"to":{"db":"ideas","id":"ID-two"}}`, 200)
	after, err := s.Store.Export(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(linked, current) {
		t.Fatal("idempotent link changed state", err)
	}
	updated := resultObject(t, a, "update", `{"db":"ideas","id":"ID-one","rev":2,"patch":{"author":null,"added_by":"forged","decided_by":"forged","extra":null,"labels":[1,null],"number":9007199254740993}}`, 200)
	for _, key := range []string{"author", "added_by", "decided_by"} {
		if updated[key] != "principal-a" {
			t.Fatal(updated)
		}
	}
	if updated["number"] != json.Number("9007199254740993") || updated["comments"].([]any)[0].(map[string]any)["author"] != "historical" || !reflect.DeepEqual(updated["unknown"], current["unknown"]) {
		t.Fatal("lossless/provenance regression", updated)
	}
	if _, has := updated["extra"]; has {
		t.Fatal("null patch did not delete field")
	}
	comment := resultObject(t, a, "comment", `{"db":"ideas","id":"ID-one","text":"spoof attempt","author":"Architect"}`, 200)
	if comment["comments"].([]any)[1].(map[string]any)["author"] != "principal-a" {
		t.Fatal(comment)
	}
	promoted := resultObject(t, a, "inbox_promote", `{"id":"IN-001","to_db":"ideas","fields":{"kind":"idea","author":"forged","added_by":"forged","decided_by":"forged"}}`, 200)
	for _, key := range []string{"author", "added_by", "decided_by"} {
		if promoted["item"].(map[string]any)[key] != "principal-a" {
			t.Fatal(promoted)
		}
	}
	decision := resultObject(t, a, "decide", `{"id":"DEC-001","option":"a","author":"Architect","comment":"Choice"}`, 200)
	if decision["decided_by"] != "principal-a" {
		t.Fatal(decision)
	}
	if _, err = s.Store.Export(t.Context()); err != nil {
		t.Fatal("projection verification failed", err)
	}
}

func TestRevocationDiscardsReadAndConflictDetails(t *testing.T) {
	for _, op := range []string{"torque_task", "update"} {
		t.Run(op, func(t *testing.T) {
			s := fixtureService(t)
			var checks int
			verify := func(ctx context.Context, id json.RawMessage, name string, input map[string]any) (operations.Authority, error) {
				checks++
				auth, err := fixtureVerifier(ctx, id, name, input)
				if checks > 1 && op == "torque_task" || checks > 2 {
					auth.Allowed = false
				}
				return auth, err
			}
			s.Torque = upstreamFunc(func(context.Context, string, map[string]any) (any, error) {
				return map[string]any{"private": "protected-result"}, nil
			})
			input := `{"id":"CW-20261010-0001"}`
			if op == "update" {
				input = `{"db":"ideas","id":"ID-one","rev":1,"patch":{}}`
			}
			got := response(t, New(s, verify), request(op, input), 503)
			assertError(t, got, "unavailable")
			if strings.Contains(fmt.Sprint(got), "protected-result") || strings.Contains(fmt.Sprint(got), "One") {
				t.Fatal("revoked data disclosed", got)
			}
		})
	}
}

func TestPostCommitRevocationWithholdsResultButDoesNotRollback(t *testing.T) {
	s := fixtureService(t)
	checks := 0
	verify := func(ctx context.Context, id json.RawMessage, op string, input map[string]any) (operations.Authority, error) {
		checks++
		authority, verifyErr := fixtureVerifier(ctx, id, op, input)
		// Initial admission and the check inside the writer transaction pass.
		// The final disclosure check observes revocation after commit.
		authority.Allowed = checks < 3
		return authority, verifyErr
	}
	got := response(t, New(s, verify), request("comment", `{"db":"ideas","id":"ID-one","text":"committed-before-revocation"}`), 503)
	assertError(t, got, "unavailable")
	if strings.Contains(fmt.Sprint(got), "committed-before-revocation") || strings.Contains(fmt.Sprint(got), "historical") {
		t.Fatal("post-commit refusal disclosed result", got)
	}
	// Use an independently admitted read to observe the explicitly documented
	// limitation. HTTP 503 in this case is not rollback or safe retry evidence.
	item := resultObject(t, New(s, fixtureVerifier), "get", `{"db":"ideas","id":"ID-one"}`, 200)
	found := false
	for _, value := range item["comments"].([]any) {
		comment := value.(map[string]any)
		if comment["text"] == "committed-before-revocation" {
			found = true
			if comment["author"] != "principal-a" {
				t.Fatal(comment)
			}
		}
	}
	if !found || item["rev"] != json.Number("3") {
		t.Fatal("committed effect missing", item)
	}
}

func TestUnverifiedStaleAndChangingPrincipalsRefuse(t *testing.T) {
	s := fixtureService(t)
	for _, authority := range []operations.Authority{
		{Principal: "asserted", Verified: false, Allowed: true},
		{Principal: "stale-or-revoked", Verified: true, Allowed: false},
		{Principal: "", Verified: true, Allowed: true},
	} {
		verify := func(context.Context, json.RawMessage, string, map[string]any) (operations.Authority, error) {
			return authority, nil
		}
		for _, op := range []string{"get", "comment", "decide"} {
			assertError(t, response(t, New(s, verify), request(op, `{}`), 503), "unavailable")
		}
	}
	checks := 0
	verify := func(context.Context, json.RawMessage, string, map[string]any) (operations.Authority, error) {
		checks++
		return operations.Authority{Principal: fmt.Sprintf("principal-%d", checks), Verified: true, Allowed: true}, nil
	}
	assertError(t, response(t, New(s, verify), request("get", `{"db":"ideas","id":"ID-one"}`), 503), "unavailable")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := New(s, fixtureVerifier).HTTPHandle(ctx, request("get", `{"db":"ideas","id":"ID-one"}`))
	if err != nil || got.Status != 503 {
		t.Fatal(got, err)
	}
}

func TestMutationRechecksAuthorityAfterWriterWait(t *testing.T) {
	s := fixtureService(t)
	locked, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- s.Store.Transact(t.Context(), func(*storage.State) error { close(locked); <-release; return nil })
	}()
	<-locked
	var revoked atomic.Bool
	var once sync.Once
	initial := make(chan struct{})
	verify := func(ctx context.Context, id json.RawMessage, op string, input map[string]any) (operations.Authority, error) {
		auth, err := fixtureVerifier(ctx, id, op, input)
		auth.Allowed = !revoked.Load()
		once.Do(func() { close(initial) })
		return auth, err
	}
	a := New(s, verify)
	done := make(chan subprocess.HTTPResponse, 1)
	go func() {
		got, _ := a.HTTPHandle(t.Context(), request("comment", `{"db":"ideas","id":"ID-one","text":"must not commit"}`))
		done <- got
	}()
	<-initial
	revoked.Store(true)
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.Status != 503 {
		t.Fatalf("post-wait authority accepted: %d %s", got.Status, got.Body)
	}
	files, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(files["ideas"], []byte("must not commit")) {
		t.Fatal("revoked mutation committed")
	}
}

func TestDetachedRequestBindingAndConcurrentAuthors(t *testing.T) {
	s := fixtureService(t)
	verify := func(ctx context.Context, id json.RawMessage, op string, input map[string]any) (operations.Authority, error) {
		auth, err := fixtureVerifier(ctx, id, op, input)
		// A verifier cannot modify domain input or the next check's carrier.
		input["text"] = "verifier mutation"
		id[0] = 'x'
		return auth, err
	}
	a := New(s, verify)
	var wg sync.WaitGroup
	errs := make(chan string, 2)
	for _, handle := range []string{"fixture-a", "fixture-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := request("comment", `{"db":"ideas","id":"ID-one","text":"`+handle+`"}`)
			req.Identity = json.RawMessage(`"` + handle + `"`)
			got, err := a.HTTPHandle(t.Context(), req)
			if err != nil || got.Status != 200 {
				errs <- fmt.Sprintf("%d %s %v", got.Status, got.Body, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	item := resultObject(t, a, "get", `{"db":"ideas","id":"ID-one"}`, 200)
	authors := map[string]string{}
	for _, v := range item["comments"].([]any) {
		c := v.(map[string]any)
		authors[c["text"].(string)] = c["author"].(string)
	}
	if authors["fixture-a"] != "principal-a" || authors["fixture-b"] != "principal-b" || authors["needle"] != "historical" {
		t.Fatal("request identity mixed", authors)
	}
}

func TestTypedPagingBatchAndSafeUpstreamFailures(t *testing.T) {
	s := fixtureService(t)
	var inputs []map[string]any
	s.Torque = upstreamFunc(func(_ context.Context, name string, in map[string]any) (any, error) {
		inputs = append(inputs, in)
		if name == "torque_titles" {
			return map[string]any{"CW-20261010-0001": nil}, nil
		}
		return map[string]any{"items": []any{}, "meta": map[string]any{"has_more": true, "next_cursor": "next", "total": 100}}, nil
	})
	a := New(s, fixtureVerifier)
	got := resultObject(t, a, "torque_tasks", `{"limit":2,"offset":3,"include_total":true,"project_id":"PRJ-20261010-0001"}`, 200)
	if got["meta"].(map[string]any)["has_more"] != true || got["meta"].(map[string]any)["next_cursor"] != "next" || inputs[0]["limit"] != json.Number("2") || inputs[0]["offset"] != json.Number("3") || inputs[0]["include_total"] != true {
		t.Fatal(got, inputs)
	}
	batch := resultObject(t, a, "torque_titles", `{"ids":["CW-20261010-0001"]}`, 200)
	if value, exists := batch["CW-20261010-0001"]; !exists || value != nil {
		t.Fatal(batch)
	}
	assertError(t, response(t, a, request("torque_tasks", `{"limit":201}`), 400), "bad_request")
	ids := []string{}
	for i := 0; i < 101; i++ {
		ids = append(ids, fmt.Sprintf("CW-20261010-%04d", i))
	}
	raw, _ := json.Marshal(map[string]any{"ids": ids})
	assertError(t, response(t, a, request("torque_titles", string(raw)), 400), "bad_request")
	s.Torque = upstreamFunc(func(context.Context, string, map[string]any) (any, error) {
		return nil, errors.New("private credential/raw upstream response")
	})
	a = New(s, fixtureVerifier)
	failed := response(t, a, request("torque_task", `{"id":"CW-20261010-0001"}`), 502)
	assertError(t, failed, "unavailable")
	if strings.Contains(fmt.Sprint(failed), "private credential") {
		t.Fatal(failed)
	}
	assertError(t, response(t, a, request("get", `{"db":"ideas","id":"ID-missing"}`), 404), "not_found")
	assertError(t, response(t, a, request("create", `{"db":"ideas","item":{"title":"invalid kind","kind":"nope"}}`), 400), "invalid")
}
