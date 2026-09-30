package tesseract

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file maps Tesseract revisions onto the domain-free app-board shape.
// Every mapping in it is mechanical, and that is the point of the whole pattern
// (ADR 0007 §6): the model never shapes a payload, because there is nothing
// here a model would be better at than a `for` loop.
//
// The direction of the dependency is what keeps Tangent core domain-free while
// a Tesseract board renders: `tangent.app-board` describes a board of cards in
// columns, this file knows that a Tesseract lifecycle status is a column and a
// tag is a badge, and nothing outside this package learns either fact.

// LifecycleStatuses is the column set, and it is Tesseract's own lifecycle
// vocabulary in its own order (internal/memory/types.go: draft, reviewed,
// canonical, deprecated). Moving a card between these columns IS the
// disposition, which is why this surface is a kanban rather than a table.
var LifecycleStatuses = []string{"draft", "reviewed", "canonical", "deprecated"}

// StatusDeprecated is the one column a move into which this plugin applies
// itself. Every other move is a request handed back to the agent; see
// sync.go for why that split is the store's rule rather than a policy choice.
const StatusDeprecated = "deprecated"

// statusLabels renders the lifecycle for a column header. Anything not named
// here is title-cased from its own slug, so a status Tesseract adds later still
// gets a readable column instead of being dropped.
var statusLabels = map[string]string{
	"draft":      "Draft",
	"reviewed":   "Reviewed",
	"canonical":  "Canonical",
	"deprecated": "Deprecated",
}

// DefaultCards and MaximumCards bound one board.
//
// Measured against the running service rather than guessed. At payload_mode
// summary a revision costs ~700 bytes, so 100 cards is ~70 KB of recall and
// 200 is ~136 KB, against the app-board manifest's inline_payload_limit_bytes
// of 262144. The default is 100 because a board is a surface someone scans,
// and the ceiling is 200 because that is where the envelope stops having
// comfortable headroom — not because 201 records is a different kind of thing.
const (
	DefaultCards = 100
	MaximumCards = 200
)

// confidenceTones map the author's recorded confidence onto the board's badge
// vocabulary. The board knows five tones and no confidences; this is where the
// two meet. Thresholds are display bands, not a Tesseract concept.
func confidenceTone(confidence float64) string {
	switch {
	case confidence >= 0.9:
		return "success"
	case confidence >= 0.7:
		return "info"
	default:
		return "warning"
	}
}

// Card is one board card. It mirrors the kind's request schema rather than a
// Tesseract revision — a card has a title and badges, not a namespace and a
// confidence.
type Card struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Subtitle string  `json:"subtitle,omitempty"`
	Badges   []Badge `json:"badges,omitempty"`
	Body     string  `json:"body,omitempty"`
	// Note is the reword the participant already asked for, sent back so the
	// board can show it. Without it a sync answers a request with a fresh board
	// and an empty box, and the participant watches their own words disappear.
	Note   string         `json:"note,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
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
// agent turn.
type Sync struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"`
	Label    string `json:"label,omitempty"`
	// StageLabel names the control that stages a card into another column.
	StageLabel string `json:"stage_label,omitempty"`
	// NoteLabel names the per-card free-text control. It is the additive
	// app-board extension this plugin needed and did not have: a note is how a
	// reword reaches the agent, and app-board carried no free-text input.
	// Empty means no note control is offered.
	NoteLabel string `json:"note_label,omitempty"`
	// Scope says, in the board's own words, which records were sent and
	// whether that was all of them.
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

// BuildBoard maps a revision list onto the board shape.
//
// A revision whose status is outside the lifecycle set still becomes a card and
// lands in a column of its own appended at the end, because dropping it would
// make the board quietly disagree with Tesseract about how many records need
// dispositioning.
func BuildBoard(
	boardID string,
	title string,
	revisions []Revision,
	source Source,
	sync *Sync,
	pending map[string]Request,
	updatedAt string,
) BoardData {
	cards := make([]Card, 0, len(revisions))
	byStatus := map[string][]string{}
	order := append([]string(nil), LifecycleStatuses...)
	known := map[string]bool{}
	for _, status := range order {
		known[status] = true
		// An allocated empty slice, not nil. A nil slice marshals to `null`,
		// and the kind's schema requires `card_ids` to be an array — so an
		// empty column would fail validation at the surface. Every lifecycle
		// column is a drop target whether or not it holds anything, which is
		// what makes an empty Deprecated column correct rather than a defect:
		// recall does not return deprecated records by default, and the column
		// exists to be dropped into.
		byStatus[status] = []string{}
	}
	for _, revision := range revisions {
		cards = append(cards, CardFor(revision, pending[revision.RevisionID]))
		status := revision.Status
		if !known[status] {
			known[status] = true
			order = append(order, status)
			byStatus[status] = []string{}
		}
		byStatus[status] = append(byStatus[status], revision.RevisionID)
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
		Filters:   BuildFilters(revisions),
		Sync:      sync,
		UpdatedAt: updatedAt,
	}
}

