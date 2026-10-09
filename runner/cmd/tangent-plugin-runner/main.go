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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	sdkmanifest "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"

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
		ID:                 runner.ID,
		Name:               s.inner.Name(),
		Version:            s.inner.Version(),
		Description:        s.inner.Description(),
		Protocol:           subprocess.ProtocolVersion,
		CapabilityContract: capability.ContractVersion,
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
	declaration := sdkmanifest.Manifest{
		Capabilities:  []subprocess.CapabilityRequest{{Name: capability.MCPReach, Reason: "Call the declared Tangent tools used by this integration", Metadata: json.RawMessage(`{"tools":["tangent.turns_enqueue","tangent.turn_await","tangent.turn_ack"]}`)}},
		SchemaVersion: sdkmanifest.SchemaVersion,
		Config:        sdkmanifest.Config{},
		Runtime:       sdkmanifest.Runtime,
		Hosts:         map[string]sdkmanifest.HostRange{"tangent": {Min: "1.0.0", Max: "1.99.99"}},
		ID:            runner.ID,
		Name:          runner.New().Name(),
		Description:   runner.New().Description(),
		Version:       runner.New().Version(),
		Protocol:      subprocess.ProtocolVersion,
		Tools: []sdkmanifest.Tool{
			{
				Name:        runner.LaunchTool,
				Effect:      "write",
				Description: runner.LaunchToolDescription,
				InputSchema: runner.LaunchToolSchema,
			},
			{
				Name:        runner.SendTurnTool,
				Effect:      "write",
				Description: runner.SendTurnToolDescription,
				InputSchema: runner.SendTurnToolSchema,
			},
			{
				Name:        runner.HealthTool,
				Effect:      "read",
				Description: runner.HealthToolDescription,
				InputSchema: runner.HealthToolSchema,
			},
			{
				Name:        runner.StopTool,
				Effect:      "destructive",
				Description: runner.StopToolDescription,
				InputSchema: runner.StopToolSchema,
			},
		},
	}
	extension := tangentExtension{SchemaVersion: 1, Kinds: nil,
		Routes: []routeDecl{
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
	for _, tool := range declaration.Tools {
		extension.MCPTools = append(extension.MCPTools, tool.Name)
	}
	raw, err := json.Marshal(extension)
	if err != nil {
		return err
	}
	declaration.Tangent = raw
	artifact, err := executableArtifact("bin/tangent-plugin-runner")
	if err != nil {
		return err
	}
	declaration.Artifact = artifact
	declaration.Server = sdkmanifest.Server{Runtime: "binary", Entry: "bin/tangent-plugin-runner", Engines: map[string]sdkmanifest.HostRange{"binary": {Min: "1.0.0", Max: "1.99.99"}}}
	return sdkmanifest.Encode(out, declaration)
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
