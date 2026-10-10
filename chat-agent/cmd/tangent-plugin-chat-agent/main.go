// Command tangent-plugin-chat-agent serves an inert thin-client scaffold.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
)

const (
	pluginID          = "tangent.plugin.chat-agent"
	pluginName        = "Chat Agent Scaffold"
	pluginVersion     = "0.1.0-dev"
	pluginDescription = "Inert thin-client scaffold; backend, binding and rail integration unavailable"
	statusTool        = "tangent.chat_agent_status"
	statusPath        = "/api/plugins/chat-agent/status"
)

type settings struct {
	backend, conversationRef, agentRef, density string
	railOpen                                    bool
}

var errNotLoaded = errors.New("chat-agent: not_loaded")

var opaqueReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)

func resolveSettings(config map[string]string) (settings, error) {
	result := settings{backend: "nanite", density: "comfortable"}
	for key, value := range config {
		switch key {
		case "backend":
			if value != "nanite" {
				return settings{}, errors.New("chat-agent: backend_configuration_refused")
			}
			result.backend = value
		case "conversation_ref", "agent_ref":
			if value != "" && !opaqueReference.MatchString(value) {
				return settings{}, errors.New("chat-agent: opaque_reference_refused")
			}
			if key == "conversation_ref" {
				result.conversationRef = value
			} else {
				result.agentRef = value
			}
		case "density":
			if value != "comfortable" && value != "compact" {
				return settings{}, errors.New("chat-agent: preference_refused")
			}
			result.density = value
		case "rail_open":
			if value != "true" && value != "false" {
				return settings{}, errors.New("chat-agent: preference_refused")
			}
			result.railOpen = value == "true"
		default:
			return settings{}, errors.New("chat-agent: undeclared_configuration_refused")
		}
	}
	return result, nil
}

type served struct {
	mu                  sync.Mutex
	initialized, loaded bool
	config              settings
}

func (s *served) Init(ctx context.Context, params subprocess.InitParams) (subprocess.InitResult, error) {
	if err := ctx.Err(); err != nil {
		return subprocess.InitResult{}, err
	}
	config, err := resolveSettings(params.Config)
	if err != nil {
		return subprocess.InitResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialized {
		return subprocess.InitResult{}, errors.New("chat-agent: already_initialized")
	}
	s.config, s.initialized = config, true
	// No filesystem custody, network calls, host callbacks or workers start.
	return subprocess.InitResult{ID: pluginID, Name: pluginName, Version: pluginVersion, Description: pluginDescription, Protocol: subprocess.ProtocolVersion, CapabilityContract: capability.ContractVersion}, nil
}
func (s *served) Load(ctx context.Context) (subprocess.LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return subprocess.LoadResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initialized || s.loaded {
		return subprocess.LoadResult{}, errors.New("chat-agent: load_state_refused")
	}
	s.loaded = true
	return subprocess.LoadResult{}, nil
}
func (s *served) Unload(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded, s.initialized, s.config = false, false, settings{}
	return nil
}
func (s *served) Health(ctx context.Context) (subprocess.HealthStatus, error) {
	if err := ctx.Err(); err != nil {
		return subprocess.HealthStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return subprocess.HealthStatus{OK: false, Message: "not_loaded"}, nil
	}
	// Driver health measures the status service, not backend readiness.
	return subprocess.HealthStatus{OK: true, Message: "scaffold_only_backend_unavailable"}, nil
}

type availability struct {
	ChatAvailable          bool     `json:"chat_available"`
	AgentRunning           bool     `json:"agent_running"`
	Backend                string   `json:"backend"`
	ConversationConfigured bool     `json:"conversation_configured"`
	AgentConfigured        bool     `json:"agent_configured"`
	RailOpen               bool     `json:"rail_open"`
	Density                string   `json:"density"`
	Unavailable            []string `json:"unavailable"`
}

func (s *served) status(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return nil, errNotLoaded
	}
	// References remain private and untrusted. Presence grants no authority;
	// never disclose a reference to an unbound browser/agent caller.
	return json.Marshal(availability{Backend: s.config.backend, ConversationConfigured: s.config.conversationRef != "", AgentConfigured: s.config.agentRef != "", RailOpen: s.config.railOpen, Density: s.config.density,
		Unavailable: []string{"backend_unavailable", "participant_binding_unavailable", "plugin_ui_unavailable"}})
}
func (s *served) MCPCallTool(ctx context.Context, request subprocess.MCPCallRequest) (subprocess.MCPCallResult, error) {
	if request.ToolName != statusTool {
		return subprocess.MCPCallResult{}, errors.New("chat-agent: unknown_tool")
	}
	if len(request.Arguments) != 0 {
		return subprocess.MCPCallResult{}, errors.New("chat-agent: status_arguments_refused")
	}
	raw, err := s.status(ctx)
	if err != nil {
		return subprocess.MCPCallResult{}, err
	}
	return subprocess.MCPCallResult{Content: raw}, nil
}
func (s *served) HTTPHandle(ctx context.Context, request subprocess.HTTPRequest) (subprocess.HTTPResponse, error) {
	if request.Method != http.MethodGet || request.Path != statusPath {
		return subprocess.HTTPResponse{Status: http.StatusNotFound}, nil
	}
	if len(request.Query) != 0 || len(request.Body) != 0 {
		return subprocess.HTTPResponse{Status: http.StatusBadRequest, Body: []byte(`{"code":"status_arguments_refused"}`)}, nil
	}
	raw, err := s.status(ctx)
	if errors.Is(err, errNotLoaded) {
		return subprocess.HTTPResponse{Status: http.StatusServiceUnavailable, Body: []byte(`{"code":"not_loaded"}`)}, nil
	}
	if err != nil {
		return subprocess.HTTPResponse{}, err
	}
	return subprocess.HTTPResponse{Status: http.StatusOK, Headers: map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"}, Body: raw}, nil
}

var (
	_ subprocess.Plugin        = (*served)(nil)
	_ subprocess.HealthChecker = (*served)(nil)
	_ subprocess.MCPHandler    = (*served)(nil)
	_ subprocess.HTTPHandler   = (*served)(nil)
)

func run(args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "--manifest" {
		return emitManifest(out)
	}
	if len(args) != 0 {
		return errors.New("chat-agent: unexpected_arguments")
	}
	return subprocess.Serve(&served{})
}
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