// CardFor maps one revision.
//
// The card is identified by its REVISION id, not its memory id. That is the
// identity every downstream act names: `POST /v1/memory/deprecate` takes a
// revision_id, `GET /v1/memory/revisions/{id}` hydrates one, and a supersede
// names the revision it replaces. A card keyed on the memory would be a card
// naming something none of those calls accept.
//
// The summary becomes the card body verbatim: it is markdown in Tesseract and
// the board renders markdown through the shared renderer. It is also the whole
// of what this board shows of a record's content — see PayloadModeSummary.
func CardFor(revision Revision, pending Request) Card {
	badges := make([]Badge, 0, 6)
	if pending.Kind != "" {
		// A request already handed to the agent and not yet acted on. Without
		// it a participant who asked for a reword last sync sees a card that
		// looks untouched and asks again.
		badges = append(badges, Badge{
			ID: "pending", Label: pending.badgeLabel(), Tone: "warning",
		})
	}
	if revision.Domain != "" {
		badges = append(badges, Badge{ID: "domain", Label: revision.Domain, Tone: "neutral"})
	}
	if revision.Confidence > 0 {
		badges = append(badges, Badge{
			ID:    "confidence",
			Label: "conf " + strconv.FormatFloat(revision.Confidence, 'g', -1, 64),
			Tone:  confidenceTone(revision.Confidence),
		})
	}
	for _, tag := range revision.Tags {
		if tag != "" {
			badges = append(badges, Badge{ID: "tag:" + tag, Label: tag, Tone: "info"})
		}
	}

	fields := map[string]any{
		"revision_id": revision.RevisionID,
		"status":      revision.Status,
		"namespace":   revision.Namespace,
		"created_at":  revision.CreatedAt,
	}
	putIfPresent(fields, "memory_id", revision.MemoryID)
	putIfPresent(fields, "memory_key", revision.MemoryKey)
	putIfPresent(fields, "domain", revision.Domain)
	if len(revision.Tags) > 0 {
		fields["tags"] = revision.Tags
	}
	if revision.Confidence > 0 {
		fields["confidence"] = revision.Confidence
	}

	return Card{
		ID:       revision.RevisionID,
		Title:    cardTitle(revision),
		Subtitle: revision.Namespace,
		Badges:   badges,
		Body:     revision.Payload.Summary,
		// Only a reword carries text. A promotion request is fully described by
		// its badge, and seeding the box with anything for one would invite the
		// participant to edit a note they never wrote.
		Note:   pending.Note,
		Fields: fields,
	}
}

// cardTitle names a card.
//
// A memory_key is the name a record has and is what a `[[wikilink]]` cites, so
// it is the title wherever there is one. **An unkeyed memory is legitimate**,
// not a malformed record — Tesseract's write path makes memory_key optional and
// keyless revisions exist in the store — and the kind's schema requires a title
// of at least one character. So a keyless record is titled by its memory id
// rather than dropped or rendered blank: an id is a poor name but it is a real
// one, and it is what a caller needs to find the record again.
func cardTitle(revision Revision) string {
	if revision.MemoryKey != "" {
		return revision.MemoryKey
	}
	if revision.MemoryID != "" {
		return "(unkeyed) " + revision.MemoryID
	}
	if revision.RevisionID != "" {
		return "(unkeyed) " + revision.RevisionID
	}
	return "(unkeyed)"
}

// BuildFilters derives the facet options from the cards actually sent.
//
// Counting from the sent set rather than from Tesseract is deliberate and is
// the same rule the filters themselves follow: an option whose count came from
// a query the board cannot re-run would promise records the board does not
// hold.
func BuildFilters(revisions []Revision) []Filter {
	namespaces := map[string]int{}
	domains := map[string]int{}
	tags := map[string]int{}
	for _, revision := range revisions {
		if revision.Namespace != "" {
			namespaces[revision.Namespace]++
		}
		if revision.Domain != "" {
			domains[revision.Domain]++
		}
		for _, tag := range revision.Tags {
			if tag != "" {
				tags[tag]++
			}
		}
	}

	filters := make([]Filter, 0, 4)
	filters = append(filters, Filter{
		ID: "search", Label: "Search", Kind: "text", Field: "title",
	})
	if options := facetOptions(namespaces); len(options) > 1 {
		filters = append(filters, Filter{
			ID: "namespace", Label: "Namespace", Kind: "multi", Field: "namespace",
			Options: options,
		})
	}
	if options := facetOptions(domains); len(options) > 1 {
		filters = append(filters, Filter{
			ID: "domain", Label: "Domain", Kind: "multi", Field: "domain", Options: options,
		})
	}
	// Tags are capped: this corpus carries a long tail of one-off tags, and a
	// filter bar with three hundred buttons is a filter bar nobody uses. The
	// cap is on the OPTIONS offered, never on the cards — a card whose tags all
	// fell outside the top set is still on the board and still matches search.
	if options := facetOptions(tags); len(options) > 0 {
		if len(options) > maximumTagOptions {
			options = options[:maximumTagOptions]
		}
		filters = append(filters, Filter{
			ID: "tag", Label: "Tag", Kind: "multi", Field: "tags", Options: options,
		})
	}
	return filters
}

