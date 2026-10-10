# Portfolio storage foundation

This module implements CW-20261009-0096 / accepted ADR0015 (DEC067). It is a
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
`supersedes` to itself, and other legacy ID fields to `related`. Dedicated
future edge operations may add `informs` or richer relationships.

One lossless item JSON representation feeds the derived columns, comment/link
rows and search projection. Future writers must update these together in the
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
