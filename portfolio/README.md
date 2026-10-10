# Portfolio shadow storage, relationships and Projects

This module implements CW-20261009-0096, CW-20261009-0099, CW-20261009-0100 and CW-20261009-0101 / accepted ADR0015 (DEC067). It is a
shadow storage and copied-snapshot evaluation tool. Node ptrack remains the
single authoritative writer. There is no installed plugin, MCP/HTTP write
surface, live synchronization, UI or writer switch here. Read-only Projects clients and
internal shadow sync/view methods are source-only and have no installed carrier.

Use an owned detached copy of all seven JSON files. Never point these commands
at the live tracker directory. Every path is explicit; there is no live-root,
CWD, environment, host cache or temporary persistence fallback.

```sh
CGO_ENABLED=0 go build -o /owned/tools/portfolio-storage ./cmd/portfolio-storage
/owned/tools/portfolio-storage -database /owned/shadow.db -source-copy /owned/snapshot import
/owned/tools/portfolio-storage -database /owned/shadow.db -source-copy /owned/snapshot import
/owned/tools/portfolio-storage -database /owned/shadow.db -source-copy /owned/snapshot verify
/owned/tools/portfolio-storage -database /owned/shadow.db -export-copy /owned/new-export export
```

The first import is atomic. An exact semantic replay verifies every normalized
row and makes no change. A changed snapshot refuses an implicit refresh; use a
fresh shadow database. Export refuses an existing destination directory.
Exports are semantic JSON round trips, not byte-format reproductions: object
key order and whitespace can change; missing versus null, numbers, unknown
fields, arrays, comments, historical authors and imported item order survive.
Input files must be regular files, all seven schemas must be supported, and
ambiguous duplicate JSON keys, global IDs or comment IDs refuse before writes.

The schema contains persistent global ID reservations, items, comments, links,
typed edges, Projects source/overlay storage, import receipts and FTS5. IDs may
not be renamed, moved or destructively deleted. A dropped status preserves an
ID. External/dangling edge targets survive without invented items or projects.
Legacy `*_ids` and `depends_on` are projected without removing their source
fields; field and target identify each edge. `decision_ids` maps to
`decision_gates`, project/workstream IDs to `belongs_to`, `depends_on` to itself,
`supersedes` to itself, and other legacy ID fields to `related`. Original
`links[]` entries with kind `workstream` additionally project as `belongs_to`
with field `links.workstream`; the original kind, label, order, duplicates and
unknown fields remain in JSON and the links table. Other link kinds retain
their original meaning. Membership is explicit: ambiguous memberships and
external IDs are retained without inferring objects or project/workstream types.

Internal shadow domain methods `Link`, `Unlink`, `ListEdges`, `Backlinks` and
`DecisionIDs` cover the six edge types: `decision_gates`, `informs`,
`supersedes`, `belongs_to`, `depends_on`, `related`. Direction is always source
item → referenced object. Thus an idea's `decision_gates` target is its gating
decision, and incoming decision backlinks list the items it gates. `DecisionIDs`
is a sorted, deduplicated view of these edges, including unresolved IDs. It does
not overwrite or materialize the historical `decision_ids` field on reads.
Edge lists retain the source `field` so multiple representations are visible.
Lists sort by type, target/source, field; an empty type filter includes all types.

New relationships are encoded in an additive, reserved JSON extension:

```json
"_portfolio_relationships": {
  "version": 1,
  "edges": [{"type": "informs", "target": "DEC-example"}]
}
```

The extension is the source for its derived edge rows, never a second writable
graph. Version 1 requires an edges array of objects with a supported type and
nonempty string target. Unsupported versions/malformed metadata refuse before
import or mutation. Unknown fields in the metadata and edge entries survive
semantic export/reimport; duplicate entries remain in JSON but project once.
New links use this extension for every type, leaving legacy fields readable.
An existing `(source, target, type)` in any source field is an idempotent link
no-op. Unlink removes **all** representations of that pair/type from legacy
ID arrays, scalar `supersedes`, original workstream links and the extension.
It retains empty arrays, removes a matching scalar supersedes field, and leaves
unrelated data intact. An absent pair/type is an idempotent unlink no-op.
These are explicitly new shadow semantics, not claimed legacy registry parity.

