package tesseract

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	plugin "github.com/hollis-labs/plugin-sdk"
)

// The sync flow, and the line it holds.
//
// The assertions worth reading twice are the two that encode CW-20260910-0054's
// rule as the STORE turned out to define it: a move into `deprecated` reaches
// Tesseract, and a move up the lifecycle does not — it becomes a request. That
// split is not a policy this plugin invented; a memory revision is immutable
// except for deprecation, so a promotion is a new revision with `supersedes`,
// which is authoring.

// surfaceWithBoard renders a Tangent surface snapshot holding one presented
// board, plus a draft carrying what the participant staged and wrote.
//
// The envelope is put in `external_refs.legacy_envelope` and its DATA BLOCK
// ALONE in `request_snapshot`, which is what a real surface does. Putting the
// whole envelope in both would hide a decode bug that only shows up against the
// real thing.
func surfaceWithBoard(
	t *testing.T,
	staged map[string]string,
	notes map[string]string,
	revisions []Revision,
	pending map[string]Request,
) string {
	t.Helper()
	recall := OpenInput{Namespaces: []string{"user/example/memory/decisions"}}.recall()
	data := BuildBoard(
		"board-1", "Tesseract review", revisions,
		Source{App: "tesseract", Label: "Tesseract"},
		&Sync{
			Enabled: true, Endpoint: SyncPath, Label: "Sync",
			StageLabel: "Disposition", NoteLabel: "Reword",
			Scope: "scope sentence",
		},
		pending, "2026-09-10T20:00:00Z",
	)
	envelope := boardEnvelope("env-1", "Tesseract review", data, recall, pending)

	requestSnapshot, err := json.Marshal(envelope["data"])
	if err != nil {
		t.Fatalf("encode request snapshot: %v", err)
	}
	presented, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode presented envelope: %v", err)
	}

	stagedChanges := map[string]map[string]string{}
	for cardID, columnID := range staged {
		stagedChanges[cardID] = map[string]string{"column_id": columnID}
	}
	draft, err := json.Marshal(map[string]any{
		"board_id":       "board-1",
		"filters":        map[string][]string{},
		"staged_changes": stagedChanges,
		"staged_notes":   notes,
	})
	if err != nil {
		t.Fatalf("encode draft: %v", err)
	}

	payload, err := json.Marshal(map[string]any{
		"interactions": []map[string]any{{
			"interaction_id":   "int-1",
			"state":            "presented",
			"revision":         3,
			"request_snapshot": json.RawMessage(requestSnapshot),
			"external_refs": map[string]any{
				"legacy_room_id":     "room-1",
				"legacy_envelope_id": "env-1",
				"legacy_envelope":    json.RawMessage(presented),
			},
			"definition_binding": map[string]any{"kind": EnvelopeType},
		}},
		"drafts": []map[string]any{
			{"interaction_id": "int-1", "revision": 1, "payload": json.RawMessage(`{"staged_changes":{}}`)},
			{"interaction_id": "int-1", "revision": 2, "payload": json.RawMessage(draft)},
		},
	})
	if err != nil {
		t.Fatalf("encode surface: %v", err)
	}
	return string(payload)
}

// TestSyncDeprecatesWhatWasStagedForRetirement is the one write this plugin
// makes, end to end and with no agent turn.
func TestSyncDeprecatesWhatWasStagedForRetirement(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	host.tools.answer("tangent.surface_get",
		surfaceWithBoard(t, map[string]string{"rev-1": StatusDeprecated}, nil, revisions(), nil))

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if got := fake.retired(); len(got) != 1 || got[0] != "rev-1" {
		t.Errorf("deprecated in Tesseract = %v, want [rev-1]", got)
	}
	if len(result.Deprecated) != 1 || result.Deprecated[0].RevisionID != "rev-1" {
		t.Errorf("result.Deprecated = %+v, want rev-1", result.Deprecated)
	}
	if result.Deprecated[0].FromStatus != "draft" {
		t.Errorf("from_status = %q, want the status the board showed", result.Deprecated[0].FromStatus)
	}
	if len(result.Requests) != 0 {
		t.Errorf("result.Requests = %+v, want none — a deprecation is not handed back", result.Requests)
	}
	// The card is gone from the fresh board, which is the feedback.
	if result.Cards != 2 {
		t.Errorf("cards after sync = %d, want 2", result.Cards)
	}

	// Push before pull, so the re-query observes the result rather than the
	// state before it. Two recalls: one for the board that was open, one fresh.
	want := []string{"tangent.surface_get", "tangent.interaction_cancel", "tangent.session_advance"}
	if got := host.tools.names(); len(got) != 3 ||
		got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("tool calls = %v, want %v", got, want)
	}
}

