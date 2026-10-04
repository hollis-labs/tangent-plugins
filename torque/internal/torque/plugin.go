// Package torque is the compiled-in plugin that carries Torque (CW-20260910-0031).
//
// It opens with one surface — Torque's task list on a Tangent board — and it is
// named for the application rather than for that surface. Chrispian,
// 2026-09-11: "it won't just be a board and that name makes it sound like a
// component like appboard. just name the plugin torque - it will carry any/all
// functionality related to torque just like tesseract will for that app." The
// package was `torqueboard` until CW-20260911-0036 renamed it.
//
// # Who calls what
//
// Settled 2026-09-09 (Tesseract `agents_drive_tangent_apps_are_called`), and
// every line here is a consequence of it:
//
//   - An **agent** — running anywhere, including inside Torque — is the caller.
//     It calls one tool and passes filters. It never shapes a payload.
//   - This **plugin** holds the Torque dependency. Userland is where a
//     dependency belongs.
//   - **Torque** is unchanged: an engine that is called and returns. It never
//     initiates a Tangent interaction.
//   - **Tangent core** stays domain-free. Nothing outside this package knows
//     Torque exists.
//
// The board itself is `tangent.app-board`, a domain-free kind: cards in
// columns with a filter bar and a detail pane. board.go is where a Torque
// status becomes a column and a Torque tag becomes a badge, and that mapping
// is mechanical from end to end — which is the point. A model asked to shape
// this payload would be doing a `for` loop expensively and occasionally wrong.
//
// # The compiled-in tradeoff, stated plainly
//
// ADR 0007 §6 says the test this pattern must keep passing is that no write to
// the owning application originates in Tangent's process. Compiled in, this
// plugin's Torque writes DO originate there. Chrispian accepted that knowingly
// for the prototype — using it sooner outweighs the purity — and
// `CW-20260910-0034` (plugin-sdk subprocess mode) is the real fix, with this
// workload as its first motivating case. ADR 0007 §6 records the exception
// rather than being quietly contradicted by it.
//
// # What a sync is
//
// Two directions in one press, and neither of them is Tangent deciding
// anything:
//
//   - **Push.** The participant stages a card into another column. That is
//     recorded in the interaction's draft revision — staged intent, not a
//     decision — and it stays there until they press Sync. The press is the
//     decision, and this plugin applies it through Torque's own API.
//   - **Pull.** Torque is re-queried with the board's original filters and the
//     board is replaced with fresh cards.
//
// The button reaches this plugin over its own HTTP route (ADR 0007 §4), so it
// costs no agent turn. Chrispian, verbatim: "add a sync button for both ways
// so I can just push the button without having to ask an agent, that's
// wasteful."
package torque

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
const ID = "tangent.plugin.torque"

// Tool names. Both are in the `tangent.` namespace because every tool this
// build serves is, and the documentation gate matches that spelling — a plugin
// tool is a shipped tool and is documented like one.
//
// The shape is `tangent.torque_<verb>_<surface>`, Chrispian's call on
// 2026-09-11, and it is chosen for the surfaces that do not exist yet:
// `torque_open_sprint` and `torque_sync_sprint` slot in beside these without a
// collision and without renaming anything. They were `tangent.torque_board` and
// `tangent.torque_board_sync`, which described a board back when a board was
// all there would be. Spending the plain verb on this surface —
// `torque_open`, `torque_sync` — reads best today and was rejected for the
// same reason: it makes the second surface's name the awkward one.
const (
	// OpenTool opens a board. One call, filters in, a room URL out.
	OpenTool = "tangent.torque_open_board"
	// SyncTool is the same sync the button performs, for an agent that wants
	// it in a turn it is already spending.
	SyncTool = "tangent.torque_sync_board"
)

// SyncPath is the plugin-served route the board's Sync button POSTs to. It is
// under tangentplugin.RoutePrefix, which is what keeps it from colliding with
// /api/hitl, /api/rooms, /api/channels, /api/effects or the SPA.
//
// It stays scoped to the board rather than following the package to
// `torque/sync`, for the reason the tool names above were chosen: this plugin
// grows surfaces, and a route named for the plugin would be the one this
// surface happened to claim first.
const SyncPath = tangentplugin.RoutePrefix + "torque-board/sync"

// EnvelopeType is the domain-free kind this plugin supplies content to. It is
// host plumbing, installed by extensions.RegisterAll and owned by nothing in
// userland: the kind describes a board, and a board is not a Torque concept.
const EnvelopeType = "tangent.app-board"

// Plugin is the Torque board adapter.
type Plugin struct {
	client *Client

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

// New returns an unloaded Torque board plugin pointed at the local Torque.
func New() *Plugin {
	return &Plugin{client: NewClient(os.Getenv(BaseURLEnv))}
}

// NewWithClient returns a plugin over a supplied Torque client. Tests use it;
// production goes through New.
func NewWithClient(client *Client) *Plugin { return &Plugin{client: client} }

func (p *Plugin) ID() string      { return ID }
func (p *Plugin) Name() string    { return "Torque board" }
func (p *Plugin) Version() string { return "0.2.0-dev" }

func (p *Plugin) Description() string {
	return "Puts Torque's task list on a tangent.app-board: one agent call opens it, " +
		"and a sync button pushes staged status changes back and pulls fresh cards down " +
		"without an agent turn."
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
		return fmt.Errorf("torque: host is nil")
	}
	tangentHost, ok := host.(Host)
	if !ok {
		return fmt.Errorf(
			"torque: host does not offer Tangent's plugin registration surfaces "+
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
//
// The tool and route it registered keep dispatching afterwards, into a plugin
// that now answers "not loaded". A readable refusal beats a surface that
// answers nothing.
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
		return nil, fmt.Errorf("torque: plugin is not loaded")
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
			"torque: no handler for tool %q", request.ToolName)
	}
}

// HTTPHandle services the board's Sync button.
//
// It is the same Sync the tool performs, reached without an agent turn. The
// request has already passed the same-origin guard, the participant session
// and the ADR 0004 §7 capability check by the time it arrives — the host mounts
// a plugin route through the door every other browser route uses.
func (p *Plugin) HTTPHandle(
	ctx context.Context,
	request subprocess.HTTPRequest,
) (subprocess.HTTPResponse, error) {
	if request.Path != SyncPath {
		return subprocess.HTTPResponse{}, fmt.Errorf(
			"torque: no handler for route %s %s", request.Method, request.Path)
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
		// Torque outage is the sentence the operator actually needs.
		return subprocess.HTTPResponse{}, err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return subprocess.HTTPResponse{}, fmt.Errorf("torque: encode sync result: %w", err)
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
		return fmt.Errorf("torque: re-encode arguments: %w", err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		return fmt.Errorf("torque: decode arguments: %w", err)
	}
	return nil
}

func jsonResult(value any) (subprocess.MCPCallResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return subprocess.MCPCallResult{}, fmt.Errorf("torque: encode result: %w", err)
	}
	return subprocess.MCPCallResult{Content: encoded}, nil
}

// Compile-time proof this satisfies the SDK contracts it registers against.
var (
	_ plugin.Plugin          = (*Plugin)(nil)
	_ subprocess.MCPHandler  = (*Plugin)(nil)
	_ subprocess.HTTPHandler = (*Plugin)(nil)
)
