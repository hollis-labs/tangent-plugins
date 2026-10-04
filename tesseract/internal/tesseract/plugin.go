// Package tesseract is the compiled-in plugin that puts Tesseract's records on
// a Tangent board for review and disposition (CW-20260910-0054).
//
// It is the second application plugin, and it exists to be a DIFFERENT shape of
// problem from the first: a different application, and a workflow whose
// consequential act is not a status transition an API will perform. What that
// difference found is written down in sync.go, because it moved where the
// mechanical/authored line falls.
//
// # Who calls what
//
// Settled 2026-09-09 (Tesseract `agents_drive_tangent_apps_are_called`), and
// every line here is a consequence of it:
//
//   - An **agent** is the caller. It calls one tool and passes a recall. It
//     never shapes a payload.
//   - This **plugin** holds the Tesseract dependency, in one file. Userland is
//     where a dependency belongs.
//   - **Tesseract** is unchanged: a store that is called and returns. It never
//     initiates a Tangent interaction.
//   - **Tangent core** stays domain-free. Nothing outside this package knows
//     Tesseract exists — an explicit condition on this task, checkable with one
//     grep.
//
// The board itself is `tangent.app-board`, a domain-free kind the host ships.
// board.go is where a Tesseract lifecycle status
// becomes a column and a tag becomes a badge, and that mapping is mechanical
// from end to end — a model asked to shape this payload would be doing a `for`
// loop expensively and occasionally wrong.
//
// # The compiled-in tradeoff, stated plainly
//
// ADR 0007 §6 says the test this pattern must keep passing is that no write to
// the owning application originates in Tangent's process. Compiled in, this
// plugin's one Tesseract write — a deprecation — DOES originate there. That is
// the exception §6 records as a property of the compiled-in host rather than
// per plugin, and this package doc is how "who is inside the exception" stays a
// grep rather than a memory. `CW-20260910-0034` (plugin-sdk subprocess mode) is
// the real fix and closes it for every plugin at once.
//
// Note what the exception does NOT establish. Tangent holds no Tesseract
// credential and issues no identity, so a write this plugin makes is authorized
// by Tesseract's own policy and by nothing this host vouched for
// (`CW-20260910-0045` is where that becomes enforceable). The write surface is
// deliberately one call wide, which is the cheapest way to keep that true.
//
// # What a sync is
//
// Two directions in one press, and neither of them is Tangent deciding
// anything:
//
//   - **Push.** The participant stages a card into another column, or writes a
//     note on it. That is recorded in the interaction's draft revision — staged
//     intent, not a decision — until they press Sync. The press is the
//     decision. This plugin then applies the one disposition that is mechanical
//     and hands the rest back to the agent as a work list.
//   - **Pull.** Tesseract is re-recalled with the board's original recall and
//     the board is replaced with fresh cards.
//
// The button reaches this plugin over its own HTTP route (ADR 0007 §4), so the
// mechanical half costs no agent turn.
package tesseract

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	plugin "github.com/hollis-labs/plugin-sdk"
	"github.com/hollis-labs/plugin-sdk/subprocess"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// ID is the plugin identifier, and the name that appears in host logs.
const ID = "tangent.plugin.tesseract"

// Tool names. Both are in the `tangent.` namespace because every tool this
// build serves is, and the documentation gate matches that spelling — a plugin
// tool is a shipped tool and is documented like one.
//
// They are named for what they do rather than for the kind they render on. The
// board is a review surface over Tesseract, not a generic board, and an agent
// choosing between `tangent.torque_open_board` and this one is choosing between two
// applications rather than two boards.
const (
	// OpenTool opens a review board. One call, a recall in, a room URL out.
	OpenTool = "tangent.tesseract_review"
	// SyncTool is the same sync the button performs, for an agent that wants
	// it in a turn it is already spending — and the way an agent collects the
	// work list the participant left.
	SyncTool = "tangent.tesseract_review_sync"
)

// SyncPath is the plugin-served route the board's Sync button POSTs to. It is
// under tangentplugin.RoutePrefix, which is what keeps it from colliding with
// /api/hitl, /api/rooms, /api/channels, /api/effects or the SPA.
const SyncPath = tangentplugin.RoutePrefix + "tesseract-review/sync"

// EnvelopeType is the domain-free kind this plugin supplies content to. It is
// host plumbing, installed by extensions.RegisterAll and owned by nothing in
// userland: the kind describes a board, and a board is not a Tesseract concept.
const EnvelopeType = "tangent.app-board"

// Plugin is the Tesseract review board adapter.
type Plugin struct {
	client *Client
	// namespaces is the board's default recall scope, from NamespacesEnv.
	// Empty means a caller must name one.
	namespaces []string

	mu   sync.Mutex
	host Host
	// caller is set only in subprocess mode; see WithToolCaller.
	caller tangentplugin.ToolCaller
	status plugin.PluginStatus
}

