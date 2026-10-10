# Messaging consumer

`tangent.plugin.messaging` is a manifest-v2/protocol-2 subprocess plugin. It
reads explicitly configured Tether channels and enqueues immutable original
text plus bounded stage metadata through public `tangent.turns_enqueue` 1.2.
It uses Tangent v0.19.0's public contract, the published `libs/message-pipeline`
v0.1.0 library and builtin
[stateless summarizer](internal/summarizer/README.md).

The source does not install or enable the plugin, change routing, start agents,
change JEV/key handling, or invoke follow-ups. Replies require an actual user
resolution of a locally mapped routed item; summary text is untrusted display
data, never permission to act. A configured live summarizer sends source text
to its configured gateway; synthetic tests do not prove source redaction or
provider availability. Secret redaction/retention and activation are separate
before-1.0/rollout work.

## Explicit configuration

Host-managed initialization uses manifest settings from `Init.Config`.
Choose `configuration_mode=file` with only an absolute `configuration_path`,
or `configuration_mode=settings` with explicit `endpoint_ref`, `tether_address`,
`caller_urn`, `channels_json`, `stages_json`, `request_timeout_ms`,
`reconnect_min_ms`, `reconnect_max_ms`, and `history_limit`. The lists are JSON
strings decoded and validated by this plugin, never by the host. Optional
`instruction_path` selects the summarizer document; supplying it together with
a stage's `instruction_path` refuses ambiguity. The single builtin stage and
all original identity/finite-bound constraints still apply. File mode refuses
settings-mode fields, and settings mode refuses a file path. Conditional
requirements are checked before the plugin acquires its ledger or calls services.

`tether_token` is a separate write-only host secret passed only at runtime.
Omitting it in a supplied host configuration means an empty token; it never
falls back to the environment. The host client separately requires its explicit
`TANGENT_MCP_URL` endpoint. No setting grants capabilities or enables a plugin.

Only an entirely empty `Init.Config` selects standalone mode: set
`TANGENT_MESSAGING_CONFIG` to an absolute JSON file and optionally `TETHER_TOKEN`.
A legacy supplied `configuration_path` without a mode remains explicit file
mode. Partial or unknown supplied settings refuse rather than using ambient
inputs. Manifest environment names declare inputs, not inheritance permission.
The following is the file-mode document (settings mode carries the same typed
values through the declared scalar fields):

```json
{
  "schema_version": 1,
  "endpoint_ref": "owned-tether",
  "tether_address": "unix:/absolute/owned/tether.sock",
  "caller_urn": "msg://agent/local/owned-consumer",
  "channels": ["human-review"],
  "request_timeout_ms": 15000,
  "reconnect_min_ms": 250,
  "reconnect_max_ms": 5000,
  "history_limit": 10,
  "stages": [{
    "id": "summarize",
    "priority": 0,
    "endpoint_url": "unix:/absolute/owned/tether.sock",
    "caller_id": "owned-messaging-consumer",
    "timeout_ms": 15000
  }]
}
```

`endpoint_url` is an explicit HTTP(S) **base URL** or Unix socket address. The
summarizer appends `/ai/chat`; it is not a completed route URL. Optional
`provider_hint`, `model_hint` and absolute `instruction_path` are snapshotted
once at startup. An absent instruction path selects the bundled document.
Timeout zero selects the documented 15-second summarizer default. Transport
bounds are explicit and finite; channel names are unique and limited to eight.
Unknown stages, malformed/duplicate JSON keys, invalid Unicode, missing source
identity and embedded URL credentials refuse initialization.

One builtin stage is registered in this MVP. The portable stage port supports
future explicitly registered processors; filters and follow-ups remain separate.

## Intake, persistence and replay

The host supplies an existing owner-only `Init.DataDir`. The ledger requires
mode 0700 for that directory, creates its SQLite/owner files with mode 0600 and
holds an exclusive owner lock. Data never falls back to cache, bundle or a host
application database. The host's installed-plugin snapshot creates the private
data root separately from the immutable executable. Permissions and locks
protect ordinary local custody; they do not isolate a hostile process with the
same OS user. Only one incarnation can execute stages for this DataDir.

