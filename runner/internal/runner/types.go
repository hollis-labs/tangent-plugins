package runner

import (
	"encoding/json"
	"time"
)

const (
	// ID is the unique plugin identifier in the host roster.
	ID = "tangent.plugin.runner"

	// PluginVersion is the semantic version of this runner plugin.
	PluginVersion = "0.1.0"

	// Tools served by the runner plugin.
	LaunchTool   = "tangent.runner_launch"
	SendTurnTool = "tangent.runner_send_turn"
	HealthTool   = "tangent.runner_health"
	StopTool     = "tangent.runner_stop"

	LaunchToolDescription   = "Launch an agent run (embedded subprocess or delegated to Tether)"
	SendTurnToolDescription = "Deliver operator reply to a running agent session"
	HealthToolDescription   = "Inspect live session status, turn progress, and resource health"
	StopToolDescription     = "Terminate/interrupt an active agent run"

	// Routes served by the runner plugin.
	LaunchPath   = "/api/plugins/runner/launch"
	SessionsPath = "/api/plugins/runner/sessions"

	// Tangent tools the runner calls back into: the agent-turns inbox, in the
	// order a turn uses them. These are the host's tools, not the runner's own.
	EnqueueTurnTool = "tangent.turns_enqueue"
	AwaitTurnTool   = "tangent.turn_await"
	AckTurnTool     = "tangent.turn_ack"
)

// Mode determines whether execution runs as a local supervised subprocess
// or delegates to the Tether runtime daemon.
type Mode string

const (
	ModeEmbedded Mode = "embedded"
	ModeTether   Mode = "tether"
)

// Lifecycle determines the subprocess I/O contract.
type Lifecycle string

const (
	LifecycleStreamingStdio Lifecycle = "streaming_stdio"
	LifecycleJSONRPCStdio   Lifecycle = "jsonrpc_stdio"
	LifecyclePTY            Lifecycle = "pty"
	LifecycleACP            Lifecycle = "acp"
)

// LaunchParams describes the inputs to launch an agent run.
type LaunchParams struct {
	SessionID    string         `json:"session_id,omitempty"`
	AgentID      string         `json:"agent_id"`
	AgentLabel   string         `json:"agent_label,omitempty"`
	Prompt       string         `json:"prompt"`
	WorkingDir   string         `json:"working_dir,omitempty"`
	Mode         Mode           `json:"mode,omitempty"`
	Lifecycle    Lifecycle      `json:"lifecycle,omitempty"`
	Command      string         `json:"command,omitempty"`
	Args         []string       `json:"args,omitempty"`
	Env          []string       `json:"env,omitempty"`
	Correlations map[string]any `json:"correlations,omitempty"`
}

// LaunchResult is returned upon launching an agent session.
type LaunchResult struct {
	SessionID string    `json:"session_id"`
	AgentID   string    `json:"agent_id"`
	Mode      Mode      `json:"mode"`
	Lifecycle Lifecycle `json:"lifecycle"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Status    string    `json:"status"`
}

// SendTurnParams delivers operator guidance back to a running session.
type SendTurnParams struct {
	SessionID      string `json:"session_id"`
	ResponseText   string `json:"response_text"`
	Action         string `json:"action,omitempty"`
	SelectedOption string `json:"selected_option,omitempty"`
}

// SendTurnResult confirms delivery of an operator turn reply.
type SendTurnResult struct {
	SessionID   string    `json:"session_id"`
	Delivered   bool      `json:"delivered"`
	DeliveredAt time.Time `json:"delivered_at"`
	LiveState   string    `json:"live_state"`
}

// SessionHealthResult provides the runtime state of a session.
type SessionHealthResult struct {
	SessionID  string         `json:"session_id"`
	Alive      bool           `json:"alive"`
	LiveState  string         `json:"live_state"` // "idle", "processing", "stopped"
	TurnID     string         `json:"turn_id,omitempty"`
	TurnsCount int            `json:"turns_count"`
	LastOutput string         `json:"last_output,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// LaunchToolSchema defines the JSON schema for tangent.runner_launch.
var LaunchToolSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"session_id": {"type": "string", "description": "Optional explicit session ID"},
		"agent_id": {"type": "string", "description": "Logical identifier of the agent"},
		"agent_label": {"type": "string", "description": "Human-readable agent label"},
		"prompt": {"type": "string", "description": "Initial task prompt or instructions"},
		"working_dir": {"type": "string", "description": "Working directory for execution"},
		"mode": {"type": "string", "enum": ["embedded", "tether"], "description": "Execution mode (default: embedded)"},
		"lifecycle": {"type": "string", "enum": ["streaming_stdio", "jsonrpc_stdio", "pty", "acp"], "description": "Subprocess lifecycle (default: streaming_stdio)"},
		"command": {"type": "string", "description": "Binary command to execute (e.g. claude, codex)"},
		"args": {"type": "array", "items": {"type": "string"}, "description": "Command-line arguments"},
		"correlations": {"type": "object", "description": "Correlations dictionary (task_id, project_id, etc.)"}
	},
	"required": ["agent_id", "prompt"]
}`)

// SendTurnToolSchema defines the JSON schema for tangent.runner_send_turn.
var SendTurnToolSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"session_id": {"type": "string", "description": "Session identifier"},
		"response_text": {"type": "string", "description": "Operator reply text or answer"},
		"action": {"type": "string", "description": "Resolution action (respond, approve, reject)"},
		"selected_option": {"type": "string", "description": "Option choice if options were provided"}
	},
	"required": ["session_id", "response_text"]
}`)

// HealthToolSchema defines the JSON schema for tangent.runner_health.
var HealthToolSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"session_id": {"type": "string", "description": "Session identifier"}
	},
	"required": ["session_id"]
}`)

// StopToolSchema defines the JSON schema for tangent.runner_stop.
var StopToolSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"session_id": {"type": "string", "description": "Session identifier"},
		"reason": {"type": "string", "description": "Optional stop reason"}
	},
	"required": ["session_id"]
}`)