// TestSyncHandsAPromotionBackInsteadOfApplyingIt is the finding this task
// existed to produce, held as a test.
//
// Tesseract's memory revisions are immutable except for deprecation
// (internal/memory/types.go), and the domain exposes no status route. So a move
// up the lifecycle is a new revision with `supersedes` — authoring — and the
// agent makes it. The assertion that matters most is the negative one: NOTHING
// reached Tesseract.
func TestSyncHandsAPromotionBackInsteadOfApplyingIt(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	setSurface(t, board, map[string]string{"rev-1": "canonical"}, nil, nil)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if got := fake.retired(); len(got) != 0 {
		t.Fatalf("a promotion wrote to Tesseract: %v — the plugin makes one write and it is deprecate", got)
	}
	if len(result.Requests) != 1 {
		t.Fatalf("result.Requests = %+v, want one promotion", result.Requests)
	}
	request := result.Requests[0]
	if request.Kind != RequestPromotion {
		t.Errorf("kind = %q, want %q", request.Kind, RequestPromotion)
	}
	// Everything the agent needs to write the superseding revision without
	// re-deriving anything.
	if request.RevisionID != "rev-1" || request.ToStatus != "canonical" ||
		request.FromStatus != "draft" || request.MemoryID != "mem-1" ||
		request.Namespace != "user/example/memory/decisions" ||
		request.MemoryKey != "portfolio_ui_layering" {
		t.Errorf("request = %+v, want it to name the revision, memory, namespace, key and both statuses",
			request)
	}
}

// TestSyncHandsARewordBackAsAWorkItem — affordance 2. The note reaches the
// agent and nothing reaches Tesseract.
func TestSyncHandsARewordBackAsAWorkItem(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	setSurface(t, board, nil, map[string]string{"rev-2": "Say this in plainer words."}, nil)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := fake.retired(); len(got) != 0 {
		t.Fatalf("a reword wrote to Tesseract: %v", got)
	}
	if len(result.Requests) != 1 {
		t.Fatalf("result.Requests = %+v, want one reword", result.Requests)
	}
	request := result.Requests[0]
	if request.Kind != RequestReword || request.RevisionID != "rev-2" {
		t.Errorf("request = %+v, want a reword of rev-2", request)
	}
	if request.Note != "Say this in plainer words." {
		t.Errorf("note = %q, want the participant's own words", request.Note)
	}
	if request.Summary == "" {
		t.Error("the request carries no summary; the agent would have to re-read to see what was commented on")
	}
}

// TestAStagedNoteSurvivesTheRoundTrip: the note is written into a draft, read
// back off the surface, and reaches the agent — through the same JSON a real
// browser and a real surface would carry.
func TestAStagedNoteSurvivesTheRoundTrip(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	note := "Reword: this says 'plugin' where it means 'kind'."
	host.tools.answer("tangent.surface_get",
		surfaceWithBoard(t, nil, map[string]string{"rev-3": note}, revisions(), nil))

	state, err := readBoard(context.Background(), host.tools, "room-1")
	if err != nil {
		t.Fatalf("readBoard: %v", err)
	}
	if state.notes["rev-3"] != note {
		t.Fatalf("staged note read back = %q, want %q", state.notes["rev-3"], note)
	}

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Requests) != 1 || result.Requests[0].Note != note {
		t.Errorf("requests = %+v, want the note carried through verbatim", result.Requests)
	}
}

// TestAWhitespaceOnlyNoteIsNotARequest: an emptied box is an unwritten note.
func TestAWhitespaceOnlyNoteIsNotARequest(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	setSurface(t, board, nil, map[string]string{"rev-1": "   \n  "}, nil)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Requests) != 0 {
		t.Errorf("requests = %+v, want none for a blank note", result.Requests)
	}
}

