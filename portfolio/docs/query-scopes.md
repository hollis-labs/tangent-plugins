# Query scopes (source evaluation)

CW-20261009-0105 adds a domain filter shared by the operation registry, HTTP
arguments and MCP schemas. This is an injected, copied-shadow implementation.
The executable still has no caller verifier, store or upstream client.
Full upstream structural-list/facet acceptance remains pending manager/PM review.

The strict optional argument is one of:

```json
{"scope":{"kind":"project","id":"msg://project/example/a"}}
{"scope":{"kind":"workstream","id":"WS-example"}}
{"scope":{"kind":"unscoped"}}
```

Null, strings, extra scope keys, labels and aliases refuse. Unknown canonical
projects or workstreams return not_found; they never broaden a query. A project
must have usable synced registry source, rather than merely a valid URN.

Membership follows typed belongs_to edges, their preserved legacy project_ids,
workstream_ids and workstream links, and explicit torque_project_ids mappings
from canonical registry external IDs. Only actual workstream nodes are followed.
Related, decision and dependency edges are not membership. Paths are unioned,
deduplicated and cycle checked. No source rank chooses a winner.

Torque tasks additionally match workstreams by exact configured torque_tag
slug or explicit torque_project_ids. Display names, overlay tags and ws-* label
conventions never create membership. Absent, denied or invalidated canonical
source cannot confirm a project. Retained stale/partial source remains explicitly
labeled evidence; it does not establish current caller authority.

Unscoped is diagnostic overlap: no resolved project, ambiguous project candidates
or unresolved paths. Workstream-only items retain proven workstream membership
and also appear in unscoped diagnostics. An ambiguous item may appear in each
proven selected project cohort and in the explicit unscoped query. This is not an
exclusive partition or a conclusion that an unresolved reference is absent.

Without scope, list/search keep their legacy arrays and include every unscoped
item. Existing page requests keep their local page envelope. With scope,
list/search return items, total (exact selected local cohort), returned, partial,
scope, membership and unscoped. Optional page adds the existing snapshot,
has_more, next_offset and paging fields. Membership is keyed by displayed ID:

```json
{
  "projects":["msg://project/example/a"],
  "workstreams":["WS-example"],
  "unresolved":[],
  "ambiguous":false,
  "sources":[{"urn":"msg://project/example/a","state":"fresh",
              "torque_ids":["PRJ-20261009-0001"],"environment":"owned-fixture"}]
}
```

Unscoped contains ids, returned and diagnostic_overlap=true for displayed rows.
It never exposes unrelated global diagnostics or counts. Scoped databases returns
selected item counts without the aggregate envelope's unrelated updated date.
Board retains sections/counts/totals, with scope, displayed membership,
unscoped diagnostics and per-status upstream evidence; totals_kind is
selected_estimates. Board task keys use source:id to avoid namespace collisions.

Local scope applies before filtering/order/paging/counts and board caps/activity.
One verified detached database read contains the items and allowlisted canonical
membership facts. Continuations bind query, complete selected result, relevant
revisions and transitive membership proof. Changing a scope, cohort or relevant
mapping requires restart; unrelated titles, overlays and source rows do not.
The token contains no disclosed proof or credential.

Injected current operation/resource verification must cover the entire selected
cohort and its disclosed evidence before paging or totals. It must refuse mixed
granted/ungranted cohorts, rather than silently omit denied rows. Scope is not a
grant; body labels or service credentials supply no caller authority. Verification
still runs before access and before result/error disclosure with a stable principal.
This injection contract is not an atomic host/database authority transaction.

The scoped Torque task scanner intersects explicit project/tag selectors with
caller filters, verifies returned selector membership, deduplicates rows and
refuses inconsistent repeated IDs, malformed metadata or repeated cursors.
Each selector follows at most five pages of 200 rows. The entire request,
including all board statuses and activity, shares 20 selectors, 100 reads,
20,000 rows, 8 MiB of decoded upstream JSON and a 30-second execution deadline.
The injected reader must also bound transport bodies before decoding.

Scanner evidence retains selector identities, page counts, completeness and
budget failures. Exhaustive selected task totals are exact within that read;
partial scans omit total and expose lower_bound and partial. Overlapping upstream
totals are never summed. No selector cursor serves as a multiplex continuation;
nonzero upstream offset or caller cursor refuses because upstream snapshot
continuation is unavailable. No project selection retries as an aggregate query.
Explicit unscoped selects a bounded aggregate scan with honest completeness.
Upstream reads are not a stable remote snapshot, even when all pages complete.

Scoped board activity requires an explicit comments array of objects with valid
created_at timestamps and comments_meta.has_more=false to prove completeness.
Missing arrays, non-array values and malformed elements remain incomplete.
The scoped scanner accepts only public CW task IDs and checks returned rows
against observable project/epic/sprint, status, priority, tag and update-time
filter intersections. Full-text q remains delegated to the upstream search
contract because task summaries cannot prove its full-text semantics. Missing/incomplete activity or ordinary errors conservatively keep
selected doing tasks in flight. Upstream authority denial or inconsistent pages
invalidate prior protected task rows and metadata; local board rows survive.
The real client/activity batching work remains in 0102.

Structural list and facet scope currently return unsupported pending the
full-coverage disposition. The pinned legacy structural adapter flattens capped
pages to arrays and erases completeness; facet aggregates do not prove a
deduplicated selector union. This provisional refusal does not fulfill every-list
task acceptance. No live integration, dataset census, UI, CLI, writer cutover,
credential provisioning, host adoption or SDK numeric-carrier change is included.

Generated benchmarks use 1,000 content items, 100 workstreams, 20 projects and
3,000 item belongs_to edges, then multiply each population by ten. Content is
80% ideas/20% roadmap; deterministic 5% ambiguous/5% dangling item paths and 1%
cyclic workstreams. These are explicit assumptions, not measured operator sizes.
The selected project remains a small cohort as all populations scale together.
Resolver and end-to-end list/search/board include verified snapshot costs; raw
receipts record runtime, allocations and the fixed sampling duration.

Traversal visits completed nodes once per root and diagnoses active back edges;
diamond joins do not enumerate paths. Task membership combines matching
workstream roots in one traversal to avoid repeatedly walking shared chains.
An owned 2,048-workstream chain/diamond/cycle fixture checks retained terminal
project, workstream membership and cycle diagnostics. Local content roots and
continuation proofs can still traverse separately; recursion depth scales with
reachable nodes and no constant depth cap is claimed.

The recorded post-sort scoped list/search/board samples allocate approximately
35 MB per operation at scale 1 and 360 MB at scale 10, including the full verified
snapshot even for a small selected cohort. These are cumulative allocation bytes,
not resident memory. Short 100 ms sampling includes one end-to-end iteration at
scale 10; these samples do not establish precise production performance targets
or full remote workload performance.
