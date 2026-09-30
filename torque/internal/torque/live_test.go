package torque

import (
	"context"
	"os"
	"testing"
)

// A live check against the real Torque, off by default.
//
// It is here rather than in a scratch program for the reason the Tesseract
// plugin's equivalent is: `internal/` packages cannot be imported from outside
// the module, and the thing worth checking is this client's own bytes against
// the running service. This board's truncation signal is a read of Torque's
// actual paging behavior, and CW-20260910-0043's own description got that
// wrong — it said the list API returns `has_more` and a cursor, which is true
// of `torque_task_list` (the MCP door) and not of `GET /api/v1/tasks` (the
// door this plugin uses, which answers `{tasks, total}` with `total` equal to
// the length of the page).
//
// Run it with TANGENT_TORQUE_LIVE_TEST=1. It is skipped otherwise, so
// `make test` never depends on a service being up. It only reads.
func TestLiveTorque(t *testing.T) {
	if os.Getenv("TANGENT_TORQUE_LIVE_TEST") == "" {
		t.Skip("set TANGENT_TORQUE_LIVE_TEST=1 to run against the real Torque")
	}
	// Through the production constructor, so the environment override this
	// reads is the one the shipped plugin reads.
	client := New().client
	ctx := context.Background()

	if err := client.Health(ctx); err != nil {
		t.Fatalf("health against %s: %v", client.BaseURL(), err)
	}
	t.Logf("health ok at %s", client.BaseURL())

	filters := ListFilters{Statuses: ActiveStatuses}
	page, err := client.ListTasks(ctx, filters)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Tasks) > DefaultCards {
		t.Fatalf("list returned %d tasks past a cap of %d; the probe row leaked into the board",
			len(page.Tasks), DefaultCards)
	}
	t.Logf("list: %d task(s), more=%v", len(page.Tasks), page.More)
	t.Logf("scope: %s", scopeSentence(filters, page))

	// A limit above what Torque holds must not claim to be cut. The pair is
	// the assertion: a warning that fires on a complete set is as useless as
	// one that never fires.
	small, err := client.ListTasks(ctx, ListFilters{Statuses: []string{"doing"}, Limit: MaximumCards})
	if err != nil {
		t.Fatalf("list doing: %v", err)
	}
	t.Logf("doing: %d task(s), more=%v", len(small.Tasks), small.More)
	t.Logf("scope: %s", scopeSentence(ListFilters{Statuses: []string{"doing"}}, small))
}
