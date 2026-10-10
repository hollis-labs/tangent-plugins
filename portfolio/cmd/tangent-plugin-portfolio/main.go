// Command tangent-plugin-portfolio serves a default-refusing source adapter.
// No caller verifier, data root, client or writer is enabled by this executable.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/httpapi"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/mcpapi"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
)

const (
	pluginID          = "tangent.plugin.portfolio"
	pluginName        = "Portfolio Source Adapter"
	pluginVersion     = "0.1.0-dev"
	pluginDescription = "Source-only portfolio HTTP/MCP adapter; production caller authority unavailable"
)

type served struct {
	mu                  sync.Mutex
	initialized, loaded bool
	adapter             *httpapi.Adapter
	mcp                 *mcpapi.Adapter
}

func (s *served) Init(ctx context.Context, params subprocess.InitParams) (subprocess.InitResult, error) {
	if err := ctx.Err(); err != nil {
		return subprocess.InitResult{}, err
	}
	if len(params.Config) != 0 {
		return subprocess.InitResult{}, errors.New("portfolio: configuration unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialized {
		return subprocess.InitResult{}, errors.New("portfolio: already initialized")
	}
	// DataDir and connection Identity cannot enable the source evaluator.
	// Production adoption needs explicit caller/transaction authority first.
	s.adapter = httpapi.New(operations.Service{}, nil)
	s.mcp = mcpapi.New(operations.Service{}, nil)
	s.initialized = true
	return subprocess.InitResult{ID: pluginID, Name: pluginName, Version: pluginVersion, Description: pluginDescription, Protocol: subprocess.ProtocolVersion, CapabilityContract: capability.ContractVersion}, nil
}
func (s *served) Load(ctx context.Context) (subprocess.LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return subprocess.LoadResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initialized || s.loaded {
		return subprocess.LoadResult{}, errors.New("portfolio: load state refused")
	}
	s.loaded = true
	return subprocess.LoadResult{}, nil
}
func (s *served) Unload(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialized, s.loaded, s.adapter = false, false, nil
	s.mcp = nil
	return nil
}

func (s *served) MCPCallTool(ctx context.Context, request subprocess.MCPCallRequest) (subprocess.MCPCallResult, error) {
	s.mu.Lock()
	adapter, loaded := s.mcp, s.loaded
	s.mu.Unlock()
	if !loaded || adapter == nil {
		return subprocess.MCPCallResult{IsError: true, Content: []byte(`{"error":{"code":"unavailable","message":"source adapter not loaded","details":{}}}`)}, nil
	}
	return adapter.MCPCallTool(ctx, request)
}
func (s *served) Health(ctx context.Context) (subprocess.HealthStatus, error) {
	if err := ctx.Err(); err != nil {
		return subprocess.HealthStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return subprocess.HealthStatus{OK: s.loaded, Message: "source_only_caller_authority_unavailable"}, nil
}
func (s *served) HTTPHandle(ctx context.Context, request subprocess.HTTPRequest) (subprocess.HTTPResponse, error) {
	s.mu.Lock()
	adapter := s.adapter
	loaded := s.loaded
	s.mu.Unlock()
	if !loaded || adapter == nil {
		return subprocess.HTTPResponse{Status: 503, Headers: map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"}, Body: []byte(`{"error":{"code":"unavailable","message":"source adapter not loaded","details":{}}}`)}, nil
	}
	return adapter.HTTPHandle(ctx, request)
}

var (
	_ subprocess.Plugin        = (*served)(nil)
	_ subprocess.HealthChecker = (*served)(nil)
	_ subprocess.HTTPHandler   = (*served)(nil)
	_ subprocess.MCPHandler    = (*served)(nil)
)

func run(args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "--manifest" {
		return emitManifest(out)
	}
	if len(args) != 0 {
		return errors.New("portfolio: unexpected arguments")
	}
	return subprocess.Serve(&served{})
}
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
