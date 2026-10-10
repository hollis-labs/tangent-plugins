# Tangent plugin declarations

The build emits SDK manifest schema 2 as canonical JSON in `plugin.yaml`.
The declaration has identity, strict SemVer plugin version (`0.2.0-dev`),
protocol 2, runtime `subprocess`, a native server entry under `bin/`, and the
SDK artifact inventory/tree digest. Tool schemas are inline objects, not JSON
strings. Effects are `read`, `write` or `destructive`: runner health reads,
runner stop is destructive, and the other tools write. Opening a board or review
writes Tangent interaction state even when the application data is only read.

Compatibility is against public contracts, not the Tangent application version.
These bundles require Tangent plugin contract `1.0.0` through `1.99.99` and native
binary-runner contract `1.0.0` through `1.99.99`. The host adoption must expose and
check those explicit contracts; unresolved contracts refuse activation. The
extension has its own `schema_version: 1`. Unknown extension versions/fields,
unresolved kinds and unbound tool references must fail before registration.

## Tangent extension

```json
{
  "schema_version": 1,
  "kinds": [{"kind": "tangent.app-board", "version": "0.3", "package": "tangent.appboard"}],
  "routes": [{"method": "POST", "path": "/api/plugins/torque-board/sync", "capability": "draft"}],
  "mcp_tools": ["tangent.torque_open_board", "tangent.torque_sync_board"]
}
```

`kinds` references exact host-approved definition versions/packages. These
plugins do not own, republish or supply renderer assets for those definitions.
Torque and Tesseract use `tangent.app-board@0.3` from `tangent.appboard`; GitHub
uses `tangent.external-review@0.1` from `tangent.review`. Runner has no kind
reference. Tangent resolves references through its approved definition catalog,
not from executable self-declarations. Definition versions retain the catalog's
version syntax; they are independent of the strict SemVer plugin version.

`routes` preserves existing method/path and participant capability checks:
board sync and runner launch require `draft`, runner sessions and GitHub state
require `view`, GitHub actions require `resolve`. The host must enforce owner
path admission, participant authorization, request limits and cookie isolation.
A manifest does not grant route authority.

`mcp_tools` contains names referencing the common `tools` list. That list alone
owns description, input schema, effect and optional MCP annotations. No second
schema or effect is declared in the Tangent block. Host admission resolves and
publishes bindings only after validation and policy approval.

## Requests and configuration

Each manifest requests `mcp.reach` with host-owned metadata `{"tools": [...]}`
listing the exact Tangent callback tool names used by that plugin. This is a
review input, not a grant or the SDK's resolved scope transport. Host adoption
must resolve approved tools to pinned definitions and bounded scopes, apply
caller policy, and refuse unsupported required requests. It must not interpret
this metadata as ambient MCP access. Current callbacks still use the existing
hostclient; scoped stdio dispatch belongs to the host authority adoption.

Torque declares its API URL field. Tesseract declares API URL/namespaces fields
and a token secret reference, without a secret value or default. Runner and
GitHub have no host-managed settings today; GitHub's `gh` authentication remains
external application custody. These declarations do not implement secret
resolution or broad environment inheritance. No host bearer or raw secret is
placed in Init.Config by this migration.

## Build and review

`make dist` builds a fresh staged bundle with the native executable under `bin/`
before invoking its `--manifest` build flag. Only a successful build/declaration
replaces the generated distribution directory; repeat builds remove stale files,
including legacy root-level binaries. A failed build leaves the previous bundle
in place. Distribution directories are generated output, not installed plugins
or writable operator data. `--manifest` hashes its own executable without
initializing clients or running tools, then uses SDK `TreeDigest` and `Encode`. Each current
bundle has only the native payload; adding assets requires inventorying all
regular payload files. `plugin.yaml` is excluded from the artifact digest.
Review must pin the manifest separately along with identity/version, payload
digest and grants; hosts verify the immutable reviewed bundle before launch.

The SDK remains pinned to the published `libs/plugin-mcp` v0.1.1. No installed
release pin is advanced. These bundles are source integration artifacts until
the approved SDK release and Tangent decoder/compatibility adoption land.
The messaging module now ships its own reviewed declaration and host-managed
scalar configuration (see [its guide](../messaging/README.md)).

## Chat-agent source scaffold

The [chat-agent module](../chat-agent/README.md) contributes
`tangent.chat_agent_status` with read effect and
`GET /api/plugins/chat-agent/status` with participant View. Its manifest has no
kinds, browser assets, hooks, capability requests or secrets. Reviewed configuration
contains only backend selection, opaque conversation/agent references and UI
preferences. The host owns persistence and enable intent; the child snapshots
Init.Config and writes no conversation content or private files. Status reports
backend/binding/UI unavailability rather than pretending those contracts exist.
The chat-agent guide includes the existing host documentation gate's
`requires-tool` marker. Stage that guide under the host's `docs/plugins/` when
adopting the bundle in a future source-smoke/release roster update; the host
includes such manuals only when their declared tool actually ships.

The existing host's unknown-ID default is enabled. A reviewed public default-off
install contract is still required before claiming opt-in installation acceptance;
a manifest cannot override that host policy. Source-only packaged lifecycle tests
use test-owned host/data and do not install or enable live plugins.
