package mcpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

func shadow(t *testing.T) operations.Service {
	t.Helper()
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "synthetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	files := map[string][]byte{}
	for _, db := range []string{"priorities", "workstreams", "roadmap", "ideas", "decisions", "risks", "inbox"} {
		items := `[]`
		if db == "ideas" {
			items = `[{"id":"ID-one","title":"One","kind":"idea","status":"new","unknown":{"exact":9007199254740993,"null":null},"comments":[{"id":"c-9","author":"historical","text":"old","created":"2020-01-01"}]}]`
		}
		if db == "decisions" {
			items = `[{"id":"DEC-001","title":"Choice","status":"needs-decision","options":[{"id":"a","label":"A"}]}]`
		}
		files[db] = []byte(`{"schema":"portfolio/` + db + `@1","items":` + items + `}`)
	}
	snap, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	return operations.Service{Store: store}
}

// Synthetic handles are owned by this test verifier. Agent labels never grant.
func verifier(_ context.Context, identity json.RawMessage, op string, input map[string]any) (operations.Authority, error) {
	principal := ""
	switch string(identity) {
	case `"synthetic-reader"`:
		principal = "fixture-reader"
	case `"synthetic-writer-a"`:
		principal = "fixture-a"
	case `"synthetic-writer-b"`:
		principal = "fixture-b"
	default:
		return operations.Authority{}, errors.New("unknown synthetic handle")
	}
	allowed := op == "list" || op == "get" || op == "contract" || op == "schema"
	if op == "list" || op == "get" {
		allowed = input["db"] == "ideas"
	}
	if op == "get" {
		allowed = allowed && input["id"] == "ID-one"
	}
	if principal != "fixture-reader" {
		if op == "comment" {
			allowed = input["db"] == "ideas" && input["id"] == "ID-one"
		}
		if op == "decide" {
			allowed = input["id"] == "DEC-001"
		}
	}
	return operations.Authority{Principal: principal, Verified: true, Allowed: allowed}, nil
}

func invoke(t *testing.T, a *Adapter, name, handle string, in map[string]any) (subprocess.MCPCallResult, any) {
	t.Helper()
	identity, _ := json.Marshal(handle)
	result, err := a.MCPCallTool(t.Context(), subprocess.MCPCallRequest{ToolName: Prefix + name, Arguments: in, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(result.Content))
	d.UseNumber()
	var value any
	if err = d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return result, value
}

func TestReaderGrantsExplicitTargetsAndDefaultRefusal(t *testing.T) {
	s := shadow(t)
	a := New(s, verifier)
	for _, handle := range []string{"Architect", "Planner", "owner"} {
		result, _ := invoke(t, a, "get", handle, map[string]any{"db": "ideas", "id": "ID-one", "author": "owner"})
		if !result.IsError {
			t.Fatal("label admitted", handle)
		}
	}
	result, _ := invoke(t, a, "list", "synthetic-reader", map[string]any{"db": "ideas", "page": map[string]any{"limit": 1}})
	if result.IsError {
		t.Fatal(string(result.Content))
	}
	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{"get", map[string]any{"db": "decisions", "id": "DEC-001"}},
		{"get", map[string]any{"db": "ideas", "id": "ID-other"}},
		{"comment", map[string]any{"db": "ideas", "id": "ID-one", "text": "Denied", "author": "owner"}},
		{"decide", map[string]any{"id": "DEC-001", "option": "a"}},
	} {
		result, _ = invoke(t, a, tc.name, "synthetic-reader", tc.input)
		if !result.IsError {
			t.Fatal("read grant widened", tc.name)
		}
	}
	// A verifier on the incoming service cannot bypass this adapter's refusal.
	s.Admit = func(context.Context, operations.Caller, string, map[string]any) (operations.Authority, error) {
		return operations.Authority{Principal: "owner", Verified: true, Allowed: true}, nil
	}
	result, _ = invoke(t, New(s, nil), "get", "synthetic-writer-a", map[string]any{"db": "ideas", "id": "ID-one"})
	if !result.IsError {
		t.Fatal("inherited service verifier admitted")
	}
}

