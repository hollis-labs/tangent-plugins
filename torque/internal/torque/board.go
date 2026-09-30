package torque

import (
	"fmt"
	"sort"
	"strings"
)

// This file maps Torque tasks onto the domain-free app-board shape. Every
// mapping in it is mechanical, and that is the point of the whole pattern
// (ADR 0007 §6): the model never shapes a payload, because there is nothing
// here a model would be better at than a `for` loop.
//
// The direction of the dependency is what keeps ADR 0005's "Tangent is not a
// task tracker" true while a Torque board renders: `tangent.app-board`
// describes a board of cards in columns, this file knows that a Torque status
// is a column and a Torque tag is a badge, and nothing in Tangent core learns
// either fact.

// DefaultCards and MaximumCards bound one board.
//
// Measured against real Torque data rather than guessed, the way the Tesseract
// board's pair was: over the 1111 tasks in the active statuses on this machine,
// one mapped card marshals to ~2.8 KB (median 2.4 KB, p90 5.0 KB) because a
// Torque description is long by design and becomes the card body verbatim.
// Against the app-board manifest's `inline_payload_limit_bytes` of 262144 that
// puts the default at ~168 KB and the ceiling at ~224 KB. The default is 60
// because a board is a surface someone scans, and the ceiling is 80 because
// that is where the envelope stops having comfortable headroom.
//
// There was no bound at all before CW-20260910-0043: a board opened with no
// `limit` sent every matching task, which on this machine is ~2.8 MB — 10.7×
// the limit its own kind declares. A cap is what makes a truncation sentence
// necessary, and the sentence is what makes the cap honest; neither works
// alone.
const (
	DefaultCards = 60
	MaximumCards = 80
)

// clampCards resolves a requested card limit.
//
// It is applied both where the tool input becomes filters — so the number
// recorded on the envelope is the effective one — and inside the client, so a
// board opened before this cap existed still syncs bounded rather than
// re-reading its own unbounded snapshot.
func clampCards(limit int) int {
	if limit <= 0 {
		return DefaultCards
	}
	if limit > MaximumCards {
		return MaximumCards
	}
	return limit
}

// ActiveStatuses is the default column set: the statuses a person is looking
// at when they say "my work". Terminal and archival statuses are excluded by
// default because a board that opens with 370 done cards is a board nobody
// scrolls.
var ActiveStatuses = []string{"backlog", "todo", "queued", "doing", "review", "blocked", "paused"}

// statusLabels renders Torque's status vocabulary for a column header.
// Anything not named here is title-cased from its own slug, so a status Torque
// adds later still gets a readable column instead of being dropped.
var statusLabels = map[string]string{
	"backlog":   "Backlog",
	"todo":      "Todo",
	"queued":    "Queued",
	"doing":     "Doing",
	"review":    "Review",
	"blocked":   "Blocked",
	"paused":    "Paused",
	"done":      "Done",
	"archived":  "Archived",
	"abandoned": "Abandoned",
	"cancelled": "Cancelled",
}

// priorityTones map a Torque priority onto the board's badge vocabulary. The
// board knows five tones and no priorities; this is where the two meet.
var priorityTones = map[int]string{
	1: "danger",
	2: "warning",
	3: "info",
}

// Card is one board card. It mirrors the kind's request schema rather than
// Torque's task shape — a card has a title and badges, not a status and a
// sprint.
type Card struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Subtitle string         `json:"subtitle,omitempty"`
	Badges   []Badge        `json:"badges,omitempty"`
	Body     string         `json:"body,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
}

// Badge is one card badge.
type Badge struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label"`
	Tone  string `json:"tone,omitempty"`
}

// Column is one board column.
type Column struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	CardIDs []string `json:"card_ids"`
}

