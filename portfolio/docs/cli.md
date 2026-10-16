# ptrack source client

Build from `portfolio/` with `go build ./cmd/ptrack`. This source client does not
install or replace the existing `ptrack`, change agent instructions, select a
live data directory, or enable the plugin writer. The original Node CLI remains
the reference and the explicit backend before operator-controlled cutover.

Supply a version 1 config with `--config PATH` or `PTRACK_CONFIG`. There is no
implicit HOME config search. `--backend node|plugin` overrides only the transport
selection in that file. A missing config produces one typed error on stderr and
exit 1. Help works without a config.

An explicit legacy configuration uses absolute, operator-selected paths:

```json
{
  "version": 1,
  "backend": "node",
  "node": {
    "executable": "/explicit/runtime/node",
    "script": "/explicit/reference/bin/ptrack.mjs"
  }
}
```

Node mode forwards arguments, stdin, stdout, stderr and the child exit status.
Only `--config` and `--backend` with their values are removed. Portfolio scope
selectors are rejected rather than passed to a reference that would ignore
them. Existing `torque --project` is an upstream filter and is forwarded.

The plugin transport uses an explicit HTTP(S) origin:

```json
{
  "version": 1,
  "backend": "plugin",
  "plugin": {"base_url": "http://127.0.0.1:PORT"}
}
```

This example describes configuration, not a deployed endpoint. Configuration
does not supply caller authority. No bearer loader, cookies, identity headers,
service credentials, proxy from the environment or automatic Node fallback is
used. The inspected host HTTP dispatcher does not populate the portfolio
request's Identity, and the source native plugin has no verifier/store binding.
Reachable but unprovisioned calls therefore refuse. Positive acceptance tests
use a generated shadow and a test-owned verifier/courier in an injected HTTP
server; they do not demonstrate production provisioning or full host transport.

Plugin commands issue one literal POST to
`/api/plugins/portfolio/operations/<operation>` with a JSON object. Both context
and HTTP timeout are bounded to 15 seconds. Bodies are limited to 32 KiB and
responses to 1 MiB. Redirects and retries are disabled. A mutation can commit
before a 503 authority refusal or a lost response; reconcile effects before any
manual retry. Neither an error nor a timeout promises rollback or idempotency.
Host HTML, arbitrary errors and transport URLs are not copied into diagnostics.
Typed operation errors retain their code, message and details, including CAS
current-item details, within a single JSON error envelope.

The existing command surface is mapped to the shared API, including `unlink`,
the inbox subcommands and all seven Torque subcommands. Administrative `migrate`
has no HTTP/MCP grant and refuses in plugin mode; it remains available through
explicit Node mode. The CLI does not import a store or execute a domain service.
`--as` remains an attribution label where the command carries one; the verifier
owns new authors and decision attribution. A label never authorizes a call.

`list`, `search` and `board` accept `--project msg://project/...` or
`--workstream WS-...`. Torque query commands retain their existing `--project`
upstream meaning and use `--scope-project` for the portfolio selector. Portfolio
selectors are mutually exclusive and validated before dispatch. The DTO is
`{"scope":{"kind":"project|workstream","id":"..."}}`. Current main refuses
scope as unsupported; CLI mapping does not establish successful scope coverage.
Structural lists/facets remain subject to the separate 0105 contract decision.
Unsupported command/selector combinations refuse; no filtering, membership
resolver, totals reconstruction or pagination loop runs in the client.

Default success output is compact JSON and one newline. `--text` prints the
legacy six-column table for arrays of objects with IDs, the departure board
display for `board`, and otherwise indented JSON. Failures produce only
`{"error":{"code":...,"message":...,"details":...}}` on stderr and exit 1;
normal plugin success exits 0. Explicit Node children preserve their exit codes.
JSON key ordering follows the selected server; byte ordering is not a fleet
compatibility claim.

Repeated `--field` and `--set` use the last value for a key. JSON overrides those
fields. Values parse as JSON when valid, otherwise as strings. Legacy numeric flags use ECMAScript Number grammar and binary64 rounding,
including large unsigned hex/binary/octal integers. Blank values become zero;
invalid forms and nonfinite values become JSON null. Underscores, signed radix
prefixes and hex floats cannot become revisions. Domain validation may reject
that null; parity checks cover request planning, not domain acceptance. Raw JSON
data tokens retain their separate exact mapping. Scalar flags use
the last occurrence and unknown legacy value flags remain accepted. Plugin
JSON rejects duplicate keys and nesting beyond 64; Node behavior stays in the
immutable reference. HTTP mapping retains number tokens, nulls and unknown
fields without a float64 round trip. That does not prove the deployed host/SDK
wire lossless: MCP structured writes retain their separate strictly-below-2^53
policy, and decimals/exponents can normalize there. Raw-number carrier work and
caller provisioning remain separate. No raw JSON alternative or SDK patch is
introduced here.

Focused tests cover command inputs, config refusal, Unicode displays, Node
forwarding, bounds, cancellation, unreachable/lost HTTP responses, redirects,
typed errors, source-adapter attribution and missing-Identity refusal. Scratch
comparisons execute the actual private reference parser with its call seam
replaced in memory, on generated arguments only; private source is not copied
into this public module. They do not establish live fleet/agent-file acceptance.
Typed edges use `edge list DB:ID [--type TYPE]`,
`edge add|remove DB:ID TARGET --type TYPE [--to-db DB] [--rev REV]`,
`edge backlinks TARGET [--type TYPE]`, and `edge decision-gates DB:ID`.
They map to `edge_list`, `edge_add`, `edge_remove`, `edge_backlinks`, and
`decision_gates` in the shared registry. Node mode refuses these new commands.

Edge operations require both ordinary admission and an optional trusted typed
cohort verifier. Default adapters supply no cohort verifier and refuse all edge
reads and writes. The verifier receives detached resources resolved from the
same frozen read snapshot or evolving write transaction, including requested
source/target facts, disclosed references and backlink sources. Removed and
touched references remain subject to final checks. After each typed verification
returns, cancellation and the same principal's current ordinary grant are checked
again. Batch entries resolve their own cohorts from evolving transaction state;
refusal before commit rolls back the batch. This provides no production identity
issuer or atomic guarantee across host authority changes and storage commit.

Explicit local target bindings must exist in the specified database; unqualified
targets retain external/dangling reference semantics. CAS precedes noop detection;
noops leave revisions, timestamps and import receipts untouched. Changes bump the
source once and preserve unknown and legacy relationship data. Outgoing edges
sort by type, target ID, field; backlinks sort by type, source ID, field.

Failed or short stdout writes return a typed error and exit 1. A write failure
or postcommit refusal can follow an effect; retry and rollback are not assured.

Two line-local gosec exclusions were reviewed by the task manager: G204 for
intentional execution of the operator-selected absolute runtime/script from
explicit configuration, and G304 for the bounded explicitly selected config
file read. Absolute paths alone are not a taint defense; local execution
configuration is trusted by this contract. No service request, attribution label,
endpoint or response chooses the executable. No other scanner exclusions were
introduced. Original unsuppressed findings are retained in task evidence.
