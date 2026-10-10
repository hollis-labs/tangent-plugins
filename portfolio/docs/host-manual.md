<!-- requires-tool: tangent.portfolio_databases -->

# Portfolio source MCP tools

This manual is staged for the host's `docs/plugins/portfolio.md`. It is not
installed host documentation. Tangent's documentation gate selects that manual
only when its required tool is in the shipped surface. The portfolio module is
outside the root install roster; source declarations do not advance a host
installation pin, adopt this manual, or demonstrate that gate passing.

The native source executable declares the tools below but opens no database,
uses no DataDir, resolves no secret or upstream client, and refuses every call
without current verified caller authority. The positive SDK transcript uses a
generated synthetic database and injected test-owned verifier, not a deployed
issuer, real Architect/Planner identity or live host admission.

Use read operations to inspect a verified, admitted cohort before proposing a
mutation. A read grant supplies no write or decide grant. Every operation needs
explicit current resource grants, including both link targets, every reordered
item and both sides of promotion. Labels, body author, session or room identity,
plugin incarnation and upstream app credentials never supply caller authority.
New author, added_by and decided_by values belong to the verifier; imported
authors remain historical attribution. Manifest effects and hints are not grants.

## Tools

| Tool | Agent use |
| --- | --- |
| `tangent.portfolio_databases` | Inspect local database names, prefixes and item counts in the admitted shadow. |
| `tangent.portfolio_contract` | Inspect shared registry operation schemas and write metadata. |
| `tangent.portfolio_schema` | Inspect one database's item fields, required fields and enums. |
| `tangent.portfolio_list` | List a database with typed exact field filters; array fields match one member. Optional page returns a bounded cohort slice. |
| `tangent.portfolio_get` | Retrieve one admitted database/item pair with revision and historical fields. |
| `tangent.portfolio_search` | Search substrings with every term required, optionally selecting a database and page. |
| `tangent.portfolio_create` | Create a database-specific item with verifier-owned author and generated defaults; structured numbers obey the MCP input bound. |
| `tangent.portfolio_update` | Apply a typed patch and optional rev CAS; null removes fields and structured numbers obey the MCP input bound. |
| `tangent.portfolio_comment` | Append text to an admitted item; the current verifier owns the comment author and permanent comment ID allocation. |
| `tangent.portfolio_link` | Add a reference between existing admitted local targets; a reference already present is a no-op. |
| `tangent.portfolio_unlink` | Remove a local reference; an absent reference is a no-op. |
| `tangent.portfolio_link_add` | Add an external pointer; duplicate kind/ref conflicts. File pointers are never read. |
| `tangent.portfolio_link_remove` | Remove an external pointer; an absent pointer returns not_found. |
| `tangent.portfolio_reorder` | Reorder named items followed by other ordered items; admission must cover all affected items. |
| `tangent.portfolio_decide` | Select an existing decision option under an explicit decide grant. |
| `tangent.portfolio_defer` | Defer a decision under an explicit decide grant. |
| `tangent.portfolio_reopen` | Reopen a decision and clear its selected outcome under an explicit decide grant. |
| `tangent.portfolio_inbox_add` | Create an inbox item with verifier-owned added_by; file paths remain pointers. |
| `tangent.portfolio_inbox_promote` | Promote an inbox item with destination fields in one transaction, requiring grants on both source and target. |
| `tangent.portfolio_inbox_dismiss` | Dismiss an admitted inbox item, optionally appending a note. |
| `tangent.portfolio_torque_task` | Read a Torque task through an explicitly injected read-only integration. |
| `tangent.portfolio_torque_tasks` | Read a bounded Torque task page, preserving upstream paging and partial metadata. |
| `tangent.portfolio_torque_titles` | Read titles for explicitly named Torque task IDs. |
| `tangent.portfolio_torque_projects` | Read Torque project metadata. |
| `tangent.portfolio_torque_epics` | Read Torque epic metadata. |
| `tangent.portfolio_torque_sprints` | Read Torque sprint metadata. |
| `tangent.portfolio_torque_facets` | Read Torque task facets for selected filters/dimensions. |
| `tangent.portfolio_board` | Compose local and optional upstream rows, retaining notices and partial/estimated totals. |
| `tangent.portfolio_edge_list` | List outgoing typed relationships of an admitted source. |
| `tangent.portfolio_edge_add` | Add a typed relationship with optional source revision CAS. |
| `tangent.portfolio_edge_remove` | Remove every representation of a typed relationship with optional source revision CAS. |
| `tangent.portfolio_edge_backlinks` | List admitted incoming typed relationships and their source cohort. |
| `tangent.portfolio_decision_gates` | Return distinct decision-gate target IDs of an admitted source. |
| `tangent.portfolio_batch` | Apply bounded local registry mutations in one transaction with every operation/resource grant and one stable principal. |

Administrative migration has no MCP grant or declaration. No tool provisions a
caller, configures a service, writes upstream apps or enables the installed writer.

## Input and result contracts

Create, update and list filter schemas select a database-specific branch.
Unknown item fields remain preserved JSON extensions. Update arrays replace the
whole field and null removes it; removing a required field fails final merged
item validation. Identity, created, comments and rev cannot be patched. Optional
update rev compares against the current item; conflict details are disclosed only
after a fresh authority check. No rev parameter means no CAS guarantee.