// TestAbandoningStagedChangesChangesNothing is the property that makes ADR 0007
// §5's "a draft is not a decision" real rather than verbal. The participant
// staged a deprecation and never pressed Sync; Tesseract is untouched.
func TestAbandoningStagedChangesChangesNothing(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)

	// The board is opened and the draft is written — everything a participant
	// does short of pressing the button.
	if _, err := board.Open(context.Background(), OpenInput{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	host.tools.answer("tangent.surface_get", surfaceWithBoard(t,
		map[string]string{"rev-1": StatusDeprecated, "rev-2": "canonical"},
		map[string]string{"rev-3": "reword this"}, revisions(), nil))

	if got := fake.retired(); len(got) != 0 {
		t.Fatalf("staging alone wrote to Tesseract: %v — the press is the decision", got)
	}
	_ = board
}

// TestSyncReportsARefusalAndStillRefreshes: a sync that aborted on the first
// refusal would hide the rest, and would leave the participant looking at a
// stale board plus an error.
func TestSyncReportsARefusalAndStillRefreshes(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	fake.refuse["rev-1"] = "memory not found: revision_id rev-1"
	board, _ := loadedPlugin(t, fake)
	setSurface(t, board, map[string]string{
		"rev-1": StatusDeprecated, "rev-2": StatusDeprecated,
	}, nil, nil)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Failed) != 1 || result.Failed[0].RevisionID != "rev-1" {
		t.Errorf("result.Failed = %+v, want rev-1", result.Failed)
	}
	if !strings.Contains(result.Failed[0].Reason, "memory not found") {
		t.Errorf("reason = %q, want Tesseract's own message", result.Failed[0].Reason)
	}
	if len(result.Deprecated) != 1 || result.Deprecated[0].RevisionID != "rev-2" {
		t.Errorf("result.Deprecated = %+v, want the other one to have gone through",
			result.Deprecated)
	}
}

// TestAStagedMoveIntoTheColumnACardIsAlreadyInIsSkipped.
func TestAStagedMoveIntoTheSameColumnIsSkipped(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	// rev-3 is already canonical.
	setSurface(t, board, map[string]string{"rev-3": "canonical"}, nil, nil)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Requests) != 0 || len(result.Deprecated) != 0 {
		t.Errorf("Sync = %+v, want nothing done for a move that is already true", result)
	}
}

// TestAnOutstandingRequestIsCarriedForwardAndSelfClears.
//
// A sync REPLACES the board, so a note the participant wrote would vanish from
// the screen the moment they submitted it. The request rides on the new
// envelope's meta and shows as a badge — and it drops itself when the revision
// it names stops coming back, which is exactly when the agent has done the
// work, because every way of servicing one supersedes the revision.
func TestAnOutstandingRequestIsCarriedForwardAndSelfClears(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	setSurface(t, board, nil, map[string]string{"rev-2": "reword this"}, nil)

	first, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(first.Requests) != 1 {
		t.Fatalf("first sync requests = %+v, want one", first.Requests)
	}
	// The fresh board carries it, and the card wears a badge.
	data := boardDataFrom(t, host)
	card := cardByID(t, data, "rev-2")
	if !hasBadge(card, "pending") {
		t.Errorf("card badges = %+v, want a pending-request badge", card.Badges)
	}
	if !strings.Contains(data.Sync.Scope, "1 card(s) carry a request") {
		t.Errorf("scope = %q, want it to count the outstanding requests", data.Sync.Scope)
	}

	// Now the agent does the work: the revision it named is superseded, so it
	// stops coming back from recall.
	carried := map[string]Request{"rev-2": first.Requests[0]}
	remaining := []Revision{revisions()[0], revisions()[2]}
	fake.mu.Lock()
	fake.revisions = remaining
	fake.mu.Unlock()
	host.tools.answer("tangent.surface_get", surfaceWithBoard(t, nil, nil, revisions(), carried))

	second, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if len(second.Requests) != 0 {
		t.Errorf("second sync requests = %+v, want the serviced one dropped", second.Requests)
	}
}

// TestSyncRefusesABoardThisPluginDidNotOpen: without the recall on the meta
// there is nothing to re-query with, and guessing would silently show a
// different set of records than the board was opened over.
func TestSyncRefusesABoardThisPluginDidNotOpen(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	host.tools.answer("tangent.surface_get", `{
	  "interactions": [{
	    "interaction_id": "int-1", "state": "presented", "revision": 1,
	    "request_snapshot": {"board_id": "someone-elses", "cards": []},
	    "external_refs": {"legacy_envelope": {"id": "env-1", "title": "x", "meta": {}, "data": {"board_id": "someone-elses", "cards": []}}},
	    "definition_binding": {"kind": "tangent.app-board"}
	  }],
	  "drafts": []
	}`)

	_, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err == nil {
		t.Fatal("Sync accepted a board this plugin did not open")
	}
	if !strings.Contains(err.Error(), metaRecallKey) {
		t.Errorf("Sync = %v, want the missing meta key named", err)
	}
}