func TestDetachedConcurrentCallersAndRevocation(t *testing.T) {
	s := shadow(t)
	a := New(s, func(ctx context.Context, id json.RawMessage, op string, in map[string]any) (operations.Authority, error) {
		a, err := verifier(ctx, id, op, in)
		// Deliberately mutate copies. It must not change dispatched input or the
		// identity seen by the next verification pass or concurrent request.
		if len(id) > 0 {
			id[0] = 'x'
		}
		in["text"] = "tampered"
		return a, err
	})
	var wg sync.WaitGroup
	for _, handle := range []string{"synthetic-writer-a", "synthetic-writer-b"} {
		wg.Go(func() {
			identity, _ := json.Marshal(handle)
			got, err := a.MCPCallTool(t.Context(), subprocess.MCPCallRequest{ToolName: Prefix + "comment", Identity: identity, Arguments: map[string]any{"db": "ideas", "id": "ID-one", "text": handle}})
			if err != nil || got.IsError {
				t.Errorf("concurrent comment: %s %v", got.Content, err)
			}
		})
	}
	wg.Wait()
	_, value := invoke(t, a, "get", "synthetic-reader", map[string]any{"db": "ideas", "id": "ID-one"})
	comments := value.(map[string]any)["comments"].([]any)
	authors := map[string]string{}
	for _, value := range comments {
		c := value.(map[string]any)
		authors[c["text"].(string)] = c["author"].(string)
	}
	if authors["synthetic-writer-a"] != "fixture-a" || authors["synthetic-writer-b"] != "fixture-b" || authors["old"] != "historical" || authors["tampered"] != "" {
		t.Fatal(authors)
	}
	checks := 0
	a = New(s, func(ctx context.Context, id json.RawMessage, op string, in map[string]any) (operations.Authority, error) {
		checks++
		if checks > 2 {
			return operations.Authority{}, errors.New("revoked private detail")
		}
		return verifier(ctx, id, op, in)
	})
	result, _ := invoke(t, a, "get", "synthetic-reader", map[string]any{"db": "ideas", "id": "ID-one"})
	if !result.IsError || bytes.Contains(result.Content, []byte("historical")) || bytes.Contains(result.Content, []byte("private detail")) {
		t.Fatal(string(result.Content))
	}
}

type fixturePlugin struct{ *Adapter }

func (*fixturePlugin) Init(context.Context, subprocess.InitParams) (subprocess.InitResult, error) {
	return subprocess.InitResult{ID: "fixture.portfolio", Name: "Synthetic portfolio", Version: "0.1.0", Protocol: subprocess.ProtocolVersion, CapabilityContract: capability.ContractVersion}, nil
}
func (*fixturePlugin) Load(context.Context) (subprocess.LoadResult, error) {
	return subprocess.LoadResult{}, nil
}
func (*fixturePlugin) Unload(context.Context) error { return nil }