// Filter is one filter-bar facet. Filters are a VIEW over the cards this
// plugin sent, never a query the host re-runs — see BoardData.Sync.Scope for
// what the board tells the participant about that.
type Filter struct {
	ID       string         `json:"id"`
	Label    string         `json:"label"`
	Kind     string         `json:"kind"`
	Field    string         `json:"field,omitempty"`
	Options  []FilterOption `json:"options,omitempty"`
	Selected []string       `json:"selected,omitempty"`
}

// FilterOption is one selectable facet value.
type FilterOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
	Count int    `json:"count"`
}

// Source labels which application supplied the cards. It is a label for the
// reader; Tangent calls nothing through it.
type Source struct {
	App   string `json:"app"`
	Label string `json:"label"`
}

// Sync is what the board needs in order to talk back to this plugin without an
// agent turn (CW-20260910-0030).
//
// Endpoint is a same-origin path under the reserved plugin prefix. The
// renderer refuses anything else — a caller-supplied absolute URL would make
// the board a general-purpose fetch surface, which is what the host-mediated
// effect broker exists to prevent.
type Sync struct {
	Enabled bool `json:"enabled"`
	// Endpoint is the path the sync button POSTs to.
	Endpoint string `json:"endpoint"`
	// Label names the button.
	Label string `json:"label,omitempty"`
	// StageLabel names the control that stages a card into another column.
	// Empty means staging is not offered even when Enabled is true, which is
	// how a read-only board is expressed.
	StageLabel string `json:"stage_label,omitempty"`
	// Scope says, in the board's own words, which records were sent — because
	// a filter that "finds nothing" for a task that plainly exists is the
	// single most confusing thing a card-set-scoped filter can do.
	Scope string `json:"scope,omitempty"`
}

