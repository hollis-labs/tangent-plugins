# tangent-plugins

First-party plugins for [Tangent](https://github.com/hollis-labs/tangent).

> **Pre-release.** Built in the open: this README describes what exists today,
> not a pitch for what's planned. Interfaces and behavior change without notice.

A plugin is its own program. Tangent spawns it as a subprocess and talks to it
over the [plugin-sdk](https://github.com/hollis-labs/libs/tree/main/plugin-mcp/plugin-sdk) subprocess
wire; the plugin contributes MCP tools and browser routes, and calls back into
Tangent as an ordinary local MCP client. Plugins live here, not in Tangent,
because each carries its own application's dependency and ships on its own
schedule. Tangent's binary stays domain-free.

## Plugins

| Plugin | ID | What it does |
|---|---|---|
| `github` | `tangent.plugin.github` | PR review rooms with body, agent notes, revision-pinned approval and merge, through the operator's gh authentication. |
| `runner` | `tangent.plugin.runner` | Supervises an agent process (or a Tether session) and carries its turns to Tangent's agent-turns inbox and the operator's answers back. |
| `tesseract` | `tangent.plugin.tesseract` | A review board over Tesseract records: deprecate in place, hand promotions and rewords back to the agent. |
| `torque` | `tangent.plugin.torque` | A board over Torque's task list; Sync pushes staged status moves back without an agent turn. |
| `chat-agent` | `tangent.plugin.chat-agent` | Inert chat thin-client scaffold with host settings and read-only availability; backend, binding, UI and default-off install remain gated. |
| `messaging` | `tangent.plugin.messaging` | Serial committed-channel intake with a private SQLite ledger, bounded stateless stages and immutable prepared Tangent publications. |

The [chat-agent guide](chat-agent/README.md) documents its source-only lifecycle,
settings and default-off host prerequisite. Its public Tangent source pin is
separate from installed release pins.

Each plugin's README documents its tools. Tangent's
[`docs/mcp-integration.md`](https://github.com/hollis-labs/tangent/blob/main/docs/mcp-integration.md) links the installed-plugin guides.

## Protocol-2 source integration

The original four modules use the SDK from the released
`github.com/hollis-labs/libs/plugin-mcp` module at v0.1.1, use protocol 2 and acknowledge
capability contract 1. Init and generated manifests report `0.2.0-dev` until
the first protocol-2 release tag. They require a protocol-2 host with SDK
manifest-v2 support; legacy Tangent cannot load these bundles. The SDK accepts finite forward-call `context` budgets, including Init, and
retains strict decoding and cancellation handling. Accepted embedded runner
sessions belong to the runner engine, not the completed launch request. Startup
still observes caller cancellation; Stop and Unload cancel and reap owned
embedded children within a finite cleanup budget. Delegated sessions remain
owned by Tether.
No installed-plugin version pin, existing release, installation or deployment
changes here. The new [messaging module](messaging/README.md) uses the public
1.1 turn contract for stage metadata and nonreplyable ordinary publications;
its activation and reply delivery are separate work.

`--manifest` emits canonical JSON (valid `plugin.yaml` content), including the
native `bin/` entry and its SHA-256 artifact inventory. See
[the manifest contract](docs/manifest-v2.md) for the Tangent extension and the
host contracts required before activation.

## Build and install

```bash
make dist
tangent plugin install "$PWD/dist/tangent.plugin.torque"   # and the others
tangent plugin list
```

Restart `tangent` to load what you installed. From a Tangent checkout,
`make install-plugins` installs the release of these plugins that Tangent pins.

The current plugin implementation reads these environment variables. The manifest
declares settings and secret references for host review; it does not implement
configuration delivery, a secret broker or permission to inherit the host environment:

| Variable | Plugin | Default |
|---|---|---|
| `TANGENT_TORQUE_API_URL` | torque | `http://127.0.0.1:8990` |
| `TANGENT_TESSERACT_API_URL` | tesseract | `http://127.0.0.1:8089` |
| `TANGENT_TESSERACT_TOKEN` | tesseract | unset (no auth header) |
| `TANGENT_TESSERACT_NAMESPACES` | tesseract | unset: a review must name its namespaces |

## Layout

One directory per plugin, each its own Go module:

```
torque/                     module github.com/hollis-labs/tangent-plugins/torque
  cmd/tangent-plugin-torque/  entrypoint; `--manifest` prints its plugin.yaml
  internal/torque/            the plugin
  Makefile                    test / lint / build / dist
github/, runner/, tesseract/, messaging/, chat-agent/  the same shape
dist/tangent.plugin.<p>/    built, installable plugin directory (gitignored)
```

`make test`, `make lint` and `make dist` at the root iterate every plugin listed
in `PLUGINS`. Adding a plugin means adding its directory name there. Each plugin
is released under its own module tag (`torque/v0.1.0`).

Each bundle contains `plugin.yaml` beside `bin/tangent-plugin-<p>`. The manifest
hashes the already-built native executable. Hosts must review the declaration
and verify the same immutable payload before spawning it.

## Writing a plugin

A plugin uses public Tangent surfaces and SDK subprocess/manifest packages:

```go
tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"            // surfaces, participant capability, ToolCaller
              "github.com/hollis-labs/tangent/pkg/plugin/hostclient" // Tangent's tool surface over MCP
              "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"         // the wire
              "github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/manifest"           // declarations
```

Never anything under `tangent/internal/`. Go refuses it from another module
anyway; if a plugin needs something from there, the public surface is missing a
piece, so raise it against Tangent rather than reaching in. Tangent's
[`docs/writing-a-plugin.md`](https://github.com/hollis-labs/tangent/blob/main/docs/writing-a-plugin.md)
is the guide.

The original four `go.mod` files require the published Tangent v0.17.0 release and
`libs/plugin-mcp` v0.1.1, with no `replace`. Their public handler types use the
same SDK package. This replaces the temporary adoption-candidate dependency;
module pins do not install a daemon or activate plugins. To develop against a
local Tangent checkout, use a Go workspace, which is gitignored here:

```bash
go work init ./torque ../tangent   # adjust the path to your checkout
```

CI runs with `GOWORK=off`, so a workspace never changes what CI builds.

## License

MIT — see [LICENSE](LICENSE).