Identity is the exact endpoint/channel/publication-ID tuple. Actual source
sequence orders progress; message count, timestamp, received high-water hints
and runtime turn IDs do not substitute for it. `output_id` is additional
attribution. A real routed notice needs its original kind, matching session
sender/thread and actual turn/session labels; final maps to terminal. An
ordinary publication is always a nonreplyable checkpoint, including when
optional supplied runtime labels are retained. Missing labels stay absent.

The original received text and decoded source envelope are captured privately
before processing. Saved outcomes (including stage failures) and exact prepared
enqueue bytes are reused on retry. The sink item receipt and cursor commit in
one SQLite transaction. A crash or ambiguous response after the sink commits
retries the saved publication key/request, rather than generating another
summary or payload. Provider exactly-once is not claimed across a crash between
provider acceptance and local stage persistence.

History is read serially per channel; reconnect uses the locally committed
cursor and passes a pointer to zero when no cursor exists. Sequence gaps are
legal. Unload cancels the loaded incarnation, joins channel readers, active
stage executions, reply polling and HTTP callbacks, then closes ports and the
ledger. Finite Load-call completion does not cancel workers. A cleanup deadline
retains ownership and reports failure rather than claiming a successful join.

A genuine source purge records a retention outcome without processing or
recreating its body. Previously captured originals remain in this private
ledger; local retention policy is separate. Other malformed/oversized input is
not truncated or silently skipped: a bounded refusal identity/digest is saved,
health reports admission refusal, and the cursor remains pending. This is not
a lossless archive of refused raw payloads. Source replay or a real purge is
needed to resolve the pending publication.

## User-resolved replies

Only settled local publication-to-item mappings can supply a reply target.
Previously saved contract 1.1 requests replay with their original bytes and
matching receipt version; the upgrade does not normalize or rewrite them.
The configured endpoint/channel scope is snapshotted; retained records from
other sources cannot dispatch through a replacement client. Ordinary
publications remain nonreplyable, including ones with supplied runtime labels.
The worker awaits the real host resolution only for a never-prepared item.
It records the exact body, caller, interrupt choice and idempotency key before
the dedicated public Tether `Reply` call. Initial interrupt comes from the
immutable user resolution, with an absent flag meaning false; runtime reply
and interrupt support are checked for the actual source session.

Queue acceptance and a duplicate receipt do not mean delivered. The receipt
is persisted before polling `ReplyDelivery`; actual delivered state is saved
before `turn_ack`. An ambiguous send repeats the same saved body/key/flag.
A saved receipt prevents another send. After host acknowledgement succeeds but
its local write fails, the saved attempt resumes acknowledgement even when
Await no longer returns that row. Absence itself never proves acknowledgement.
Terminal refusal/undeliverable outcomes remain preserved and unacknowledged.

`GET /api/plugins/messaging/delivery?item_id=...` returns body-free status and
capabilities under the host's participant view guard. It sends no reply and
does not acknowledge anything. `POST /api/plugins/messaging/retry` is a new
explicit user action under the draft guard: it supplies `item_id`, the saved
`expected_version`, a fresh `action_id` and an explicit `interrupt` flag. Only
the latest terminal failed attempt can be superseded; the old attempt stays
intact. Repeating the same click uses the saved action/key. Pending, unknown or
delivered attempts cannot be replaced. Polling never manufactures user retries.
One controller serializes worker and HTTP advancement; shutdown closes admission,
cancels requests and joins callbacks before releasing private ledger custody.

These capability/participant checks retain the accepted single-owner MVP
boundary. Tether's full reply-to-sender authorization policy is separate
before-1.0 work; source attribution and configured caller labels are not proof
of authenticated human identity. No live reply or interrupt is exercised by
synthetic fixtures.

## Checks and packaging

From this module, `make test`, `make lint` and `make dist` use the public pins.
Tests use private synthetic SQLite, channel/MCP and HTTP fixtures. They cover
immutable stage/result snapshots, crash-boundary rollback, saved-request replay,
source admission, reconnect cursors, reply receipt/delivery/ack recovery,
explicit retry rollback and lifecycle joins. Packaging only builds
the binary and emits a manifest whose artifact inventory binds that binary.
