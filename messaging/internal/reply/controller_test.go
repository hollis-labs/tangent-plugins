package reply

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	tether "github.com/hollis-labs/go-tether-client"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

// The port fixture snapshots records as encoded bytes. Production SQLite and
// loaded-owner wiring belong to 0070's ledger owner, not this fake store.
type snapshotStore struct {
	rows       map[string][]byte
	latest     map[string]string
	failUpdate bool
}

func newStore() *snapshotStore {
	return &snapshotStore{rows: map[string][]byte{}, latest: map[string]string{}}
}
func key(p Prepared) string { return p.Binding.ItemID + "/" + p.Resolution.ID + "/" + p.ActionID }
func (s *snapshotStore) get(k string) (Record, error) {
	var r Record
	raw, ok := s.rows[k]
	if !ok {
		return r, ErrNotFound
	}
	err := json.Unmarshal(raw, &r)
	return r, err
}
func (s *snapshotStore) put(r Record) {
	raw, _ := json.Marshal(r)
	s.rows[key(r.Prepared)] = raw
	s.latest[r.Prepared.Binding.ItemID] = key(r.Prepared)
}
func (s *snapshotStore) Prepare(ctx context.Context, p Prepared, previous *Reference) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if old, err := s.get(key(p)); err == nil {
		if !reflect.DeepEqual(old.Prepared, p) {
			return old, ErrConflict
		}
		return old, nil
	}
	if previous != nil {
		old, err := s.Latest(ctx, p.Binding.ItemID)
		if err != nil || old.Reference() != *previous || !old.TerminalFailure() || old.Prepared.CallerURN != p.CallerURN || old.Prepared.Binding != p.Binding || old.Prepared.Resolution != p.Resolution {
			return old, ErrConflict
		}
	} else if _, ok := s.latest[p.Binding.ItemID]; ok {
		return Record{}, ErrConflict
	}
	r := Record{Version: 1, Prepared: p}
	s.put(r)
	return s.get(key(p))
}
func (s *snapshotStore) Update(ctx context.Context, ref Reference, next Record) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if s.failUpdate {
		return Record{}, errors.New("synthetic ledger write failure")
	}
	old, err := s.get(key(next.Prepared))
	if err != nil || old.Reference() != ref || !reflect.DeepEqual(old.Prepared, next.Prepared) {
		return old, ErrConflict
	}
	next.Version = old.Version + 1
	s.put(next)
	return s.get(key(next.Prepared))
}
func (s *snapshotStore) Latest(ctx context.Context, id string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	return s.get(s.latest[id])
}

type fakeDispatcher struct {
	store           *snapshotStore
	sends           []Prepared
	capCalls, polls int
	interrupt       bool
	capFailure      bool
	sendError       error
	state           tether.ReplyState
	reason          string
	cancel          context.CancelFunc
}

func (d *fakeDispatcher) RoutingCapabilities(_ context.Context, id string) (tether.RoutingCapabilitiesResponse, error) {
	d.capCalls++
	return tether.RoutingCapabilitiesResponse{SessionID: id, RouteSupported: !d.capFailure, ReplyToSender: !d.capFailure, Interrupt: d.interrupt}, nil
}
func (d *fakeDispatcher) Reply(ctx context.Context, id, body string, opts tether.ReplyOptions) (tether.ReplyReceipt, error) {
	if err := ctx.Err(); err != nil {
		return tether.ReplyReceipt{}, err
	}
	r, err := d.store.Latest(ctx, "item-fixture")
	if err != nil || r.Prepared.Body != body || r.Prepared.Key != opts.IdempotencyKey || r.Prepared.Interrupt != opts.Interrupt || r.Prepared.Binding.Source.MessageID != id {
		panic("send before exact preparation")
	}
	d.sends = append(d.sends, r.Prepared)
	if d.cancel != nil {
		d.cancel()
		return tether.ReplyReceipt{}, context.Canceled
	}
	if d.sendError != nil {
		return tether.ReplyReceipt{}, d.sendError
	}
	return tether.ReplyReceipt{ReplyID: "reply-fixture", ParentID: id, State: "queued", TargetSessionID: "session-fixture", Duplicate: len(d.sends) > 1}, nil
}
func (d *fakeDispatcher) ReplyDelivery(ctx context.Context, id string) (tether.ReplyDelivery, error) {
	d.polls++
	r, err := d.store.Latest(ctx, "item-fixture")
	if err != nil || r.Receipt == nil {
		panic("poll before durable receipt")
	}
	return tether.ReplyDelivery{ReplyID: id, ParentID: "publication-fixture", OriginalSessionID: "session-fixture", TargetSessionID: "successor-fixture", DeliveredToSessionID: "successor-fixture", State: d.state, Reason: d.reason, Attempts: 1, Detail: "synthetic private diagnostic must not project"}, nil
}

