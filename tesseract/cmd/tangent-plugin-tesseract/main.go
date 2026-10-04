// Command tangent-plugin-tesseract is the Tesseract review integration, as its
// own program.
//
// # This is the binary the boundary was for
//
// Until CW-20260911-0070 this code was linked into `tangent`, so Tangent's
// server held Tesseract's HTTP client, its memory model, and its credential.
// ADR 0005 says Tangent is the separate-window app surface and not the
// applications it renders — which stayed true only because nothing outside one
// package read those types. That was a boundary a reviewer maintained rather
// than one the build enforced.
//
// Now it is a separate process. Tangent's binary does not link this package,
// cannot be taken down by it, and does not version with it. The credential
// point is sharper here than for Torque: `CW-20260910-0045` recorded that
// Tangent holds no Tesseract credential and vouches for no write this plugin
// makes. With the plugin out of process that is structural rather than stated —
// the credential is read from this process's own environment and never crosses
// into the host.
//
// # What it is, mechanically
//
// A thin `subprocess.Serve` wrapper around the same plugin that ran compiled
// in. The mapping, the tools and the sync are unchanged; what changes is where
// its tool calls go — through an MCP client against Tangent's own `/mcp`
// instead of through an in-process handle. See tangent's pkg/plugin/hostclient.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/hollis-labs/plugin-sdk/capability"
	sdkmanifest "github.com/hollis-labs/plugin-sdk/manifest"
	"github.com/hollis-labs/plugin-sdk/subprocess"

	"github.com/hollis-labs/tangent-plugins/tesseract/internal/tesseract"
	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
	"github.com/hollis-labs/tangent/pkg/plugin/hostclient"
)

// served adapts the plugin to the SDK's subprocess lifecycle.
//
// It does NOT call the plugin's own Load, and that is deliberate: that method
// registers tools and routes on a host, and a child has no host. What this
// plugin serves is declared in its plugin.yaml and registered by Tangent —
// the host manifest is authoritative (ADR 0008 §3), so a child is told what it
// was registered for rather than announcing it.
type served struct {
	inner *tesseract.Plugin
	// client is closed on unload. It is created at Init rather than at first
	// use so a plugin started without TANGENT_MCP_URL fails immediately, with
	// the reason, instead of at whatever tool call happens to need it first.
	client *hostclient.Client
}

func (s *served) Init(_ context.Context, _ subprocess.InitParams) (subprocess.InitResult, error) {
	client, err := hostclient.New()
	if err != nil {
		return subprocess.InitResult{}, err
	}
	s.client = client
	// The plugin reads its own environment for TANGENT_TESSERACT_API_URL,
	// TANGENT_TESSERACT_TOKEN and TANGENT_TESSERACT_NAMESPACES, exactly as it
	// did compiled in. The host sends an empty config map and holds no
	// plugin configuration, so it can never hold this plugin's credentials.
	s.inner = tesseract.New().WithToolCaller(client)
	return subprocess.InitResult{
		ID:                 tesseract.ID,
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

// emitManifest prints this plugin's plugin.yaml.
//
// # Why the binary generates its own manifest
//
// The declaration has to hold the tool names, descriptions and input schemas,
// and those already exist as Go values in the plugin package. Hand-writing a
// second copy in YAML would be two sources for one fact, kept in agreement by
// nobody — and a test asserting they agree is the shape this repository treats
// as a decision to raise rather than a step to take.
//
// So `make` runs this at BUILD time and captures the result. That is not the
// runtime self-declaration ADR 0008 §3 rules out: the manifest is produced
// before installation, read by the host at install and at boot, and fixed from
// then on. A RUNNING child is still never asked what it serves.
func emitManifest(out io.Writer) error {
	declaration := sdkmanifest.Manifest{
		Capabilities:  []subprocess.CapabilityRequest{{Name: capability.MCPReach, Reason: "Call the declared Tangent tools used by this integration", Metadata: json.RawMessage(`{"tools":["tangent.session_create","tangent.session_advance","tangent.surface_get","tangent.interaction_cancel"]}`)}},
		SchemaVersion: sdkmanifest.SchemaVersion,
		Config:        sdkmanifest.Config{Fields: map[string]sdkmanifest.Field{"api_url": {Type: "string", Label: "Tesseract API URL", Env: "TANGENT_TESSERACT_API_URL"}, "namespaces": {Type: "string", Label: "Review namespaces", Env: "TANGENT_TESSERACT_NAMESPACES"}}, Secrets: map[string]sdkmanifest.Secret{"token": {Label: "Tesseract token", Env: "TANGENT_TESSERACT_TOKEN"}}},
		Runtime:       sdkmanifest.Runtime,
		Hosts:         map[string]sdkmanifest.HostRange{"tangent": {Min: "1.0.0", Max: "1.99.99"}},
		ID:            tesseract.ID,
		Name:          tesseract.New().Name(),
		Description:   tesseract.New().Description(),
		Version:       tesseract.New().Version(),
		Protocol:      subprocess.ProtocolVersion,
		Tools: []sdkmanifest.Tool{
			{
				Name:        tesseract.OpenTool,
				Effect:      "write",
				Description: tesseract.OpenToolDescription,
				InputSchema: tesseract.OpenToolSchema,
			},
			{
				Name:        tesseract.SyncTool,
				Effect:      "write",
				Description: tesseract.SyncToolDescription,
				InputSchema: tesseract.SyncToolSchema,
			},
		},
	}
	extension := tangentExtension{SchemaVersion: 1, Kinds: []kindRef{{Kind: tesseract.EnvelopeType, Version: "0.3", Package: "tangent.appboard"}},
		Routes: []routeDecl{{
			Method:     http.MethodPost,
			Path:       tesseract.SyncPath,
			Capability: string(tangentplugin.CapabilityDraft),
		}},
	}
	for _, tool := range declaration.Tools {
		extension.MCPTools = append(extension.MCPTools, tool.Name)
	}
	raw, err := json.Marshal(extension)
	if err != nil {
		return err
	}
	declaration.Tangent = raw
	artifact, err := executableArtifact("bin/tangent-plugin-tesseract")
	if err != nil {
		return err
	}
	declaration.Artifact = artifact
	declaration.Server = sdkmanifest.Server{Runtime: "binary", Entry: "bin/tangent-plugin-tesseract", Engines: map[string]sdkmanifest.HostRange{"binary": {Min: "1.0.0", Max: "1.99.99"}}}
	return sdkmanifest.Encode(out, declaration)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--manifest" {
		if err := emitManifest(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "tangent-plugin-tesseract: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := subprocess.Serve(&served{}); err != nil {
		fmt.Fprintf(os.Stderr, "tangent-plugin-tesseract: %v\n", err)
		os.Exit(1)
	}
}
