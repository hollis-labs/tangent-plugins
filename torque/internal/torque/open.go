package torque

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// Opening a board.
//
// The whole flow is four steps and none of them is a judgement call: query
// Torque, shape the cards, create a room, present the envelope. It runs through
// Tangent's own tool surface (see pluginhost/tools.go) rather than through a
// private host API, so the plugin is a caller of the same tools an agent calls,
// with the same authority and no more.
//
// The envelope goes out with completion mode "async". That is what keeps the
// board open: the receipt returns immediately, nothing is cancelled, and the
// interaction stays pending for as long as the participant works in it. A
// "wait" here would block this call across a human decision, which for a board
// means forever.

// metaFiltersKey is where the board's originating filters are recorded on the
// envelope.
//
// They have to survive somewhere, because a sync re-queries Torque with the
// SAME filters and nothing else knows what they were. The envelope's own
// request snapshot is the right place: it is immutable, it is already durable,
// and it is read back from the interaction record — so a sync cannot re-query
// with filters that drifted from the ones the board was opened with.
const metaFiltersKey = "torque_board_filters"

// metaBoardKey records the board id on the envelope alongside the filters.
const metaBoardKey = "torque_board_id"

// OpenInput is the open tool's arguments.
type OpenInput struct {
	Title     string   `json:"title,omitempty"`
	Statuses  []string `json:"statuses,omitempty"`
	ProjectID string   `json:"project_id,omitempty"`
	SprintID  string   `json:"sprint_id,omitempty"`
	EpicID    string   `json:"epic_id,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Executor  string   `json:"executor,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Search    string   `json:"search,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	RoomID    string   `json:"room_id,omitempty"`
	ReadOnly  bool     `json:"read_only,omitempty"`
}

// filters projects the tool input onto the Torque query.
//
// Statuses default to the active set rather than to everything: a board that
// opens with every done task in the store is a board nobody scrolls, and the
// caller can ask for them explicitly.
func (in OpenInput) filters() ListFilters {
	statuses := in.Statuses
	if len(statuses) == 0 {
		statuses = ActiveStatuses
	}
	return ListFilters{
		Statuses: statuses, ProjectID: in.ProjectID, SprintID: in.SprintID,
		EpicID: in.EpicID, Kind: in.Kind, Executor: in.Executor,
		Tags: in.Tags, Search: in.Search, Limit: clampCards(in.Limit),
	}
}

// OpenResult is what the open tool returns.
type OpenResult struct {
	RoomID string `json:"room_id"`
	URL    string `json:"url"`
	// BoardID is the handle a sync names alongside the room.
	BoardID string `json:"board_id"`
	// Cards is how many tasks were sent. It is the number the board's own
	// filter bar narrows, which is why it is worth reporting.
	Cards int `json:"cards"`
	// Truncated says Torque held more matches than the board was allowed to
	// send. The participant is told in the scope line; the agent is told here,
	// because an agent that opened a board over a cut set and does not know it
	// will reason about the cards as though they were the whole answer.
	Truncated bool `json:"truncated"`
	// Scope says which records were sent, in the same words the board shows
	// the participant.
	Scope string `json:"scope"`
	// Source is where the cards came from.
	Source string `json:"source"`
}

// Open queries Torque, shapes a board, and presents it in a room.
func (p *Plugin) Open(ctx context.Context, input OpenInput) (OpenResult, error) {
	tools, err := p.tools()
	if err != nil {
		return OpenResult{}, err
	}

	filters := input.filters()
	page, err := p.client.ListTasks(ctx, filters)
	if err != nil {
		return OpenResult{}, err
	}

	roomID := input.RoomID
	url := ""
	if roomID == "" {
		roomID, url, err = createRoom(ctx, tools, p.boardTitle(input, filters))
		if err != nil {
			return OpenResult{}, err
		}
	}

	boardID := newBoardID()
	data := p.boardData(boardID, input, filters, page)
	envelope := boardEnvelope(newEnvelopeID(), p.boardTitle(input, filters), data, filters)

	if err := advance(ctx, tools, roomID, envelope); err != nil {
		return OpenResult{}, err
	}
	return OpenResult{
		RoomID: roomID, URL: url, BoardID: boardID,
		Cards: len(page.Tasks), Truncated: page.More,
		Scope: data.Sync.scopeOrEmpty(), Source: p.client.BaseURL(),
	}, nil
}

// boardData assembles the envelope's data block.
func (p *Plugin) boardData(
	boardID string,
	input OpenInput,
	filters ListFilters,
	page TaskPage,
) BoardData {
	sync := &Sync{
		Enabled:    !input.ReadOnly,
		Endpoint:   SyncPath,
		Label:      "Sync",
		StageLabel: "Move to",
		Scope:      scopeSentence(filters, page),
	}
	if input.ReadOnly {
		sync.Label = ""
		sync.StageLabel = ""
	}
	return BuildBoard(
		boardID,
		p.boardTitle(input, filters),
		page.Tasks,
		filters.Statuses,
		Source{App: "torque", Label: "Torque · " + p.client.BaseURL()},
		sync,
		nowRFC3339(),
	)
}

