package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	plugin "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// Host is the narrow slice of the Tangent plugin host this plugin uses.
type Host interface {
	RegisterMCPTool(tangentplugin.MCPTool) error
	RegisterHTTPRoute(tangentplugin.HTTPRoute) error
	Tools() (tangentplugin.ToolCaller, error)
}

// Plugin implements the plugin-sdk Plugin interface for the runner.
type Plugin struct {
	mu     sync.Mutex
	engine *Engine
	host   Host
	caller tangentplugin.ToolCaller
	status plugin.PluginStatus
}

// New returns an uninitialized runner Plugin.
func New() *Plugin {
	return &Plugin{
		engine: NewEngine(nil),
	}
}

// WithToolCaller attaches an in-process MCP tool caller for Tangent turns integration.
func (p *Plugin) WithToolCaller(caller tangentplugin.ToolCaller) *Plugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.caller = caller
	p.engine.SetToolCaller(caller)
	return p
}

// Engine returns the underlying runner engine.
func (p *Plugin) Engine() *Engine {
	return p.engine
}

func (p *Plugin) ID() string      { return ID }
func (p *Plugin) Name() string    { return "Agent Runner" }
func (p *Plugin) Version() string { return PluginVersion }
func (p *Plugin) Description() string {
	return "Embedded and delegated agent execution runner with conversational turn extraction."
}
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Status() plugin.PluginStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Plugin) Load(host plugin.Host) error {
	if host == nil {
		return fmt.Errorf("runner: host is nil")
	}
	tangentHost, ok := host.(Host)
	if !ok {
		return fmt.Errorf(
			"runner: host does not offer Tangent's plugin registration surfaces "+
				"(RegisterMCPTool, RegisterHTTPRoute); got %T", host)
	}

	p.mu.Lock()
	p.host = tangentHost
	p.mu.Unlock()

	// Resolve tool caller from host if available
	if caller, err := tangentHost.Tools(); err == nil && caller != nil {
		p.mu.Lock()
		p.caller = caller
		p.engine.SetToolCaller(caller)
		p.mu.Unlock()
	}

	// Register MCP tools
	tools := []tangentplugin.MCPTool{
		{
			Name:        LaunchTool,
			Description: LaunchToolDescription,
			InputSchema: LaunchToolSchema,
			Handler:     p,
		},
		{
			Name:        SendTurnTool,
			Description: SendTurnToolDescription,
			InputSchema: SendTurnToolSchema,
			Handler:     p,
		},
		{
			Name:        HealthTool,
			Description: HealthToolDescription,
			InputSchema: HealthToolSchema,
			Handler:     p,
		},
		{
			Name:        StopTool,
			Description: StopToolDescription,
			InputSchema: StopToolSchema,
			Handler:     p,
		},
	}

	for _, t := range tools {
		if err := tangentHost.RegisterMCPTool(t); err != nil {
			return p.failLoad(err)
		}
	}

	// Register HTTP routes
	routes := []tangentplugin.HTTPRoute{
		{
			Method:     http.MethodPost,
			Path:       LaunchPath,
			Capability: tangentplugin.CapabilityDraft,
			Handler:    p,
		},
		{
			Method:     http.MethodGet,
			Path:       SessionsPath,
			Capability: tangentplugin.CapabilityView,
			Handler:    p,
		},
	}

	for _, r := range routes {
		if err := tangentHost.RegisterHTTPRoute(r); err != nil {
			return p.failLoad(err)
		}
	}

	p.mu.Lock()
	p.status = plugin.PluginStatus{
		Loaded:   true,
		Enabled:  true,
		LoadedAt: time.Now().UTC(),
	}
	p.mu.Unlock()
	return nil
}

func (p *Plugin) failLoad(err error) error {
	p.mu.Lock()
	p.status.LastError = err.Error()
	p.mu.Unlock()
	return err
}

func (p *Plugin) Unload() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.host = nil
	p.status = plugin.PluginStatus{
		Loaded:  false,
		Enabled: false,
	}
	return nil
}