// maximumTagOptions bounds the tag facet. See BuildFilters.
const maximumTagOptions = 24

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

// ScopeSentence is what the board tells the participant about its own card set,
// and it is the one place this plugin is required not to repeat
// CW-20260910-0043 — the open bug for the Torque board failing to say when it
// cut its cards.
//
// Recall's manifest hands over `results_total`, `results_returned`, `truncated`
// and `truncation_reason` exactly, so a cut set is a fact this board holds
// rather than a guess. Two things have to come out of this sentence:
//
//   - A filter that finds nothing reads as a SCOPE rather than an emptiness.
//     The filter bar is a view over what was sent, not a query.
//   - A cut set never reads as the whole set. "100 of 1426" is the difference
//     between a participant who knows to narrow and one who believes they have
//     seen everything.
func ScopeSentence(input RecallInput, manifest Manifest, pending map[string]Request) string {
	var sentence strings.Builder
	if manifest.Truncated && manifest.ResultsTotal > manifest.ResultsReturned {
		fmt.Fprintf(&sentence, "%d of %d matching record(s)",
			manifest.ResultsReturned, manifest.ResultsTotal)
	} else {
		fmt.Fprintf(&sentence, "%d record(s)", manifest.ResultsReturned)
	}
	if len(input.Namespaces) > 0 {
		sentence.WriteString(" in " + strings.Join(input.Namespaces, ", "))
	}
	if len(input.Filters.Statuses) > 0 {
		sentence.WriteString(", status " + strings.Join(input.Filters.Statuses, "/"))
	}
	if len(input.Filters.Tags) > 0 {
		sentence.WriteString(", tagged " + strings.Join(input.Filters.Tags, "/"))
	}
	if input.Query != "" {
		sentence.WriteString(", matching " + strconv.Quote(input.Query))
	}
	sentence.WriteString(", ranked by " + rankingLabel(input.Ranking) + ".")

	if manifest.Truncated {
		fmt.Fprintf(&sentence,
			" This is NOT the whole set — recall cut it (%s). Narrow the filters or raise the limit to see the rest.",
			truncationLabel(manifest.TruncationReason))
	}
	// Two counts, because a supersede has no card to wear a badge: it names a
	// record this board just retired, so it is invisible unless the scope line
	// says it is there. That invisibility is exactly how the first one got
	// lost.
	onCard, offCard := 0, 0
	for revisionID, request := range pending {
		if request.Kind == RequestSupersede {
			offCard++
			continue
		}
		_ = revisionID
		onCard++
	}
	if onCard > 0 {
		fmt.Fprintf(&sentence,
			" %d card(s) carry a request already handed to the agent and not yet applied.", onCard)
	}
	if offCard > 0 {
		fmt.Fprintf(&sentence,
			" %d supersede request(s) name records retired from this board and are waiting for the agent.",
			offCard)
	}
	sentence.WriteString(
		" Filters below narrow this set; press Sync to apply what you staged and re-query Tesseract.")
	return sentence.String()
}

func rankingLabel(ranking string) string {
	switch ranking {
	case RankingChronological:
		return "chronological (newest first)"
	case RankingRelevance:
		return "relevance"
	case "", RankingActivation:
		return "activation (hottest first)"
	default:
		return ranking
	}
}

// truncationLabel renders recall's own truncation reason. The vocabulary is
// Tesseract's, and an unrecognized reason is reported verbatim rather than
// flattened to "truncated" — a reason this plugin has not seen is still a
// reason the operator can act on.
func truncationLabel(reason string) string {
	switch reason {
	case "limit":
		return "the card limit"
	case "budget_bytes", "budget_tokens":
		return "a size budget: " + reason
	case "payload_mode_limit_cap":
		return "the payload-mode page cap"
	case "":
		return "reason not reported"
	default:
		return reason
	}
}

func (s *Sync) scopeOrEmpty() string {
	if s == nil {
		return ""
	}
	return s.Scope
}
