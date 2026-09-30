// Command tangent-plugin-runner is the lightweight agent execution runner,
// packaged as its own program outside Tangent core.
//
// # What it is, mechanically
//
// A subprocess.Serve implementation around runner.Plugin. It can execute agents
// either as supervised local subprocesses (with stream sieving to extract human
// conversational turns at LiveStateIdle) or delegated to Tether.
//
// When running in subprocess mode, Tangent passes TANGENT_MCP_URL, allowing
// the runner to connect back to Tangent and enqueue conversational turns
// into Tangent's agent-turn FIFO inbox (/turns).
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/plugin-sdk/subprocess"

	"github.com/hollis-labs/tangent-plugins/runner/internal/runner"
	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
	"github.com/hollis-labs/tangent/pkg/plugin/hostclient"
)

// served adapts the plugin to the SDK's subprocess lifecycle.
type served struct {
	inner  *runner.Plugin
	client *hostclient.Client
}

func (s *served) Init(_ context.Context, _ subprocess.InitParams) (subprocess.InitResult, error) {
	client, err := hostclient.New()
	if err != nil {
		return subprocess.InitResult{}, err
	}
	s.client = client
	s.inner = runner.New().WithToolCaller(client)
	return subprocess.InitResult{
		ID:          runner.ID,
		Name:        s.inner.Name(),
		Version:     s.inner.Version(),
		Description: s.inner.Description(),
		Protocol:    subprocess.ProtocolVersion,
	}, nil
}

func (s *served) Load(context.Context) (subprocess.LoadResult, error) {
	return subprocess.LoadResult{}, nil
}

func (s *served) Unload(context.Context) error {
	if s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *served) MCPCallTool(
	ctx context.Context, request subprocess.MCPCallRequest,
) (subprocess.MCPCallResult, error) {
	return s.inner.MCPCallTool(ctx, request)
}

func (s *served) HTTPHandle(
	ctx context.Context, request subprocess.HTTPRequest,
) (subprocess.HTTPResponse, error) {
	return s.inner.HTTPHandle(ctx, request)
}

var (
	_ subprocess.Plugin      = (*served)(nil)
	_ subprocess.MCPHandler  = (*served)(nil)
	_ subprocess.HTTPHandler = (*served)(nil)
)

func emitManifest(out io.Writer) error {
	manifest := tangentplugin.Manifest{
		ID:          runner.ID,
		Name:        runner.New().Name(),
		Description: runner.New().Description(),
		Version:     runner.New().Version(),
		Protocol:    subprocess.ProtocolVersion,
		Entrypoint:  "tangent-plugin-runner",
		Tools: []tangentplugin.ToolDecl{
			{
				Name:        runner.LaunchTool,
				Description: runner.LaunchToolDescription,
				InputSchema: string(runner.LaunchToolSchema),
			},
			{
				Name:        runner.SendTurnTool,
				Description: runner.SendTurnToolDescription,
				InputSchema: string(runner.SendTurnToolSchema),
			},
			{
				Name:        runner.HealthTool,
				Description: runner.HealthToolDescription,
				InputSchema: string(runner.HealthToolSchema),
			},
			{
				Name:        runner.StopTool,
				Description: runner.StopToolDescription,
				InputSchema: string(runner.StopToolSchema),
			},
		},
		Routes: []tangentplugin.RouteDecl{
			{
				Method:     http.MethodPost,
				Path:       runner.LaunchPath,
				Capability: string(tangentplugin.CapabilityDraft),
			},
			{
				Method:     http.MethodGet,
				Path:       runner.SessionsPath,
				Capability: string(tangentplugin.CapabilityView),
			},
		},
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	encoder := yaml.NewEncoder(out)
	encoder.SetIndent(2)
	if err := encoder.Encode(manifest); err != nil {
		return err
	}
	return encoder.Close()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--manifest" {
		if err := emitManifest(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "tangent-plugin-runner: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := subprocess.Serve(&served{}); err != nil {
		fmt.Fprintf(os.Stderr, "tangent-plugin-runner: %v\n", err)
		os.Exit(1)
	}
}
