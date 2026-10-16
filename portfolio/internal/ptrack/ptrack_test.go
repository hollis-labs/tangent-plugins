package ptrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

type clientFunc func(context.Context, string, json.RawMessage) (json.RawMessage, error)

type failedWriter struct{ short bool }

func (w failedWriter) Write(raw []byte) (int, error) {
	if w.short {
		return len(raw) / 2, nil
	}
	return 0, errors.New("private stdout failure")
}

func TestStdoutFailureNeverClaimsSuccessOrRetries(t *testing.T) {
	path := pluginConfig(t, "http://127.0.0.1:1")
	for _, short := range []bool{false, true} {
		for _, helpMode := range []bool{false, true} {
			var stderr bytes.Buffer
			calls := 0
			args := []string{"--config", path, "comment", "ideas", "ID-owned", "Synthetic"}
			if helpMode {
				args = []string{"--help"}
			}
			code := Run(t.Context(), args, Options{Stdout: failedWriter{short: short}, Stderr: &stderr, Client: clientFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
				calls++
				return json.RawMessage(`{"rev":1}`), nil
			})})
			want := 1
			if helpMode {
				want = 0
			}
			if code != 1 || calls != want || strings.Count(stderr.String(), "\n") != 1 || !strings.Contains(stderr.String(), `"code":"unavailable"`) || strings.Contains(stderr.String(), "private") {
				t.Fatalf("exit%d calls%d stderr%q", code, calls, stderr.String())
			}
			if !helpMode && !strings.Contains(stderr.String(), "effects may have committed") {
				t.Fatal("writer failure promised receipt")
			}
		}
	}
}

func (f clientFunc) Call(ctx context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
	return f(ctx, name, raw)
}