type fakeHost struct {
	store *snapshotStore
	acks  int
	fail  bool
}

func (h *fakeHost) Await(context.Context, string) ([]ResolvedItem, error) { return nil, nil }
func (h *fakeHost) Ack(ctx context.Context, id, res string) error {
	r, err := h.store.Latest(ctx, id)
	if err != nil || r.Delivery == nil || r.Delivery.State != tether.ReplyDelivered || res != r.Prepared.Resolution.ID {
		panic("ack without durable delivered evidence")
	}
	h.acks++
	if h.fail {
		return errors.New("synthetic ack failure")
	}
	return nil
}
func fixture(t *testing.T) (*Controller, *snapshotStore, *fakeDispatcher, *fakeHost, Binding, ResolvedItem) {
	t.Helper()
	s := newStore()
	d := &fakeDispatcher{store: s, state: tether.ReplyQueued, interrupt: true}
	h := &fakeHost{store: s}
	c, err := New(s, d, h, "msg://service/local/fixture-consumer", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{ItemID: "item-fixture", SessionID: "session-fixture", TurnID: "turn-fixture", AgentID: "agent-fixture", Kind: "question", Source: plugin.TurnSourceMessage{SchemaVersion: 1, Origin: "routed", EndpointRef: "fixture-endpoint", Channel: "fixture-channel", MessageID: "publication-fixture", Sequence: 1, SenderURN: "msg://session/local/session-fixture", Attribution: plugin.TurnSourceAttribution{Kind: "question", LogicalAgentID: "agent-fixture"}}}
	i := ResolvedItem{ItemID: b.ItemID, SessionID: b.SessionID, TurnID: b.TurnID, AgentID: b.AgentID, Kind: b.Kind, Replyable: true, Source: &b.Source, Resolution: &Resolution{ID: "resolution-fixture", Action: "respond", ResponseText: " exact user text\n"}}
	return c, s, d, h, b, i
}

func TestPreparationAcceptanceAndDeliveryAreSeparate(t *testing.T) {
	c, s, d, h, b, i := fixture(t)
	ctx := context.Background()
	r, err := c.Advance(ctx, b, i, Choice{})
	if err != nil || r.Receipt == nil || r.Acknowledged || h.acks != 0 || d.sends[0].Interrupt || r.Prepared.Body != i.Resolution.ResponseText {
		t.Fatalf("acceptance must not ack: err=%v ack=%d", err, h.acks)
	}
	// Recreate the controller against the saved bytes, as after a restart.
	c, err = New(s, d, h, "msg://service/local/fixture-consumer", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	d.state, d.reason = tether.ReplyDelivered, tether.ReplyReasonTurnFailed
	h.fail = true
	r, err = c.Advance(ctx, b, i, Choice{})
	if !errors.Is(err, ErrUnknown) || len(d.sends) != 1 || r.Delivery == nil || r.Acknowledged {
		t.Fatalf("ack refusal resent or erased delivered state: %v", err)
	}
	h.fail = false
	r, err = c.Advance(ctx, b, i, Choice{})
	if err != nil || !r.Acknowledged || len(d.sends) != 1 || d.polls != 2 || h.acks != 2 || r.Delivery.Detail != "" {
		t.Fatalf("delivered ack retry: %v", err)
	}
	_, err = c.Advance(ctx, b, i, Choice{})
	if err != nil || h.acks != 2 || len(d.sends) != 1 {
		t.Fatal("acknowledged resolution repeated an effect")
	}
}

func TestAmbiguousSendReusesExactPreparedBodyKeyAndFlag(t *testing.T) {
	c, _, d, h, b, i := fixture(t)
	i.Resolution.Interrupt = true
	d.sendError = errors.New("synthetic ambiguous acceptance")
	_, err := c.Advance(context.Background(), b, i, Choice{})
	if !errors.Is(err, ErrUnknown) || h.acks != 0 {
		t.Fatal(err)
	}
	d.sendError = nil
	_, err = c.Advance(context.Background(), b, i, Choice{})
	if err != nil || len(d.sends) != 2 || !reflect.DeepEqual(d.sends[0], d.sends[1]) || !d.sends[1].Interrupt {
		t.Fatalf("ambiguous retry changed preparation: %v", err)
	}
	i.Resolution.ResponseText = "changed answer"
	_, err = c.Advance(context.Background(), b, i, Choice{})
	if !errors.Is(err, ErrConflict) || len(d.sends) != 2 {
		t.Fatal("changed saved body was sent")
	}
}

func TestTerminalFailurePreservedAndOnlyExplicitRetryGetsNewKey(t *testing.T) {
	c, s, d, h, b, i := fixture(t)
	ctx := context.Background()
	d.state = tether.ReplyUndeliverable
	d.reason = tether.ReplyReasonInterruptUnconfirmed
	r, err := c.Advance(ctx, b, i, Choice{})
	if err != nil || !r.TerminalFailure() || h.acks != 0 {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		_, err = c.Advance(ctx, b, i, Choice{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(d.sends) != 1 || d.polls != 1 {
		t.Fatal("Await repeated terminal delivery")
	}
	_, err = c.Retry(ctx, b.ItemID, r.Version+1, "new-user-click", false)
	if !errors.Is(err, ErrConflict) || len(d.sends) != 1 {
		t.Fatal("stale retry had effects")
	}
	d.state = tether.ReplyDelivered
	next, err := c.Retry(ctx, b.ItemID, r.Version, "new-user-click", false)
	if err != nil || !next.Acknowledged || len(d.sends) != 2 || d.sends[0].Key == d.sends[1].Key || h.acks != 1 {
		t.Fatalf("explicit new retry: %v", err)
	}
	old, err := s.get(key(r.Prepared))
	if err != nil || !old.TerminalFailure() || old.Acknowledged {
		t.Fatal("old failure overwritten")
	}
	_, err = c.Retry(ctx, b.ItemID, r.Version, "new-user-click", false)
	if err != nil || len(d.sends) != 2 {
		t.Fatal("same user click resent")
	}
}

func TestUnknownDeliveryAndRefusalNeverAcknowledgeOrRepeatedlySend(t *testing.T) {
	for _, state := range []tether.ReplyState{tether.ReplyPending, tether.ReplyDelivering, "future_state"} {
		t.Run(string(state), func(t *testing.T) {
			c, _, d, h, b, i := fixture(t)
			d.state = state
			_, err := c.Advance(context.Background(), b, i, Choice{})
			if state == "future_state" && !errors.Is(err, ErrUnknown) {
				t.Fatal(err)
			}
			_, _ = c.Advance(context.Background(), b, i, Choice{})
			if len(d.sends) != 1 || h.acks != 0 {
				t.Fatal("unknown/pending became delivery")
			}
		})
	}
	for _, code := range []string{tether.CodeInterruptUnsupported, tether.CodeTurnNotYetStarted, tether.CodeReplyTargetNotSession, "not_implemented"} {
		t.Run(code, func(t *testing.T) {
			c, _, d, h, b, i := fixture(t)
			status := 409
			if code == "not_implemented" {
				status = 501
			}
			d.sendError = &tether.APIError{Code: code, StatusCode: status, Message: "private fixture", Body: "private fixture"}
			r, err := c.Advance(context.Background(), b, i, Choice{})
			if err != nil || r.FailureCode != code {
				t.Fatalf("typed refusal not retained: %v", err)
			}
			_, _ = c.Advance(context.Background(), b, i, Choice{})
			if len(d.sends) != 1 || h.acks != 0 || d.polls != 0 {
				t.Fatal("refusal repeated send/ack")
			}
		})
	}
}

func TestCapabilityAndShutdownGuardsPreservePending(t *testing.T) {
	c, _, d, h, b, i := fixture(t)
	i.Resolution.Interrupt = true
	d.interrupt = false
	_, err := c.Advance(context.Background(), b, i, Choice{})
	if !errors.Is(err, ErrUnsupported) || len(d.sends) != 0 || h.acks != 0 {
		t.Fatal("missing interrupt capability had effects")
	}
	d.interrupt = true
	_, err = c.Advance(context.Background(), b, i, Choice{})
	if err != nil || len(d.sends) != 0 {
		t.Fatal("capability refusal auto-reopened")
	}
	c, s, d, h, b, i := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	_, err = c.Advance(ctx, b, i, Choice{})
	saved, loadErr := s.Latest(context.Background(), b.ItemID)
	if !errors.Is(err, context.Canceled) || loadErr != nil || saved.Receipt != nil || saved.TerminalFailure() || h.acks != 0 {
		t.Fatal("shutdown erased pending or claimed ordinary failure")
	}
	d.cancel = nil
	_, err = c.Advance(context.Background(), b, i, Choice{})
	if err != nil || len(d.sends) != 2 || !reflect.DeepEqual(d.sends[0], d.sends[1]) {
		t.Fatal("resume changed cancelled preparation")
	}
}

func TestOrdinaryOrMismatchedItemRefusedBeforePreparation(t *testing.T) {
	for _, change := range []func(*Binding, *ResolvedItem){
		func(b *Binding, _ *ResolvedItem) { b.Source.Origin = "publication" }, func(_ *Binding, i *ResolvedItem) { i.ItemID = "another-item" },
		func(_ *Binding, i *ResolvedItem) { i.SessionID = "another-session" }, func(_ *Binding, i *ResolvedItem) { i.Resolution = nil },
		func(_ *Binding, i *ResolvedItem) { i.Replyable = false },
	} {
		c, s, d, h, b, i := fixture(t)
		change(&b, &i)
		_, err := c.Advance(context.Background(), b, i, Choice{})
		if !errors.Is(err, ErrRefused) || len(s.rows) != 0 || len(d.sends) != 0 || h.acks != 0 {
			t.Fatal("unowned/publication effect")
		}
	}
}

func TestControllerWaitIsContextBoundAndRetryDoesNotReenter(t *testing.T) {
	c, s, d, _, binding, item := fixture(t)
	if err := c.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Advance(ctx, binding, item, Choice{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.Retry(ctx, binding.ItemID, 1, "action", false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(s.rows) != 0 || len(d.sends) != 0 {
		t.Fatal("waiting request performed effects")
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer waitCancel()
	if _, err := c.Advance(waitCtx, binding, item, Choice{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting admission: %v", err)
	}
	c.unlock()
	d.state = tether.ReplyUndeliverable
	previous, err := c.Advance(context.Background(), binding, item, Choice{})
	if err != nil {
		t.Fatal(err)
	}
	d.state = tether.ReplyDelivered
	retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
	defer retryCancel()
	if _, err = c.Retry(retryCtx, binding.ItemID, previous.Version, "explicit", false); err != nil {
		t.Fatalf("retry reentered lock: %v", err)
	}
}

func TestWorkerResumesSavedExplicitRetryRatherThanTerminalPredecessor(t *testing.T) {
	c, s, d, h, binding, item := fixture(t)
	d.state = tether.ReplyUndeliverable
	first, err := c.Advance(context.Background(), binding, item, Choice{})
	if err != nil {
		t.Fatal(err)
	}
	d.state = tether.ReplyQueued
	queued, err := c.Retry(context.Background(), binding.ItemID, first.Version, "user-action", false)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Prepared.Key == first.Prepared.Key || len(d.sends) != 2 || h.acks != 0 {
		t.Fatal("retry not distinct")
	}
	d.state = tether.ReplyDelivered
	completed, err := c.Advance(context.Background(), binding, item, Choice{})
	if err != nil || !completed.Acknowledged || completed.Prepared.ActionID != "user-action" || len(d.sends) != 2 || h.acks != 1 {
		t.Fatalf("worker retry resume %+v %v", completed, err)
	}
	old, err := s.get(key(first.Prepared))
	if err != nil || !old.TerminalFailure() || old.Acknowledged {
		t.Fatal("predecessor lost")
	}
	if _, err = c.Retry(context.Background(), binding.ItemID, first.Version+1, "user-action", false); !errors.Is(err, ErrConflict) {
		t.Fatal("same action changed predecessor version")
	}
}