func TestSyncNeedsARoomID(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	if _, err := board.Sync(context.Background(), SyncInput{}); err == nil {
		t.Fatal("Sync accepted an empty room id")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// setSurface is the common case: one presented board over the standard
// fixtures, with whatever the participant staged.
func setSurface(t *testing.T, board *Plugin, staged, notes map[string]string, pending map[string]Request) {
	t.Helper()
	host, ok := board.host.(*fakeHost)
	if !ok {
		t.Fatalf("plugin is not loaded on a fakeHost: %T", board.host)
	}
	host.tools.answer("tangent.surface_get",
		surfaceWithBoard(t, staged, notes, revisions(), pending))
}

func cardByID(t *testing.T, data BoardData, id string) Card {
	t.Helper()
	for _, card := range data.Cards {
		if card.ID == id {
			return card
		}
	}
	t.Fatalf("no card %q on the board", id)
	return Card{}
}

func hasBadge(card Card, id string) bool {
	for _, badge := range card.Badges {
		if badge.ID == id {
			return true
		}
	}
	return false
}

// ── The SDK's base host contract, refusing everything ───────────────────────
//
// This plugin calls none of it. RegisterCRUDHandler and GetService are the two
// worth noticing: Tangent's real host leaves both unimplemented on purpose, and
// a plugin that reached for either would be reaching past the boundary this
// whole pattern exists to keep.

type baseHostOnly struct{}

func (baseHostOnly) GetPlugin(string) (plugin.Plugin, bool) { return nil, false }

func (baseHostOnly) RegisterCRUDHandler(string, plugin.CRUDHandler) error { return errNotHonored }
func (baseHostOnly) RegisterEventHook([]string, plugin.EventHook) error   { return errNotHonored }
func (baseHostOnly) RegisterUIComponent(plugin.UIComponent) error         { return errNotHonored }
func (baseHostOnly) GetService(string) (interface{}, error)               { return nil, errNotHonored }
func (baseHostOnly) GetConfig(string) (string, error)                     { return "", errNotHonored }
func (baseHostOnly) SetConfig(string, string) error                       { return errNotHonored }
func (baseHostOnly) RegisterConfigSchema([]plugin.ConfigFieldDef) error   { return errNotHonored }
func (baseHostOnly) RegisterConnector(string, plugin.Connector) error     { return errNotHonored }
func (baseHostOnly) RegisterProvider(string, interface{}) error           { return errNotHonored }
func (baseHostOnly) RegisterCLIAdapter(string, interface{}) error         { return errNotHonored }
func (baseHostOnly) Logger() plugin.Logger                                { return nil }
func (baseHostOnly) Context() context.Context                             { return context.Background() }

var errNotHonored = errors.New("not honored")

var _ plugin.Host = baseHostOnly{}

// ── Seeing a request you already made (found on first use) ──────────────────
//
// The board answers a sync with a FRESH envelope, so anything the participant
// typed is gone from the screen the moment they submit it. Chrispian, first
// use: "The other card is still there but the text I typed is not."
//
// The request had in fact been captured and carried — the card even wore a
// badge saying so — but the board could say a note EXISTED and not what it
// SAID, which made the most valuable thing on the board the one thing it
// discarded.

// TestACarriedRewordComesBackOnTheCard: the note is on the card, so the box the
// participant sees is the one they filled in.
func TestACarriedRewordComesBackOnTheCard(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	note := "Say this in plainer words."
	setSurface(t, board, nil, map[string]string{"rev-2": note}, nil)

	if _, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	card := cardByID(t, boardDataFrom(t, host), "rev-2")
	if card.Note != note {
		t.Errorf("card note = %q, want the participant's own words back", card.Note)
	}
	if !hasBadge(card, "pending") {
		t.Errorf("card badges = %+v, want the request still marked", card.Badges)
	}
}

// TestAPromotionRequestSeedsNoNote: a promotion has no note box, so seeding one
// would invite editing a note nobody wrote.
func TestAPromotionRequestSeedsNoNote(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	setSurface(t, board, map[string]string{"rev-1": "canonical"}, nil, nil)

	if _, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	card := cardByID(t, boardDataFrom(t, host), "rev-1")
	if card.Note != "" {
		t.Errorf("card note = %q, want empty for a promotion request", card.Note)
	}
	if !hasBadge(card, "pending") {
		t.Errorf("card badges = %+v, want the promotion still marked", card.Badges)
	}
}

// TestClearingTheNoteWithdrawsTheRequest: with the box seeded, an emptied box
// is the participant clearing it on purpose. A cleared box that left the
// request standing would be a control that visibly did nothing.
func TestClearingTheNoteWithdrawsTheRequest(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	carried := map[string]Request{
		"rev-2": {Kind: RequestReword, RevisionID: "rev-2", Note: "old ask"},
	}
	setSurface(t, board, nil, map[string]string{"rev-2": ""}, carried)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Requests) != 0 {
		t.Errorf("requests = %+v, want the cleared one withdrawn", result.Requests)
	}
	card := cardByID(t, boardDataFrom(t, host), "rev-2")
	if card.Note != "" || hasBadge(card, "pending") {
		t.Errorf("card = %+v, want no note and no badge after a withdrawal", card)
	}
}

// TestAnEmptyNoteDoesNotWithdrawAPromotion: a promotion has no note box, so an
// empty note says nothing about it. Letting it cancel one would mean clearing a
// box the participant never filled retracts a disposition they did stage.
func TestAnEmptyNoteDoesNotWithdrawAPromotion(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	carried := map[string]Request{
		"rev-1": {Kind: RequestPromotion, RevisionID: "rev-1", ToStatus: "canonical"},
	}
	setSurface(t, board, nil, map[string]string{"rev-1": ""}, carried)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Requests) != 1 || result.Requests[0].Kind != RequestPromotion {
		t.Errorf("requests = %+v, want the promotion untouched", result.Requests)
	}
}

