package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	tether "github.com/hollis-labs/go-tether-client"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/reply"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

func replyFixture(t *testing.T) (*Ledger, *ReplyStore, reply.Prepared, string) {
	t.Helper()
	ctx := context.Background()
	ledger, data := openTestLedger(t)
	p := admitted(t)
	if _, err := ledger.Capture(ctx, p); err != nil {
		t.Fatal(err)
	}
	source := plugin.TurnSourceMessage{SchemaVersion: 1, Origin: p.Origin, EndpointRef: p.Source.EndpointRef, Channel: p.Source.Channel,
		MessageID: p.MessageID, Sequence: p.Sequence, SenderURN: p.SenderURN, OutputID: p.OutputID,
		Attribution: plugin.TurnSourceAttribution{Kind: p.Metadata["kind"], LogicalAgentID: p.Metadata["logical_agent_id"], Runtime: p.Metadata["runtime"], Confidence: p.Metadata["confidence"]}}
	request := plugin.AgentTurnRequest{ContractVersion: plugin.AgentTurnContractVersion, IdempotencyKey: p.Key(), Content: p.Original,
		Kind: p.Kind, SessionID: p.SessionID, TurnID: p.TurnID, Source: plugin.AgentTurnSource{AgentID: p.AgentID}, SourceMessage: &source}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = ledger.Prepare(ctx, p.Source, p.MessageID, raw); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Settle(ctx, p.Source, p.MessageID, 0, SinkReceipt{ItemID: "item-1", IdempotencyKey: p.Key()}); err != nil {
		t.Fatal(err)
	}
	store, err := NewReplyStore(ctx, ledger, testSource())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.Binding(ctx, "item-1")
	if err != nil {
		t.Fatal(err)
	}
	prepared := reply.Prepared{Binding: binding, Resolution: reply.Resolution{ID: "resolution-1", Action: "respond", ResponseText: " exact user reply "},
		CallerURN: "msg://service/local/messaging", Body: " exact user reply "}
	prepared.Key = testReplyKey(prepared)
	return ledger, store, prepared, data
}

func testReplyKey(p reply.Prepared) string {
	raw, _ := json.Marshal([]string{p.Binding.Source.EndpointRef, p.Binding.Source.Channel, p.Binding.Source.MessageID,
		p.Binding.ItemID, p.Resolution.ID, p.CallerURN, p.ActionID})
	digest := sha256.Sum256(raw)
	return "tether-reply:v1:" + hex.EncodeToString(digest[:])
}

func TestReplyStorePreparationRestartAndDetachedSnapshots(t *testing.T) {
	ctx := context.Background()
	ledger, store, prepared, data := replyFixture(t)
	record, err := store.Prepare(ctx, prepared, nil)
	if err != nil || record.Version != 1 || record.Prepared != prepared {
		t.Fatal("initial preparation", record, err)
	}
	record.Prepared.Body = "caller mutation"
	identical, err := store.Prepare(ctx, prepared, nil)
	if err != nil || identical.Prepared.Body != prepared.Body || identical.Version != 1 {
		t.Fatal("identical preparation changed", identical, err)
	}
	changed := prepared
	changed.Body = "changed reply"
	if _, err = store.Prepare(ctx, changed, nil); err == nil {
		t.Fatal("changed body accepted")
	}
	if err = ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	store, err = NewReplyStore(ctx, reopened, testSource())
	if err != nil {
		t.Fatal(err)
	}
	actual, err := store.Latest(ctx, prepared.Binding.ItemID)
	if err != nil || !reflect.DeepEqual(actual, identical) {
		t.Fatal("restart lost exact request/key", actual, err)
	}
	var version int
	if err = reopened.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatal("reply changed publication schema", version, err)
	}
}

func acceptedReply(record reply.Record) reply.Record {
	record.Receipt = &tether.ReplyReceipt{ReplyID: "reply-1", ParentID: record.Prepared.Binding.Source.MessageID, TargetSessionID: "bound-target", State: "queued"}
	return record
}

func deliveredReply(record reply.Record) reply.Record {
	record.Delivery = &tether.ReplyDelivery{ReplyID: record.Receipt.ReplyID, ParentID: record.Receipt.ParentID, State: tether.ReplyDelivered,
		OriginalSessionID: record.Prepared.Binding.SessionID, TargetSessionID: record.Receipt.TargetSessionID, DeliveredToSessionID: "bound-target", Attempts: 1}
	return record
}

