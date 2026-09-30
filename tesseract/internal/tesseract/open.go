package tesseract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// Opening a review board.
//
// The flow is four steps and none of them is a judgement call: recall from
// Tesseract, shape the cards, create a room, present the envelope. It runs
// through Tangent's own tool surface (pluginhost/tools.go) rather than through a
// private host API, so the plugin is a caller of the same tools an agent calls,
// with the same authority and no more.
//
// The envelope goes out with completion mode "async". That is what keeps the
// board open: the receipt returns immediately, nothing is cancelled, and the
// interaction stays pending for as long as the participant works in it.

// metaRecallKey is where the board's originating recall is recorded on the
// envelope.
//
// It has to survive somewhere, because a sync re-queries Tesseract with the
// SAME recall and nothing else knows what it was. The envelope's own request
// snapshot is the right place: immutable, already durable, and read back from
// the interaction record — so a sync cannot re-query with a recall that drifted
// from the one the board was opened with.
const metaRecallKey = "tesseract_board_recall"

// metaBoardKey records the board id on the envelope alongside the recall.
const metaBoardKey = "tesseract_board_id"

// metaRequestsKey carries the requests already handed to the agent and not yet
// applied.
//
// They ride on the envelope because a sync REPLACES the board, and the
// replacement is a different envelope with a fresh draft sequence — so a note
// the participant wrote would vanish from the screen the moment they pressed
// the button that submitted it. Carrying it forward is what makes the handoff
// legible: the card keeps a badge until the agent has done the work.
//
// They clear themselves, and that is why nothing has to clear them. Every
// request is keyed by revision_id, and every way of servicing one — a supersede
// for a reword, a supersede for a promotion — writes a NEW revision and
// deprecates the old. So the revision a request names stops appearing in the
// fresh card set exactly when the work is done, and a carried request with no
// card is dropped. See carryForward.
const metaRequestsKey = "tesseract_board_requests"