Both mutations return whether a change occurred. Empty IDs/unknown types return
`ErrInvalidRelationship`; an absent source returns `ErrItemNotFound`. Targets
need not exist; no target entity, status conversion or identity is invented.
No-op calls preserve rev, timestamps and receipts. Real changes increment the
source item's rev once (missing rev starts at zero), set its UTC updated time,
and update JSON, normalized rows, comments, links, edges and FTS in one immediate
transaction. Exhausted revisions refuse. Writers verify complete source/projection
parity first, so mutations cannot conceal drift. This full shadow verification
favors correctness over large-dataset throughput; callers provide a bounded
context, and SQLite waits at most its configured five-second busy timeout.

The original import digest/time remains provenance. An actual relationship
mutation atomically marks its receipt `mutated_at`, permanently refusing replay
into that store even if links are later undone. Export/verify of the current
shadow remains supported; its export can bootstrap and replay in a fresh store.
Migration 002 adds this marker and projects existing workstream links without
rewriting historical JSON or migration 001.

`IdeaDiagnostics` reports ideas with `decision-needed` status as owner conversion
candidates and retains any historical `decision` value, including explicit null.
It never changes status or creates a decision; candidate counts come from the
selected snapshot, not a historical brief count. These internal methods add no
CLI/API/MCP write exposure or authenticated authority. Future caller admission
and production host binding remain0067/0103 obligations.

One lossless item JSON representation feeds the derived columns, comment/link
rows and search projection. Writers must update these together in the
same transaction; the verifier/export rejects divergence. Rev defaults to zero
only in the normalized column; missing JSON rev is not materialized. Comment
`n` is its original ordinal, with original `c-N` identity stored independently.
Imported file order is distinct from optional numeric list order. Projects metadata is independent of the copied-item projection, as described below.

Pure-Go modernc SQLite1.60.1 uses its matching libc1.77.1. Connections enable
foreign keys, bounded busy timeout, immediate transactions, WAL and explicit
FULL synchronization. WAL needs local storage. Migration hashes are checked on
reopen; unknown or changed schema versions refuse. The shadow database is
private0600. FTS5 availability and transactional rollback are exercised by
fixtures; search retains AND-of-case-insensitive-substrings, including short
terms, rather than replacing it with word MATCH. No backup/restore or platform
release claim is made by this foundation; consistent snapshot backup and host
DataDir lifecycle remain obligations before installed writer adoption.

`make check` runs lint, race fixtures and no-cgo build. Tests use synthetic
private roots; explicit copied-data parity runs are acceptance artifacts rather
than tests against a mutable operator dataset. Production caller handoff0067, real upstream integrations, host UI0064, CLI migration
and owner-approved cutover remain later slices. The source registry has29
operations; its internal shadow registry is described below.

## Read-only Projects (0101)

