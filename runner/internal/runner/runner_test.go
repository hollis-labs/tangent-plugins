package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	plugin "github.com/hollis-labs/plugin-sdk"
	"github.com/hollis-labs/plugin-sdk/subprocess"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

type fakeTools struct {
	mu    sync.Mutex
	calls []toolCall
}

type toolCall struct {
	Name      string
	Arguments map[string]any
}

func (f *fakeTools) CallTool(
	_ context.Context, name string, arguments any,
) (tangentplugin.ToolResult, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return tangentplugin.ToolResult{}, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return tangentplugin.ToolResult{}, err
	}

	f.mu.Lock()
	f.calls = append(f.calls, toolCall{Name: name, Arguments: decoded})
	f.mu.Unlock()
	if name == EnqueueTurnTool {
		if refusal, ok := checkEnqueue(decoded); !ok {
			return refusal, nil
		}
	}

	return tangentplugin.ToolResult{Content: json.RawMessage(`{"status":"ok"}`)}, nil
}

func (f *fakeTools) getCalls(name string) []toolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []toolCall
	for _, c := range f.calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

type baseHostOnly struct{}

func (baseHostOnly) GetPlugin(string) (plugin.Plugin, bool) { return nil, false }
func (baseHostOnly) RegisterCRUDHandler(string, plugin.CRUDHandler) error {
	return errors.New("not honored")
}
func (baseHostOnly) RegisterEventHook([]string, plugin.EventHook) error {
	return errors.New("not honored")
}
func (baseHostOnly) RegisterUIComponent(plugin.UIComponent) error { return errors.New("not honored") }
func (baseHostOnly) GetService(string) (interface{}, error)       { return nil, errors.New("not honored") }
func (baseHostOnly) GetConfig(string) (string, error)             { return "", errors.New("not honored") }
func (baseHostOnly) SetConfig(string, string) error               { return errors.New("not honored") }
func (baseHostOnly) RegisterConfigSchema([]plugin.ConfigFieldDef) error {
	return errors.New("not honored")
}
func (baseHostOnly) RegisterConnector(string, plugin.Connector) error {
	return errors.New("not honored")
}
func (baseHostOnly) RegisterProvider(string, interface{}) error   { return errors.New("not honored") }
func (baseHostOnly) RegisterCLIAdapter(string, interface{}) error { return errors.New("not honored") }
func (baseHostOnly) Logger() plugin.Logger                        { return nil }
func (baseHostOnly) Context() context.Context                     { return context.Background() }

var _ plugin.Host = baseHostOnly{}

type fakeHost struct {
	baseHostOnly
	tools  map[string]tangentplugin.MCPTool
	routes map[string]tangentplugin.HTTPRoute
	caller tangentplugin.ToolCaller
}

func newFakeHost(caller tangentplugin.ToolCaller) *fakeHost {
	return &fakeHost{
		tools:  make(map[string]tangentplugin.MCPTool),
		routes: make(map[string]tangentplugin.HTTPRoute),
		caller: caller,
	}
}

func (h *fakeHost) RegisterMCPTool(tool tangentplugin.MCPTool) error {
	h.tools[tool.Name] = tool
	return nil
}

func (h *fakeHost) RegisterHTTPRoute(route tangentplugin.HTTPRoute) error {
	key := fmt.Sprintf("%s %s", route.Method, route.Path)
	h.routes[key] = route
	return nil
}

func (h *fakeHost) Tools() (tangentplugin.ToolCaller, error) {
	return h.caller, nil
}