// OpenInput is the open tool's arguments.
type OpenInput struct {
	Title      string   `json:"title,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
	Statuses   []string `json:"statuses,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Query      string   `json:"query,omitempty"`
	Ranking    string   `json:"ranking,omitempty"`
	Since      string   `json:"since,omitempty"`
	Until      string   `json:"until,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	RoomID     string   `json:"room_id,omitempty"`
	ReadOnly   bool     `json:"read_only,omitempty"`
	// Requests are work items a previous board handed back and the agent has
	// not serviced yet, passed in so the new board can show them.
	//
	// It exists because outstanding requests live on the ENVELOPE, which is the
	// only durable thing this plugin owns, and an envelope belongs to one room.
	// Open a second board and the first board's requests are invisible — the
	// agent still has them, having been handed them in a sync result, but the
	// participant sees a clean board and re-asks for what they already asked
	// for. Chrispian hit that within a minute of the feature existing.
	//
	// The AGENT is the right carrier rather than a store of this plugin's own:
	// it is the party that received them and the party that services them, and
	// giving this plugin a durable request queue would make it own application
	// state, which is the boundary the whole pattern exists to keep. A request
	// whose record has already been serviced is dropped on the way in, so
	// passing a stale list costs nothing.
	Requests []Request `json:"requests,omitempty"`
}

// NamespacesEnv names the namespaces a board opens over when the caller names
// none, comma-separated. The prefix form matches every memory type under it
// (decisions, followups, learnings, limitations…), which is the set a review
// pass is actually over.
//
// It is read from the plugin's own environment for the same reason BaseURLEnv
// is: the host holds no plugin configuration. It is a default rather than a
// constraint — any namespace the caller names is passed through untouched.
//
// Unset means there is no default, and a board opened without namespaces is
// refused rather than aimed somewhere. No namespace is neutral: every one
// belongs to somebody, and a recall over the wrong one either shows a clean
// board that is not clean or reads records the caller never asked for.
const NamespacesEnv = "TANGENT_TESSERACT_NAMESPACES"

// ErrNoNamespaces is Open's refusal when neither the caller nor NamespacesEnv
// names a namespace.
var ErrNoNamespaces = errors.New("tesseract: no namespaces named: pass namespaces, or set " + NamespacesEnv + " to give the board a default")

// parseNamespaces splits a NamespacesEnv value, dropping blanks.
func parseNamespaces(raw string) []string {
	var namespaces []string
	for _, namespace := range strings.Split(raw, ",") {
		if namespace = strings.TrimSpace(namespace); namespace != "" {
			namespaces = append(namespaces, namespace)
		}
	}
	return namespaces
}

// recall projects the tool input onto the Tesseract query.
//
// Ranking defaults the way Tesseract's own default does — relevance when there
// is a query, activation otherwise — rather than being pinned here, so a caller
// who passes a query and no ranking gets the mode that answers it.
//
// Namespaces are passed through as given; Open fills the default in before
// this runs.
func (in OpenInput) recall() RecallInput {
	namespaces := in.Namespaces
	ranking := in.Ranking
	if ranking == "" {
		if in.Query != "" {
			ranking = RankingRelevance
		} else {
			ranking = RankingActivation
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultCards
	}
	if limit > MaximumCards {
		limit = MaximumCards
	}
	return RecallInput{
		Namespaces: namespaces,
		Ranking:    ranking,
		Query:      in.Query,
		Filters: RecallFilters{
			Statuses: in.Statuses,
			Tags:     in.Tags,
			Since:    in.Since,
			Until:    in.Until,
		},
		Limit:       limit,
		PayloadMode: PayloadModeSummary,
	}
}

// OpenResult is what the open tool returns.
type OpenResult struct {
	RoomID  string `json:"room_id"`
	URL     string `json:"url"`
	BoardID string `json:"board_id"`
	// Cards is how many revisions were sent. It is the number the board's own
	// filter bar narrows.
	Cards int `json:"cards"`
	// Matching is how many records matched before the card limit, and
	// Truncated says whether the two differ. Reported separately from Cards
	// because "you are looking at 100 of 1426" is the fact a caller most needs
	// and cannot derive from a card count alone.
	Matching  int  `json:"matching"`
	Truncated bool `json:"truncated"`
	// Scope says which records were sent, in the same words the board shows
	// the participant.
	Scope string `json:"scope"`
	// Source is which Tesseract the cards came from.
	Source string `json:"source"`
}

// Open recalls from Tesseract, shapes a board, and presents it in a room.
func (p *Plugin) Open(ctx context.Context, input OpenInput) (OpenResult, error) {
	tools, err := p.tools()
	if err != nil {
		return OpenResult{}, err
	}

	if len(input.Namespaces) == 0 {
		if len(p.namespaces) == 0 {
			return OpenResult{}, ErrNoNamespaces
		}
		input.Namespaces = p.namespaces
	}
	recall := input.recall()
	result, err := p.client.Recall(ctx, recall)
	if err != nil {
		return OpenResult{}, err
	}

	roomID := input.RoomID
	url := ""
	if roomID == "" {
		roomID, url, err = createRoom(ctx, tools, p.boardTitle(input, recall))
		if err != nil {
			return OpenResult{}, err
		}
	}

	// Requests the caller carried over, narrowed to the ones still outstanding
	// against what this board actually shows.
	carried := map[string]Request{}
	for _, request := range input.Requests {
		if request.RevisionID != "" && request.Kind != "" {
			carried[request.RevisionID] = request
		}
	}
	pending := carryForward(carried, nil, nil, result.Revisions)
	result.Revisions = append(result.Revisions, p.hydrateRetired(ctx, pending, result.Revisions)...)

	boardID := newBoardID()
	data := p.boardData(boardID, p.boardTitle(input, recall), input.ReadOnly, recall, result, pending)
	envelope := boardEnvelope(newEnvelopeID(), data.Title, data, recall, pending)

	if err := advance(ctx, tools, roomID, envelope); err != nil {
		return OpenResult{}, err
	}
	return OpenResult{
		RoomID: roomID, URL: url, BoardID: boardID,
		Cards:     len(result.Revisions),
		Matching:  result.Manifest.ResultsTotal,
		Truncated: result.Manifest.Truncated,
		Scope:     data.Sync.scopeOrEmpty(),
		Source:    p.client.BaseURL(),
	}, nil
}

// boardData assembles the envelope's data block.
//
// It is the one place a board is shaped, so the open and sync paths cannot
// produce boards that differ in anything but their content.
func (p *Plugin) boardData(
	boardID string,
	title string,
	readOnly bool,
	recall RecallInput,
	result RecallResult,
	pending map[string]Request,
) BoardData {
	sync := &Sync{
		Enabled:    !readOnly,
		Endpoint:   SyncPath,
		Label:      "Sync",
		StageLabel: "Disposition",
		NoteLabel:  "Ask the agent to reword this",
		Scope:      ScopeSentence(recall, result.Manifest, pending),
	}
	if readOnly {
		sync.Label = ""
		sync.StageLabel = ""
		sync.NoteLabel = ""
	}
	return BuildBoard(
		boardID,
		title,
		result.Revisions,
		Source{App: "tesseract", Label: "Tesseract · " + p.client.BaseURL()},
		sync,
		pending,
		nowRFC3339(),
	)
}

func (p *Plugin) boardTitle(input OpenInput, recall RecallInput) string {
	if input.Title != "" {
		return input.Title
	}
	parts := make([]string, 0, 3)
	if len(recall.Filters.Statuses) > 0 {
		parts = append(parts, strings.Join(recall.Filters.Statuses, "/"))
	}
	parts = append(parts, strings.Join(recall.Namespaces, ", "))
	if recall.Query != "" {
		parts = append(parts, "matching "+recall.Query)
	}
	return "Tesseract review — " + strings.Join(parts, " · ")
}

// boardEnvelope wraps the board data in an app-board envelope.
//
// The recall and the outstanding requests ride in `meta` so a later sync can
// read them back off the immutable request snapshot rather than guessing or
// being told again.
func boardEnvelope(
	id, title string,
	data BoardData,
	recall RecallInput,
	pending map[string]Request,
) map[string]any {
	meta := map[string]any{
		metaBoardKey:  data.BoardID,
		metaRecallKey: recall,
	}
	if len(pending) > 0 {
		meta[metaRequestsKey] = pending
	}
	return map[string]any{
		"v":            1,
		"id":           id,
		"type":         EnvelopeType,
		"title":        title,
		"presentation": "fullscreen",
		"data":         data,
		"meta":         meta,
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
		"meta":  map[string]any{"app": "tesseract", "surface": "review-board"},
	})
	if err != nil {
		return "", "", err
	}
	var created struct {
		RoomID string `json:"roomID"`
		URL    string `json:"url"`
	}
	if err := result.Unmarshal(&created); err != nil {
		return "", "", fmt.Errorf("tesseract: create room: %w", err)
	}
	if created.RoomID == "" {
		return "", "", fmt.Errorf("tesseract: tangent.session_create returned no room id")
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
		return fmt.Errorf("tesseract: present board: %s", string(result.Content))
	}
	return nil
}

// nowRFC3339 is the timestamp a fresh board carries. It is one function so the
// open and sync paths cannot stamp a board differently.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func newBoardID() string { return "tesseract-board-" + randomSuffix() }

func newEnvelopeID() string { return "tesseract-board-env-" + randomSuffix() }

// randomSuffix is a short unique tail. Envelope identity is what Tangent's
// idempotency is keyed on, so two boards opened in the same second must not
// collide — a timestamp alone would.
func randomSuffix() string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// crypto/rand failing is not a condition this plugin can paper over
		// with a weaker id: a colliding envelope id would be read as a retry of
		// a different board. The clock is a last resort and is still unique per
		// nanosecond on one process.
		return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}

// decodeRecall reads the originating recall back off an envelope's meta.
func decodeRecall(meta map[string]json.RawMessage) (RecallInput, bool) {
	raw, ok := meta[metaRecallKey]
	if !ok {
		return RecallInput{}, false
	}
	var recall RecallInput
	if err := json.Unmarshal(raw, &recall); err != nil {
		return RecallInput{}, false
	}
	return recall, true
}

// decodeRequests reads the outstanding requests back off an envelope's meta.
//
// An absent or unreadable block is no requests rather than an error: the
// requests are a courtesy to the participant's eyes, and losing them costs a
// badge, not a disposition.
func decodeRequests(meta map[string]json.RawMessage) map[string]Request {
	raw, ok := meta[metaRequestsKey]
	if !ok {
		return nil
	}
	var requests map[string]Request
	if err := json.Unmarshal(raw, &requests); err != nil {
		return nil
	}
	return requests
}