`internal/projects` owns small typed HTTP adapters, based on immutable public
[Tether source 964d32d](https://github.com/hollis-labs/tether/tree/964d32d2a109f85c04e397d60dabd79bcfb4d3ad)
(`internal/api/registry.go`, `internal/registry/model.go`) and
[Torque source d92fd2d](https://github.com/hollis-labs/torque/tree/d92fd2d39464d34b246bdf9d1b8f0d6ead2cbe71)
(`internal/httpserver/projects.go`, `internal/httpserver/tasks.go`). No internal
upstream packages are imported and no service internals are copied. The reviewed
0097 note is design provenance; these pinned route sources define the wire shape.

The directory reads `GET /registry/projects?include=external_ids`. That pinned
route is **unpaged and defaults to active projects**. An exhaustive successful
response marks missing rows `absent` from that directory, preserving their URN
and metadata; it does not claim deregistration/deletion. `RegistryProject` uses
an explicitly encoded URN for lookup. Only `external_ids` is requested, never
`full`, callbacks, host addresses, kind metadata or operational payloads. Decode
retains only URN/kind/display name/title/status/tags, source environment/timestamps
and the external-ID array `{substrate, external_id, attached_at}`. Unexpected
operational fields are discarded, never dereferenced or stored. Directory reads
bound the body to 8MiB and rows to 1000; an oversized result cannot mark absence.
If a source reports partial/page/cursor/total evidence, it is retained and cannot
prove absence; this client does not invent registry paging parameters.

Torque enrichment uses only validated explicit `PRJ-YYYYMMDD-NNNN` IDs and GET
project reads, bounded to 200 per sync. It stores name/status/source timestamp
under each external ID, never replaces registry identity or relocates overlay.
`MappingState` distinguishes missing, invalid, duplicate and ambiguous mappings;
all original external-ID provenance remains available. Several workstreams or
registry identities may share one Torque project, and one registry identity may
carry several IDs. Names and display labels never resolve membership.

`Config` requires an explicit loopback HTTP or HTTPS endpoint, credential reference,
reference epoch and injected resolver. Requests use only that credential, with no
anonymous retry, operator-token fallback, token-file discovery, forwarding from a
host caller, keychain, provisioning, SSH or daemon startup. The resolver is the
source-only secret seam; deployment must supply a stable credential for each epoch
and reconstruct the client with a new epoch on replacement. No credential files
or real service tokens were used here. An injected transport can target an
explicitly selected local UDS; this does not implement future ADR0062 device
transport, endpoints, grants, pairing or renewal. The adapter refuses redirects,
binds a 4s request deadline (configurable up to 30s), closes and bounds bodies,
validates IDs/selectors, encodes filters/cursors and returns only safe error codes.
The transport must honor request contexts and the selected origin. No raw config,
credential or upstream error body is persisted, logged or returned.

Migration003 preserves the original unused `projects` placeholder as
`legacy_projects`, and creates derived `projects`, independent `project_overlays`,
and sync/partition provenance tables. Historical placeholder content is not
promoted to trusted registry source. These tables are excluded from the lossless
seven-file item projection intentionally: sync cannot change item JSON, edges,
ID reservations, FTS, import receipt, export or exact replay. Projects are a
separate derived view; exporting the copied items does not export this disposable
remote cache or independent overlay. There is no second canonical item truth.

`SyncProjects` fetches outside its immediate transaction (30s overall deadline),
then atomically updates directory/enrichment and provenance. Repeated reads upsert
by URN without duplicate identities; fresh fetched times record each observation.
Failed/partial results retain prior safe rows with stale/partial markers. Torque
failure is independent of registry state. Network outages may retain prior
allowlisted enrichment for IDs still explicitly mapped; deleted/rebound mappings
cannot reuse unrelated enrichment. Authentication denial or missing/invalid
integration removes protected cached metadata across the whole affected source,
including rows absent from a partial response. Endpoint/reference-epoch changes
invalidate prior metadata using opaque partition digests. URNs and local overlay
survive invalidation. `ProjectSyncEvidence` exposes completeness/failure evidence;
`SyncProjects` returns local persistence/validation errors while recording safe
upstream failure states. None of these metadata states authorizes a caller.

`SetProjectOverlay` is an **internal test-owned shadow operation**: notes/tags/custom
JSON fields keyed by explicit project URN, independent revision/CAS, identical
writes preserve revision, and sync never calls it. It permits an external project
URN without manufacturing a directory or local item. There is no CLI, HTTP, MCP,
UI, installed plugin or authenticated writer exposing it. Verified0067 caller
binding and all authoritative writes remain separate; Node stays the sole writer.

`ViewProject` follows explicit `belongs_to` item→project and item→workstream→project
edges, plus explicit `torque_project_ids`. Missing/cyclic/dangling membership and
all ambiguous candidates remain visible. It combines attached local categories
(including workstreams, roadmap, ideas, decisions and risks) with synthetic
read-only Torque tasks; remote tasks are never inserted as local items. Attached
workstreams' explicit `torque_project_ids` and `torque_tag` produce GET-only task
queries, deduplicated and bounded to20 selectors,5 pages ×200 tasks each. Filters
are sent upstream before page caps and totals; empty selectors refuse. Each result
is checked against its selector and cursors cannot repeat. A denial clears all
previously fetched task rows/totals and protected Torque enrichment; subsequent
selectors do not retry anonymously or widen scope.

Local and fetched-task membership are filtered **before** view limits (1..200).
Local total/truncation and a bounded explicit unscoped/ambiguous bucket are
separate. TaskTotal counts unique matching fetched tasks, not all upstream tasks;
TaskTruncated reflects the final view cap. Per-query upstream totals/cursors/
partial evidence remain separate, because overlapping selectors cannot be summed
into an exact global total. This project view does not implement0105's global
scope API,0102's board/full Torque port,0103–0106's operation/admission surfaces,
0108 UI,0109 cutover, Tether workstream remote reads or Tesseract content reads.
No new workstream route or filter combinations are invented here.

Synthetic private fixtures exercise idempotent sync, independent overlay/CAS,
transaction rollback, source change/denial invalidation, outage/partial retention,
encoded URNs, safe redaction/errors, limits/redirects/deadlines, upstream task scope
and paging, many-to-one/ambiguous/unresolved membership, external project links,
and copied-item import/export/verifier/replay regression. There is no live
integration, credential, grant, host, installed caller or deployment acceptance.

## Internal operation registry (0099)

`internal/operations` now owns the pinned tracker `5ff4d2ee3b533359052c1cb09037191667ef3040`
operation declarations: names, descriptions, input schemas, write metadata and
one domain dispatcher. `Registry` returns detached metadata and `Service.Call`
uses those same declarations for input validation. This package is internal;
no CLI, MCP, HTTP, installed plugin, live client or authoritative writer exposes
it. The existing storage CLI retains its copied-snapshot commands.

Local behavior covers discovery/schema/contract, ordered/filterable lists,
get/create/update, comments, item links, external pointers, reorder, decision
lifecycle, inbox and compatibility migrate. Optional update rev checks return a
`conflict` with the complete current item. Missing rev is zero. Empty update
patches bump rev; repeated link/unlink calls preserve rev and timestamps;
reorder bumps only items whose order changes, while saving the envelope date.
Comments allocate `c-N` independently for each item. Sequential IDs retain the
maximum existing width; slug IDs use the source's ASCII slug and collision
suffix rules. Global reservations and dropped items survive permanently.

Each mutation verifies the current lossless snapshot under an immediate SQLite
transaction, applies domain changes, then reuses the importer projection builder.
Item JSON, core columns, comments, links, typed edges, FTS, envelope dates, ID
reservations and the receipt commit together. Inbox promotion creates the target
and marks the source in that same transaction. Callback, child-projection or
receipt faults roll back both items and the allocator. Existing identities and
file ordinals cannot move or disappear. Projects metadata and overlays remain
independent, and the 0100 relationship extension survives legacy operations.
Exact replay into a mutated shadow still refuses. This verified whole-snapshot
facade prioritizes correctness; it makes no large-dataset throughput claim.

Unknown fields and their JSON numbers, arrays and explicit nulls survive.
Null patches delete a field and arrays replace it. Opaque pointer-like fields
remain JSON even when they cannot form edges; only nonempty string references
project. For example, `depends_on: 42` is opaque on an idea but fails the declared
roadmap item schema on create/update. Empty strings in a declared string array
are source-valid and remain JSON without invented edge targets. Import retains
historical provenance rather than applying every new-write domain constraint.
Malformed reserved `_portfolio_relationships` metadata still refuses under the
existing shadow extension contract. Format annotations remain descriptive, as
in the Node subset validator.

`Caller.Binding` is an opaque request-local courier. An explicitly injected
`VerifyCaller` must resolve a currently verified principal and permission for
the exact operation. A missing verifier, empty/unverified principal, stale or
refused binding denies mutations with `unavailable`; no body label, role or
plugin incarnation creates authority. Newly assigned `author`, `added_by` and
`decided_by` values come from that principal, including create/update/promote,
comments and decisions. Imported authors stay historical. Clearing a decision
on reopen retains the source's empty `decided_by` marker. This intentionally
differs from Node's self-declared authors. `migrate` needs its own explicit
operation grant and never runs at startup. The verifier is a test-owned seam;
production host0067 handoff/admission remains unimplemented. Unverified reads
are allowed only by this internal shadow evaluation policy.

Errors retain the source domain codes `not_found`, `invalid`, `conflict`,
`locked`, `bad_request` and `unavailable`; SQLite writer contention maps to
`locked`. Revision projection parses decimal/exponent notation exactly, retaining
its original JSON number. Fractional or out-of-int64-range revisions refuse
rather than round. Mutation counters and numeric ID/comment allocation refuse
exhaustion at JavaScript's safe integer boundary. These are bounded storage
safeguards, not a promise to reproduce JavaScript precision loss.

Search evaluates the verified FTS5 text projection from the same read snapshot,
with AND-of-case-insensitive-substrings, short terms, punctuation and comments.
Array-object text/label values use the source's JSON-value string conversion.
Full language-neutral Unicode lowercasing preserves dotted-I expansion and
contextual final sigma; ECMAScript whitespace and UTF-16 relational string
ordering preserve tested search/filter/list behavior. Migration004 rebuilds only
the derived FTS projection; fixtures verify unchanged JSON, revisions, receipts,
Projects, relationships and reservations, corrected search after reopen, and
atomic migration failure/retry. Board ID ties use English collation from pinned
`golang.org/x/text v0.40.0`. Evidence is bounded to pinned Node22.12.0 and the
synthetic Unicode/English ordering cases; arbitrary locales, ICU versions and
new Unicode-version differences are not claimed equivalent.

Board preserves active_hours2, recent_hours72, limit30, source group ordering,
comment activity fallback, plain-day inclusion at end-of-day, displayed age at
start-of-day, future-date clamping, fixed seven-day roadmap landings, partial
outages and capped counts versus estimated totals. The verified ISO/plain-day
fixtures do not assert all permissive JavaScript `Date.parse` grammars. Board
and `torque_*` reads use only an injected domain `Upstream`, with source filter/ID
validation and safe typed failures. There is no configured/default endpoint,
credential discovery or network implementation here; real integrations remain
0102. Source outage messages from safe typed failures are retained; untyped
adapter errors are replaced by a generic notice.

Compatibility `migrate` repairs missing rev and legacy active decisions, applies
historical decision seeds and the source idea pointer repair explicitly. A
complete copied snapshot always has an inbox envelope, so an empty inbox is
existing and receives no missing-file inbox seeds. Historical missing-file
bootstrap is not represented by the storage foundation. No automatic seed,
refresh, synchronization, write-through, cutover, host authority, transport or UI
adoption is included.

Synthetic fixtures exercise each behavior independently, actual promotion faults
after target/source work, two-handle CAS/reservation/comment/promotion conflicts,
disjoint updates, projection/replay preservation and pre-v4 upgrade rollback.
Private pinned-Node service/reference checks independently verified those shared
behavior expectations on generated seven-envelope fixtures with an explicit
private data root, isolated HOME and only a test-owned ephemeral Torque fake.
Neither the private source nor operator data is bundled, and tests assert no
registry count or mutable cross-source agreement. `make check` covers the entire
portfolio module (lint, race and no-cgo build); concurrency stress is limited to
changed transaction/registry concurrency fixtures.
