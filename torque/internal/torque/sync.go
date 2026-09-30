package torque

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// Syncing a board, both ways, in one press.
//
// # The order, and why it is the order
//
//  1. Read the surface. The board's pending interaction, its immutable request
//     snapshot (which carries the filters it was opened with), and its latest
//     draft revision (which carries what the participant staged).
//  2. Apply the staged changes through Torque's own API. This is the push, and
//     it happens first so the re-query below observes the result rather than
//     the state before it.
//  3. Re-query Torque with the SAME filters. This is the pull.
//  4. Replace the board.
//
// A failure in step 2 does not abort the sync. A task Torque refused to move is
// reported by name with Torque's own message, and the board is still refreshed,
// because the useful thing after a partial failure is a board showing what
// actually happened — not the stale one the participant was looking at plus an
// error.
//
// # Why a draft is not a decision
//
// ADR 0007 §5 is explicit that a caller must not present a draft as a decision:
// a draft is what the user is looking at, and only a resolution is what they
// decided. Staged changes live in the draft and this plugin does not act on
// them until the participant presses Sync. **The press is the decision.** The
// draft is where the intent was accumulated; the POST is the act. That is why
// a board left open with staged changes and never synced changes nothing in
// Torque, which is the property that makes the distinction real rather than
// verbal.
//
// # Why the board is cancelled and re-presented rather than superseded
//
// `tangent.interaction_supersede` needs its replacement to exist first, and the
// only thing that can put an envelope in front of a participant is a room
// advance — which refuses while another envelope is pending. So the pending
// board is withdrawn (a caller-side terminal disposition, which retires the
// room's presentation) and the fresh board is advanced onto the same room. The
// participant sees one board become another.

// The caller identity this plugin asserts on the generic interaction tools.
//
// It is not a choice so much as an observation: the room-backed tools carry no
// caller argument, so everything this plugin opens is owned by the anonymous
// partition of the local authority (tangent's internal/mcp/caller_identity.go says why),
// and a withdrawal has to name the same caller or it is not the owner. The
// values are duplicated rather than imported because tangent's internal/roomflow is
// Tangent core and a plugin is userland; a plugin reaching into core for a
// constant would be a coupling this whole boundary exists to prevent.
const (
	callerPartition    = "anonymous"
	callerPrincipalRef = "loopback-mcp-caller"
)

// SyncInput is the sync tool's and the sync route's arguments.
type SyncInput struct {
	RoomID string `json:"room_id"`
	// Force reopens a task leaving a terminal status. Torque guards `done` and
	// `archived` behind it so a finished task is not reopened by a stray call;
	// a board move out of Done is not stray, but it is still deliberate, so the
	// caller says so.
	Force bool `json:"force,omitempty"`
}

