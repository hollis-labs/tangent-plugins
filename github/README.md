# GitHub PR reviews

The GitHub plugin fetches a pull request and opens a durable Tangent Inbox room.
It maps PR metadata to the domain-free `tangent.external-review` kind; GitHub
schemas, authentication and writes stay in this plugin.

Call `tangent.github_open_pr`:

```json
{
  "pr_url": "https://github.com/OWNER/REPO/pull/123",
  "idempotency_key": "my-agent:review:OWNER/REPO:123:HEAD_SHA",
  "summary": "What changed and what needs your review.",
  "notes": "Checks, tradeoffs and anything the reviewer should know."
}
```

The tool returns immediately with a durable handle and room locator. An
identical key/request reuses the retained review even after restart. Changing
URL or notes with the same key is refused; use a new key to review new commits.
Summary and notes render as Markdown, separately from the PR's own body.

Approve records a GitHub APPROVE review for the displayed head commit and
keeps the room open. Merge uses an enabled repository merge method and passes
that same head SHA to GitHub. Finish review saves an approval-only result;
merging saves the merge result automatically. Dismiss closes the Tangent
request without changing GitHub. Refresh reconciles approval and merge status
from GitHub, including requests whose network response was lost.

GitHub enforces its normal protections and permissions. Drafts, closed PRs,
conflicts and changed head commits cannot be acted on from a stale room. GitHub
forbids approving your own PR; Merge remains available for an authorized user.
No admin bypass, auto-merge, comments, replies or branch deletion is implemented.

## Install and authentication

Build `make -C github dist`, then install the resulting
`dist/tangent.plugin.github` with `tangent plugin install` and restart Tangent.
The host must support `tangent.external-review`.

The plugin executes the fixed `gh api --hostname github.com` command, without
a shell. Install GitHub CLI and run `gh auth login` as the Tangent service user,
or supply `GH_TOKEN` through that user's own service environment. The plugin
uses gh's authentication; Tangent core holds no plugin configuration or secret.
Grant only repository permissions you intend to use. No secret is included in
the room or tool response.

Cerberus's GitHub connector is read-only for status/releases/Actions and uses
its own declared credential mechanism. It is not a dependency of this plugin.

## Verify

`make -C github test lint dist` checks the API requests, stale-head handling,
retry/restart reconciliation and emitted manifest. An opt-in live read test
uses `TANGENT_GITHUB_LIVE_TEST=https://github.com/OWNER/REPO/pull/123` and never
approves or merges a real PR. All write tests use a fake API.