func (p *Plugin) boardTitle(input OpenInput, filters ListFilters) string {
	if input.Title != "" {
		return input.Title
	}
	parts := make([]string, 0, 4)
	if filters.ProjectID != "" {
		parts = append(parts, "project "+filters.ProjectID)
	}
	if filters.SprintID != "" {
		parts = append(parts, "sprint "+filters.SprintID)
	}
	if len(filters.Tags) > 0 {
		parts = append(parts, "tagged "+strings.Join(filters.Tags, "/"))
	}
	if len(parts) == 0 {
		return "Torque — active work"
	}
	return "Torque — " + strings.Join(parts, ", ")
}

// scopeSentence is what the board tells the participant about its own card set.
//
// Two things have to come out of it, and the second is CW-20260910-0043:
//
//   - A filter that finds nothing reads as a SCOPE rather than an emptiness.
//     The filter bar is a VIEW over the cards this plugin sent, not a query
//     Tangent re-runs, and without a sentence saying so a task that plainly
//     exists in Torque looks missing.
//   - A cut set never reads as the whole set. "60 task(s)" alone is what a
//     participant filters against, finds nothing in, and cannot tell from an
//     absence — so a bounded card set says it was bounded.
//
// Unlike the Tesseract board's sentence there is no total here, because the
// Torque door this plugin uses does not report one (see TaskPage.More). That
// makes the sentence shorter, not vaguer: "and there are more" is a fact, and
// it carries the remedy beside it.
func scopeSentence(filters ListFilters, page TaskPage) string {
	scope := fmt.Sprintf("%d task(s)", len(page.Tasks))
	if len(filters.Statuses) > 0 {
		scope += " in " + strings.Join(filters.Statuses, ", ")
	}
	if filters.ProjectID != "" {
		scope += ", project " + filters.ProjectID
	}
	if filters.SprintID != "" {
		scope += ", sprint " + filters.SprintID
	}
	if len(filters.Tags) > 0 {
		scope += ", tagged " + strings.Join(filters.Tags, "/")
	}
	if page.More {
		scope += ", and there are more. This is NOT the whole set — the card " +
			"limit cut it, and Torque reports no total, so the board cannot say " +
			"how many more. Raise the limit or narrow your filters to see the rest."
	} else {
		scope += "."
	}
	return scope + " Filters below narrow this set; press Sync to re-query Torque."
}

func (s *Sync) scopeOrEmpty() string {
	if s == nil {
		return ""
	}
	return s.Scope
}

// boardEnvelope wraps the board data in an app-board envelope.
//
// The filters ride in `meta` so a later sync can read them back off the
// immutable request snapshot rather than guessing or being told again.
func boardEnvelope(id, title string, data BoardData, filters ListFilters) map[string]any {
	return map[string]any{
		"v":            1,
		"id":           id,
		"type":         EnvelopeType,
		"title":        title,
		"presentation": "fullscreen",
		"data":         data,
		"meta": map[string]any{
			metaBoardKey:   data.BoardID,
			metaFiltersKey: filters,
		},
	}
}

// createRoom opens a browser room through tangent.session_create.
func createRoom(
	ctx context.Context,
	tools tangentplugin.ToolCaller,
	title string,
) (roomID string, url string, err error) {
	result, err := tools.CallTool(ctx, "tangent.session_create", map[string]any{
		"title": title,
		"meta":  map[string]any{"app": "torque", "surface": "board"},
	})
	if err != nil {
		return "", "", err
	}
	var created struct {
		RoomID string `json:"roomID"`
		URL    string `json:"url"`
	}
	if err := result.Unmarshal(&created); err != nil {
		return "", "", fmt.Errorf("torque: create room: %w", err)
	}
	if created.RoomID == "" {
		return "", "", fmt.Errorf("torque: tangent.session_create returned no room id")
	}
	return created.RoomID, created.URL, nil
}

// advance presents the envelope on the room and returns as soon as the durable
// receipt exists.
//
// Mode "async" is not an optimization. A board is a surface a person works in
// for as long as they want to; blocking this call until they finish would hold
// a request open across a human decision, and for the HTTP sync route that
// means holding a browser request open across one.
func advance(
	ctx context.Context,
	tools tangentplugin.ToolCaller,
	roomID string,
	envelope map[string]any,
) error {
	result, err := tools.CallTool(ctx, "tangent.session_advance", map[string]any{
		"roomID":     roomID,
		"envelope":   envelope,
		"completion": map[string]any{"mode": "async"},
	})
	if err != nil {
		return err
	}
	if result.IsError {
		return fmt.Errorf("torque: present board: %s", string(result.Content))
	}
	return nil
}

// nowRFC3339 is the timestamp a fresh board carries. It is one function so the
// open and sync paths cannot stamp a board differently.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func newBoardID() string { return "torque-board-" + randomSuffix() }

func newEnvelopeID() string { return "torque-board-env-" + randomSuffix() }

// randomSuffix is a short unique tail. Envelope identity is what Tangent's
// idempotency is keyed on, so two boards opened in the same second must not
// collide — a timestamp alone would.
func randomSuffix() string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// crypto/rand failing is not a condition this plugin can paper over
		// with a weaker id: a colliding envelope id would be read as a retry
		// of a different board. The clock is a last resort and is still
		// unique per nanosecond on one process.
		return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}

// decodeFilters reads the originating filters back off an envelope's meta.
func decodeFilters(meta map[string]json.RawMessage) (ListFilters, bool) {
	raw, ok := meta[metaFiltersKey]
	if !ok {
		return ListFilters{}, false
	}
	var filters ListFilters
	if err := json.Unmarshal(raw, &filters); err != nil {
		return ListFilters{}, false
	}
	return filters, true
}
