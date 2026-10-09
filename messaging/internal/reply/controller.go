package reply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	gomsg "github.com/hollis-labs/go-messaging"
	tether "github.com/hollis-labs/go-tether-client"
)

// Controller performs one bounded advance. The loaded owner schedules later
// polls with bounded backoff and joins them on shutdown. It supplies the real
// configured caller, the saved publication mapping, and any explicit choice.
type Controller struct {
	gate       chan struct{}
	store      Store
	dispatcher Dispatcher
	host       Host
	caller     string
	timeout    time.Duration
}

func New(store Store, dispatcher Dispatcher, host Host, caller string, timeout time.Duration) (*Controller, error) {
	if store == nil || dispatcher == nil || host == nil || timeout <= 0 || timeout > time.Minute {
		return nil, ErrRefused
	}
	if _, err := gomsg.ParseURN(caller); err != nil {
		return nil, ErrRefused
	}
	return &Controller{gate: make(chan struct{}, 1), store: store, dispatcher: dispatcher, host: host, caller: caller, timeout: timeout}, nil
}

func validID(value string) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 256
}

func prepare(binding Binding, item ResolvedItem, caller string, choice Choice) (Prepared, error) {
	if !validID(binding.ItemID) || !validID(binding.SessionID) || !validID(binding.TurnID) || !validID(binding.AgentID) ||
		binding.Source.SchemaVersion != 1 || binding.Source.Origin != "routed" || !validID(binding.Source.EndpointRef) || !validID(binding.Source.Channel) || !validID(binding.Source.MessageID) || binding.Source.Sequence <= 0 ||
		binding.Source.SenderURN != "msg://session/local/"+binding.SessionID || item.Resolution == nil || !item.Replyable ||
		item.ItemID != binding.ItemID || item.SessionID != binding.SessionID || item.TurnID != binding.TurnID ||
		item.AgentID != binding.AgentID || item.Kind != binding.Kind || item.Source == nil || !reflect.DeepEqual(*item.Source, binding.Source) {
		return Prepared{}, ErrRefused
	}
	switch binding.Kind {
	case "question", "approval", "failure", "terminal":
	default:
		return Prepared{}, ErrRefused
	}
	resolution := *item.Resolution
	if !validID(resolution.ID) || !validID(resolution.Action) ||
		(choice.ActionID != "" && !validID(choice.ActionID)) || (choice.Previous == nil) != (choice.ActionID == "") || (choice.ActionID == "" && choice.OverrideInterrupt != nil) {
		return Prepared{}, ErrRefused
	}
	switch resolution.Action {
	case "respond", "approve", "reject":
	default:
		return Prepared{}, ErrRefused
	}
	// Preserve typed text exactly. A button-only resolution uses its actual
	// selected value/action; a supplied note is included without trimming.
	body := resolution.ResponseText
	if body == "" {
		body = resolution.SelectedOption
	}
	if body == "" {
		body = resolution.Action
	}
	if resolution.Note != "" {
		body += ": " + resolution.Note
	}
	if !utf8.ValidString(body) || strings.TrimSpace(body) == "" || len(body) > 128*1024 {
		return Prepared{}, ErrRefused
	}
	tuple, _ := json.Marshal([]string{binding.Source.EndpointRef, binding.Source.Channel, binding.Source.MessageID,
		binding.ItemID, resolution.ID, caller, choice.ActionID})
	digest := sha256.Sum256(tuple)
	interrupt := resolution.Interrupt
	if choice.OverrideInterrupt != nil {
		interrupt = *choice.OverrideInterrupt
	}
	var previous *Reference
	if choice.Previous != nil {
		copied := *choice.Previous
		previous = &copied
	}
	return Prepared{binding, resolution, caller, choice.ActionID, body, "tether-reply:v1:" + hex.EncodeToString(digest[:]), interrupt, previous}, nil
}

// Advance never treats queue acceptance, binding handoff, or an unknown state
// as delivered. Each invocation performs at most one send and one delivery
// read, under a finite child budget of the owner-supplied context.
// lock serializes worker advancement and explicit HTTP retry in this loaded
// owner. Waiting is canceled with the request; no second send can race a receipt.
func (c *Controller) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Controller) unlock() { <-c.gate }

