// Package replyprojection serves metadata, never original/reply text or keys.
// Routes are mounted by Tangent's actual participant/capability guard; cookies
// and participant session IDs are intentionally not forwarded to this handler.
package replyprojection

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	tether "github.com/hollis-labs/go-tether-client"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/reply"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

const DeliveryPath = "/api/plugins/messaging/delivery"
const RetryPath = "/api/plugins/messaging/retry"

// Mappings reads only locally settled, exact publication->item mappings.
// Absence must not be filled by trusting browser-supplied runtime identity.
type Mappings interface {
	Binding(context.Context, string) (reply.Binding, error)
}

type Reader struct {
	Mappings   Mappings
	Store      reply.Store
	Dispatcher reply.Dispatcher
	Timeout    time.Duration
}

type View struct {
	SchemaVersion        int    `json:"schema_version"`
	ItemID               string `json:"item_id"`
	Version              int64  `json:"version"`
	ResolutionID         string `json:"resolution_id,omitempty"`
	ActionID             string `json:"action_id,omitempty"`
	State                string `json:"state"`
	Reason               string `json:"reason,omitempty"`
	FailureCode          string `json:"failure_code,omitempty"`
	FailureStatus        int    `json:"failure_status,omitempty"`
	Accepted             bool   `json:"accepted"`
	Acknowledged         bool   `json:"acknowledged"`
	ReplyID              string `json:"reply_id,omitempty"`
	OriginalSessionID    string `json:"original_session_id,omitempty"`
	TargetSessionID      string `json:"target_session_id,omitempty"`
	DeliveredToSessionID string `json:"delivered_to_session_id,omitempty"`
	Attempts             int    `json:"attempts"`
	ReplySupported       bool   `json:"reply_supported"`
	InterruptSupported   bool   `json:"interrupt_supported"`
	InterruptRequested   bool   `json:"interrupt_requested"`
	RetryAllowed         bool   `json:"retry_allowed"`
}

func (r Reader) Read(ctx context.Context, itemID string) (View, error) {
	view := View{SchemaVersion: 1, ItemID: itemID, State: "not_submitted"}
	if r.Mappings == nil || r.Store == nil || r.Dispatcher == nil || r.Timeout <= 0 || r.Timeout > time.Minute {
		return view, reply.ErrRefused
	}
	bounded, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	binding, err := r.Mappings.Binding(bounded, itemID)
	if err != nil {
		return view, err
	}
	if binding.ItemID != itemID {
		return view, reply.ErrConflict
	}
	if binding.Source.Origin != "routed" {
		view.State = "nonreplyable"
		return view, nil
	}
	capabilities, capErr := r.Dispatcher.RoutingCapabilities(bounded, binding.SessionID)
	if capErr == nil && capabilities.SessionID == binding.SessionID {
		view.ReplySupported = capabilities.RouteSupported && capabilities.ReplyToSender
		view.InterruptSupported = view.ReplySupported && capabilities.Interrupt
	}
	record, err := r.Store.Latest(bounded, itemID)
	if errors.Is(err, reply.ErrNotFound) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	if record.Prepared.Binding != binding {
		return view, reply.ErrConflict
	}
	view.Version, view.ResolutionID, view.ActionID = record.Version, record.Prepared.Resolution.ID, record.Prepared.ActionID
	view.InterruptRequested, view.Acknowledged = record.Prepared.Interrupt, record.Acknowledged
	view.FailureCode, view.FailureStatus = record.FailureCode, record.FailureStatus
	view.State = "prepared"
	if record.Receipt != nil {
		view.Accepted, view.ReplyID, view.State = true, record.Receipt.ReplyID, "accepted"
	}
	if record.Delivery != nil {
		view.State, view.Reason, view.Attempts = string(record.Delivery.State), record.Delivery.Reason, record.Delivery.Attempts
		if !knownState(view.State) {
			view.State = "unknown"
		}
		view.Reason = closedReason(view.Reason)
		view.OriginalSessionID, view.TargetSessionID, view.DeliveredToSessionID = record.Delivery.OriginalSessionID, record.Delivery.TargetSessionID, record.Delivery.DeliveredToSessionID
	}
	if record.FailureCode != "" {
		view.State = "refused"
	}
	view.RetryAllowed = record.TerminalFailure() && view.ReplySupported
	return view, nil
}