// Host is the narrow slice of the Tangent plugin host this plugin needs. It is
// declared here, at the consumer, so the plugin depends on what it uses rather
// than on the concrete host type.
type Host interface {
	RegisterMCPTool(tangentplugin.MCPTool) error
	RegisterHTTPRoute(tangentplugin.HTTPRoute) error
	Tools() (tangentplugin.ToolCaller, error)
}

// New returns an unloaded plugin pointed at the local Tesseract.
//
// The environment is read here rather than through the host: GetConfig is
// deliberately unimplemented, and a plugin reading its own environment is
// userland doing userland's job.
func New() *Plugin {
	return &Plugin{
		client:     NewClient(os.Getenv(BaseURLEnv), os.Getenv(TokenEnv)),
		namespaces: parseNamespaces(os.Getenv(NamespacesEnv)),
	}
}

// NewWithClient returns a plugin over a supplied Tesseract client. Tests use
// it; production goes through New.
func NewWithClient(client *Client) *Plugin { return &Plugin{client: client} }

func (p *Plugin) ID() string      { return ID }
func (p *Plugin) Name() string    { return "Tesseract review" }
func (p *Plugin) Version() string { return "0.2.0-dev" }

func (p *Plugin) Description() string {
	return "Puts Tesseract records that need dispositioning on a tangent.app-board: one agent " +
		"call opens it, and a sync button deprecates what the participant retired and hands " +
		"promotions and reword requests back as a work list, without an agent turn."
}

// Dependencies returns none.
//
// It used to name `tangent.plugin.appboard`, and that declaration described the
// wrong thing: `tangent.app-board` is host plumbing, installed by
// extensions.RegisterAll before any plugin loads (CW-20260911-0036). A plugin's
// dependency list is for plugins it needs loaded first, and a build that
// shipped this one without the host's own kind is not a build — it is a
// compile error.
func (p *Plugin) Dependencies() []string { return nil }

// Load registers the two tools and the one route.
//
// The host is Tangent's, not the SDK's base contract: RegisterMCPTool and
// RegisterHTTPRoute are extensions, which the SDK explicitly names as the way a
// host adds its own registration surfaces. A plugin that finds neither is
// running on a host that cannot serve it, and saying so at load beats
// discovering it when someone presses a button.
func (p *Plugin) Load(host plugin.Host) error {
	if host == nil {
		return fmt.Errorf("tesseract: host is nil")
	}
	tangentHost, ok := host.(Host)
	if !ok {
		return fmt.Errorf(
			"tesseract: host does not offer Tangent's plugin registration surfaces "+
				"(RegisterMCPTool, RegisterHTTPRoute); got %T", host)
	}

	if err := tangentHost.RegisterMCPTool(tangentplugin.MCPTool{
		Name:        OpenTool,
		Description: OpenToolDescription,
		InputSchema: OpenToolSchema,
		Handler:     p,
	}); err != nil {
		return p.failLoad(err)
	}
	if err := tangentHost.RegisterMCPTool(tangentplugin.MCPTool{
		Name:        SyncTool,
		Description: SyncToolDescription,
		InputSchema: SyncToolSchema,
		Handler:     p,
	}); err != nil {
		return p.failLoad(err)
	}
	// `draft` rather than `submit`: ADR 0004 §7's participant row is
	// {view, draft, resolve, cancel}, and a sync is the participant applying
	// what they staged — authoring, not settling. The board interaction is
	// still pending afterwards, which is the test for whether `resolve` would
	// have been the right word.
	if err := tangentHost.RegisterHTTPRoute(tangentplugin.HTTPRoute{
		Method:     http.MethodPost,
		Path:       SyncPath,
		Capability: tangentplugin.CapabilityDraft,
		Handler:    p,
	}); err != nil {
		return p.failLoad(err)
	}

	p.mu.Lock()
	p.host = tangentHost
	p.status = plugin.PluginStatus{Loaded: true, Enabled: true, LoadedAt: time.Now().UTC()}
	p.mu.Unlock()
	return nil
}

func (p *Plugin) failLoad(err error) error {
	p.mu.Lock()
	p.status.LastError = err.Error()
	p.mu.Unlock()
	return err
}

// Unload drops the plugin's own state. It unregisters nothing, which is the
// host's stated contract (tangent's internal/pluginhost/lifecycle.go) and not this
// plugin's decision: this host's registries are boot-time, and a plugin that
// should not be present is one that is not loaded at boot.
func (p *Plugin) Unload() error {
	p.mu.Lock()
	p.host = nil
	p.status = plugin.PluginStatus{}
	p.mu.Unlock()
	return nil
}

func (p *Plugin) Status() plugin.PluginStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// tools resolves the host's tool caller at dispatch time.
//
// Not at Load: plugins load before the MCP server exists, because a
// plugin-contributed envelope kind has to be in the registry the MCP server
// reads. Holding a handle from Load would mean holding nil.
func (p *Plugin) tools() (tangentplugin.ToolCaller, error) {
	p.mu.Lock()
	host, caller := p.host, p.caller
	p.mu.Unlock()
	if caller != nil {
		return caller, nil
	}
	if host == nil {
		return nil, fmt.Errorf("tesseract: plugin is not loaded")
	}
	return host.Tools()
}