func sdkSession(t *testing.T, adapter *Adapter) func(int64, string, any) subprocess.RPCResponse {
	t.Helper()
	// Each RPC gets its own bounded wait. One deadline for the entire transcript
	// expires during otherwise healthy later writes under race instrumentation.
	ctx, cancel := context.WithCancel(t.Context())

	in, input := io.Pipe()
	output, out := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- subprocess.ServeWithOptions(&fixturePlugin{adapter}, subprocess.ServeOptions{Context: ctx, Input: in, Output: out})
	}()
	encoder, decoder := json.NewEncoder(input), json.NewDecoder(output)
	closeSession := func() {
		cancel()
		_ = input.Close()
		_ = in.Close()
		_ = output.Close()
		_ = out.Close()
	}
	// Register before init/load so a failed handshake also releases the server.
	t.Cleanup(func() {
		defer closeSession()
		_ = input.Close()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-timer.C:
			t.Error("SDK fixture shutdown exceeded its bounded wait")
		}
	})
	call := func(id int64, method string, params any) subprocess.RPCResponse {
		t.Helper()
		callCtx, cancelCall := context.WithTimeout(ctx, 10*time.Second)
		// Cancellation must unblock pipe I/O as well as cancel the handler.
		stop := context.AfterFunc(callCtx, closeSession)
		defer func() { stop(); cancelCall() }()
		if err := encoder.Encode(subprocess.RPCRequest{JSONRPC: "2.0", ID: subprocess.NumberID(id), Method: method, Params: params}); err != nil {
			t.Fatal(err)
		}
		var reply subprocess.RPCResponse
		if err := decoder.Decode(&reply); err != nil {
			t.Fatal(err)
		}
		if err := callCtx.Err(); err != nil {
			t.Fatalf("SDK RPC %d %s exceeded its bounded wait: %v", id, method, err)
		}
		if reply.Error != nil {
			t.Fatal(reply.Error)
		}
		return reply
	}
	dir := t.TempDir()
	call(1, subprocess.MethodInit, subprocess.InitParams{PluginDir: dir, DataDir: filepath.Join(dir, "data"), CacheDir: filepath.Join(dir, "cache"), LogLevel: "info", Config: map[string]string{}, HostInfo: subprocess.HostInfo{Version: "fixture", Protocol: subprocess.ProtocolVersion}, CapabilityContract: capability.ContractVersion, Incarnation: capability.RuntimeIdentity{HostInstance: "fixture-host", OwnerID: "fixture-plugin", OwnerGeneration: 1}})
	call(2, subprocess.MethodLoad, map[string]any{})
	return call
}

func TestSDKOwnedSmokeTranscript(t *testing.T) {
	// Public SDK protocol path, synthetic data/grants only; no deployed host.
	call := sdkSession(t, New(shadow(t), verifier))
	for i, tc := range []struct {
		name, handle string
		input        map[string]any
	}{
		{"list", "synthetic-reader", map[string]any{"db": "ideas", "page": map[string]any{"limit": 1}}},
		{"get", "synthetic-reader", map[string]any{"db": "ideas", "id": "ID-one"}},
		{"comment", "synthetic-writer-a", map[string]any{"db": "ideas", "id": "ID-one", "text": "SDK synthetic comment", "author": "forged"}},
		{"decide", "synthetic-writer-b", map[string]any{"id": "DEC-001", "option": "a", "author": "forged"}},
	} {
		identity, _ := json.Marshal(tc.handle)
		reply := call(int64(i+3), subprocess.MethodMCPCallTool, subprocess.MCPCallRequest{ToolName: Prefix + tc.name, Identity: identity, Arguments: tc.input})
		raw, err := json.Marshal(reply.Result)
		if err != nil {
			t.Fatal(err)
		}
		var result subprocess.MCPCallResult
		if err = json.Unmarshal(raw, &result); err != nil || result.IsError {
			t.Fatal(result, err)
		}
		if strings.Contains(string(result.Content), "forged") {
			t.Fatal("body principal admitted")
		}
		t.Logf("TEST-OWNED SDK transcript %s: %s", Prefix+tc.name, result.Content)
	}
	call(7, subprocess.MethodUnload, map[string]any{})
}

