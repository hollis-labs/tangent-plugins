# Stage 1 synthetic smoke

Run `make smoke` at the repository root, or `make smoke-stage1` in messaging.
The suite builds the current messaging subprocess and the Tangent command from
its actual published module dependency. It starts both against private temporary
state and loopback ports, through the existing protocol-2 driver and public
HTTP/MCP interfaces. The embedded browser bundle is not built or exercised.

Synthetic channel history and SSE, normalized AI gateway responses, and reply
receipts/delivery observations are the only upstream traffic. There is no real
channel, provider, dispatcher runtime, installed plugin, catalog edit or agent
turn. Source sender/turn identifiers are explicitly test-owned fixtures.

The smoke checks immutable preparation and stage reuse across a real child crash
after the host committed an inbox item but before its receipt reached the local
ledger; the replacement retries the same key/request and earns its cursor from
the real host receipt. It also checks live SSE/reconnect, multiple outputs with
the same turn/body, ordinary nonreplyability, purge retirement, fail-open stage
refusal/timeout/tool-use output, and malformed/oversized admission remaining
pending. Operator resolutions use the real private host participant surface;
synthetic queue acceptance remains unacknowledged until observed delivered, and
unsupported interrupt is refused before a send. Each owned child and reader is
joined or explicitly contained in the deliberate crash fixture. A second crash
withholds a committed acknowledgement: the replacement resumes saved delivery
without another send or delivery poll, even when host Await omits the item.
Delivery projections use the driver HTTP handle; installed browser routes are
not exercised.

Stage 2 remains separate: approved live routes and deployed capabilities under
CW-20261009-0049, then the owner's installation/dogfood CW-20261009-0052. Passing
this smoke does not prove live delivery, verified human identity, redaction,
provider exactly-once billing or production rollout.