// WithToolCaller makes this plugin reach Tangent through the supplied caller
// instead of through a host handle, which is how it runs OUT OF PROCESS.
//
// A subprocess plugin has no host handle: the plugin wire is host-initiated and
// carries no way to ask Tangent for anything (`libs/plugin-sdk/subprocess`, "the
// subprocess does not initiate requests"). So a child is given an
// `pkg/plugin/hostclient` instead — an MCP client against Tangent's own
// `/mcp`, which tangent's `internal/pluginhost/tools.go` already defines the in-process
// caller as being equivalent to.
//
// Nothing else about this plugin changes between the two modes. The mapping,
// the tools and the sync are the same code; only where the tool calls go
// differs, which is what makes the migration a change of wiring rather than a
// rewrite.
func (p *Plugin) WithToolCaller(caller tangentplugin.ToolCaller) *Plugin {
	p.mu.Lock()
	p.caller = caller
	p.mu.Unlock()
	return p
}

// MCPCallTool services both of this plugin's tools.
func (p *Plugin) MCPCallTool(
	ctx context.Context,
	request subprocess.MCPCallRequest,
) (subprocess.MCPCallResult, error) {
	switch request.ToolName {
	case OpenTool:
		var input OpenInput
		if err := decodeArguments(request.Arguments, &input); err != nil {
			return subprocess.MCPCallResult{}, err
		}
		result, err := p.Open(ctx, input)
		if err != nil {
			return subprocess.MCPCallResult{}, err
		}
		return jsonResult(result)
	case SyncTool:
		var input SyncInput
		if err := decodeArguments(request.Arguments, &input); err != nil {
			return subprocess.MCPCallResult{}, err
		}
		result, err := p.Sync(ctx, input)
		if err != nil {
			return subprocess.MCPCallResult{}, err
		}
		return jsonResult(result)
	default:
		return subprocess.MCPCallResult{}, fmt.Errorf(
			"tesseract: no handler for tool %q", request.ToolName)
	}
}

// HTTPHandle services the board's Sync button.
//
// It is the same Sync the tool performs, reached without an agent turn. The
// request has already passed the same-origin guard, the participant session and
// the ADR 0004 §7 capability check by the time it arrives — the host mounts a
// plugin route through the door every other browser route uses.
func (p *Plugin) HTTPHandle(
	ctx context.Context,
	request subprocess.HTTPRequest,
) (subprocess.HTTPResponse, error) {
	if request.Path != SyncPath {
		return subprocess.HTTPResponse{}, fmt.Errorf(
			"tesseract: no handler for route %s %s", request.Method, request.Path)
	}
	var input SyncInput
	if len(request.Body) > 0 {
		if err := json.Unmarshal(request.Body, &input); err != nil {
			return badRequest("the sync request body is not JSON: " + err.Error())
		}
	}
	if input.RoomID == "" {
		input.RoomID = request.Query["room_id"]
	}
	if input.RoomID == "" {
		return badRequest("sync needs a room_id")
	}

	result, err := p.Sync(ctx, input)
	if err != nil {
		// Returned as an error rather than a status: the host renders it as a
		// refusal the SPA can show, with this plugin's own message, which for a
		// Tesseract outage is the sentence the operator actually needs.
		return subprocess.HTTPResponse{}, err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return subprocess.HTTPResponse{}, fmt.Errorf("tesseract: encode sync result: %w", err)
	}
	return subprocess.HTTPResponse{
		Status:  http.StatusOK,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
	}, nil
}

func badRequest(message string) (subprocess.HTTPResponse, error) {
	body, _ := json.Marshal(map[string]string{"code": "invalid_request", "message": message})
	return subprocess.HTTPResponse{
		Status:  http.StatusBadRequest,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
	}, nil
}

// decodeArguments re-decodes the host's generic argument map into a typed
// input. The host has already validated it against the tool's own schema, so
// this cannot reject anything the schema admits; it is a shape conversion.
func decodeArguments(arguments map[string]any, into any) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return fmt.Errorf("tesseract: re-encode arguments: %w", err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		return fmt.Errorf("tesseract: decode arguments: %w", err)
	}
	return nil
}

func jsonResult(value any) (subprocess.MCPCallResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return subprocess.MCPCallResult{}, fmt.Errorf("tesseract: encode result: %w", err)
	}
	return subprocess.MCPCallResult{Content: encoded}, nil
}

// Compile-time proof this satisfies the SDK contracts it registers against.
var (
	_ plugin.Plugin          = (*Plugin)(nil)
	_ subprocess.MCPHandler  = (*Plugin)(nil)
	_ subprocess.HTTPHandler = (*Plugin)(nil)
)
