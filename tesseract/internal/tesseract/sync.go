package tesseract

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// Syncing a review board, in one press.
//
// # The line this file exists to hold
//
// **The plugin applies mechanical changes. The agent applies authored ones.**
//
// That was CW-20260910-0054's rule going in, and Tesseract turned out to put
// the boundary in a different place than the task expected. The task assumed a
// status flip was mechanical — a `for` loop, the way a Torque transition is.
// It is not, and the store says so in as many words:
//
//	internal/memory/types.go: "Revision is an immutable memory revision. The
//	only field that may be mutated after write is Status, and only via the
//	deprecation code path."
//
// The memory domain exposes no status route. Verified against the running
// service: POST /v1/context/status-set does not exist, and its real cousin
// POST /v1/context/status/promote reaches the CONTEXT store — aimed at a real
// memory record it answers `not_found: sql: no rows in result set`. So:
//
//   - **Into `deprecated`** is one call naming one revision id. Nothing is
//     authored, nothing is copied, and the plugin applies it.
//   - **draft → reviewed → canonical** is `POST /v1/memory/write` carrying the
//     whole payload forward with `supersedes`, which deprecates the old
//     revision and stamps a fresh `created_at`. That is a new revision, and the
//     task's own rule reserves those for an author: "in an append-only store a
//     reword is a new revision with `supersedes`, which only an author should
//     write." Promotion is the same act, so it gets the same answer.
//
// Chrispian's call, 2026-09-10: the agent does that write. So a promotion
// leaves here as a REQUEST, on the same work list as a reword, and this plugin
// makes exactly one kind of write to Tesseract.
//
// # The order, and why it is the order
//
//  1. Read the surface. The board's pending interaction, its immutable request
//     snapshot (which carries the recall it was opened with and any requests
//     already outstanding), and its latest draft revision (which carries what
//     the participant staged and wrote).
//  2. Apply the deprecations. This happens first so the re-query below observes
//     the result rather than the state before it — a deprecated revision drops
//     out of recall, so the card goes away, which is the feedback.
//  3. Collect the requests: promotions and rewords, carried forward with any
//     still outstanding from before.
//  4. Re-query Tesseract with the SAME recall, and replace the board.
//
// A failure in step 2 does not abort the sync. A revision Tesseract refused is
// reported by name with Tesseract's own message, and the board is still
// refreshed, because the useful thing after a partial failure is a board
// showing what actually happened.
//
// # Why a draft is not a decision
//
// ADR 0007 §5 is explicit that a caller must not present a draft as a decision.
// Staged moves and staged notes live in the draft and this plugin does not act
// on them until the participant presses Sync. **The press is the decision.**
// That is why a board left open with a staged deprecation and never synced
// changes nothing in Tesseract, which is the property that makes the
// distinction real rather than verbal.
//
// # Why the board is cancelled and re-presented rather than superseded
//
// `tangent.interaction_supersede` needs its replacement to exist first, and the
// only thing that can put an envelope in front of a participant is a room
// advance — which refuses while another envelope is pending. So the pending
// board is withdrawn and the fresh board is advanced onto the same room.

// The caller identity this plugin asserts on the generic interaction tools.
//
// The room-backed tools carry no caller argument, so everything this plugin
// opens is owned by the anonymous partition of the local authority, and a
// withdrawal has to name the same caller or it is not the owner. The values are
// duplicated rather than imported because tangent's internal/roomflow is Tangent core and
// a plugin is userland.
const (
	callerPartition    = "anonymous"
	callerPrincipalRef = "loopback-mcp-caller"
)

// Request kinds. Both are work handed to the agent, and neither is a write this
// plugin makes.
const (
	// RequestPromotion is a staged move up the lifecycle. It is authored
	// because Tesseract has no in-place promotion; see this file's header.
	RequestPromotion = "promotion"
	// RequestReword is the participant asking for different wording. It is
	// authored because wording needs judgement.
	RequestReword = "reword"
	// RequestSupersede is a note on a card the participant ALSO retired:
	// "retire this and say it better."
	//
	// It is a separate kind rather than a reword against a dead record because
	// in an append-only store that pairing is not a contradiction — it is the
	// definition of a supersede, and it is probably the most useful thing a
	// review pass produces. The first version of this file read the note as
	// junk attached to a doomed record and dropped it silently; Chrispian lost
	// a real one to that on the first board he used.
	RequestSupersede = "supersede"
)