// AppliedChange is one status move this sync pushed to Torque.
type AppliedChange struct {
	TaskID string `json:"task_id"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// FailedChange is one status move Torque refused, with Torque's own reason.
type FailedChange struct {
	TaskID string `json:"task_id"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// SyncResult is what moved, in both directions.
type SyncResult struct {
	RoomID  string          `json:"room_id"`
	BoardID string          `json:"board_id"`
	Applied []AppliedChange `json:"applied"`
	Failed  []FailedChange  `json:"failed"`
	Cards   int             `json:"cards"`
	// Truncated says Torque held more matches than the fresh board could
	// send. Reported to the agent for the reason OpenResult.Truncated is.
	Truncated bool   `json:"truncated"`
	Scope     string `json:"scope"`
	// SyncedAt is when the fresh cards were read.
	SyncedAt string `json:"synced_at"`
}

// Sync applies staged changes and replaces the board with fresh cards.
func (p *Plugin) Sync(ctx context.Context, input SyncInput) (SyncResult, error) {
	if input.RoomID == "" {
		return SyncResult{}, fmt.Errorf("torque: sync needs a room_id")
	}
	tools, err := p.tools()
	if err != nil {
		return SyncResult{}, err
	}

	board, err := readBoard(ctx, tools, input.RoomID)
	if err != nil {
		return SyncResult{}, err
	}

	applied, failed := p.applyStaged(ctx, board, input.Force)

	page, err := p.client.ListTasks(ctx, board.filters)
	if err != nil {
		return SyncResult{}, err
	}

	// Withdraw before advancing: the room admits one pending envelope, and the
	// withdrawal is what retires the presentation so the next advance is not
	// refused as busy.
	if err := withdraw(ctx, tools, input.RoomID, board); err != nil {
		return SyncResult{}, err
	}

	openInput := OpenInput{
		Title:    board.title,
		Statuses: board.filters.Statuses,
		ReadOnly: !board.syncEnabled,
	}
	data := p.boardData(board.boardID, openInput, board.filters, page)
	envelope := boardEnvelope(newEnvelopeID(), board.title, data, board.filters)
	if err := advance(ctx, tools, input.RoomID, envelope); err != nil {
		return SyncResult{}, err
	}

	return SyncResult{
		RoomID: input.RoomID, BoardID: board.boardID,
		Applied: applied, Failed: failed,
		Cards: len(page.Tasks), Truncated: page.More,
		Scope: data.Sync.scopeOrEmpty(), SyncedAt: data.UpdatedAt,
	}, nil
}

// applyStaged pushes each staged change to Torque.
//
// A refusal is collected rather than raised: Torque's transition messages name
// the remedy ("done is terminal — pass force=true to reopen this task"), and a
// sync that aborted on the first one would hide the other nine.
func (p *Plugin) applyStaged(
	ctx context.Context,
	board boardState,
	force bool,
) ([]AppliedChange, []FailedChange) {
	applied := make([]AppliedChange, 0, len(board.staged))
	failed := make([]FailedChange, 0)

	taskIDs := make([]string, 0, len(board.staged))
	for taskID := range board.staged {
		taskIDs = append(taskIDs, taskID)
	}
	// Sorted so two syncs of the same staged set apply in the same order, which
	// is what makes a partial failure reproducible.
	sort.Strings(taskIDs)

	for _, taskID := range taskIDs {
		target := board.staged[taskID]
		current := board.statusOf[taskID]
		if target == "" || target == current {
			continue
		}
		if err := p.client.Transition(ctx, taskID, target, force); err != nil {
			failed = append(failed, FailedChange{TaskID: taskID, To: target, Reason: err.Error()})
			continue
		}
		applied = append(applied, AppliedChange{TaskID: taskID, From: current, To: target})
	}
	return applied, failed
}

// boardState is everything a sync needs to read off the surface before it acts.
type boardState struct {
	interactionID string
	revision      int64
	boardID       string
	title         string
	filters       ListFilters
	syncEnabled   bool
	// staged maps a card id to the column the participant moved it into.
	staged map[string]string
	// statusOf is the status each card was in when the board was sent, used to
	// skip a staged change that is already true and to report `from`.
	statusOf map[string]string
}

// readBoard hydrates the surface and finds the pending board.
func readBoard(
	ctx context.Context,
	tools tangentplugin.ToolCaller,
	roomID string,
) (boardState, error) {
	result, err := tools.CallTool(ctx, "tangent.surface_get", map[string]any{
		"surface_id": roomID,
		// The caller reading back the surface it opened, which is the pull ADR
		// 0007 §5 describes rather than a third party reading someone else's
		// view state.
		"requester_scope": callerPartition,
	})
	if err != nil {
		return boardState{}, err
	}
	var snapshot surfaceSnapshot
	if decodeErr := result.Unmarshal(&snapshot); decodeErr != nil {
		return boardState{}, fmt.Errorf("torque: read surface %s: %w", roomID, decodeErr)
	}

	pending := snapshot.pendingBoard()
	if pending == nil {
		return boardState{}, fmt.Errorf(
			"torque: room %s has no open Torque board to sync", roomID)
	}

	envelope, err := pending.envelope()
	if err != nil {
		return boardState{}, err
	}
	filters, ok := decodeFilters(envelope.Meta)
	if !ok {
		return boardState{}, fmt.Errorf(
			"torque: the board in room %s was not opened by this plugin "+
				"(its envelope carries no %s), so there are no filters to re-query with",
			roomID, metaFiltersKey)
	}

	state := boardState{
		interactionID: pending.ID,
		revision:      pending.Revision,
		boardID:       envelope.Data.BoardID,
		title:         envelope.Title,
		filters:       filters,
		syncEnabled:   envelope.Data.Sync != nil && envelope.Data.Sync.Enabled,
		staged:        map[string]string{},
		statusOf:      map[string]string{},
	}
	if state.title == "" {
		state.title = envelope.Data.Title
	}
	for _, card := range envelope.Data.Cards {
		if status, ok := card.Fields["status"].(string); ok {
			state.statusOf[card.ID] = status
		}
	}
	for cardID, change := range snapshot.latestDraft(pending.ID).StagedChanges {
		state.staged[cardID] = change.ColumnID
	}
	return state, nil
}

// withdraw terminalizes the pending board so the room can be advanced again.
//
// `caller_withdrawn` is the honest cause: this plugin opened the interaction
// and is replacing it. The participant did not cancel anything, and recording
// that they did would put a wrong fact in an immutable record.
//
// # Why it retries exactly once
//
// A terminal disposition pins the revision it expected, and an interaction's
// revision moves on its own: presenting it to a browser bumps it, and that
// happens asynchronously after the envelope is delivered. So a sync issued in
// the seconds after a board opened can read revision N and find N+1 by the time
// it writes. That is a benign race — nothing about the board changed, only its
// presentation state — and re-reading and retrying once is the honest response.
//
// It is once, not a loop. A second conflict means something is genuinely
// changing the interaction underneath this sync, and grinding against it would
// turn a fast refusal the participant can act on into a slow one they cannot.
func withdraw(
	ctx context.Context,
	tools tangentplugin.ToolCaller,
	roomID string,
	board boardState,
) error {
	err := cancelInteraction(ctx, tools, board.interactionID, board.revision)
	if err == nil || !isStaleRevision(err) {
		return err
	}
	fresh, readErr := readBoard(ctx, tools, roomID)
	if readErr != nil {
		return err
	}
	if fresh.interactionID != board.interactionID {
		// A different board is open now: something else replaced it while this
		// sync was running, and withdrawing the new one would destroy work this
		// sync never read.
		return fmt.Errorf(
			"torque: the board in room %s was replaced while syncing; press Sync again", roomID)
	}
	return cancelInteraction(ctx, tools, fresh.interactionID, fresh.revision)
}

// isStaleRevision reports the one refusal withdraw retries.
func isStaleRevision(err error) bool {
	return err != nil && strings.Contains(err.Error(), "stale_revision")
}

func cancelInteraction(
	ctx context.Context,
	tools tangentplugin.ToolCaller,
	interactionID string,
	revision int64,
) error {
	board := boardState{interactionID: interactionID, revision: revision}
	return cancelOnce(ctx, tools, board)
}

func cancelOnce(ctx context.Context, tools tangentplugin.ToolCaller, board boardState) error {
	result, err := tools.CallTool(ctx, "tangent.interaction_cancel", map[string]any{
		"interaction_id":    board.interactionID,
		"expected_revision": board.revision,
		// The caller a room workflow resolves to. A room-backed tool carries no
		// caller argument, so it owns its interactions as the anonymous
		// partition of the local authority — and only that caller may withdraw
		// one. This plugin asserts the same identity because it IS that caller:
		// the board it is replacing is the board it opened.
		"requester": map[string]any{
			"scope": callerPartition, "principal_ref": callerPrincipalRef,
		},
		"cause":  "caller_withdrawn",
		"reason": "replaced by a synced board",
	})
	if err != nil {
		return err
	}
	if result.IsError {
		return fmt.Errorf("torque: withdraw the open board: %s", string(result.Content))
	}
	return nil
}

// ── The projections this plugin reads off Tangent's tool results ────────────
//
// Each one is the narrow slice of a canonical record this plugin uses, decoded
// from the tool's JSON. Declaring them here rather than importing Tangent's own
// record types is deliberate: a plugin reads a tool's public output, and a
// plugin that reached for the internal type would be a plugin coupled to a
// shape nothing promised it.

type surfaceSnapshot struct {
	Interactions []interactionRecord `json:"interactions"`
	Drafts       []draftRevision     `json:"drafts"`
}

// interactionRecord is the slice of a canonical interaction a sync reads.
//
// The two payload fields are NOT interchangeable, and confusing them is the
// bug this comment exists to prevent. `request_snapshot` is the canonical,
// schema-validated request — for a room workflow that is the envelope's `data`
// block alone. The whole envelope, with its title and its meta, is retained
// separately as `external_refs.legacy_envelope`, which roomflow documents as
// "the v0.12 presentation artifact restart reconstruction rebuilds the room
// from". This plugin needs both: the data for the cards, the meta for the
// filters it opened the board with.
type interactionRecord struct {
	ID              string          `json:"interaction_id"`
	State           string          `json:"state"`
	Revision        int64           `json:"revision"`
	RequestSnapshot json.RawMessage `json:"request_snapshot"`
	ExternalRefs    struct {
		LegacyEnvelope struct {
			ID    string                     `json:"id"`
			Title string                     `json:"title"`
			Meta  map[string]json.RawMessage `json:"meta"`
			Data  json.RawMessage            `json:"data"`
		} `json:"legacy_envelope"`
	} `json:"external_refs"`
	Definition struct {
		Kind string `json:"kind"`
	} `json:"definition_binding"`
}

type draftRevision struct {
	InteractionID string          `json:"interaction_id"`
	Revision      int64           `json:"revision"`
	Payload       json.RawMessage `json:"payload"`
}

// appBoardDraft is the participant's view state, as ui/src/components/
// envelopes/AppBoard.tsx writes it. Only StagedChanges is read here: the
// filters and the selection are what the participant is looking at, and this
// plugin has no business acting on either.
type appBoardDraft struct {
	StagedChanges map[string]struct {
		ColumnID string `json:"column_id"`
	} `json:"staged_changes"`
}

// boardEnvelopeRecord is the board as it was presented: the envelope's data
// block, plus the title and meta from the retained presentation artifact.
type boardEnvelopeRecord struct {
	Title string
	Meta  map[string]json.RawMessage
	Data  struct {
		BoardID string `json:"board_id"`
		Title   string `json:"title"`
		Cards   []Card `json:"cards"`
		Sync    *Sync  `json:"sync"`
	}
}

// openStates are the interaction states a board is still on screen in.
//
// Stated positively, and that is the safety property rather than a style
// preference: a state this plugin does not recognize is treated as NOT open, so
// the worst case is a sync that reports "no open board" instead of one that
// withdraws an interaction somebody already settled. The canonical vocabulary
// is internal/interaction's, which this package deliberately does not import —
// a plugin is userland, and coupling it to core's record types is the thing
// this whole boundary exists to prevent.
var openStates = map[string]bool{
	"submitted": true, "validated": true, "staged": true,
	"presented": true, "in_progress": true,
}

// pendingBoard returns the surface's open app-board interaction, newest first.
// A surface carries the history of every board it has shown; only one of them
// is the one on screen.
func (s surfaceSnapshot) pendingBoard() *interactionRecord {
	var newest *interactionRecord
	for index := range s.Interactions {
		record := &s.Interactions[index]
		if record.Definition.Kind != EnvelopeType || !openStates[record.State] {
			continue
		}
		if newest == nil || record.Revision >= newest.Revision {
			newest = record
		}
	}
	return newest
}

// latestDraft returns the highest-revision draft for an interaction.
//
// Highest rather than last-in-the-array: the draft sequence is the contract —
// the store computes MAX(revision) + 1 and refuses anything else — so revision
// order is the only ordering guaranteed to be the participant's.
func (s surfaceSnapshot) latestDraft(interactionID string) appBoardDraft {
	var newest *draftRevision
	for index := range s.Drafts {
		draft := &s.Drafts[index]
		if draft.InteractionID != interactionID {
			continue
		}
		if newest == nil || draft.Revision > newest.Revision {
			newest = draft
		}
	}
	if newest == nil {
		return appBoardDraft{}
	}
	var payload appBoardDraft
	if err := json.Unmarshal(newest.Payload, &payload); err != nil {
		// A draft this plugin cannot read is a board with nothing staged, not
		// a failed sync: the pull direction still works and is what the
		// participant pressed the button for.
		return appBoardDraft{}
	}
	return payload
}

// envelope reassembles the board from the two places it is retained.
//
// The data block is preferred from the retained presentation artifact and falls
// back to the canonical request snapshot. They are the same bytes for this kind
// — the app-board handler presents a clone of the request — but the fallback
// costs one line and means a board still reads if a future revision of the
// handler presents something narrower.
func (r *interactionRecord) envelope() (boardEnvelopeRecord, error) {
	envelope := boardEnvelopeRecord{
		Title: r.ExternalRefs.LegacyEnvelope.Title,
		Meta:  r.ExternalRefs.LegacyEnvelope.Meta,
	}
	data := r.ExternalRefs.LegacyEnvelope.Data
	if len(data) == 0 {
		data = r.RequestSnapshot
	}
	if len(data) == 0 {
		return envelope, fmt.Errorf(
			"torque: interaction %s retains no board content to read", r.ID)
	}
	if err := json.Unmarshal(data, &envelope.Data); err != nil {
		return envelope, fmt.Errorf(
			"torque: decode the board on interaction %s: %w", r.ID, err)
	}
	return envelope, nil
}
