// Package reply owns dedicated reply preparation and delivery observation.
// The loaded plugin owns its workers and concrete private ledger; this package
// starts no goroutine and never injects a turn itself.
package reply

import (
	"context"
	"errors"

	tether "github.com/hollis-labs/go-tether-client"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

var (
	ErrNotFound    = errors.New("reply: not_found")
	ErrConflict    = errors.New("reply: saved_reply_conflict")
	ErrRefused     = errors.New("reply: request_refused")
	ErrUnsupported = errors.New("reply: capability_unavailable")
	ErrUnknown     = errors.New("reply: outcome_unknown")
)

// Binding must come from the locally settled publication/item mapping, never
// from a browser's assertion or an arbitrary awaited item.
type Binding struct {
	ItemID    string                   `json:"item_id"`
	SessionID string                   `json:"session_id"`
	TurnID    string                   `json:"turn_id"`
	AgentID   string                   `json:"agent_id"`
	Kind      string                   `json:"kind"`
	Source    plugin.TurnSourceMessage `json:"source"`
}

// Resolution mirrors the existing public turn_await wire. This is an operator
// resolution, not a model annotation or an authentication claim.
type Resolution struct {
	ID             string `json:"resolution_id"`
	Action         string `json:"action"`
	ResponseText   string `json:"response_text,omitempty"`
	SelectedOption string `json:"selected_option,omitempty"`
	Note           string `json:"note,omitempty"`
	Interrupt      bool   `json:"interrupt,omitempty"`
}

type ResolvedItem struct {
	ItemID     string                    `json:"item_id"`
	SessionID  string                    `json:"session_id"`
	TurnID     string                    `json:"turn_id"`
	AgentID    string                    `json:"agent_id"`
	Kind       string                    `json:"kind"`
	Replyable  bool                      `json:"replyable"`
	Source     *plugin.TurnSourceMessage `json:"source_message"`
	Resolution *Resolution               `json:"resolution"`
}

// Choice is supplied by an explicit user action. Empty ActionID is the first
// resolution-derived submission; a retry uses a fresh user action ID and names
// its terminal predecessor. Await/polling must never fabricate a retry choice.
type Choice struct {
	// OverrideInterrupt is allowed only for a new explicit retry action.
	OverrideInterrupt *bool
	ActionID          string
	Previous          *Reference
}

type Reference struct {
	ItemID       string `json:"item_id"`
	ResolutionID string `json:"resolution_id"`
	ActionID     string `json:"action_id"`
	Version      int64  `json:"version"`
}

type Prepared struct {
	Binding    Binding    `json:"binding"`
	Resolution Resolution `json:"resolution"`
	CallerURN  string     `json:"caller_urn"`
	ActionID   string     `json:"action_id"`
	Body       string     `json:"body"`
	Key        string     `json:"key"`
	Interrupt  bool       `json:"interrupt"`
	Previous   *Reference `json:"previous,omitempty"`
}

// Record contains private exact preparation plus body-free delivery metadata.
// Accepted is not delivered. A failed acknowledgement retains delivered state
// and is retried without re-submitting the reply.
type Record struct {
	Version       int64                 `json:"version"`
	Prepared      Prepared              `json:"prepared"`
	Receipt       *tether.ReplyReceipt  `json:"receipt,omitempty"`
	Delivery      *tether.ReplyDelivery `json:"delivery,omitempty"`
	FailureCode   string                `json:"failure_code,omitempty"`
	FailureStatus int                   `json:"failure_status,omitempty"`
	Acknowledged  bool                  `json:"acknowledged"`
}

func (r Record) Reference() Reference {
	return Reference{r.Prepared.Binding.ItemID, r.Prepared.Resolution.ID, r.Prepared.ActionID, r.Version}
}

func (r Record) TerminalFailure() bool {
	return r.FailureCode != "" || (r.Delivery != nil && r.Delivery.State == tether.ReplyUndeliverable)
}

// Store is implemented by the plugin's existing private-ledger owner.
// All returns are detached snapshots. Prepare atomically inserts exact data or
// returns an identical existing preparation; different bytes/fields conflict.
// A nonnil previous must match the latest terminal failed attempt and version,
// have the same binding/resolution/caller, and name a different ActionID. It
// preserves that old attempt. An initial preparation cannot supersede a retry.
// Update is a CAS, increments Version, preserves Prepared, and never replaces
// terminal outcomes or an acknowledgement. Latest is a read-only projection
// lookup. One loaded owner serializes advancement; the port is not a lease for
// two concurrent dispatchers.
type Store interface {
	Prepare(context.Context, Prepared, *Reference) (Record, error)
	Update(context.Context, Reference, Record) (Record, error)
	Latest(context.Context, string) (Record, error)
}

type Dispatcher interface {
	RoutingCapabilities(context.Context, string) (tether.RoutingCapabilitiesResponse, error)
	Reply(context.Context, string, string, tether.ReplyOptions) (tether.ReplyReceipt, error)
	ReplyDelivery(context.Context, string) (tether.ReplyDelivery, error)
}

type Host interface {
	Await(context.Context, string) ([]ResolvedItem, error)
	Ack(context.Context, string, string) error
}