func configFile(t *testing.T, config any) string {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func pluginConfig(t *testing.T, base string) string {
	t.Helper()
	return configFile(t, object{"version": 1, "backend": "plugin", "plugin": object{"base_url": base}})
}

func TestCommandContracts(t *testing.T) {
	cases := []struct {
		argv      []string
		operation string
		body      string
	}{
		{[]string{"list", "ideas", "--status", "new", "--field", "status=review", "--field", "tags=\"alpha\"", "--sort", "order"}, "list", `{"db":"ideas","filters":{"status":"review","tags":"alpha"},"sort":"order"}`},
		{[]string{"get", "ideas", "ID-a"}, "get", `{"db":"ideas","id":"ID-a"}`},
		{[]string{"add", "ideas", "--title", "first", "--field", "title=second", "--field", "odd=9007199254740993", "--json", `{"title":"third","nested":{"decimal":1.2500e-3,"empty":null}}`}, "create", `{"db":"ideas","item":{"title":"third","odd":9007199254740993,"nested":{"decimal":1.2500e-3,"empty":null}}}`},
		{[]string{"update", "ideas", "ID-a", "--set", "notes=null", "--set", "x=[true,2]", "--rev", "3"}, "update", `{"db":"ideas","id":"ID-a","patch":{"notes":null,"x":[true,2]},"rev":3}`},
		{[]string{"comment", "ideas", "ID-a", "hello", "world", "--kind", "decision", "--as", "label"}, "comment", `{"db":"ideas","id":"ID-a","text":"hello world","kind":"decision","author":"label"}`},
		{[]string{"link", "ideas:ID-a", "workstreams:WS-b", "--field", "related_ids", "--field", "ignored"}, "link", `{"from":{"db":"ideas","id":"ID-a"},"to":{"db":"workstreams","id":"WS-b"},"field":"related_ids"}`},
		{[]string{"unlink", "ideas:ID-a", "workstreams:WS-b"}, "unlink", `{"from":{"db":"ideas","id":"ID-a"},"to":{"db":"workstreams","id":"WS-b"}}`},
		{[]string{"link-add", "ideas:ID-a", "--kind", "url", "--ref", "https://example.invalid", "--label", "a"}, "link_add", `{"db":"ideas","id":"ID-a","link":{"kind":"url","ref":"https://example.invalid","label":"a"}}`},
		{[]string{"link-remove", "ideas:ID-a", "--kind", "url", "--ref", "https://example.invalid"}, "link_remove", `{"db":"ideas","id":"ID-a","kind":"url","ref":"https://example.invalid"}`},
		{[]string{"reorder", "ideas", "ID-b", "ID-a"}, "reorder", `{"db":"ideas","ids":["ID-b","ID-a"]}`},
		{[]string{"decide", "DEC-a", "--option", "b", "--comment", "why", "--as", "label"}, "decide", `{"id":"DEC-a","option":"b","comment":"why","author":"label"}`},
		{[]string{"defer", "DEC-a", "--note", "later"}, "defer", `{"id":"DEC-a","note":"later"}`},
		{[]string{"reopen", "DEC-a"}, "reopen", `{"id":"DEC-a"}`},
		{[]string{"inbox", "add", "a", "title", "--path", "/pointer", "--body", "text", "--as", "label"}, "inbox_add", `{"title":"a title","path":"/pointer","body":"text","added_by":"label"}`},
		{[]string{"inbox", "promote", "IN-a", "--to", "ideas", "--field", "kind=idea"}, "inbox_promote", `{"id":"IN-a","to_db":"ideas","fields":{"kind":"idea"}}`},
		{[]string{"inbox", "dismiss", "IN-a", "--note", "later"}, "inbox_dismiss", `{"id":"IN-a","note":"later"}`},
		{[]string{"torque", "task", "CW-a"}, "torque_task", `{"id":"CW-a"}`},
		{[]string{"torque", "titles", "CW-a", "CW-b"}, "torque_titles", `{"ids":["CW-a","CW-b"]}`},
		{[]string{"torque", "tasks", "--project", "PRJ-upstream", "--priority", "2", "--limit", "oops", "--offset", "0", "--cursor", "next", "--sort", "updated_at", "--dir", "desc"}, "torque_tasks", `{"project_id":"PRJ-upstream","priority":2,"limit":"oops","offset":0,"cursor":"next","sort_by":"updated_at","sort_dir":"desc"}`},
		{[]string{"torque", "projects", "--status", "active"}, "torque_projects", `{"status":"active"}`},
		{[]string{"torque", "epics", "--project", "PRJ-upstream"}, "torque_epics", `{"project_id":"PRJ-upstream"}`},
		{[]string{"torque", "sprints", "--project", "PRJ-upstream", "--epic", "EP-a"}, "torque_sprints", `{"project_id":"PRJ-upstream","epic_id":"EP-a"}`},
		{[]string{"torque", "facets", "--project", "PRJ-upstream", "--dimensions", "status,tags", "--tags", "a,b", "--tag", "c", "--status", "doing,review"}, "torque_facets", `{"project_id":"PRJ-upstream","dimensions":["status","tags"],"tags":"a,b","tag":"c","status":"doing,review"}`},
		{[]string{"board", "--recent-hours", "2.5", "--limit", "7"}, "board", `{"recent_hours":2.5,"limit":7}`},
		{[]string{"schema", "ideas"}, "schema", `{"db":"ideas"}`},
		{[]string{"databases"}, "databases", `{}`},
		{[]string{"contract"}, "contract", `{}`},
		{[]string{"search", "some", "terms", "--db", "ideas"}, "search", `{"q":"some terms","db":"ideas"}`},
		{[]string{"edge", "list", "ideas:ID-a", "--type", "informs"}, "edge_list", `{"db":"ideas","id":"ID-a","type":"informs"}`},
		{[]string{"edge", "add", "ideas:ID-a", "ID-b", "--type", "related", "--to-db", "ideas", "--rev", "0"}, "edge_add", `{"from":{"db":"ideas","id":"ID-a"},"to":{"db":"ideas","id":"ID-b"},"type":"related","rev":0}`},
		{[]string{"edge", "remove", "ideas:ID-a", "EXT-owned", "--type", "depends_on"}, "edge_remove", `{"from":{"db":"ideas","id":"ID-a"},"to":{"id":"EXT-owned"},"type":"depends_on"}`},
		{[]string{"edge", "backlinks", "EXT-owned", "--type", "related"}, "edge_backlinks", `{"id":"EXT-owned","type":"related"}`},
		{[]string{"edge", "decision-gates", "ideas:ID-a"}, "decision_gates", `{"db":"ideas","id":"ID-a"}`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			a, err := parse(tc.argv)
			if err != nil {
				t.Fatal(err)
			}
			name, body, err := plan(a)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			actual, err := decodeJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := decodeJSON([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if name != tc.operation || !reflect.DeepEqual(actual, expected) {
				t.Fatalf("%s %s; want %s %s", name, raw, tc.operation, tc.body)
			}
		})
	}
}

func TestScopeSelectorsAndLegacyRefusal(t *testing.T) {
	for _, argv := range [][]string{{"list", "ideas", "--project", "msg://project/test/a"}, {"search", "a", "--workstream", "WS-a"}, {"board", "--project", "msg://project/test/a"}, {"torque", "tasks", "--project", "PRJ-upstream", "--scope-project", "msg://project/test/a"}} {
		a, _ := parse(argv)
		name, body, err := plan(a)
		if err != nil {
			t.Fatal(err)
		}
		if body["scope"] == nil {
			t.Fatal("scope dropped")
		}
		if name == "torque_tasks" && body["project_id"] != "PRJ-upstream" {
			t.Fatal("upstream filter lost")
		}
		if err = rejectNodeScope(a); err == nil {
			t.Fatal("legacy ignored scope")
		}
	}
	for _, argv := range [][]string{{"list", "ideas", "--project", "label"}, {"list", "ideas", "--project", "msg://project/test/a", "--workstream", "WS-a"}, {"get", "ideas", "ID-a", "--workstream", "WS-a"}, {"list", "ideas", "--scope-project", "msg://project/test/a"}, {"torque", "task", "CW-a", "--scope-project", "msg://project/test/a"}} {
		a, _ := parse(argv)
		if _, _, err := plan(a); err == nil {
			t.Fatalf("accepted %v", argv)
		}
	}
	a, _ := parse([]string{"torque", "tasks", "--project", "PRJ-upstream"})
	if err := rejectNodeScope(a); err != nil {
		t.Fatal(err)
	}
}

func TestRunOutputErrorsAndNumbers(t *testing.T) {
	path := pluginConfig(t, "http://127.0.0.1:1")
	cases := []struct {
		name    string
		result  string
		failure error
		text    bool
		want    string
		exit    int
	}{
		{"get", `{ "id":"ID-a", "extension":9007199254740993,"nil":null }`, nil, false, "{\"id\":\"ID-a\",\"extension\":9007199254740993,\"nil\":null}\n", 0},
		{"get", `{"id":"ID-a"}`, nil, true, "{\n  \"id\": \"ID-a\"\n}\n", 0},
		{"list", `[]`, nil, true, "id  status  priority  order  rev  title\n", 0},
		{"get", "", &Error{Code: "conflict", Message: "stale", Details: object{"current": object{"rev": json.Number("3")}}}, false, "{\"error\":{\"code\":\"conflict\",\"message\":\"stale\",\"details\":{\"current\":{\"rev\":3}}}}\n", 1},
		{"get", "", errors.New("secret raw host URL"), false, "{\"error\":{\"code\":\"unavailable\",\"message\":\"ptrack operation unavailable\",\"details\":{}}}\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := []string{"--config", path, tc.name, "ideas", "ID-a"}
			if tc.text {
				args = append(args, "--text")
			}
			calls := 0
			code := Run(t.Context(), args, Options{Stdout: &out, Stderr: &stderr, Client: clientFunc(func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("no finite deadline")
				}
				return json.RawMessage(tc.result), tc.failure
			})})
			got := out.String()
			if code != 0 {
				got = stderr.String()
				if out.Len() != 0 {
					t.Fatal("failure wrote stdout")
				}
			} else if stderr.Len() != 0 {
				t.Fatal("success wrote stderr")
			}
			if code != tc.exit || got != tc.want || calls != 1 {
				t.Fatalf("code%d out%q err%q calls%d", code, out.String(), stderr.String(), calls)
			}
		})
	}
	var out, stderr bytes.Buffer
	code := Run(t.Context(), []string{"get", "ideas", "ID-a"}, Options{Stdout: &out, Stderr: &stderr})
	if code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), "--config") {
		t.Fatal("missing config did not fail actionably")
	}
}

