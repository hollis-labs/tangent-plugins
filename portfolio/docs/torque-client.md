# Bounded read-only Torque source client (0102)

`portfolio/torqueclient` provides the seven existing `torque_*` reads and a
complete-only comment activity reader used by the board. Wire evidence is the
public Torque commit `d92fd2d39464d34b246bdf9d1b8f0d6ead2cbe71`; reference
behavior is tracker commit `5ff4d2ee3b533359052c1cb09037191667ef3040`.
These pins establish source contracts, not a running daemon or current grant.
No upstream internals are imported or copied into the package.

`New(Config)` requires an explicit HTTPS or loopback HTTP `/api/v1` endpoint,
Host override and integration identity. It discovers no environment, proxy,
credential file or host provider. The default transport disables redirects,
proxy use and connection reuse; there is no application retry. An injected
transport must honor contexts, the configured origin and the no-retry contract.
Only validated literal GET routes are emitted. Errors contain safe codes, never
raw upstream bodies, URLs, configuration or bearer values.

`Read(ctx, proof, operation, input)` passes opaque proof to an injected trusted
verifier. With no verifier it refuses before network or cache access. Each
`Request` contains the trusted endpoint/Host/integration, exact operation/path/
query, phase and detached resource evidence. The verifier must independently
admit both the query and every returned resource. IDs, filter/tag labels, service
credentials and partition epochs do not authorize reads. Its `Grant` supplies
current principal, scope/epoch and credential reference/epoch. The resolver
obtains the credential for that current grant, with no caller token forwarding.
The initial grant remains fixed across pages and concurrent title requests;
principal, scope or credential drift refuses the whole operation.

`Bind(proof, ParentGuard)` creates an immutable request-local view sharing only
the client's capacity and partitioned metadata cache. A nil guard refuses.
Every bound child check verifies the current ordinary parent before and after
child-verifier waits, and compares both parent principals with the child grant.
All injected verification, resolution and parent callbacks must honor context.
Checks also run after capacity/credential waits, before send, after cache waits
and insertion, and before disclosure. The last check is not an atomic revocation
barrier with the return; there is no unchecked blocking wait after a successful
final disclosure check.

The client has six shared GET slots. Each GET has an absolute four-second budget
including verification, capacity, resolution and body reads; each standalone
operation has twenty seconds. Constructor overrides can only lower finite
bounds. Bodies and retained result JSON are bounded to 8 MiB, lists/comments to
five pages of at most 200 rows. Structural list truncation refuses rather than
returning an apparently complete array. Duplicate keys, trailing JSON, excessive
nesting, malformed timestamps/envelopes and inconsistent paging refuse.

Metadata cache defaults are fifteen seconds, 1000 entries and 16 MiB of retained
serialized DTO/resource evidence plus bounded per-entry accounting. Returned
values are deep clones. Keys partition endpoint, Host, integration, current
credential epoch, verified principal, scope/epoch and exact operation/query;
current admission is still required on hits. Full task descriptions, comment
bodies and activity results are never cached. Failures/incomplete/malformed
results are not cached; authentication denial clears retained metadata.
Titles map genuine authorized 404s to null. Missing/null titles map to an empty
string only in `torque_titles`; compact/full task titles retain null.

## Service and board binding

The optional internal seam is:

```go
type CurrentParent func(context.Context) (Authority, error)
type BindUpstream func(context.Context, Caller, string, map[string]any,
    Authority, CurrentParent) (Upstream, error)
```

`Service.BindTorque` runs only for the board/seven Torque reads, after current
resource-level `Service.Admit` succeeds. It receives the actual `Caller.Binding`,
detached parent input and current admitted authority. Service rechecks context
and the same ordinary principal immediately after factory work and before
result/error disclosure. A configured factory with only `Verify` refuses;
factory error/nil never falls back to ambient Torque. With a nil factory, the
existing explicit `Service.Torque` remains a synthetic evaluation injection.
`NewTorqueUpstream(client, proof, currentParent)` enforces the real bound guard.
The public client has no mutable current caller or verifier override.

A board parent does not confer global Torque access. A trusted factory/verifier
must supply distinct exact child list grants and a separate comment grant for
the fetched doing IDs, and admit child result resources. The synthetic positive
controls demonstrate this seam; they do not provision a production grant.
Both existing HTTP and MCP `New` constructors clear inherited Verify, Admit and
BindTorque. MCP installing its own ordinary Admit cannot restore a factory.
Explicit synthetic evaluation injection remains usable through those adapters.
The native executable, production verifier/factory and credentials remain unwired.

The whole board has one absolute thirty-second budget including factory and all
child work. Batch activity reads the pinned flat `/comments` wire with
`entity_type=task` and JSON-array `entity_ids`, creation-time descending order,
and ID ascending for ties. The 200 requested-ID bound is local policy, not a
claimed server cardinality cap. Only proven exhaustion produces `Complete`;
parsed maximum `created_at` determines activity, never `updated_at` or lexical
order. Any error, cap or malformed page marks every fetched doing task in flight
with the existing notice, with no per-task fanout after batch failure. Historical
synthetic fallback rejects absent/nonarray/incomplete comments and malformed
creation times; an explicit complete empty array remains valid. Domain
`composeBoard`, section ordering, dates, counts/totals and registry schemas are
unchanged.

## Verification and reference limits

Focused fake tests exercise constructor requests, separate grants, foreign
resources, independent ordinary-parent revocation/principal/cancellation during
child waits and blocked cache insertion, concurrent callers, six shared slots,
timeouts, paging failures and equal-time cross-page comment ordering.

A private owned harness compares actual Go Service plus the bound client against
pinned `composeBoard` on the same generated snapshot and fake Torque responses:
default/nondefault windows and limits, exact activity edges, comments created
versus edited, plain days, future clamping, ordering, totals, tag labels, activity
outage and total outage. JSON semantic comparison retains every board field;
outage notices use the declared safe failure text. The seven client DTOs also
match original `createTorque` fake reads, including nullable/missing/empty titles,
404 titles, full comments, tags, two-page structures, sprint clipping and facets.
The harness, tool receipts and raw results are private acceptance artifacts,
not shipped JavaScript or a test suite tied to a mutable corpus.

Intentional differences from the historical client include strict authority,
finite whole-operation deadlines, safe errors, compact cache retention and
refusal of malformed/incomplete structural results. Input numeric/string
coercions, permissive JavaScript date parsing and arbitrary word-shaped sort
selectors are not promised. UTF-16 query length and ordinary sprint clipping
match the reference; a clipped surrogate boundary or an authored lone surrogate
cannot preserve JavaScript's ill-formed string in Go and is outside parity.
Locale/ICU and all Unicode ordering variants are not certified. Independent
Node baseline tests alone are not Go/reference parity evidence. No live read,
full accepted-note/current claim, installed binding, provider adoption or cutover
is asserted by these source checks.
