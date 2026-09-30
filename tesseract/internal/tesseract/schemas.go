package tesseract

import "encoding/json"

// The tool contracts.
//
// They are raw JSON rather than a Go struct with reflection, for the reason
// tangentplugin.MCPTool takes raw JSON: this is what a subprocess plugin would put
// on the wire, so the declaration a compiled-in plugin writes today is the one
// it keeps when CW-20260910-0034 makes subprocess mode real.
//
// Every filter here is one `tesseract_recall` already takes, in the same
// vocabulary. The plugin invents no filter language of its own — an agent that
// knows how to recall from Tesseract already knows how to open a review board
// over the same records. The one place the shapes differ is that this tool
// takes the filters FLAT, because a nested object whose keys are Go field names
// is Tesseract's HTTP-door detail and not something a caller should have to
// know; tesseract.go's RecallFilters is where that translation lives.

// OpenToolSchema advertises the open tool's input.
var OpenToolSchema = json.RawMessage(`{
  "title": "tangent.tesseract_review input",
  "description": "Which Tesseract records the review board shows. Every field is optional, except namespaces when the plugin has no default.",
  "type": "object",
  "properties": {
    "title": {
      "type": "string",
      "description": "Board heading. Defaults to a description of the recall."
    },
    "namespaces": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Tesseract namespaces. The prefix form (user/<name>/memory) matches every memory type under it. Defaults to the plugin's TANGENT_TESSERACT_NAMESPACES; with that unset, this field is required."
    },
    "statuses": {
      "type": "array",
      "items": {"type": "string", "enum": ["draft", "reviewed", "canonical", "deprecated"]},
      "description": "Narrow to these lifecycle statuses. Omit for everything recall returns — which excludes deprecated records unless you ask for them. The board always shows all four columns regardless, because a column is a drop target whether or not it holds anything."
    },
    "tags": {"type": "array", "items": {"type": "string"}},
    "query": {
      "type": "string",
      "description": "A semantic query. Setting it switches the default ranking to relevance."
    },
    "ranking": {
      "type": "string",
      "enum": ["activation", "chronological", "relevance"],
      "description": "The board's sort, and the only one there is: app-board has no client-side sort. activation is what is hottest, chronological is what is newest, relevance answers a query. Defaults to relevance when query is set and activation otherwise."
    },
    "since": {"type": "string", "description": "RFC3339 lower bound on created_at."},
    "until": {"type": "string", "description": "RFC3339 upper bound on created_at."},
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 200,
      "description": "Maximum cards to send; defaults to 100 and is clamped at 200. Send a deliberate superset: the board's own filter bar is a VIEW over what you sent, so narrowing past it needs a sync. A cut set is reported in the board's scope line rather than passed off as the whole set."
    },
    "room_id": {
      "type": "string",
      "description": "Open the board in an existing room instead of creating one."
    },
    "read_only": {
      "type": "boolean",
      "description": "Offer no disposition control, no note box and no sync button. The board becomes a view of Tesseract that cannot change it."
    },
    "requests": {
      "type": "array",
      "description": "Work items a previous board handed you and you have not serviced yet, as tangent.tesseract_review_sync returned them. Pass them back and the new board shows them again — a badge on the card, a note in the box, and a retired record kept visible until its replacement exists. Outstanding requests live on the board that raised them, so without this a second board looks clean and the participant re-asks for what they already asked for. Anything already serviced is dropped on the way in, so passing a stale list is safe.",
      "items": {
        "type": "object",
        "properties": {
          "kind": {"type": "string", "enum": ["promotion", "reword", "supersede"]},
          "revision_id": {"type": "string", "minLength": 1},
          "memory_id": {"type": "string"},
          "namespace": {"type": "string"},
          "memory_key": {"type": "string"},
          "from_status": {"type": "string"},
          "to_status": {"type": "string"},
          "note": {"type": "string"},
          "summary": {"type": "string"}
        },
        "required": ["kind", "revision_id"]
      }
    }
  }
}`)

const OpenToolDescription = "Open a Tesseract review board in Tangent: the records matching your " +
	"recall, in kanban columns by lifecycle status (draft, reviewed, canonical, deprecated), in a " +
	"browser room. Returns the room URL and a board handle. The participant dispositions records by " +
	"moving cards between columns and can leave a per-card note asking for different wording. " +
	"Pressing Sync applies the deprecations with no agent turn and hands the rest back as a work " +
	"list — promotions and rewords are new revisions with supersedes, so an author makes them."

// SyncToolSchema advertises the sync tool's input.
var SyncToolSchema = json.RawMessage(`{
  "title": "tangent.tesseract_review_sync input",
  "description": "Apply what the participant staged on a Tesseract review board, collect the work they asked for, and replace the board with fresh cards.",
  "type": "object",
  "properties": {
    "room_id": {
      "type": "string",
      "minLength": 1,
      "description": "The room the board is open in, as tangent.tesseract_review returned it."
    }
  },
  "required": ["room_id"]
}`)

const SyncToolDescription = "Sync a Tesseract review board: deprecate what the participant staged " +
	"for retirement, re-recall, and replace the board with fresh cards. Returns what was deprecated " +
	"and, in `requests`, the work handed back to you, each naming the revision to act on: a " +
	"`promotion` up the lifecycle, a `reword` of a record that stays, or a `supersede` — a note on a " +
	"card the participant ALSO retired, meaning \"retire this and say it better\". Tesseract memory " +
	"revisions are immutable except for deprecation, so all three are a new revision with " +
	"`supersedes` and all three are yours to write. The board's own Sync button does the deprecating " +
	"half without an agent turn; call this when you are already in a turn, or to collect the work list."
