# tangent-plugins

First-party plugins for [Tangent](https://github.com/hollis-labs/tangent).

> **Pre-release.** Built in the open: this README describes what exists today,
> not a pitch for what's planned. Interfaces and behavior change without notice.

A plugin is its own program. Tangent spawns it as a subprocess and talks to it
over the [plugin-sdk](https://github.com/hollis-labs/plugin-sdk) subprocess
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

Each plugin's README documents its tools. Tangent's
[`docs/mcp-integration.md`](https://github.com/hollis-labs/tangent/blob/main/docs/mcp-integration.md) links the installed-plugin guides.

## Build and install

```bash
make dist
tangent plugin install "$PWD/dist/tangent.plugin.torque"   # and the others
tangent plugin list
```

Restart `tangent` to load what you installed. From a Tangent checkout,
`make install-plugins` installs the release of these plugins that Tangent pins.

A plugin inherits the environment of the `tangent` that spawns it, so set its
variables where `tangent` runs:

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
github/, runner/, tesseract/         the same shape
dist/tangent.plugin.<p>/    built, installable plugin directory (gitignored)
```

`make test`, `make lint` and `make dist` at the root iterate every plugin listed
in `PLUGINS`. Adding a plugin means adding its directory name there. Each plugin
is released under its own module tag (`torque/v0.1.0`).

This mirrors [cerberus-plugins](https://github.com/hollis-labs/cerberus-plugins)
with one deliberate difference: the manifest comes from `<binary> --manifest`
and the entrypoint sits beside `plugin.yaml`, because that is Tangent's host
convention. Cerberus uses `write-dist` and a `bin/` directory.

## Writing a plugin

A plugin imports two packages from Tangent and one from the SDK:

```go
tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"            // surfaces, capability, plugin.yaml, ToolCaller
              "github.com/hollis-labs/tangent/pkg/plugin/hostclient" // Tangent's tool surface over MCP
              "github.com/hollis-labs/plugin-sdk/subprocess"         // the wire
```

Never anything under `tangent/internal/`. Go refuses it from another module
anyway; if a plugin needs something from there, the public surface is missing a
piece, so raise it against Tangent rather than reaching in. Tangent's
[`docs/writing-a-plugin.md`](https://github.com/hollis-labs/tangent/blob/main/docs/writing-a-plugin.md)
is the guide.

Each `go.mod` requires a released `github.com/hollis-labs/tangent` and carries no
`replace`. To develop a plugin against a local Tangent checkout, use a Go
workspace, which is gitignored here:

```bash
go work init ./torque ../tangent   # adjust the path to your checkout
```

CI runs with `GOWORK=off`, so a workspace never changes what CI builds.

## License

MIT — see [LICENSE](LICENSE).