The pinned SDK and host decode object argument numbers through float64 before
the plugin handler. Internal shadow json.Number preservation and exact output
tokens do not establish lossless public object input. Every nested number in
write arguments, including CAS, unknown objects/arrays and batch entries, must
have decoded absolute value strictly below 2^53 (9007199254740992). The inclusive
boundary is refused because an authored unsafe odd integer can round down to
exactly that boundary before the handler. Numeric refusal returns typed invalid
before effects, and refuses an entire batch without changes. Integer input is
therefore restricted to the conservative strictly-inside range; decimal and
exponent tokens can still round or normalize below that bound and are not
lossless. Read filters use the same upstream map decoder and do not gain exact
numeric token semantics. No JSON-text alternative or raw carrier patch is present.
Direct subprocess strict JSON checks reject ambiguous payloads; an upstream host
map decode can erase duplicate keys before that boundary. This manual does not
claim wire-level duplicate recovery or general JSON-number losslessness.
This bounded structured-wire policy follows PM01a1265f-2739 and manager decision
15968 on CW-20261009-0104. Raw-number host/SDK carrier work is separately tracked
as CW-20261010-0197 (manual backlog); production authority remains unavailable.

List/search accept an optional `page` object with limit (default 50, maximum 200),
offset (0 through 100000) and snapshot. A first page example is
`{"db":"ideas","page":{"limit":20}}`. Continuation copies snapshot and
next_offset from the previous result. Snapshot binds operation, query and the
complete ordered selected cohort, including relevant revisions. Unrelated
database changes leave continuation valid; cohort or query changes require a
restart. Results contain items, returned, total, has_more, next_offset, snapshot
and partial=false. Local total is exact only for that complete admitted cohort;
an upstream board estimate is a different contract. At the offset ceiling,
has_more can remain true while next_offset is null; narrow the query or restart.

Domain filters and whole-cohort admission apply before paging and totals. No
project/workstream membership algorithm is implemented in this slice. Explicit
scope requests are refused as unsupported, never broadened or silently ignored.
Missing upstream integrations remain unavailable or board notices, not complete
empty result sets. Input is bounded to 32 KiB and output to 1 MiB. An oversized
result is refused without partial disclosure.

The shared internal batch accepts 1 through 20 entries, each with operation and
input. It permits local registry mutations only, excluding administrative import,
nested batch and upstream calls. All schemas and grants are checked before
effects and again after the writer wait; every check must retain one principal.
Handlers run sequentially on one transaction state. An update CAS observes the
revision produced by earlier entries. There is no generated-result reference
syntax. Precommit schema, domain, CAS, storage or authority failure rolls back all
entries, projections, children, FTS and allocation effects. Detached success is
encoded within the transaction and bounded before publication. Batch is not
exposed as a new HTTP route.

Typed edges require an additional trusted, optional typed resource verifier;
ordinary operation admission alone cannot enable them. Default HTTP/MCP
constructors refuse every edge read/write, clear ambient service authority, and
install no production cohort verifier. Injected source adapters bind verification
to the copied request Identity and the same current principal. Caller input
cannot supply cohorts or grant labels.

The cohort is derived from the same detached State used by each handler before
lookups, CAS errors or result disclosure. It covers requested sources/targets,
actual local database/existence facts, all disclosed references and backlink
sources. Full source-item results also cover legacy and external pointers.
Explicit target database bindings must match an existing local item; unqualified
targets retain opaque external/dangling semantics without supplying authority.
Frozen removed/touched references remain checked before commit and final
disclosure. Immediately after a typed verifier returns, cancellation and fresh
ordinary admission for the same principal must still pass. Batch edge entries
resolve cohorts against evolving transaction state before each mutation and
recheck retained cohorts before commit and final disclosure.

Relationship types are `informs`, `depends_on`, `supersedes`, `related`,
`belongs_to`, and `decision_gates`. Outgoing order is type, target ID, field;
backlink order is type, source ID, field. Revision CAS is checked before noop
recognition. A noop leaves revisions, envelope timestamps and receipts unchanged;
a change bumps once and updates projections in the existing State transaction,
preserving legacy representations and unknown metadata outside the removal.

Typed tool failures use IsError and `{"error":{"code":...,"message":...,
"details":...}}`. Codes include bad_request, invalid, not_found, conflict,
locked, unavailable and unsupported. Internal failures are sanitized. Error
details and results both need fresh authority before disclosure. Production
commit-time authority remains unavailable: a final check may refuse after a
committed mutation. Neither that error nor a lost response promises rollback,
idempotency or safe automatic retry; reconcile the effect before retrying.

## Verification limits

Focused source controls cover typed schemas, null/removal and exact internal
numbers; selected-cohort continuation; sequential batch CAS and rollback;
resource/principal refusal, output-bound rollback and postcommit refusal; the
public SDK synthetic list/get/comment/decide transcript and detached callers.
The source manifest is checked through the public host decoder. These controls
do not establish deployed caller issuance, host-carried authentication, atomic
host authority, general JSON-number losslessness or the host documentation gate adopting
this manual. Production carrier, caller provisioning, scope acceptance, UI and writer
cutover remain separately owned work.