func (c *Controller) Advance(ctx context.Context, binding Binding, item ResolvedItem, choice Choice) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.lock(ctx); err != nil {
		return Record{}, err
	}
	defer c.unlock()
	// Await continues to carry the original immutable resolution. A saved retry
	// is an existing user action, so later worker polls resume it, never the old
	// terminal predecessor and never a newly invented retry/key.
	if choice.Previous == nil && choice.ActionID == "" && choice.OverrideInterrupt == nil {
		original, err := prepare(binding, item, c.caller, choice)
		if err != nil {
			return Record{}, err
		}
		latest, readErr := c.store.Latest(ctx, binding.ItemID)
		if readErr != nil && !errors.Is(readErr, ErrNotFound) {
			return Record{}, readErr
		}
		if readErr == nil {
			if latest.Prepared.Binding != original.Binding || latest.Prepared.Resolution != original.Resolution || latest.Prepared.CallerURN != original.CallerURN {
				return latest, ErrConflict
			}
			if latest.Prepared.ActionID != "" {
				flag := latest.Prepared.Interrupt
				choice = Choice{OverrideInterrupt: &flag, ActionID: latest.Prepared.ActionID, Previous: latest.Prepared.Previous}
			}
		}
	}
	return c.advance(ctx, binding, item, choice)
}

func (c *Controller) advance(ctx context.Context, binding Binding, item ResolvedItem, choice Choice) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	prepared, err := prepare(binding, item, c.caller, choice)
	if err != nil {
		return Record{}, err
	}
	if choice.Previous != nil && (choice.Previous.ItemID != binding.ItemID || choice.Previous.ResolutionID != prepared.Resolution.ID || choice.Previous.ActionID == choice.ActionID) {
		return Record{}, ErrRefused
	}
	record, err := c.store.Prepare(ctx, prepared, choice.Previous)
	if err != nil {
		return Record{}, err
	}
	if !reflect.DeepEqual(record.Prepared, prepared) || record.Version < 1 {
		return record, ErrConflict
	}
	if record.Acknowledged || record.TerminalFailure() {
		return record, nil
	}
	bounded, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if record.Receipt == nil {
		capabilities, capErr := c.dispatcher.RoutingCapabilities(bounded, binding.SessionID)
		if capErr != nil {
			return record, closedError(capErr)
		}
		if capabilities.SessionID != binding.SessionID || !capabilities.RouteSupported || !capabilities.ReplyToSender || (prepared.Interrupt && !capabilities.Interrupt) {
			record.FailureCode, record.FailureStatus = "reply_unsupported", 409
			if prepared.Interrupt && !capabilities.Interrupt {
				record.FailureCode = tether.CodeInterruptUnsupported
			}
			updated, saveErr := c.store.Update(bounded, record.Reference(), record)
			if saveErr != nil {
				return updated, saveErr
			}
			return updated, ErrUnsupported
		}
		if err = bounded.Err(); err != nil {
			return record, err
		}
		receipt, sendErr := c.dispatcher.Reply(bounded, binding.Source.MessageID, prepared.Body,
			tether.ReplyOptions{Interrupt: prepared.Interrupt, IdempotencyKey: prepared.Key})
		if sendErr != nil {
			var refusal *tether.APIError
			if errors.As(sendErr, &refusal) && ((refusal.StatusCode >= 400 && refusal.StatusCode < 500) || (refusal.StatusCode == 501 && refusal.Code == "not_implemented")) {
				// Definitive dedicated-endpoint refusal, not ambiguous acceptance.
				record.FailureCode, record.FailureStatus = refusalCode(refusal), refusal.StatusCode
				return c.store.Update(bounded, record.Reference(), record)
			}
			return record, closedError(sendErr)
		}
		if !validID(receipt.ReplyID) || receipt.ParentID != binding.Source.MessageID || !validID(receipt.TargetSessionID) || !knownState(tether.ReplyState(receipt.State)) {
			return record, ErrUnknown // Keep prepared; same-key replay is the only safe send.
		}
		record.Receipt = &receipt
		record, err = c.store.Update(bounded, record.Reference(), record)
		if err != nil {
			return record, err
		}
	}
	if record.Delivery == nil || record.Delivery.State != tether.ReplyDelivered {
		if err = bounded.Err(); err != nil {
			return record, err
		}
		delivery, readErr := c.dispatcher.ReplyDelivery(bounded, record.Receipt.ReplyID)
		if readErr != nil {
			return record, closedError(readErr)
		}
		if delivery.ReplyID != record.Receipt.ReplyID || delivery.ParentID != binding.Source.MessageID || delivery.OriginalSessionID != binding.SessionID || !validID(delivery.TargetSessionID) || delivery.Attempts < 0 || len(delivery.State) > 128 || len(delivery.Reason) > 128 || (delivery.DeliveredToSessionID != "" && !validID(delivery.DeliveredToSessionID)) {
			return record, ErrUnknown
		}
		// Raw dispatcher diagnostic detail is not a private-store/projection API.
		delivery.Detail = ""
		record.Delivery = &delivery
		record, err = c.store.Update(bounded, record.Reference(), record)
		if err != nil {
			return record, err
		}
		if !knownState(delivery.State) {
			return record, ErrUnknown
		}
	}
	if record.Delivery.State != tether.ReplyDelivered {
		return record, nil
	}
	if err = bounded.Err(); err != nil {
		return record, err
	}
	if err = c.host.Ack(bounded, binding.ItemID, prepared.Resolution.ID); err != nil {
		return record, closedError(err)
	}
	record.Acknowledged = true
	return c.store.Update(bounded, record.Reference(), record)
}