func TestReplyStoreCASReceiptDeliveryBeforeAckAndTerminalPreservation(t *testing.T) {
	ctx := context.Background()
	_, store, prepared, _ := replyFixture(t)
	initial, err := store.Prepare(ctx, prepared, nil)
	if err != nil {
		t.Fatal(err)
	}
	early := initial
	early.Acknowledged = true
	if _, err = store.Update(ctx, initial.Reference(), early); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("ack without delivery accepted", err)
	}
	accepted, err := store.Update(ctx, initial.Reference(), acceptedReply(initial))
	if err != nil || accepted.Version != 2 {
		t.Fatal("receipt not durable", accepted, err)
	}
	if _, err = store.Update(ctx, initial.Reference(), acceptedReply(initial)); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("stale update accepted", err)
	}
	detached, err := store.Latest(ctx, prepared.Binding.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	detached.Receipt.ReplyID = "mutated caller snapshot"
	durable, err := store.Latest(ctx, prepared.Binding.ItemID)
	if err != nil || durable.Receipt.ReplyID != accepted.Receipt.ReplyID {
		t.Fatal("receipt aliases caller memory", durable, err)
	}
	mutated := accepted
	mutated.Receipt = &tether.ReplyReceipt{ReplyID: "other", ParentID: prepared.Binding.Source.MessageID, TargetSessionID: "bound-target"}
	if _, err = store.Update(ctx, accepted.Reference(), mutated); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("earned receipt replaced", err)
	}
	delivered, err := store.Update(ctx, accepted.Reference(), deliveredReply(accepted))
	if err != nil {
		t.Fatal(err)
	}
	// A failed ack does not require any store write: a restart sees delivery.
	saved, err := store.Latest(ctx, prepared.Binding.ItemID)
	if err != nil || saved.Delivery.State != tether.ReplyDelivered || saved.Acknowledged {
		t.Fatal("delivery not preserved before external ack", saved, err)
	}
	rollback := delivered
	rollback.Delivery = &tether.ReplyDelivery{ReplyID: "reply-1", ParentID: prepared.Binding.Source.MessageID, OriginalSessionID: prepared.Binding.SessionID, TargetSessionID: "bound-target", State: tether.ReplyQueued}
	if _, err = store.Update(ctx, delivered.Reference(), rollback); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("delivered outcome replaced", err)
	}
	delivered.Acknowledged = true
	acked, err := store.Update(ctx, saved.Reference(), delivered)
	if err != nil || !acked.Acknowledged {
		t.Fatal(err)
	}
	ackedRef := acked.Reference()
	acked.Acknowledged = false
	if _, err = store.Update(ctx, ackedRef, acked); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("acknowledgement cleared", err)
	}
}