// ── Retire it and say it better (found on first use) ────────────────────────
//
// Chrispian staged a record into Deprecated AND wrote a note on it, pressed
// Sync, and the note was gone. The deprecation applied, a reword request was
// raised for the same revision, and carryForward's self-clearing rule then ate
// it — because the rule is "drop a request whose revision left the board", and
// the deprecation is what removed it. Silent loss of the participant's words.
//
// The pairing is not a contradiction to resolve. In an append-only store
// "retire this and say it better" IS a supersede, and it is probably the most
// useful thing a review pass produces.

// TestANoteOnADeprecatedCardBecomesASupersedeRequest.
func TestANoteOnADeprecatedCardBecomesASupersedeRequest(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	setSurface(t, board,
		map[string]string{"rev-1": StatusDeprecated},
		map[string]string{"rev-1": "Retire this and say it in plainer words."}, nil)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// The deprecation still applies — this does not trade one for the other.
	if got := fake.retired(); len(got) != 1 || got[0] != "rev-1" {
		t.Errorf("deprecated = %v, want [rev-1]", got)
	}
	if len(result.Requests) != 1 {
		t.Fatalf("requests = %+v, want the note to survive as one request", result.Requests)
	}
	request := result.Requests[0]
	if request.Kind != RequestSupersede {
		t.Errorf("kind = %q, want %q", request.Kind, RequestSupersede)
	}
	if request.Note != "Retire this and say it in plainer words." {
		t.Errorf("note = %q, want the participant's own words", request.Note)
	}
	// Everything the agent needs to author the replacement without re-reading.
	if request.RevisionID != "rev-1" || request.MemoryKey == "" || request.Namespace == "" ||
		request.Summary == "" {
		t.Errorf("request = %+v, want the retired revision fully identified", request)
	}
}

// TestASupersedeRequestSurvivesTheSyncThatRaisedIt is the regression proper.
//
// The revision it names is gone from the fresh card set by construction, so a
// rule that clears on "revision absent" drops it the instant it is raised.
func TestASupersedeRequestSurvivesTheSyncThatRaisedIt(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	setSurface(t, board,
		map[string]string{"rev-1": StatusDeprecated},
		map[string]string{"rev-1": "say it better"}, nil)

	if _, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	envelope := boardEnvelopeFrom(t, host)
	meta, _ := envelope["meta"].(map[string]any)
	carried, _ := meta[metaRequestsKey].(map[string]any)
	if len(carried) != 1 {
		t.Fatalf("carried requests = %#v, want the supersede to persist past its own sync", carried)
	}
	// And it is visible: it has no card to wear a badge, so the scope line is
	// the only thing that can say it is outstanding.
	scope := boardDataFrom(t, host).Sync.Scope
	if !strings.Contains(scope, "1 supersede request(s)") {
		t.Errorf("scope = %q, want it to count the off-board request", scope)
	}
}