func knownState(state string) bool {
	switch tether.ReplyState(state) {
	case tether.ReplyPending, tether.ReplyQueued, tether.ReplyDelivering, tether.ReplyDelivered, tether.ReplyUndeliverable:
		return true
	}
	return false
}

func closedReason(reason string) string {
	switch reason {
	case "", tether.ReplyReasonHandedOff, tether.ReplyReasonTurnFailed, tether.ReplyReasonSessionEndedNoBinding,
		tether.ReplyReasonBoundSessionNotRunning, tether.ReplyReasonPullOnlyBinding, tether.ReplyReasonResolveFailed,
		tether.ReplyReasonSubmitFailed, tether.ReplyReasonNoTurnFeed, tether.ReplyReasonDaemonRestartedDuringDelivery,
		tether.ReplyReasonInterruptUnconfirmed, tether.ReplyReasonBodyPurged, tether.ReplyReasonWaitingForIdle:
		return reason
	}
	return "unknown_reason"
}

type Handler struct {
	Reader     Reader
	Controller *reply.Controller
}

// Routes are declarations for the plugin owner's manifest/wiring. GET performs
// only lookup/capability reads. POST is a deliberate retry, guarded as a plugin
// button by the host's participant CapabilityDraft path. Neither grants human
// authentication beyond the accepted single-owner MVP boundary.
func (h Handler) Routes() []plugin.HTTPRoute {
	return []plugin.HTTPRoute{
		{Method: http.MethodGet, Path: DeliveryPath, Capability: plugin.CapabilityView, Handler: h},
		{Method: http.MethodPost, Path: RetryPath, Capability: plugin.CapabilityDraft, Handler: h},
	}
}

func (h Handler) HTTPHandle(ctx context.Context, req subprocess.HTTPRequest) (subprocess.HTTPResponse, error) {
	if req.Method == http.MethodGet && req.Path == DeliveryPath {
		id := req.Query["item_id"]
		if id == "" || len(id) > 1024 || len(req.Body) != 0 || len(req.Query) != 1 {
			return response(http.StatusBadRequest, map[string]string{"code": "invalid_request"}), nil
		}
		view, err := h.Reader.Read(ctx, id)
		if err != nil {
			return refusal(err), nil
		}
		return response(http.StatusOK, view), nil
	}
	if req.Method == http.MethodPost && req.Path == RetryPath {
		if h.Controller == nil || len(req.Body) > 4096 || len(req.Query) != 0 {
			return response(http.StatusBadRequest, map[string]string{"code": "invalid_request"}), nil
		}
		var action struct {
			ItemID          string `json:"item_id"`
			ExpectedVersion int64  `json:"expected_version"`
			ActionID        string `json:"action_id"`
			Interrupt       bool   `json:"interrupt"`
		}
		if !decodeAction(req.Body, &action) {
			return response(http.StatusBadRequest, map[string]string{"code": "invalid_request"}), nil
		}
		_, err := h.Controller.Retry(ctx, action.ItemID, action.ExpectedVersion, action.ActionID, action.Interrupt)
		if err != nil {
			return refusal(err), nil
		}
		view, err := h.Reader.Read(ctx, action.ItemID)
		if err != nil {
			return refusal(err), nil
		}
		return response(http.StatusAccepted, view), nil
	}
	return response(http.StatusNotFound, map[string]string{"code": "not_found"}), nil
}

func response(status int, value any) subprocess.HTTPResponse {
	body, _ := json.Marshal(value)
	return subprocess.HTTPResponse{Status: status, Headers: map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"}, Body: body}
}

func refusal(err error) subprocess.HTTPResponse {
	status, code := http.StatusServiceUnavailable, "reply_unavailable"
	switch {
	case errors.Is(err, reply.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, reply.ErrConflict):
		status, code = http.StatusConflict, "reply_conflict"
	case errors.Is(err, reply.ErrRefused):
		status, code = http.StatusBadRequest, "reply_refused"
	case errors.Is(err, reply.ErrUnsupported):
		status, code = http.StatusConflict, "reply_unsupported"
	}
	return response(status, map[string]string{"code": code})
}