func TestEngine_EmbeddedSubprocessLifecycle(t *testing.T) {
	fakeCaller := &fakeTools{}
	engine := NewEngine(nil)
	engine.SetToolCaller(fakeCaller)

	ctx := context.Background()

	// Launch with cat (which reflects stdin to stdout)
	res, err := engine.Launch(ctx, LaunchParams{
		SessionID:  "sess-embed-1",
		AgentID:    "agent-claude",
		AgentLabel: "Claude Assistant",
		Prompt:     "Please confirm if you want to proceed with deployment?",
		Command:    "cat",
		Lifecycle:  LifecycleStreamingStdio,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if res.SessionID != "sess-embed-1" {
		t.Errorf("expected session_id 'sess-embed-1', got %q", res.SessionID)
	}
	if res.Status != "running" {
		t.Errorf("expected status 'running', got %q", res.Status)
	}

	// Wait for cat to echo the prompt and engine's idle timer (300ms) to fire
	var calls []toolCall
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		calls = fakeCaller.getCalls("tangent.turns_enqueue")
		if len(calls) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(calls) == 0 {
		t.Fatal("expected auto-enqueue turn call on tangent.turns_enqueue, but none received")
	}

	// The turn is the tool's arguments, not a JSON string wrapped in a field:
	// that wrapper is refused by the real tool's schema (see
	// tangent's make smoke, which boots this runner against the real tool).
	enqueued := calls[0].Arguments
	if _, wrapped := enqueued["request"]; wrapped {
		t.Fatalf("turn is wrapped in a request field: %v", enqueued)
	}
	if enqueued["session_id"] != "sess-embed-1" {
		t.Errorf("expected session_id 'sess-embed-1', got %v", enqueued["session_id"])
	}
	if enqueued["kind"] != "approval" {
		t.Errorf("expected kind 'approval', got %v", enqueued["kind"])
	}

	// Send an operator reply back into cat's stdin
	sendRes, err := engine.SendTurn(ctx, SendTurnParams{
		SessionID:    "sess-embed-1",
		ResponseText: "Yes, please proceed.",
		Action:       "approve",
	})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if !sendRes.Delivered {
		t.Errorf("expected delivered=true, got %v", sendRes.Delivered)
	}

	// Check health
	health, err := engine.Health(ctx, "sess-embed-1")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Alive {
		t.Errorf("expected alive=true, got %v", health.Alive)
	}
	if health.TurnsCount < 2 {
		t.Errorf("expected turns_count >= 2, got %d", health.TurnsCount)
	}

	// Stop session
	if err = engine.Stop("sess-embed-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Confirm stopped state
	healthStopped, err := engine.Health(ctx, "sess-embed-1")
	if err != nil {
		t.Fatalf("Health after stop: %v", err)
	}
	if healthStopped.Alive {
		t.Errorf("expected alive=false after stop, got %v", healthStopped.Alive)
	}
}

// refusingTools answers every call the way a tool that refuses does: no
// transport error, IsError set. It is the case the engine used to drop.
type refusingTools struct{ body string }

func (r refusingTools) CallTool(context.Context, string, any) (tangentplugin.ToolResult, error) {
	return tangentplugin.ToolResult{Content: json.RawMessage(r.body), IsError: true}, nil
}

func TestEngine_LogsARefusedEnqueueInsteadOfDroppingIt(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	engine := NewEngine(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	engine.SetToolCaller(refusingTools{body: `{"code":"validation_failed"}`})

	if _, err := engine.Launch(context.Background(), LaunchParams{
		SessionID: "sess-refused", AgentID: "agent", Prompt: "Please confirm this?", Command: "cat",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = engine.Stop("sess-refused") }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := logs.String()
		mu.Unlock()
		if strings.Contains(got, "refused the enqueued turn") && strings.Contains(got, "validation_failed") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("a refused enqueue was not logged; log was:\n%s", logs.String())
}

// lockedWriter lets the engine's goroutine log while the test reads the buffer.
type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestEngine_ACPLifecycle(t *testing.T) {
	engine := NewEngine(nil)
	ctx := context.Background()

	// Launch in ACP mode
	res, err := engine.Launch(ctx, LaunchParams{
		SessionID: "sess-acp-1",
		AgentID:   "agent-codex",
		Prompt:    "Initialize project workspace",
		Command:   "cat",
		Lifecycle: LifecycleACP,
	})
	if err != nil {
		t.Fatalf("Launch ACP: %v", err)
	}
	if res.Lifecycle != LifecycleACP {
		t.Errorf("expected lifecycle 'acp', got %q", res.Lifecycle)
	}

	// Send turn in ACP mode
	sendRes, err := engine.SendTurn(ctx, SendTurnParams{
		SessionID:    "sess-acp-1",
		ResponseText: "Proceed with TypeScript template",
	})
	if err != nil {
		t.Fatalf("SendTurn ACP: %v", err)
	}
	if !sendRes.Delivered {
		t.Errorf("expected delivered=true, got false")
	}

	_ = engine.Stop("sess-acp-1")
}

func TestPlugin_FullLifecycleAndSubprocessContracts(t *testing.T) {
	fakeCaller := &fakeTools{}
	host := newFakeHost(fakeCaller)
	p := New()

	// 1. Load on host
	if err := p.Load(host); err != nil {
		t.Fatalf("Plugin.Load: %v", err)
	}

	status := p.Status()
	if !status.Loaded || !status.Enabled {
		t.Errorf("unexpected status: %+v", status)
	}
	if p.ID() != ID {
		t.Errorf("expected ID %q, got %q", ID, p.ID())
	}
	if p.Name() != "Agent Runner" {
		t.Errorf("expected Name 'Agent Runner', got %q", p.Name())
	}

	// Verify tool registrations on host
	for _, toolName := range []string{LaunchTool, SendTurnTool, HealthTool, StopTool} {
		if _, ok := host.tools[toolName]; !ok {
			t.Errorf("tool %q was not registered on host", toolName)
		}
	}

	// Verify route registrations on host
	if _, ok := host.routes["POST "+LaunchPath]; !ok {
		t.Errorf("route POST %s was not registered on host", LaunchPath)
	}
	if _, ok := host.routes["GET "+SessionsPath]; !ok {
		t.Errorf("route GET %s was not registered on host", SessionsPath)
	}

	ctx := context.Background()

	// 2. Subprocess MCPCallTool: LaunchTool
	launchArgs := map[string]any{
		"session_id": "sess-subproc-1",
		"agent_id":   "agent-sub",
		"prompt":     "Run analysis",
		"command":    "cat",
	}
	mcpRes, err := p.MCPCallTool(ctx, subprocess.MCPCallRequest{
		ToolName:  LaunchTool,
		Arguments: launchArgs,
	})
	if err != nil {
		t.Fatalf("MCPCallTool Launch: %v", err)
	}
	var launchRes LaunchResult
	if err = json.Unmarshal(mcpRes.Content, &launchRes); err != nil {
		t.Fatalf("unmarshal launch result: %v", err)
	}
	if launchRes.SessionID != "sess-subproc-1" {
		t.Errorf("expected session_id 'sess-subproc-1', got %q", launchRes.SessionID)
	}

	// 3. Subprocess MCPCallTool: HealthTool
	healthRes, err := p.MCPCallTool(ctx, subprocess.MCPCallRequest{
		ToolName: HealthTool,
		Arguments: map[string]any{
			"session_id": "sess-subproc-1",
		},
	})
	if err != nil {
		t.Fatalf("MCPCallTool Health: %v", err)
	}
	var hRes SessionHealthResult
	if err = json.Unmarshal(healthRes.Content, &hRes); err != nil {
		t.Fatalf("unmarshal health result: %v", err)
	}
	if !hRes.Alive {
		t.Errorf("expected alive=true, got %v", hRes.Alive)
	}

	// 4. Subprocess MCPCallTool: SendTurnTool
	sendTurnRes, err := p.MCPCallTool(ctx, subprocess.MCPCallRequest{
		ToolName: SendTurnTool,
		Arguments: map[string]any{
			"session_id":    "sess-subproc-1",
			"response_text": "Good to go.",
		},
	})
	if err != nil {
		t.Fatalf("MCPCallTool SendTurn: %v", err)
	}
	var stRes SendTurnResult
	if err = json.Unmarshal(sendTurnRes.Content, &stRes); err != nil {
		t.Fatalf("unmarshal send_turn result: %v", err)
	}
	if !stRes.Delivered {
		t.Errorf("expected delivered=true, got %v", stRes.Delivered)
	}

	// 5. Subprocess HTTPHandle: GET SessionsPath
	httpRes, err := p.HTTPHandle(ctx, subprocess.HTTPRequest{
		Method: http.MethodGet,
		Path:   SessionsPath,
	})
	if err != nil {
		t.Fatalf("HTTPHandle GET sessions: %v", err)
	}
	if httpRes.Status != http.StatusOK {
		t.Errorf("expected status 200, got %d", httpRes.Status)
	}
	var sessionsList []map[string]any
	if err = json.Unmarshal(httpRes.Body, &sessionsList); err != nil {
		t.Fatalf("unmarshal sessions list: %v", err)
	}
	if len(sessionsList) != 1 || sessionsList[0]["session_id"] != "sess-subproc-1" {
		t.Errorf("unexpected sessions list: %+v", sessionsList)
	}

	// 6. Subprocess MCPCallTool: StopTool
	stopRes, err := p.MCPCallTool(ctx, subprocess.MCPCallRequest{
		ToolName: StopTool,
		Arguments: map[string]any{
			"session_id": "sess-subproc-1",
		},
	})
	if err != nil {
		t.Fatalf("MCPCallTool Stop: %v", err)
	}
	var stopMap map[string]any
	if err := json.Unmarshal(stopRes.Content, &stopMap); err != nil {
		t.Fatalf("unmarshal stop result: %v", err)
	}
	if stopMap["status"] != "stopped" {
		t.Errorf("expected status 'stopped', got %v", stopMap["status"])
	}

	// 7. Unload
	if err := p.Unload(); err != nil {
		t.Fatalf("Plugin.Unload: %v", err)
	}
	if p.Status().Loaded {
		t.Errorf("expected Loaded=false after Unload")
	}
}

var (
	_ plugin.Plugin = (*Plugin)(nil)
)