// TestASupersedeClearsWhenTheReplacementLandsUnderTheSameKey.
func TestASupersedeClearsWhenTheReplacementLandsUnderTheSameKey(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, _ := loadedPlugin(t, fake)
	carried := map[string]Request{
		"rev-1": {
			Kind: RequestSupersede, RevisionID: "rev-1",
			MemoryKey: "portfolio_ui_layering", Note: "say it better",
		},
	}

	// Still outstanding while nothing under that key has come back.
	fake.mu.Lock()
	fake.revisions = []Revision{revisions()[1], revisions()[2]}
	fake.mu.Unlock()
	setSurface(t, board, nil, nil, carried)
	still, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(still.Requests) != 1 {
		t.Fatalf("requests = %+v, want the supersede still outstanding", still.Requests)
	}

	// The agent writes the replacement: a new revision under the same key.
	replacement := summaryRevision(
		"rev-1b", "mem-1", "portfolio_ui_layering", "canonical", 0.9)
	fake.mu.Lock()
	fake.revisions = []Revision{replacement, revisions()[1], revisions()[2]}
	fake.mu.Unlock()
	setSurface(t, board, nil, nil, carried)

	done, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if len(done.Requests) != 0 {
		t.Errorf("requests = %+v, want the supersede cleared by its replacement", done.Requests)
	}
}

// TestARefusedDeprecationLeavesTheNoteAReword: the kind is keyed on the
// deprecation SUCCEEDING. A record Tesseract refused to retire is still there,
// so the note is an ordinary reword against it.
func TestARefusedDeprecationLeavesTheNoteAReword(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	fake.refuse["rev-1"] = "memory not found: revision_id rev-1"
	board, _ := loadedPlugin(t, fake)
	setSurface(t, board,
		map[string]string{"rev-1": StatusDeprecated},
		map[string]string{"rev-1": "say it better"}, nil)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Failed) != 1 {
		t.Fatalf("failed = %+v, want the refusal reported", result.Failed)
	}
	if len(result.Requests) != 1 || result.Requests[0].Kind != RequestReword {
		t.Errorf("requests = %+v, want a reword against the record that is still there",
			result.Requests)
	}
}

// TestAnUnkeyedSupersedeIsReportedButNotCarried: there is no handle to watch
// for a replacement, so carrying it would carry it forever.
func TestAnUnkeyedSupersedeIsReportedButNotCarried(t *testing.T) {
	unkeyed := summaryRevision("rev-7", "mem-7", "", "draft", 0.5)
	unkeyed.MemoryKey = ""
	fake := startFakeTesseract(t, []Revision{unkeyed})
	board, host := loadedPlugin(t, fake)
	setSurface(t, board, nil, nil, nil)
	host.tools.answer("tangent.surface_get", surfaceWithBoard(t,
		map[string]string{"rev-7": StatusDeprecated},
		map[string]string{"rev-7": "say it better"}, []Revision{unkeyed}, nil))

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Reported: the response is the fast path and it has been taken.
	if len(result.Requests) != 1 || result.Requests[0].Kind != RequestSupersede {
		t.Fatalf("requests = %+v, want the supersede reported", result.Requests)
	}
	envelope := boardEnvelopeFrom(t, host)
	meta, _ := envelope["meta"].(map[string]any)
	if carried, ok := meta[metaRequestsKey]; ok {
		t.Errorf("meta carries %#v, want an unkeyed supersede not carried forever", carried)
	}
}

