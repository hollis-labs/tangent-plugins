# Summarizer adapter

This internal package implements the summarize-only pipeline stage over the
published Tether normalized `POST /ai/chat` wire. It starts no agent session,
has no tool loop, and performs no replies or other effects.

Create it once at plugin startup with `New(Config, *http.Client)`. Configuration
requires an explicit HTTP(S) base URL or `unix:/absolute/socket` address and caller identity; provider/model hints are
optional. The existing runtime owns transport credentials. The client is copied,
redirects are disabled, and the transport must honor cancellation and avoid
ambiguous billable retries. There is one adapter `Do` call per run. For Unix sockets, standard transports are cloned with context-bound dialing to the configured socket and no proxy. Custom RoundTripper middleware is retained unchanged and must wrap an owner-configured Unix transport; the adapter cannot reconstruct arbitrary middleware. The socket address remains part of the immutable configuration digest.

An empty `InstructionPath` uses the embedded `instructions/default.md`. An
operator-selected regular file is read once, bounded to 16 KiB, validated as
UTF-8, and SHA-256 digested. Changing the file later does not change the running
stage. Recreate the stage under a new immutable configuration/instruction key
for an intentional update. No instruction or source is emitted in diagnostics.

`Spec(priority)` returns the stable `summarize` version `1` stage, default
15-second timeout, fail-open mode and snapshot digests. A configured finite
positive timeout and any earlier parent deadline bound the request; the stage
never extends its parent's budget. It requests at most 512 output tokens,
limits the encoded gateway request to 1 MiB and reads at most 64 KiB of response.

Only one admitted original and its typed attribution are JSON-quoted as user
data, separate from the trusted system instruction. Previous annotations and
conversation are not sent. No tools, attachments, session ID or default provider
are supplied. Gateway budgets and refusal policy still apply.

A successful response has one assistant text part, a completed stop reason,
no refusal/tool call, and exactly `{"summary":"nonempty plain text"}` with at
most 600 Unicode characters. Unknown fields, duplicate decoded keys, excessive
nesting, invalid Unicode, truncation, action/options fields, and oversized bodies
are rejected. Provider/network errors expose only closed package errors;
refusal uses `pipeline.ErrRefused`. The pipeline persists a failure marker and
passes the original on ordinary errors. Parent cancellation remains pending.
There is no automatic retry after ambiguous acceptance or timeout.

Schema validation and absent tools do not prove that a model cannot leak source
secrets or write misleading prose. Summary text is untrusted presentation, never
approval, an executed action, or reply authority. Redaction/retention and live
availability are separate before-1.0 and rollout work.

## Owner integration

The plugin owner wires configuration and lifecycle outside this subtree:

```go
stage, err := summarizer.New(summarizer.Config{
    EndpointURL: cfg.TetherEndpoint,
    CallerID: cfg.CallerID,
    InstructionPath: cfg.InstructionPath,
}, configuredHTTPClient)
// handle err before accepting startup
spec := stage.Spec(configuredPriority)
// pipeline.New([]pipeline.StageSpec{spec}, durableResultStore, explicitBounds)
```

After canceling and joining all tracked stage calls, the owner calls `Stage.Close()`
to release idle connections, including adapter-created Unix transport clones.
It neither cancels nor joins active calls. Custom middleware must forward
`CloseIdleConnections` to its inner transport; use a dedicated stage client.

Concrete SQLite, prepared enqueue payloads and atomic sink/cursor settlement are
not owned here. Tests use only synthetic HTTP servers and private files; they do
not call a provider or running service.
