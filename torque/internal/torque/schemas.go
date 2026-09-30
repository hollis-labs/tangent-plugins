package torque

import "encoding/json"

// The tool contracts.
//
// They are raw JSON rather than a Go struct with reflection, for the reason
// tangentplugin.MCPTool takes raw JSON: this is what a subprocess plugin would put
// on the wire, so the declaration a compiled-in plugin writes today is the one
// it keeps when CW-20260910-0034 makes subprocess mode real.
//
// Every filter here is a facet `torque_task_list` already takes. The plugin
// invents no filter vocabulary of its own — an agent that knows how to list
// Torque tasks already knows how to open a board of them.

// OpenToolSchema advertises the open tool's input.
var OpenToolSchema = json.RawMessage(`{
  "title": "tangent.torque_open_board input",
  "description": "Filters selecting which Torque tasks the board shows. Every field is optional.",
  "type": "object",
  "properties": {
    "title": {
      "type": "string",
      "description": "Board heading. Defaults to a description of the filters."
    },
    "statuses": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Torque statuses to show, in column order. Defaults to the active set (backlog, todo, queued, doing, review, blocked, paused). A task whose status is outside this list still appears, in a column appended at the end."
    },
    "project_id": {"type": "string"},
    "sprint_id": {"type": "string"},
    "epic_id": {"type": "string"},
    "kind": {"type": "string", "description": "Torque task kind, e.g. agent or issue."},
    "executor": {"type": "string"},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "Tag slugs; a task matching any of them is included."},
    "search": {"type": "string"},
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 80,
      "description": "Maximum tasks to send; defaults to 60 and is clamped at 80. The ceiling is measured against the kind's inline payload limit — a Torque description becomes the card body verbatim, so one card costs ~2.8 KB. Send a deliberate superset: the board's own filter bar is a VIEW over what you sent, so narrowing past it needs a sync. A cut set is reported in the board's scope line and in this tool's truncated field rather than passed off as the whole set."
    },
    "room_id": {
      "type": "string",
      "description": "Open the board in an existing room instead of creating one."
    },
    "read_only": {
      "type": "boolean",
      "description": "Offer no staging control and no sync button. The board becomes a view of Torque that cannot change it."
    }
  }
}`)

const OpenToolDescription = "Open a Torque board in Tangent: the tasks matching your filters, " +
	"arranged in columns by status, in a browser room. Returns the room URL and a board handle. " +
	"The board stays open — the participant can stage status changes and press Sync to apply them " +
	"and pull fresh cards, with no agent turn. Filters narrow the tasks you asked for; the board's " +
	"own filter bar narrows what was sent. Cards are capped, so check `truncated`: when it is true " +
	"Torque held more matches than the board shows, and neither you nor the participant is looking " +
	"at the whole set."

// SyncToolSchema advertises the sync tool's input.
var SyncToolSchema = json.RawMessage(`{
  "title": "tangent.torque_sync_board input",
  "description": "Apply what the participant staged on a Torque board and replace the board with fresh cards.",
  "type": "object",
  "properties": {
    "room_id": {
      "type": "string",
      "minLength": 1,
      "description": "The room the board is open in, as tangent.torque_open_board returned it."
    },
    "force": {
      "type": "boolean",
      "description": "Reopen a task leaving a terminal status. Torque guards done and archived so a finished task is not reopened by a stray call; a board move out of Done is not a stray call, but it is still a deliberate one."
    }
  },
  "required": ["room_id"]
}`)

const SyncToolDescription = "Sync a Torque board both ways in one call: apply the status changes " +
	"the participant staged, re-query Torque, and replace the board with fresh cards. Returns what " +
	"moved on both sides. The board's own Sync button does exactly this without an agent turn; " +
	"call this when you are already in a turn."