// TestARetiredRecordWithASupersedeKeepsItsCard.
//
// Chrispian, after retiring a record with a note on it: "nothing came back to
// me anywhere." The request WAS captured and carried; it just had no card, so
// the only acknowledgement was a sentence in the filter bar. A request the
// participant cannot see is one they will make twice.
func TestARetiredRecordWithASupersedeKeepsItsCard(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	note := "This is incorrect so I deprecated it."
	setSurface(t, board,
		map[string]string{"rev-1": StatusDeprecated},
		map[string]string{"rev-1": note}, nil)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := fake.retired(); len(got) != 1 {
		t.Fatalf("deprecated = %v, want the record actually retired", got)
	}

	data := boardDataFrom(t, host)
	card := cardByID(t, data, "rev-1")
	// In Deprecated, which is the truth about it, wearing what was asked.
	if card.Note != note {
		t.Errorf("card note = %q, want the participant's own words on the card", card.Note)
	}
	if !hasBadge(card, "pending") {
		t.Errorf("card badges = %+v, want the supersede marked", card.Badges)
	}
	var deprecatedColumn Column
	for _, column := range data.Columns {
		if column.ID == StatusDeprecated {
			deprecatedColumn = column
		}
	}
	if len(deprecatedColumn.CardIDs) != 1 || deprecatedColumn.CardIDs[0] != "rev-1" {
		t.Errorf("deprecated column = %v, want the retired record visible in it",
			deprecatedColumn.CardIDs)
	}
	if len(result.Requests) != 1 || result.Requests[0].Kind != RequestSupersede {
		t.Errorf("requests = %+v, want the supersede reported too", result.Requests)
	}
}

// TestAFailedHydrationDegradesToACountRatherThanFailingTheSync.
func TestAFailedHydrationDegradesToACountRatherThanFailingTheSync(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)
	// A carried supersede naming a revision the store no longer has at all.
	carried := map[string]Request{
		"rev-gone": {
			Kind: RequestSupersede, RevisionID: "rev-gone",
			MemoryKey: "long_since_trimmed", Note: "say it better",
		},
	}
	setSurface(t, board, nil, nil, carried)

	result, err := board.Sync(context.Background(), SyncInput{RoomID: "room-1"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Requests) != 1 {
		t.Errorf("requests = %+v, want the request still reported", result.Requests)
	}
	scope := boardDataFrom(t, host).Sync.Scope
	if !strings.Contains(scope, "1 supersede request(s)") {
		t.Errorf("scope = %q, want the un-hydratable request still counted", scope)
	}
}

// TestAFreshBoardShowsRequestsTheCallerCarriesOver.
//
// Outstanding requests live on the envelope, and an envelope belongs to one
// room. Chrispian opened a second board and his pending promotion was gone —
// the agent still had it, but the participant saw a clean board and would have
// re-asked. The agent is the carrier, because it is the party that received the
// work and the party that does it.
func TestAFreshBoardShowsRequestsTheCallerCarriesOver(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)

	if _, err := board.Open(context.Background(), OpenInput{
		Requests: []Request{
			{
				Kind: RequestPromotion, RevisionID: "rev-1",
				MemoryKey: "portfolio_ui_layering", FromStatus: "draft", ToStatus: "canonical",
			},
			{Kind: RequestReword, RevisionID: "rev-2", MemoryKey: "x", Note: "say it better"},
		},
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	data := boardDataFrom(t, host)
	promotion := cardByID(t, data, "rev-1")
	if !hasBadge(promotion, "pending") {
		t.Errorf("card rev-1 badges = %+v, want the carried promotion marked", promotion.Badges)
	}
	reword := cardByID(t, data, "rev-2")
	if reword.Note != "say it better" {
		t.Errorf("card rev-2 note = %q, want the carried note back in the box", reword.Note)
	}
	if !strings.Contains(data.Sync.Scope, "2 card(s) carry a request") {
		t.Errorf("scope = %q, want the carried requests counted", data.Sync.Scope)
	}
}

// TestCarryingAServicedRequestIsSafe: the agent may pass a stale list, and a
// request whose record is no longer there is dropped on the way in rather than
// producing a badge for a card that does not exist.
func TestCarryingAServicedRequestIsSafe(t *testing.T) {
	fake := startFakeTesseract(t, revisions())
	board, host := loadedPlugin(t, fake)

	if _, err := board.Open(context.Background(), OpenInput{
		Requests: []Request{
			{Kind: RequestReword, RevisionID: "rev-long-gone", MemoryKey: "gone", Note: "old"},
		},
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	data := boardDataFrom(t, host)
	if strings.Contains(data.Sync.Scope, "carry a request") {
		t.Errorf("scope = %q, want a serviced request dropped rather than shown", data.Sync.Scope)
	}
	for _, card := range data.Cards {
		if hasBadge(card, "pending") {
			t.Errorf("card %s wears a badge for a request whose record is gone", card.ID)
		}
	}
}