func TestReplyStoreRetryIsAtomicAndPreservesFailedAttempt(t *testing.T) {
	ctx := context.Background()
	ledger, store, prepared, _ := replyFixture(t)
	initial, err := store.Prepare(ctx, prepared, nil)
	if err != nil {
		t.Fatal(err)
	}
	pendingRef := initial.Reference()
	premature := prepared
	premature.ActionID, premature.Previous = "premature-click", &pendingRef
	premature.Key = testReplyKey(premature)
	if _, err = store.Prepare(ctx, premature, &pendingRef); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("pending ambiguous attempt superseded", err)
	}
	next := initial
	next.FailureCode, next.FailureStatus = "interrupt_unsupported", 409
	failed, err := store.Update(ctx, initial.Reference(), next)
	if err != nil {
		t.Fatal(err)
	}
	previous := failed.Reference()
	retry := prepared
	retry.ActionID, retry.Previous, retry.Interrupt = "user-click-1", &previous, true
	retry.Key = testReplyKey(retry)
	if _, err = ledger.db.ExecContext(ctx, `CREATE TRIGGER fail_reply_head BEFORE UPDATE ON reply_heads BEGIN SELECT RAISE(ABORT,'owned retry failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Prepare(ctx, retry, &previous); err == nil {
		t.Fatal("head failure ignored")
	}
	if _, err = replyAttempt(ctx, ledger.db, reply.Reference{ItemID: previous.ItemID, ResolutionID: previous.ResolutionID, ActionID: retry.ActionID}); !errors.Is(err, reply.ErrNotFound) {
		t.Fatal("partial retry escaped transaction", err)
	}
	if _, err = ledger.db.ExecContext(ctx, `DROP TRIGGER fail_reply_head`); err != nil {
		t.Fatal(err)
	}
	created, err := store.Prepare(ctx, retry, &previous)
	if err != nil || created.Prepared.Key == failed.Prepared.Key {
		t.Fatal("fresh retry key not created", created, err)
	}
	old, err := replyAttempt(ctx, ledger.db, previous)
	if err != nil || !reflect.DeepEqual(old, failed) {
		t.Fatal("failed predecessor changed", old, err)
	}
	if _, err = store.Prepare(ctx, prepared, nil); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("Await reopened superseded initial attempt", err)
	}
	identical, err := store.Prepare(ctx, retry, &previous)
	if err != nil || !reflect.DeepEqual(identical, created) {
		t.Fatal("same browser click was not idempotent", identical, err)
	}
	competing := retry
	competing.ActionID = "other-click"
	competing.Key = testReplyKey(competing)
	if _, err = store.Prepare(ctx, competing, &previous); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("stale failed predecessor reused", err)
	}
	created.Prepared.Previous.Version = 999
	latest, err := store.Latest(ctx, prepared.Binding.ItemID)
	if err != nil || latest.Prepared.Previous.Version != previous.Version {
		t.Fatal("pointer mutation changed storage", latest, err)
	}
}

func TestReplyStoreUndeliverableCannotReopenOrAck(t *testing.T) {
	ctx := context.Background()
	_, store, prepared, _ := replyFixture(t)
	record, err := store.Prepare(ctx, prepared, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Update(ctx, record.Reference(), acceptedReply(record))
	if err != nil {
		t.Fatal(err)
	}
	next := deliveredReply(record)
	next.Delivery.State = tether.ReplyUndeliverable
	record, err = store.Update(ctx, record.Reference(), next)
	if err != nil {
		t.Fatal(err)
	}
	next = record
	next.Delivery = nil
	if _, err = store.Update(ctx, record.Reference(), next); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("terminal undeliverable outcome erased", err)
	}
	next = record
	next.Acknowledged = true
	if _, err = store.Update(ctx, record.Reference(), next); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("undeliverable was acknowledged", err)
	}
}

func TestReplyStoreCompetingPreparesAndCancelledWrite(t *testing.T) {
	ctx := context.Background()
	_, store, prepared, _ := replyFixture(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Prepare(cancelled, prepared, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled write accepted", err)
	}
	if _, err := store.Latest(ctx, prepared.Binding.ItemID); !errors.Is(err, reply.ErrNotFound) {
		t.Fatal("cancelled preparation persisted", err)
	}
	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := store.Prepare(ctx, prepared, nil)
			errorsSeen <- err
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal("identical concurrent preparation", err)
		}
	}
	latest, err := store.Latest(ctx, prepared.Binding.ItemID)
	if err != nil || latest.Version != 1 {
		t.Fatal("concurrent calls changed preparation", latest, err)
	}
}

func TestReplyStoreRefusesUnsettledOrForeignItemBinding(t *testing.T) {
	ctx := context.Background()
	ledger, store, prepared, _ := replyFixture(t)
	if _, err := store.Binding(ctx, "browser-invented"); !errors.Is(err, reply.ErrNotFound) {
		t.Fatal("invented item mapped", err)
	}
	changed := prepared
	changed.Binding.Source.MessageID = "foreign-message"
	changed.Key = testReplyKey(changed)
	if _, err := store.Prepare(ctx, changed, nil); !errors.Is(err, reply.ErrConflict) {
		t.Fatal("foreign runtime binding accepted", err)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE publications SET settled=0 WHERE item_id=?`, prepared.Binding.ItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Binding(ctx, prepared.Binding.ItemID); !errors.Is(err, reply.ErrNotFound) {
		t.Fatal("unearned item mapped", err)
	}
}

type storedReplyDispatcher struct {
	sends   int
	polls   int
	keys    []string
	bodies  []string
	binding reply.Binding
}

func (d *storedReplyDispatcher) RoutingCapabilities(context.Context, string) (tether.RoutingCapabilitiesResponse, error) {
	return tether.RoutingCapabilitiesResponse{SessionID: d.binding.SessionID, RouteSupported: true, ReplyToSender: true}, nil
}

func (d *storedReplyDispatcher) Reply(_ context.Context, _ string, body string, opts tether.ReplyOptions) (tether.ReplyReceipt, error) {
	d.sends++
	d.keys = append(d.keys, opts.IdempotencyKey)
	d.bodies = append(d.bodies, body)
	if d.sends == 1 {
		return tether.ReplyReceipt{}, errors.New("owned ambiguous transport failure")
	}
	return tether.ReplyReceipt{ReplyID: "reply-1", ParentID: d.binding.Source.MessageID, TargetSessionID: "target", State: "queued", Duplicate: true}, nil
}