func TestHTTPFailuresDoNotRetryRedirectOrLeak(t *testing.T) {
	for _, kind := range []string{"authority", "postcommit", "redirect", "html", "typed-looking-host", "disconnect", "oversize", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			var targetCalls atomic.Int32
			redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
			defer redirectTarget.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != operationPrefix+"comment" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("unexpected carrier")
				}
				raw, _ := io.ReadAll(r.Body)
				if string(raw) != `{"text":"x"}` {
					t.Error("unexpected body")
				}
				switch kind {
				case "authority", "postcommit":
					w.WriteHeader(503)
					io.WriteString(w, `{"error":{"code":"unavailable","message":"verified caller authority unavailable","details":{}}}`)
				case "redirect":
					w.Header().Set("Location", redirectTarget.URL)
					w.WriteHeader(307)
				case "html":
					w.WriteHeader(500)
					io.WriteString(w, "<html>secret endpoint token</html>")
				case "typed-looking-host":
					w.WriteHeader(502)
					io.WriteString(w, `{"error":{"code":"host_error","message":"secret endpoint token","details":{}}}`)
				case "disconnect":
					hijacker := w.(http.Hijacker)
					conn, _, _ := hijacker.Hijack()
					conn.Close()
				case "oversize":
					io.WriteString(w, strings.Repeat("x", maxOutput+1))
				}
			}))
			defer server.Close()
			client, err := newHTTPClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if kind == "canceled" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			_, err = client.Call(ctx, "comment", json.RawMessage(`{"text":"x"}`))
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != "unavailable" || strings.Contains(typed.Message, "secret") {
				t.Fatalf("bad failure %v", err)
			}
			expected := int32(1)
			if kind == "canceled" {
				expected = 0
			}
			if calls.Load() != expected || targetCalls.Load() != 0 {
				t.Fatal("retry or redirect")
			}
		})
	}
}