// BoardData is the app-board envelope's `data` block.
type BoardData struct {
	BoardID   string   `json:"board_id"`
	Title     string   `json:"title,omitempty"`
	Source    Source   `json:"source"`
	Columns   []Column `json:"columns"`
	Cards     []Card   `json:"cards"`
	Filters   []Filter `json:"filters,omitempty"`
	Sync      *Sync    `json:"sync,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// BuildBoard maps a task list onto the board shape.
//
// statuses is the column order. A task whose status is outside it still
// becomes a card and lands in a column of its own appended at the end, because
// dropping it would make the board quietly disagree with Torque about how much
// work exists.
func BuildBoard(
	boardID string,
	title string,
	tasks []Task,
	statuses []string,
	source Source,
	sync *Sync,
	updatedAt string,
) BoardData {
	if len(statuses) == 0 {
		statuses = ActiveStatuses
	}
	cards := make([]Card, 0, len(tasks))
	byStatus := map[string][]string{}
	order := append([]string(nil), statuses...)
	known := map[string]bool{}
	for _, status := range order {
		known[status] = true
		// An allocated empty slice, not nil. A nil slice marshals to `null`,
		// and the kind's schema requires `card_ids` to be an array — so an
		// empty column would fail validation at the surface, which is a defect
		// that only appears when a board is opened on a status nobody is in.
		byStatus[status] = []string{}
	}
	for _, task := range tasks {
		cards = append(cards, CardFor(task))
		status := task.Status
		if !known[status] {
			known[status] = true
			order = append(order, status)
			byStatus[status] = []string{}
		}
		byStatus[status] = append(byStatus[status], task.ID)
	}

	columns := make([]Column, 0, len(order))
	for _, status := range order {
		columns = append(columns, Column{
			ID:      status,
			Label:   statusLabel(status),
			CardIDs: byStatus[status],
		})
	}

	return BoardData{
		BoardID:   boardID,
		Title:     title,
		Source:    source,
		Columns:   columns,
		Cards:     cards,
		Filters:   BuildFilters(tasks),
		Sync:      sync,
		UpdatedAt: updatedAt,
	}
}

// CardFor maps one task.
//
// The description becomes the card body verbatim: it is markdown in Torque and
// the board renders markdown through the shared renderer, so nothing needs to
// translate it. Truncating it here would make the board a lossy view of a
// record the participant may be about to act on.
func CardFor(task Task) Card {
	badges := make([]Badge, 0, 4)
	if tone, ok := priorityTones[task.Priority]; ok {
		badges = append(badges, Badge{
			ID: "priority", Label: fmt.Sprintf("P%d", task.Priority), Tone: tone,
		})
	}
	if task.Kind != "" && task.Kind != "agent" {
		badges = append(badges, Badge{ID: "kind", Label: task.Kind, Tone: "neutral"})
	}
	if task.Executor != "" {
		badges = append(badges, Badge{ID: "executor", Label: task.Executor, Tone: "neutral"})
	}
	for _, slug := range task.TagSlugs() {
		badges = append(badges, Badge{ID: "tag:" + slug, Label: slug, Tone: "info"})
	}

	fields := map[string]any{
		"id":         task.ID,
		"status":     task.Status,
		"updated_at": task.UpdatedAt,
	}
	putIfPresent(fields, "assignee", task.AgentFile)
	putIfPresent(fields, "project", task.ProjectID)
	putIfPresent(fields, "sprint", task.SprintID)
	putIfPresent(fields, "epic", task.EpicID)
	if len(task.DependsOn) > 0 {
		fields["depends_on"] = strings.Join(task.DependsOn, ", ")
	}

	return Card{
		ID:       task.ID,
		Title:    task.Title,
		Subtitle: task.ID,
		Badges:   badges,
		Body:     task.Description,
		Fields:   fields,
	}
}

// BuildFilters derives the facet options from the cards actually sent.
//
// Counting from the sent set rather than from Torque is deliberate and is the
// same rule the filters themselves follow: an option whose count came from a
// query the board cannot re-run would promise records the board does not hold.
func BuildFilters(tasks []Task) []Filter {
	kinds := map[string]int{}
	executors := map[string]int{}
	tags := map[string]int{}
	for _, task := range tasks {
		if task.Kind != "" {
			kinds[task.Kind]++
		}
		if task.Executor != "" {
			executors[task.Executor]++
		}
		for _, slug := range task.TagSlugs() {
			tags[slug]++
		}
	}

	filters := make([]Filter, 0, 4)
	filters = append(filters, Filter{
		ID: "search", Label: "Search", Kind: "text", Field: "title",
	})
	if options := facetOptions(tags); len(options) > 0 {
		filters = append(filters, Filter{
			ID: "tag", Label: "Tag", Kind: "multi", Field: "badges", Options: options,
		})
	}
	if options := facetOptions(executors); len(options) > 0 {
		filters = append(filters, Filter{
			ID: "executor", Label: "Executor", Kind: "multi", Field: "badges", Options: options,
		})
	}
	if options := facetOptions(kinds); len(options) > 1 {
		filters = append(filters, Filter{
			ID: "kind", Label: "Kind", Kind: "multi", Field: "badges", Options: options,
		})
	}
	return filters
}

// facetOptions renders a count map as sorted options: most common first, ties
// broken by name so the order is stable across two syncs of the same data.
func facetOptions(counts map[string]int) []FilterOption {
	options := make([]FilterOption, 0, len(counts))
	for value, count := range counts {
		options = append(options, FilterOption{Value: value, Label: value, Count: count})
	}
	sort.Slice(options, func(i, j int) bool {
		if options[i].Count != options[j].Count {
			return options[i].Count > options[j].Count
		}
		return options[i].Value < options[j].Value
	})
	return options
}

func statusLabel(status string) string {
	if label, ok := statusLabels[status]; ok {
		return label
	}
	if status == "" {
		return "Unset"
	}
	return strings.ToUpper(status[:1]) + strings.ReplaceAll(status[1:], "_", " ")
}

func putIfPresent(fields map[string]any, key, value string) {
	if value != "" {
		fields[key] = value
	}
}