func TestSDKSerializedNumericRefusalAndStructuredWrites(t *testing.T) {
	s := shadow(t)
	// Explicit test-owned write/decide grants over all generated fixture targets.
	all := func(_ context.Context, identity json.RawMessage, op string, _ map[string]any) (operations.Authority, error) {
		permitted := false
		for _, candidate := range operations.Registry() {
			if candidate.Name == op && candidate.Name != "migrate" {
				permitted = true
			}
		}
		return operations.Authority{Principal: "fixture-writer", Verified: string(identity) == `"synthetic-writer-a"`, Allowed: permitted}, nil
	}
	call := sdkSession(t, New(s, all))
	id := int64(3)
	tool := func(name, arguments string) subprocess.MCPCallResult {
		t.Helper()
		// Raw request JSON is serialized by the actual SDK protocol encoder and
		// decoded by decodeParams. A direct json.Number handler call is not proof.
		params := json.RawMessage(`{"tool_name":"` + Prefix + name + `","identity":"synthetic-writer-a","arguments":` + arguments + `}`)
		reply := call(id, subprocess.MethodMCPCallTool, params)
		id++
		raw, err := json.Marshal(reply.Result)
		if err != nil {
			t.Fatal(err)
		}
		var result subprocess.MCPCallResult
		if err = json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before, err := s.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{"9007199254740992", "9007199254740993", "-9007199254740992", "-9007199254740993", "9.007199254740993e15", "-9.007199254740993e15"} {
		for _, tc := range []struct{ name, input string }{
			{"create", `{"db":"ideas","item":{"title":"Must refuse","kind":"idea","unknown":{"nested":[` + number + `]}}}`},
			{"update", `{"db":"ideas","id":"ID-one","patch":{"unknown":{"nested":[` + number + `]}}}`},
			{"update", `{"db":"ideas","id":"ID-one","rev":` + number + `,"patch":{"title":"Must refuse"}}`},
			{"inbox_add", `{"title":"Must refuse","opaque":[{"number":` + number + `}]}`},
			{"inbox_promote", `{"id":"IN-absent","to_db":"ideas","fields":{"unknown":` + number + `}}`},
			{"comment", `{"db":"ideas","id":"ID-one","text":"Must refuse","unknown":` + number + `}`},
			{"batch", `{"entries":[{"operation":"comment","input":{"db":"ideas","id":"ID-one","text":"Must roll back"}},{"operation":"update","input":{"db":"ideas","id":"ID-one","patch":{"unknown":[` + number + `]}}}]}`},
		} {
			result := tool(tc.name, tc.input)
			if !result.IsError || !bytes.Contains(result.Content, []byte(`"code":"invalid"`)) || !bytes.Contains(result.Content, []byte("below 2^53")) {
				t.Fatal("unsafe numeric input admitted", number, tc.name, string(result.Content))
			}
			after, err := s.Store.Export(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			beforeRaw, _ := json.Marshal(before)
			afterRaw, _ := json.Marshal(after)
			if !bytes.Equal(beforeRaw, afterRaw) {
				t.Fatal("numeric refusal had an effect", number, tc.name)
			}
			t.Logf("SDK numeric refusal %s input=%s result=%s", tc.name, number, result.Content)
		}
	}
	for _, number := range []string{"9007199254740991", "-9007199254740991", "0", "1.5", "1e3"} {
		result := tool("update", `{"db":"ideas","id":"ID-one","patch":{"safe":{"nested":[`+number+`]}}}`)
		if result.IsError {
			t.Fatal("safe numeric control refused", number, string(result.Content))
		}
		t.Logf("SDK safe numeric control input=%s PASS (numeric token preservation not claimed)", number)
	}
	for _, tc := range []struct{ name, input string }{
		{"create", `{"db":"ideas","item":{"id":"ID-sdk","title":"Created","kind":"idea","author":"forged","custom":null}}`},
		{"update", `{"db":"ideas","id":"ID-sdk","rev":1,"patch":{"notes":"Updated","custom":null}}`},
		{"inbox_add", `{"title":"Promote SDK","kind":"idea","added_by":"forged"}`},
		{"inbox_promote", `{"id":"IN-001","to_db":"ideas","fields":{"id":"ID-promoted","kind":"idea","priority":2}}`},
		{"batch", `{"entries":[{"operation":"create","input":{"db":"ideas","item":{"id":"ID-dependent","title":"Sequential","kind":"idea"}}},{"operation":"update","input":{"db":"ideas","id":"ID-dependent","rev":1,"patch":{"title":"Second"}}},{"operation":"comment","input":{"db":"ideas","id":"ID-dependent","text":"Third"}}]}`},
	} {
		result := tool(tc.name, tc.input)
		if result.IsError || bytes.Contains(result.Content, []byte("forged")) {
			t.Fatal("structured write control failed", tc.name, string(result.Content))
		}
		t.Logf("TEST-OWNED SDK structured write %s: %s", tc.name, result.Content)
	}
}