// Request is one piece of work handed back to the agent.
//
// It carries everything the agent needs to act without re-deriving anything:
// the revision to supersede, the memory it belongs to, where it lives, and what
// was asked for. The agent hydrates the full body by revision id.
type Request struct {
	Kind       string `json:"kind"`
	RevisionID string `json:"revision_id"`
	MemoryID   string `json:"memory_id,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	MemoryKey  string `json:"memory_key,omitempty"`
	// FromStatus is the status the record was in when the board was sent.
	FromStatus string `json:"from_status,omitempty"`
	// ToStatus is the column the participant moved it into. Set on a
	// promotion, empty on a reword.
	ToStatus string `json:"to_status,omitempty"`
	// Note is what the participant wrote. Set on a reword, empty on a
	// promotion.
	Note string `json:"note,omitempty"`
	// Summary is the record's summary as the board showed it, so the agent can
	// see what was being commented on without a second call.
	Summary string `json:"summary,omitempty"`
}

// badgeLabel names an outstanding request on a card.
func (r Request) badgeLabel() string {
	switch r.Kind {
	case RequestPromotion:
		if r.ToStatus != "" {
			return "promotion requested → " + r.ToStatus
		}
		return "promotion requested"
	case RequestReword:
		return "reword requested"
	case RequestSupersede:
		return "supersede requested"
	case "":
		return ""
	default:
		return r.Kind + " requested"
	}
}

// SyncInput is the sync tool's and the sync route's arguments.
type SyncInput struct {
	RoomID string `json:"room_id"`
}

// Deprecated is one revision this sync retired.
type Deprecated struct {
	RevisionID string `json:"revision_id"`
	MemoryKey  string `json:"memory_key,omitempty"`
	FromStatus string `json:"from_status,omitempty"`
}

// Failed is one disposition Tesseract refused, with Tesseract's own reason.
type Failed struct {
	RevisionID string `json:"revision_id"`
	MemoryKey  string `json:"memory_key,omitempty"`
	Reason     string `json:"reason"`
}

// SyncResult is what this sync did, and what it handed back.
//
// # Why the work list is in this response
//
// CW-20260910-0054 left the delivery of the reword comments open — the Sync
// response, or a later `tangent.surface_get` — and asked for the choice to be
// made on evidence. It is the response, for three reasons the code makes
// visible:
//
//  1. **The list already has to be assembled here.** A promotion request only
//     exists because this sync partitioned the staged moves, and only this sync
//     knows the from-status. Nothing else can reconstruct it without redoing
//     that work.
//  2. **The agent-callable twin returns the same struct.** An agent that syncs
//     inside a turn it is already spending gets the work list in that turn,
//     which is the point of the twin existing at all.
//  3. **surface_get still works and is not being closed off.** The notes are in
//     the draft, and the draft is what surface_get returns, so an agent that
//     was not in the room when the button was pressed can still collect them.
//     Putting the list in the response adds a fast path without removing the
//     durable one.
//
// **The agent is not automatically poked either way.** That is an accepted MVP
// limitation of this task, not a defect: nothing in this plugin notifies
// anyone, and a request waits until an agent asks. What the board does instead
// is keep the request visible — see metaRequestsKey — so the participant can
// tell what they have already asked for.
type SyncResult struct {
	RoomID  string `json:"room_id"`
	BoardID string `json:"board_id"`
	// Deprecated is what this plugin applied. It is the whole of what this
	// plugin writes to Tesseract.
	Deprecated []Deprecated `json:"deprecated"`
	// Failed is what Tesseract refused.
	Failed []Failed `json:"failed"`
	// Requests is the work handed to the agent: promotions and rewords, plus
	// anything still outstanding from an earlier sync.
	Requests []Request `json:"requests"`
	Cards    int       `json:"cards"`
	// Matching and Truncated report the fresh recall's own account of itself.
	Matching  int    `json:"matching"`
	Truncated bool   `json:"truncated"`
	Scope     string `json:"scope"`
	SyncedAt  string `json:"synced_at"`
}

// Sync applies staged deprecations, collects the work list, and replaces the
// board with fresh cards.
func (p *Plugin) Sync(ctx context.Context, input SyncInput) (SyncResult, error) {
	if input.RoomID == "" {
		return SyncResult{}, fmt.Errorf("tesseract: sync needs a room_id")
	}
	tools, err := p.tools()
	if err != nil {
		return SyncResult{}, err
	}

	board, err := readBoard(ctx, tools, input.RoomID)
	if err != nil {
		return SyncResult{}, err
	}

	deprecated, failed, requests, withdrawn := p.applyStaged(ctx, board)

	result, err := p.client.Recall(ctx, board.recall)
	if err != nil {
		return SyncResult{}, err
	}

	// Carried after the re-query, because what clears a request is the card
	// disappearing from the fresh set.
	pending := carryForward(board.requests, requests, withdrawn, result.Revisions)

	// A supersede names a record this board just retired, so recall will never
	// return it again — and a request with no card is one the participant
	// cannot see. Hydrating it back onto the board is what makes the handoff
	// visible: the record sits in Deprecated, wearing its badge and its note,
	// until the replacement lands.
	result.Revisions = append(result.Revisions, p.hydrateRetired(ctx, pending, result.Revisions)...)

	// Withdraw before advancing: the room admits one pending envelope, and the
	// withdrawal is what retires the presentation so the next advance is not
	// refused as busy.
	if err := withdraw(ctx, tools, input.RoomID, board); err != nil {
		return SyncResult{}, err
	}

	data := p.boardData(
		board.boardID, board.title, !board.syncEnabled, board.recall, result, pending)
	envelope := boardEnvelope(newEnvelopeID(), board.title, data, board.recall, pending)
	if err := advance(ctx, tools, input.RoomID, envelope); err != nil {
		return SyncResult{}, err
	}

	return SyncResult{
		RoomID: input.RoomID, BoardID: board.boardID,
		Deprecated: deprecated, Failed: failed,
		Requests:  reportedRequests(requests, pending),
		Cards:     len(result.Revisions),
		Matching:  result.Manifest.ResultsTotal,
		Truncated: result.Manifest.Truncated,
		Scope:     data.Sync.scopeOrEmpty(),
		SyncedAt:  data.UpdatedAt,
	}, nil
}

// applyStaged partitions what the participant staged and applies the half this
// plugin owns.
//
// A refusal is collected rather than raised: a sync that aborted on the first
// one would hide the other nine, and Tesseract's messages name what it could
// not find.
func (p *Plugin) applyStaged(
	ctx context.Context,
	board boardState,
) ([]Deprecated, []Failed, map[string]Request, map[string]bool) {
	deprecated := make([]Deprecated, 0, len(board.staged))
	failed := make([]Failed, 0)
	requests := map[string]Request{}
	withdrawn := map[string]bool{}

	revisionIDs := make([]string, 0, len(board.staged))
	for revisionID := range board.staged {
		revisionIDs = append(revisionIDs, revisionID)
	}
	// Sorted so two syncs of the same staged set apply in the same order, which
	// is what makes a partial failure reproducible.
	sort.Strings(revisionIDs)

	// What this sync actually retired. A note on one of these is a supersede
	// rather than a reword — and it is keyed on the deprecation SUCCEEDING, so
	// a refused deprecation leaves the note as an ordinary reword against a
	// record that is still there.
	retired := map[string]bool{}

	for _, revisionID := range revisionIDs {
		target := board.staged[revisionID]
		card := board.cards[revisionID]
		if target == "" || target == card.FromStatus {
			continue
		}
		if target != StatusDeprecated {
			// Authored. Handed back rather than applied — see the file header.
			requests[revisionID] = Request{
				Kind: RequestPromotion, RevisionID: revisionID,
				MemoryID: card.MemoryID, Namespace: card.Namespace,
				MemoryKey: card.MemoryKey, FromStatus: card.FromStatus,
				ToStatus: target, Summary: card.Summary,
			}
			continue
		}
		if err := p.client.Deprecate(ctx, revisionID); err != nil {
			failed = append(failed, Failed{
				RevisionID: revisionID, MemoryKey: card.MemoryKey, Reason: err.Error(),
			})
			continue
		}
		retired[revisionID] = true
		deprecated = append(deprecated, Deprecated{
			RevisionID: revisionID, MemoryKey: card.MemoryKey, FromStatus: card.FromStatus,
		})
	}

	// A note on a card that was ALSO staged for promotion wins: the reword is
	// the more specific ask, and the agent doing a reword decides the resulting
	// revision's status anyway, so recording both would be recording the same
	// work twice.
	//
	// An EMPTY note is a withdrawal, not a no-op. The board seeds the note box
	// from the request already on record, so a card the participant emptied is
	// one they cleared on purpose — and a cleared box that left the request
	// standing would be a control that visibly did nothing. A card they never
	// touched and that carries no note has no key here at all, which is what
	// keeps the two apart.
	for revisionID, note := range board.notes {
		note = strings.TrimSpace(note)
		if note == "" {
			withdrawn[revisionID] = true
			continue
		}
		card := board.cards[revisionID]
		kind := RequestReword
		if retired[revisionID] {
			kind = RequestSupersede
		}
		requests[revisionID] = Request{
			Kind: kind, RevisionID: revisionID,
			MemoryID: card.MemoryID, Namespace: card.Namespace,
			MemoryKey: card.MemoryKey, FromStatus: card.FromStatus,
			Note: note, Summary: card.Summary,
		}
	}
	return deprecated, failed, requests, withdrawn
}

// carryForward merges the requests already outstanding with the ones this sync
// raised, and drops the ones that have been serviced.
//
// # Two clearing rules, because there are two shapes of request
//
// A promotion or a reword names a record that is still on the board, and it
// clears when that revision stops coming back: every way of servicing one
// writes a new revision and deprecates the one the request named, so the named
// revision leaves the fresh card set exactly when the work is done. No
// cooperation from the agent is needed. A request for a record that has merely
// scrolled out of a narrowed recall is dropped too — the alternative is a badge
// promising a card that is not there.
//
// **A supersede cannot use that rule, and getting this wrong is what lost a
// real request on the first board anyone used.** A supersede names a record
// this same sync just retired, so its revision is gone from the card set the
// instant it is raised — the "serviced" test fires immediately and the
// participant's words vanish with no trace. It clears instead when a revision
// carrying the SAME memory_key comes back, which is what the agent writing the
// replacement produces.
//
// An unkeyed record has no such handle. Rather than carry one forever, it is
// reported in the sync result and not carried: the response is the fast path
// and it has already been taken. The scope line counts what is outstanding so
// the difference is visible rather than silent.
func carryForward(
	existing map[string]Request,
	raised map[string]Request,
	withdrawn map[string]bool,
	fresh []Revision,
) map[string]Request {
	if len(existing) == 0 && len(raised) == 0 {
		return nil
	}
	present := make(map[string]bool, len(fresh))
	keys := make(map[string]bool, len(fresh))
	for _, revision := range fresh {
		present[revision.RevisionID] = true
		if revision.MemoryKey != "" {
			keys[revision.MemoryKey] = true
		}
	}

	keep := func(request Request) bool {
		if request.Kind == RequestSupersede {
			// Unkeyed: nothing to watch for, so do not carry it.
			if request.MemoryKey == "" {
				return false
			}
			// A revision under this key is back, so the replacement exists.
			return !keys[request.MemoryKey]
		}
		return present[request.RevisionID]
	}

	merged := map[string]Request{}
	for revisionID, request := range existing {
		// A withdrawal retracts a REWORD or a SUPERSEDE — both are the note
		// box, and emptying it is the participant clearing what they wrote. A
		// promotion has no note box, so an empty note says nothing about one;
		// dropping it here would let clearing a box the participant never
		// filled cancel a disposition they did stage.
		if withdrawn[revisionID] && request.Kind != RequestPromotion {
			continue
		}
		if keep(request) {
			merged[revisionID] = request
		}
	}
	// Raised last so a fresh ask replaces a stale one for the same record.
	for revisionID, request := range raised {
		if keep(request) {
			merged[revisionID] = request
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// reportedRequests is the work list this sync hands back: everything raised
// here, plus everything still outstanding from before.
//
// The two are not the same set and the difference is not academic. `pending` is
// what rides on the next envelope, and some requests deliberately do not — an
// unkeyed supersede has no handle to watch for a replacement, so carrying it
// would carry it forever. Reporting only `pending` would mean those were never
// reported at all, which is the same silent loss this whole area was fixed for,
// reintroduced one layer up. The response is the fast path; it reports
// everything the agent should act on.
//
// A withdrawn request appears in neither: clearing the note box raises nothing
// and drops what was carried.
func reportedRequests(raised, pending map[string]Request) []Request {
	merged := make(map[string]Request, len(raised)+len(pending))
	for revisionID, request := range pending {
		merged[revisionID] = request
	}
	// Raised last: a fresh ask is the current one.
	for revisionID, request := range raised {
		merged[revisionID] = request
	}
	out := make([]Request, 0, len(merged))
	for _, request := range merged {
		out = append(out, request)
	}
	// Sorted so two syncs of the same state report the same thing.
	sort.Slice(out, func(i, j int) bool { return out[i].RevisionID < out[j].RevisionID })
	return out
}

// hydrateRetired fetches the records that outstanding supersede requests name
// and that the fresh recall did not return.
//
// A failure is not a failed sync. Tesseract may be down, or the revision may
// have been trimmed; either way the request is still reported in the response
// and still counted in the scope line, so the worst case is the feedback this
// board had before — a count instead of a card — rather than a refusal the
// participant cannot act on.
func (p *Plugin) hydrateRetired(
	ctx context.Context,
	pending map[string]Request,
	fresh []Revision,
) []Revision {
	if len(pending) == 0 {
		return nil
	}
	present := make(map[string]bool, len(fresh))
	for _, revision := range fresh {
		present[revision.RevisionID] = true
	}
	revisionIDs := make([]string, 0, len(pending))
	for revisionID, request := range pending {
		if request.Kind == RequestSupersede && !present[revisionID] {
			revisionIDs = append(revisionIDs, revisionID)
		}
	}
	// Sorted so two syncs of the same state build the same board.
	sort.Strings(revisionIDs)

	retired := make([]Revision, 0, len(revisionIDs))
	for _, revisionID := range revisionIDs {
		revision, err := p.client.GetRevision(ctx, revisionID)
		if err != nil {
			continue
		}
		retired = append(retired, revision)
	}
	return retired
}

// cardState is what the board knew about one record when it was sent.
type cardState struct {
	MemoryID   string
	Namespace  string
	MemoryKey  string
	FromStatus string
	Summary    string
}

// boardState is everything a sync needs to read off the surface before it acts.
type boardState struct {
	interactionID string
	revision      int64
	boardID       string
	title         string
	recall        RecallInput
	syncEnabled   bool
	// staged maps a revision id to the column the participant moved it into.
	staged map[string]string
	// notes maps a revision id to what the participant wrote about it.
	notes map[string]string
	// cards is what the board showed, used to skip a staged change that is
	// already true and to fill a request without a second read.
	cards map[string]cardState
	// requests are the ones already outstanding when this board was presented.
	requests map[string]Request
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
		return boardState{}, fmt.Errorf("tesseract: read surface %s: %w", roomID, decodeErr)
	}

	pending := snapshot.pendingBoard()
	if pending == nil {
		return boardState{}, fmt.Errorf(
			"tesseract: room %s has no open Tesseract review board to sync", roomID)
	}

	envelope, err := pending.envelope()
	if err != nil {
		return boardState{}, err
	}
	recall, ok := decodeRecall(envelope.Meta)
	if !ok {
		return boardState{}, fmt.Errorf(
			"tesseract: the board in room %s was not opened by this plugin "+
				"(its envelope carries no %s), so there is no recall to re-query with",
			roomID, metaRecallKey)
	}

	state := boardState{
		interactionID: pending.ID,
		revision:      pending.Revision,
		boardID:       envelope.Data.BoardID,
		title:         envelope.Title,
		recall:        recall,
		syncEnabled:   envelope.Data.Sync != nil && envelope.Data.Sync.Enabled,
		staged:        map[string]string{},
		notes:         map[string]string{},
		cards:         map[string]cardState{},
		requests:      decodeRequests(envelope.Meta),
	}
	if state.title == "" {
		state.title = envelope.Data.Title
	}
	for _, card := range envelope.Data.Cards {
		state.cards[card.ID] = cardState{
			MemoryID:   stringField(card.Fields, "memory_id"),
			Namespace:  stringField(card.Fields, "namespace"),
			MemoryKey:  stringField(card.Fields, "memory_key"),
			FromStatus: stringField(card.Fields, "status"),
			Summary:    card.Body,
		}
	}
	draft := snapshot.latestDraft(pending.ID)
	for cardID, change := range draft.StagedChanges {
		state.staged[cardID] = change.ColumnID
	}
	for cardID, note := range draft.StagedNotes {
		state.notes[cardID] = note
	}
	return state, nil
}

func stringField(fields map[string]any, key string) string {
	if value, ok := fields[key].(string); ok {
		return value
	}
	return ""
}

// withdraw terminalizes the pending board so the room can be advanced again.
//
// `caller_withdrawn` is the honest cause: this plugin opened the interaction
// and is replacing it. The participant did not cancel anything.
//
// # Why it retries exactly once
//
// A terminal disposition pins the revision it expected, and an interaction's
// revision moves on its own: presenting it to a browser bumps it, and that
// happens asynchronously after the envelope is delivered. So a sync issued in
// the seconds after a board opened can read revision N and find N+1 by the time
// it writes. That is a benign race, and re-reading and retrying once is the
// honest response.
//
// It is once, not a loop. A second conflict means something is genuinely
// changing the interaction underneath this sync.
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
			"tesseract: the board in room %s was replaced while syncing; press Sync again", roomID)
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
	result, err := tools.CallTool(ctx, "tangent.interaction_cancel", map[string]any{
		"interaction_id":    interactionID,
		"expected_revision": revision,
		// The caller a room workflow resolves to. A room-backed tool carries no
		// caller argument, so it owns its interactions as the anonymous
		// partition of the local authority — and only that caller may withdraw
		// one. This plugin asserts the same identity because it IS that caller.
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
		return fmt.Errorf("tesseract: withdraw the open board: %s", string(result.Content))
	}
	return nil
}

// ── The projections this plugin reads off Tangent's tool results ────────────
//
// Each one is the narrow slice of a canonical record this plugin uses, decoded
// from the tool's JSON. Declaring them here rather than importing Tangent's own
// record types is deliberate: a plugin reads a tool's public output, and a
// plugin that reached for the internal type would be coupled to a shape nothing
// promised it.

type surfaceSnapshot struct {
	Interactions []interactionRecord `json:"interactions"`
	Drafts       []draftRevision     `json:"drafts"`
}

// interactionRecord is the slice of a canonical interaction a sync reads.
//
// The two payload fields are NOT interchangeable. `request_snapshot` is the
// canonical, schema-validated request — for a room workflow that is the
// envelope's `data` block alone. The whole envelope, with its title and its
// meta, is retained separately as `external_refs.legacy_envelope`. This plugin
// needs both: the data for the cards, the meta for the recall and the requests.
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

// appBoardDraft is the participant's view state, as
// ui/src/components/envelopes/AppBoard.tsx writes it. Only the two staging maps
// are read: the filters and the selection are what the participant is looking
// at, and this plugin has no business acting on either.
type appBoardDraft struct {
	StagedChanges map[string]struct {
		ColumnID string `json:"column_id"`
	} `json:"staged_changes"`
	// StagedNotes is the additive app-board extension this plugin needed. It is
	// a card id to free text, and it is the only way a reword reaches an agent.
	StagedNotes map[string]string `json:"staged_notes"`
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
// withdraws an interaction somebody already settled.
var openStates = map[string]bool{
	"submitted": true, "validated": true, "staged": true,
	"presented": true, "in_progress": true,
}

// pendingBoard returns the surface's open app-board interaction, newest first.
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
		// A draft this plugin cannot read is a board with nothing staged, not a
		// failed sync: the pull direction still works and is what the
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
			"tesseract: interaction %s retains no board content to read", r.ID)
	}
	if err := json.Unmarshal(data, &envelope.Data); err != nil {
		return envelope, fmt.Errorf(
			"tesseract: decode the board on interaction %s: %w", r.ID, err)
	}
	return envelope, nil
}