func knownState(state tether.ReplyState) bool {
	switch state {
	case tether.ReplyPending, tether.ReplyQueued, tether.ReplyDelivering, tether.ReplyDelivered, tether.ReplyUndeliverable:
		return true
	}
	return false
}

// Resume advances an existing exact preparation, including an acknowledgement
// after the host already consumed it and no longer returns it from Await. It
// cannot create a new preparation, resolution, body, action or idempotency key.
func (c *Controller) Resume(ctx context.Context, itemID string) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.lock(ctx); err != nil {
		return Record{}, err
	}
	defer c.unlock()
	if !validID(itemID) {
		return Record{}, ErrRefused
	}
	record, err := c.store.Latest(ctx, itemID)
	if err != nil {
		return Record{}, err
	}
	if record.Version < 1 || record.Prepared.Binding.ItemID != itemID || record.Prepared.CallerURN != c.caller {
		return record, ErrConflict
	}
	p := record.Prepared
	binding, resolution := p.Binding, p.Resolution
	item := ResolvedItem{ItemID: binding.ItemID, SessionID: binding.SessionID, TurnID: binding.TurnID, AgentID: binding.AgentID, Kind: binding.Kind, Replyable: true, Source: &binding.Source, Resolution: &resolution}
	choice := Choice{}
	if p.ActionID != "" {
		flag := p.Interrupt
		choice = Choice{OverrideInterrupt: &flag, ActionID: p.ActionID, Previous: p.Previous}
	}
	expected, err := prepare(binding, item, c.caller, choice)
	if err != nil {
		return record, err
	}
	if !reflect.DeepEqual(expected, p) {
		return record, ErrConflict
	}
	return c.advance(ctx, binding, item, choice)
}

// Retry is an explicit new user action, never an Await/poll recovery method.
// The transport must enforce the real participant guard before invoking it.
// It reuses the recorded resolution/body and preserves the failed predecessor.
func (c *Controller) Retry(ctx context.Context, itemID string, expectedVersion int64, actionID string, interrupt bool) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.lock(ctx); err != nil {
		return Record{}, err
	}
	defer c.unlock()
	if !validID(itemID) || !validID(actionID) || expectedVersion < 1 {
		return Record{}, ErrRefused
	}
	record, err := c.store.Latest(ctx, itemID)
	if err != nil {
		return Record{}, err
	}
	previous := record.Reference()
	if record.Prepared.ActionID == actionID {
		// A browser retry of the same click is the same action/key, including
		// after ambiguous acceptance. It cannot change the saved choice.
		if record.Prepared.Interrupt != interrupt || record.Prepared.Previous == nil || record.Prepared.Previous.Version != expectedVersion {
			return record, ErrConflict
		}
		previous = *record.Prepared.Previous
	} else if record.Version != expectedVersion || !record.TerminalFailure() {
		return record, ErrConflict
	}
	binding, resolution := record.Prepared.Binding, record.Prepared.Resolution
	item := ResolvedItem{ItemID: binding.ItemID, SessionID: binding.SessionID, TurnID: binding.TurnID, AgentID: binding.AgentID,
		Kind: binding.Kind, Replyable: true, Source: &binding.Source, Resolution: &resolution}
	return c.advance(ctx, binding, item, Choice{OverrideInterrupt: &interrupt, ActionID: actionID, Previous: &previous})
}

func closedError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrUnknown // Never copy upstream bodies, credentials, or diagnostics.
}

func refusalCode(err *tether.APIError) string {
	switch err.Code {
	case tether.CodeInterruptUnsupported, tether.CodeTurnNotYetStarted, tether.CodeReplyTargetNotSession,
		"not_found", "payload_too_large", "invalid_request", "idempotency_conflict", "unauthorized", "forbidden", "not_implemented":
		return err.Code
	}
	return "reply_refused"
}
