# Chat Agent Scaffold

<!-- requires-tool: tangent.chat_agent_status -->

Source-only first-party thin-client scaffold for Tangent
([CW-20261009-0088](https://torque.nanite.cloud)). It packages a native protocol-2
plugin, reviewed manifest-v2 Tangent bindings, and host-managed settings. It
never starts an agent, sends a turn, reads history, contacts Nanite or a provider,
or controls a browser. Successful load means the availability service works;
it does not mean chat is ready.

## Tools and routes

| Surface | Behavior | Authority |
|---|---|---|
| `tangent.chat_agent_status` | No arguments. Reports `chat_available: false`, `agent_running: false`, configured-reference presence and presentation preferences. | Read effect; MCP hints grant nothing. |
| `GET /api/plugins/chat-agent/status` | Same projection; rejects query parameters and bodies. | Host participant View and same-origin checks. |

Status explicitly names `backend_unavailable`, `participant_binding_unavailable`
and `plugin_ui_unavailable`. Opaque references are not returned by either surface.
There are no session creation, send, approval, history, UI command or write tools.
The generated common tool declaration is the registration inventory; the Tangent
extension binds that name once. This guide carries the host documentation gate's
`requires-tool` marker. A host source-smoke adoption must stage it under
`docs/plugins/` alongside this bundle before claiming the tool in its shipped
surface; Tangent's current pinned source-smoke roster is separate and unchanged.

## Settings and custody

The host's existing settings page renders these reviewed scalar fields through
its public configuration API and kit-settings form. Save uses the host revision
CAS. Save does not change the running owner's snapshot; explicit Apply reloads
with the selected revision. Reset removes a scoped override. Enable intent is
separate from all preferences.

| Key | Type/default | Meaning |
|---|---|---|
| `backend` | select, `nanite` | Selected backend; its integration is unavailable. |
| `conversation_ref` | optional string | Opaque reference only; does not resume/create a conversation. |
| `agent_ref` | optional string | Opaque reference only; does not enroll/start an agent. |
| `rail_open` | boolean, `false` | Future presentation preference; cannot enable this plugin or render a rail. |
| `density` | select, `comfortable` or `compact` | Future presentation preference. |

References accept ASCII identifiers starting with a letter/digit, followed by
letters/digits or `._:/@-`, up to 128 bytes. They are untrusted labels, never
verified participant/conversation/browser bindings or grants. Plugin domain
validation runs at Init; the generic host form validates declared scalar types
and selects at Save. An invalid reference can save but its Apply fails explicitly
and remains pending; no prior owner is revived.

Only the host settings database persists these scalars. The plugin writes no
DataDir/CacheDir files and owns no chat, transcript, history, messages, drafts,
instructions or backend database. Init copies the provided snapshot; it does
not consult environment variables. There are no credential fields or secret
references in this scaffold: no backend is implemented to consume them. A future
backend must declare credentials through the host's reviewed secrets contract;
browser settings snapshots expose only secret presence. This module has no
keychain access, global secret fallback, host callback client or capability
requests.

## Lifecycle and explicit opt-in

Build source artifacts with `GOWORK=off make -C chat-agent dist` (or root
`make dist`). The bundle is `dist/tangent.plugin.chat-agent`, with the native
payload under `bin/`. Its build-only `--manifest` hashes the actual executable
without backend calls. This task does not update installed release pins, install
into operator data, enable a live plugin or publish a release.

Once an operator has explicitly opted in through the host, its existing
participant-guarded management actions enable, disable and reload the plugin:
`POST /api/plugin-management/tangent.plugin.chat-agent/{enable,disable,reload}`.
Disable withdraws the current owner's tool/route and stops its subprocess;
disable intent persists across host restarts. No UI contribution exists to render,
and no agent runs in either state. Saving preferences cannot enable a disabled
plugin; Apply while disabled refuses.

The source-only host pin `380db1fa961c` leaves an unknown installed ID disabled.
An ordinary `tangent plugin install <bundle>` preserves existing boolean intent,
including an explicit prior enable or disable. To opt in for the next host start,
use `tangent plugin enable tangent.plugin.chat-agent` or
`tangent plugin install --enable <bundle>`; CLI changes require host restart.
For a running host, Settings provides participant-authorized lifecycle controls
and reports desired intent separately from runtime state. An enabled-but-failed
plugin is not ready. The manifest and plugin settings cannot change enable intent.

The isolated source fixture verifies default-off installation and restarts against
that public host commit. This source pin does not adopt an installed host release:
live installation still requires release/rollout approval.

## Verification and remaining work

`GOWORK=off make -C chat-agent check` runs format, vet, pinned lint, race fixtures
and dist. Run the final author gate through `heavytest`. `make -C chat-agent smoke`
is the packaged lifecycle fixture, also exercised by the race suite and CI.
It compiles the pinned public host and plugin, installs into test-owned paths,
uses a private HOME/database/plugin root and kernel-assigned loopback port,
then verifies disabled install/restart/upgrade, settings Save while disabled and
refused Apply/Reload, explicit CLI enable, preserved true/false upgrade intent,
typed settings projection, validation, revision CAS, Save, Apply, Reset, live
disable/enable, reload and enabled restart. It checks live MCP registration,
route withdrawal and snapshot cleanup. It submits no secret fields and makes no
native keychain or provider calls. The source host uses its development HTML
placeholder: this proves settings schema/API contracts. Rendered browser form and
rail acceptance remain unresolved. Unit fixtures exercise refusal, immutable
settings snapshots, reference nondisclosure and payload tampering.

The Go module pins the public reviewed Tangent commit as a source-only
pseudo-version, with the published SDK and no `replace`; CI uses `GOWORK=off`.
The existing messaging module is a packaging/config precedent only.

Accepted host [ADR 0013](https://github.com/hollis-labs/tangent/blob/main/docs/adr/0013-tangent-chat-agent-plugin.md)
and [ADR 0014](https://github.com/hollis-labs/tangent/blob/main/docs/adr/0014-ui-commands-and-view-descriptors.md)
remain the design boundaries. Backend 0087, profile 0089, rail/UI 0092, public
plugin-host-ui 0104, the streaming bridge, and verified production
participant/conversation/browser authority are separate work. No invented UI
SDK, buffered SSE shim, synthetic production identity or ordinary write disguised
as a presentation command is included. The new module is not a production chat
release.
