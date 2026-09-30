package torque

import (
	"testing"
)

// The mapping is mechanical, which is exactly why it is worth pinning: a
// mechanical mapping that drifts produces a board that is confidently wrong
// rather than obviously broken.

func tasks() []Task {
	return []Task{
		{
			ID: "CW-1", Title: "First", Description: "# Body\n\nmarkdown", Status: "todo",
			Priority: 1, Kind: "agent", Executor: "cli", AgentFile: "clockwork-frontend",
			Tags: []Tag{{Slug: "tangent"}, {Slug: "plugins"}}, UpdatedAt: "2026-09-10T00:00:00Z",
		},
		{
			ID: "CW-2", Title: "Second", Status: "doing", Priority: 3,
			Kind: "issue", Executor: "cli", Tags: []Tag{{Slug: "tangent"}},
		},
		{ID: "CW-3", Title: "Third", Status: "done", Priority: 2},
	}
}

// TestBuildBoardPutsEachTaskInItsStatusColumn is the core mapping: a Torque
// status is a board column, and that fact lives here and nowhere in Tangent.
func TestBuildBoardPutsEachTaskInItsStatusColumn(t *testing.T) {
	board := BuildBoard("b1", "Board", tasks(), []string{"todo", "doing"},
		Source{App: "torque"}, nil, "2026-09-10T00:00:00Z")

	if len(board.Cards) != 3 {
		t.Fatalf("cards = %d, want one per task", len(board.Cards))
	}
	columns := map[string][]string{}
	for _, column := range board.Columns {
		columns[column.ID] = column.CardIDs
	}
	if got := columns["todo"]; len(got) != 1 || got[0] != "CW-1" {
		t.Errorf("todo column = %v, want [CW-1]", got)
	}
	if got := columns["doing"]; len(got) != 1 || got[0] != "CW-2" {
		t.Errorf("doing column = %v, want [CW-2]", got)
	}
	// CW-3 is `done`, which was not in the requested column set. Dropping it
	// would make the board quietly disagree with Torque about how much work
	// exists, so it gets a column appended at the end.
	if got := columns["done"]; len(got) != 1 || got[0] != "CW-3" {
		t.Errorf("done column = %v, want [CW-3] appended for the unrequested status", got)
	}
	if board.Columns[len(board.Columns)-1].ID != "done" {
		t.Errorf("the unrequested status column is not last: %v", board.Columns)
	}
}

// TestRequestedColumnsSurviveAnEmptyStatus: a column nobody is in is still a
// column. A board whose empty columns vanish is a board that reshapes itself
// every sync.
func TestRequestedColumnsSurviveAnEmptyStatus(t *testing.T) {
	board := BuildBoard("b1", "Board", nil, []string{"todo", "doing"},
		Source{App: "torque"}, nil, "")
	if len(board.Columns) != 2 {
		t.Fatalf("columns = %v, want both requested statuses", board.Columns)
	}
	for _, column := range board.Columns {
		if len(column.CardIDs) != 0 {
			t.Errorf("column %s has cards on an empty board", column.ID)
		}
	}
}

// TestCardCarriesTheStatusItWasSentWith is what a sync compares a staged
// change against. Without it, "already in that column" is unanswerable and
// every sync would re-push every card.
func TestCardCarriesTheStatusItWasSentWith(t *testing.T) {
	card := CardFor(tasks()[0])
	if card.Fields["status"] != "todo" {
		t.Errorf("card.fields.status = %v, want the task's status", card.Fields["status"])
	}
	if card.Fields["id"] != "CW-1" {
		t.Errorf("card.fields.id = %v", card.Fields["id"])
	}
	if card.Body != "# Body\n\nmarkdown" {
		t.Errorf("card.body = %q, want the description verbatim — the board renders markdown", card.Body)
	}
	if card.Subtitle != "CW-1" {
		t.Errorf("card.subtitle = %q, want the task id", card.Subtitle)
	}
}

// TestBadgesCarryPriorityKindExecutorAndTags pins the badge vocabulary, and
// with it the one place a Torque concept becomes a board label.
func TestBadgesCarryPriorityKindExecutorAndTags(t *testing.T) {
	labels := map[string]string{}
	for _, badge := range CardFor(tasks()[0]).Badges {
		labels[badge.ID] = badge.Label
	}
	if labels["priority"] != "P1" {
		t.Errorf("priority badge = %q", labels["priority"])
	}
	if labels["executor"] != "cli" {
		t.Errorf("executor badge = %q", labels["executor"])
	}
	if labels["tag:tangent"] != "tangent" {
		t.Errorf("tag badge missing: %v", labels)
	}
	// `agent` is the overwhelming majority kind; badging every card with it
	// would be noise that pushes the informative badges off the card.
	if _, present := labels["kind"]; present {
		t.Errorf("the default kind was badged: %v", labels)
	}
	if kind := CardFor(tasks()[1]).Badges; len(kind) == 0 {
		t.Error("a non-default kind should be badged")
	}
}

// TestFilterOptionsAreCountedFromTheCardsSent is the rule that keeps the
// filter bar honest: an option counted from a query the board cannot re-run
// would promise records the board does not hold.
func TestFilterOptionsAreCountedFromTheCardsSent(t *testing.T) {
	filters := BuildFilters(tasks())
	byID := map[string]Filter{}
	for _, filter := range filters {
		byID[filter.ID] = filter
	}
	tag, ok := byID["tag"]
	if !ok {
		t.Fatalf("no tag filter in %v", filters)
	}
	counts := map[string]int{}
	for _, option := range tag.Options {
		counts[option.Value] = option.Count
	}
	if counts["tangent"] != 2 || counts["plugins"] != 1 {
		t.Errorf("tag counts = %v, want them derived from the two tagged tasks", counts)
	}
	// Most common first, ties by name, so two syncs of the same data produce
	// the same bar.
	if tag.Options[0].Value != "tangent" {
		t.Errorf("tag options are not ordered by count: %v", tag.Options)
	}
	if _, ok := byID["search"]; !ok {
		t.Error("every board should carry a text filter")
	}
}

// TestASingleKindIsNotAFilter: a facet with one value narrows nothing and
// costs a row in the bar.
func TestASingleKindIsNotAFilter(t *testing.T) {
	uniform := []Task{{ID: "a", Title: "a", Kind: "agent"}, {ID: "b", Title: "b", Kind: "agent"}}
	for _, filter := range BuildFilters(uniform) {
		if filter.ID == "kind" {
			t.Errorf("a single-valued facet became a filter: %v", filter)
		}
	}
}

// TestUnknownStatusStillGetsAReadableColumn: Torque may add a status, and the
// board should show it rather than an empty header or a raw slug.
func TestUnknownStatusStillGetsAReadableColumn(t *testing.T) {
	board := BuildBoard("b", "", []Task{{ID: "x", Title: "x", Status: "needs_triage"}},
		[]string{"todo"}, Source{}, nil, "")
	var found string
	for _, column := range board.Columns {
		if column.ID == "needs_triage" {
			found = column.Label
		}
	}
	if found != "Needs triage" {
		t.Errorf("unknown status label = %q, want it title-cased", found)
	}
}