func TestHTTPInputBoundAndAdminExcluded(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if len(raw) != maxInput {
			t.Errorf("input bytes%d", len(raw))
		}
		io.WriteString(w, `{"id":"ID-a"}`)
	}))
	defer server.Close()
	client, _ := newHTTPClient(server.URL)
	exact := json.RawMessage(`{"x":"` + strings.Repeat("a", maxInput-8) + `"}`)
	if len(exact) != maxInput {
		t.Fatal("fixture")
	}
	if _, err := client.Call(t.Context(), "create", exact); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(t.Context(), "create", append(exact, ' ')); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, err := client.Call(t.Context(), "migrate", json.RawMessage(`{}`)); err == nil {
		t.Fatal("admin accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("preflight sent request")
	}
}

func TestLegacyChildForwardingAndExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses the platform shell runtime")
	}
	dir := t.TempDir()
	executable := "/bin/sh"
	script := filepath.Join(dir, "owned-reference-script")
	if err := os.WriteFile(script, []byte("printf '%s\\n' \"$@\"\nIFS= read -r received\nprintf 'stdin:%s\\n' \"$received\"\nprintf 'owned stderr\\n' >&2\nexit 7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := configFile(t, object{"version": 1, "backend": "node", "node": object{"executable": executable, "script": script}})
	var out, stderr bytes.Buffer
	code := Run(t.Context(), []string{"--config", config, "--backend", "node", "comment", "ideas", "ID-a", "hello world", "--as", "label", "--unknown", "ignored"}, Options{Stdin: strings.NewReader("generated input\n"), Stdout: &out, Stderr: &stderr})
	if code != 7 || out.String() != "comment\nideas\nID-a\nhello world\n--as\nlabel\n--unknown\nignored\nstdin:generated input\n" || stderr.String() != "owned stderr\n" {
		t.Fatalf("exit%d out%q err%q", code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	code = Run(t.Context(), []string{"--config", config, "--help"}, Options{Stdout: &out, Stderr: &stderr})
	if code != 7 || out.String() != "--help\nstdin:\n" || stderr.String() != "owned stderr\n" {
		t.Fatal("configured Node help did not delegate")
	}
}

func TestPrimitiveSpreadAndAmbiguousJSON(t *testing.T) {
	fields, err := spreadJSON(`"😀a"`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(fields)
	if err != nil || string(raw) != `{"0":"\ud83d","1":"\ude00","2":"a"}` {
		t.Fatalf("UTF16 spread%s %v", raw, err)
	}
	for _, raw := range []string{`{"x":1,"x":2}`, strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66)} {
		if _, err := spreadJSON(raw); err == nil {
			t.Fatal("ambiguous JSON accepted")
		}
		if _, err := pairs([]string{"x=" + raw}); err == nil {
			t.Fatal("ambiguous field silently became string")
		}
	}
}

func TestRequestDataCannotSelectBackendOrProgram(t *testing.T) {
	path := pluginConfig(t, "http://127.0.0.1:1")
	calls := 0
	var out, stderr bytes.Buffer
	args := []string{"--config", path, "update", "ideas", "ID-a", "--json", `{"backend":"node","node":{"executable":"/unselected/runtime","script":"/unselected/script"}}`, "--as", "/unselected/runtime", "--endpoint", "/unselected/script"}
	code := Run(t.Context(), args, Options{Stdout: &out, Stderr: &stderr, Client: clientFunc(func(_ context.Context, name string, body json.RawMessage) (json.RawMessage, error) {
		calls++
		if name != "update" || !bytes.Contains(body, []byte(`"backend":"node"`)) {
			t.Error("request mapping lost")
		}
		return json.RawMessage(`{"id":"ID-a"}`), nil
	})})
	if code != 0 || calls != 1 || stderr.Len() != 0 {
		t.Fatalf("request selected execution: %d %s", code, stderr.String())
	}
	for _, argv := range [][]string{{"edge", "list", "ideas:ID-a"}, {"edge", "add", "ideas:ID-a", "ID-b", "--type", "related"}} {
		a, _ := parse(argv)
		if err := rejectNodeScope(a); err == nil {
			t.Fatal("Node ignored typed edge command")
		}
	}
}

func TestConfigRefusalAndOverride(t *testing.T) {
	for _, config := range []any{object{"version": 2, "backend": "plugin"}, object{"version": 1, "backend": "plugin", "token": "never-loaded"}, object{"version": 1, "backend": "node", "node": object{"executable": "node", "script": "relative"}}, object{"version": 1, "backend": "plugin", "plugin": object{"base_url": "http://user@example.invalid"}}, object{"version": 1, "backend": "plugin", "plugin": object{"base_url": "https://example.invalid/path"}}} {
		path := configFile(t, config)
		a, _ := parse([]string{"get", "ideas", "ID-a", "--config", path})
		if _, err := loadConfig(a, func(string) string { return "" }); err == nil {
			t.Fatalf("accepted %v", config)
		}
	}
	path := pluginConfig(t, "http://127.0.0.1:1")
	a, _ := parse([]string{"get", "ideas", "ID-a", "--backend", "invalid"})
	if _, err := loadConfig(a, func(string) string { return path }); err == nil {
		t.Fatal("override ignored")
	}
}

func TestDisplayUnicodeAndBoard(t *testing.T) {
	rows := []any{object{"id": "ID-a", "title": "😀é", "status": nil, "priority": json.Number("2")}, object{"id": "ID-b", "title": "b"}}
	if actual := table(rows); actual != "id    status  priority  order  rev  title\nID-a  null    2                     😀é\nID-b                                b" {
		t.Fatalf("table%q", actual)
	}
	raw := `{"generated_at":"2026-10-10T12:00:00.000Z","sections":{"in_flight":[{"id":"CW-a","title":"Test","status":"review","updated":"2026-10-10T10:30:00Z"}],"landed":[],"pre_flight":[],"holds":[]},"totals":{"in_flight":2,"landed":0,"pre_flight":0,"holds":0},"notices":["synthetic outage"]}`
	var out bytes.Buffer
	if err := writeResult(&out, "board", json.RawMessage(raw), true); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"DEPARTURES  2026-10-10T12:00:00.000Z", "IN FLIGHT  1 (+1 more)", "ARRIVING", "2h", "PRE-FLIGHT  0\n  NOTHING QUEUED", "NOTICE  synthetic outage\n"} {
		if !strings.Contains(out.String(), part) {
			t.Fatalf("missing%q in%q", part, out.String())
		}
	}
}

func TestLegacyNumericFlagGrammarPlanning(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"1_0", "null"}, {"0x1p2", "null"}, {"-0x1", "null"}, {"+0x1", "null"},
		{"0xffffffffffffffffffff", "1.2089258196146292e+24"}, {"0b101", "5"}, {"0o77", "63"},
		{"+10", "10"}, {".5", "0.5"}, {"1.", "1"}, {"1e2", "100"}, {" 10 ", "10"}, {"-0", "0"},
		{"Infinity", "null"}, {"+Infinity", "null"}, {"-Infinity", "null"}, {"Inf", "null"}, {"NaN", "null"}, {"", "0"}, {"1e999", "null"},
		{"0b" + strings.Repeat("1", 80), "1.2089258196146292e+24"}, {"0x" + strings.Repeat("f", 300), "null"},
	}
	for _, c := range cases {
		for _, command := range [][]string{{"update", "ideas", "ID-owned", "--rev", c.raw}, {"board", "--recent-hours", c.raw}} {
			t.Run(strings.Join(command, "/"), func(t *testing.T) {
				a, err := parse(command)
				if err != nil {
					t.Fatal(err)
				}
				_, in, err := plan(a)
				if err != nil {
					t.Fatal(err)
				}
				key := "rev"
				if command[0] == "board" {
					key = "recent_hours"
				}
				raw, err := json.Marshal(in[key])
				if err != nil || string(raw) != c.want {
					t.Fatalf("%q => %s want %s: %v", c.raw, raw, c.want, err)
				}
			})
		}
	}
	for _, flag := range []string{"priority", "limit", "offset"} {
		if got := numberFlag("1_0", true); got != "1_0" {
			t.Fatalf("digits-only %s changed: %v", flag, got)
		}
	}
}