func (d *storedReplyDispatcher) ReplyDelivery(context.Context, string) (tether.ReplyDelivery, error) {
	d.polls++
	state := tether.ReplyQueued
	if d.polls > 1 {
		state = tether.ReplyDelivered
	}
	return tether.ReplyDelivery{ReplyID: "reply-1", ParentID: d.binding.Source.MessageID, OriginalSessionID: d.binding.SessionID,
		TargetSessionID: "target", State: state, Attempts: 1}, nil
}

type storedReplyHost struct{ acks int }

func (*storedReplyHost) Await(context.Context, string) ([]reply.ResolvedItem, error) {
	return nil, nil
}

func (h *storedReplyHost) Ack(context.Context, string, string) error {
	h.acks++
	if h.acks == 1 {
		return errors.New("owned ack transport failure")
	}
	return nil
}

func TestReplyStoreControllerRestartReusesAmbiguousSendAndRetriesOnlyAck(t *testing.T) {
	ctx := context.Background()
	ledger, store, prepared, data := replyFixture(t)
	dispatcher := &storedReplyDispatcher{binding: prepared.Binding}
	host := &storedReplyHost{}
	controller, err := reply.New(store, dispatcher, host, prepared.CallerURN, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b := prepared.Binding
	item := reply.ResolvedItem{ItemID: b.ItemID, SessionID: b.SessionID, TurnID: b.TurnID, AgentID: b.AgentID, Kind: b.Kind,
		Replyable: true, Source: &b.Source, Resolution: &prepared.Resolution}
	if _, err = controller.Advance(ctx, b, item, reply.Choice{}); !errors.Is(err, reply.ErrUnknown) {
		t.Fatal("ambiguous send marked settled", err)
	}
	record, err := controller.Advance(ctx, b, item, reply.Choice{})
	if err != nil || record.Receipt == nil || record.Delivery.State != tether.ReplyQueued || host.acks != 0 ||
		dispatcher.sends != 2 || dispatcher.keys[0] != dispatcher.keys[1] || dispatcher.bodies[0] != dispatcher.bodies[1] {
		t.Fatal("ambiguous retry changed wire or queue acked", record, err)
	}
	if err = ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	store, err = NewReplyStore(ctx, reopened, testSource())
	if err != nil {
		t.Fatal(err)
	}
	controller, err = reply.New(store, dispatcher, host, prepared.CallerURN, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	record, err = controller.Advance(ctx, b, item, reply.Choice{})
	if !errors.Is(err, reply.ErrUnknown) || record.Delivery.State != tether.ReplyDelivered || record.Acknowledged || dispatcher.sends != 2 {
		t.Fatal("restart resent receipt or lost delivered-before-ack", record, err)
	}
	record, err = controller.Advance(ctx, b, item, reply.Choice{})
	if err != nil || !record.Acknowledged || dispatcher.sends != 2 || dispatcher.polls != 2 || host.acks != 2 {
		t.Fatal("ack retry repeated send/poll", record, err)
	}
}

func TestReplyStoreConfigurationScopeDoesNotRouteRetainedOtherEndpoint(t *testing.T) {
	ctx := context.Background()
	ledger, store, prepared, _ := replyFixture(t)
	if _, err := store.Prepare(ctx, prepared, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReplyStore(ctx, ledger); !errors.Is(err, reply.ErrRefused) {
		t.Fatal("implicit source scope accepted", err)
	}
	configured := []Source{{EndpointRef: "other-daemon", Channel: prepared.Binding.Source.Channel}}
	other, err := NewReplyStore(ctx, ledger, configured...)
	if err != nil {
		t.Fatal(err)
	}
	configured[0] = testSource()
	if _, err = other.Binding(ctx, prepared.Binding.ItemID); !errors.Is(err, reply.ErrRefused) {
		t.Fatal("old endpoint binding reached new dispatcher", err)
	}
	if _, err = other.Latest(ctx, prepared.Binding.ItemID); !errors.Is(err, reply.ErrRefused) {
		t.Fatal("retry bypassed configured endpoint", err)
	}
	if _, err = other.Prepare(ctx, prepared, nil); !errors.Is(err, reply.ErrRefused) {
		t.Fatal("old preparation admitted to new endpoint", err)
	}
	if actual, err := store.Latest(ctx, prepared.Binding.ItemID); err != nil || actual.Prepared.Key != prepared.Key {
		t.Fatal("configuration change destroyed retained history", actual, err)
	}
}
