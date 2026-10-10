# Portfolio shadow storage and typed relationships

This module implements CW-20261009-0096 and CW-20261009-0100 / accepted ADR0015 (DEC067). It is a
shadow storage and copied-snapshot evaluation tool. Node ptrack remains the
single authoritative writer. There is no installed plugin, MCP/HTTP write
surface, live synchronization, Projects fetch, UI or writer switch here.

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
Imported file order is distinct from optional numeric list order. Projects are
empty until the separately scoped read-only integration is implemented.

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
than tests against a mutable operator dataset. Authenticated operation registry,
verified caller handoff0067, board/integration ports, host UI0064, CLI migration
and owner-approved cutover remain later slices. The source registry has29
operations; this module does not expose that registry yet.
