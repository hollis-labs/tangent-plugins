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
	"fmt"
	"io"
	"net/http"
	"os"

	"gopkg.in/yaml.v3"

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
		ID:          tesseract.ID,
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
	manifest := tangentplugin.Manifest{
		ID:          tesseract.ID,
		Name:        tesseract.New().Name(),
		Description: tesseract.New().Description(),
		Version:     tesseract.New().Version(),
		Protocol:    subprocess.ProtocolVersion,
		Entrypoint:  "tangent-plugin-tesseract",
		Tools: []tangentplugin.ToolDecl{
			{
				Name:        tesseract.OpenTool,
				Description: tesseract.OpenToolDescription,
				InputSchema: string(tesseract.OpenToolSchema),
			},
			{
				Name:        tesseract.SyncTool,
				Description: tesseract.SyncToolDescription,
				InputSchema: string(tesseract.SyncToolSchema),
			},
		},
		Routes: []tangentplugin.RouteDecl{{
			Method:     http.MethodPost,
			Path:       tesseract.SyncPath,
			Capability: string(tangentplugin.CapabilityDraft),
		}},
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