// MCPCallTool handles subprocess JSON-RPC tool calls from plugin-sdk.
func (p *Plugin) MCPCallTool(
	ctx context.Context,
	req subprocess.MCPCallRequest,
) (subprocess.MCPCallResult, error) {
	switch req.ToolName {
	case LaunchTool:
		var params LaunchParams
		if err := decodeArguments(req.Arguments, &params); err != nil {
			return subprocess.MCPCallResult{}, err
		}
		res, err := p.engine.Launch(ctx, params)
		if err != nil {
			return subprocess.MCPCallResult{}, err
		}
		return jsonResult(res)

	case SendTurnTool:
		var params SendTurnParams
		if err := decodeArguments(req.Arguments, &params); err != nil {
			return subprocess.MCPCallResult{}, err
		}
		res, err := p.engine.SendTurn(ctx, params)
		if err != nil {
			return subprocess.MCPCallResult{}, err
		}
		return jsonResult(res)

	case HealthTool:
		var params struct {
			SessionID string `json:"session_id"`
		}
		if err := decodeArguments(req.Arguments, &params); err != nil {
			return subprocess.MCPCallResult{}, err
		}
		res, err := p.engine.Health(ctx, params.SessionID)
		if err != nil {
			return subprocess.MCPCallResult{}, err
		}
		return jsonResult(res)

	case StopTool:
		var params struct {
			SessionID string `json:"session_id"`
		}
		if err := decodeArguments(req.Arguments, &params); err != nil {
			return subprocess.MCPCallResult{}, err
		}
		if err := p.engine.Stop(params.SessionID); err != nil {
			return subprocess.MCPCallResult{}, err
		}
		return jsonResult(map[string]any{"status": "stopped", "session_id": params.SessionID})

	default:
		return subprocess.MCPCallResult{}, fmt.Errorf("runner: no handler for tool %q", req.ToolName)
	}
}

// HTTPHandle handles subprocess HTTP requests from plugin-sdk.
func (p *Plugin) HTTPHandle(
	ctx context.Context,
	req subprocess.HTTPRequest,
) (subprocess.HTTPResponse, error) {
	if req.Method == http.MethodPost && req.Path == LaunchPath {
		var params LaunchParams
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &params); err != nil {
				return badRequest("the launch request body is not JSON: " + err.Error()), nil
			}
		}
		res, err := p.engine.Launch(ctx, params)
		if err != nil {
			return subprocess.HTTPResponse{}, err
		}
		body, err := json.Marshal(res)
		if err != nil {
			return subprocess.HTTPResponse{}, fmt.Errorf("runner: encode launch result: %w", err)
		}
		return subprocess.HTTPResponse{
			Status:  http.StatusOK,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    body,
		}, nil
	}

	if req.Method == http.MethodGet && strings.HasPrefix(req.Path, SessionsPath) {
		p.engine.mu.RLock()
		defer p.engine.mu.RUnlock()

		list := make([]map[string]any, 0, len(p.engine.sessions))
		for _, s := range p.engine.sessions {
			list = append(list, map[string]any{
				"session_id": s.ID,
				"agent_id":   s.AgentID,
				"mode":       s.Mode,
				"live_state": s.LiveState,
				"started_at": s.StartedAt,
			})
		}
		body, err := json.Marshal(list)
		if err != nil {
			return subprocess.HTTPResponse{}, fmt.Errorf("runner: encode sessions result: %w", err)
		}
		return subprocess.HTTPResponse{
			Status:  http.StatusOK,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    body,
		}, nil
	}

	return subprocess.HTTPResponse{Status: http.StatusNotFound, Body: []byte("not found")}, nil
}

func badRequest(message string) subprocess.HTTPResponse {
	body, _ := json.Marshal(map[string]string{"code": "invalid_request", "message": message})
	return subprocess.HTTPResponse{
		Status:  http.StatusBadRequest,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
	}
}

func decodeArguments(arguments map[string]any, into any) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return fmt.Errorf("runner: re-encode arguments: %w", err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		return fmt.Errorf("runner: decode arguments: %w", err)
	}
	return nil
}

func jsonResult(value any) (subprocess.MCPCallResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return subprocess.MCPCallResult{}, fmt.Errorf("runner: encode result: %w", err)
	}
	return subprocess.MCPCallResult{Content: encoded}, nil
}

// Compile-time proof this satisfies the SDK contracts it registers against.
var (
	_ plugin.Plugin          = (*Plugin)(nil)
	_ subprocess.MCPHandler  = (*Plugin)(nil)
	_ subprocess.HTTPHandler = (*Plugin)(nil)
)
