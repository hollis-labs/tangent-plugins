package messaging

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hollis-labs/tangent-plugins/messaging/internal/reply"
)

type ReplyMappings interface {
	ReplyBindings(context.Context, string, int) ([]reply.Binding, string, error)
	Latest(context.Context, string) (reply.Record, error)
}

type ReplyAdvancer interface {
	Advance(context.Context, reply.Binding, reply.ResolvedItem, reply.Choice) (reply.Record, error)
	Resume(context.Context, string) (reply.Record, error)
}

// ReplyWorker owns polling and HTTP callback custody for one loaded
// incarnation. Both paths use the same controller's cancellation-aware gate.
// Unknown sends remain prepared, queue acceptance never acknowledges an item,
// and no user retry/action is manufactured by this worker.
type ReplyWorker struct {
	mappings   ReplyMappings
	host       reply.Host
	controller ReplyAdvancer
	timeout    time.Duration
	interval   time.Duration
	mu         sync.Mutex
	lifetime   context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	active     sync.WaitGroup
	stopping   bool
	status     string
}

func NewReplyWorker(config Config, mappings ReplyMappings, host reply.Host, controller ReplyAdvancer) (*ReplyWorker, error) {
	if config.Validate() != nil || mappings == nil || host == nil || controller == nil {
		return nil, reply.ErrRefused
	}
	return &ReplyWorker{mappings: mappings, host: host, controller: controller, timeout: config.RequestTimeout(),
		interval: time.Duration(config.ReconnectMinMS) * time.Millisecond, status: "not_loaded"}, nil
}

func (w *ReplyWorker) Load(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return errors.New("messaging: reply_already_loaded")
	}
	w.lifetime, w.cancel = context.WithCancel(context.Background())
	w.done, w.stopping, w.status = make(chan struct{}), false, "watching"
	w.active.Add(1)
	go func() { defer w.active.Done(); w.run(w.lifetime) }()
	// The poll worker keeps the count nonzero until stopping closes admission.
	// HTTP additions therefore cannot race a zero-count Wait.
	go func(done chan struct{}) { w.active.Wait(); close(done) }(w.done)
	return nil
}

// Do tracks a host-owned HTTP callback under both its request context and this
// loaded lifetime. Shutdown cancellation preserves request values/identity.
func (w *ReplyWorker) Do(ctx context.Context, call func(context.Context) error) error {
	if call == nil {
		return reply.ErrRefused
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	if w.cancel == nil || w.stopping {
		w.mu.Unlock()
		return errors.New("messaging: reply_not_loaded")
	}
	lifetime := w.lifetime
	w.active.Add(1)
	w.mu.Unlock()
	defer w.active.Done()
	owned, cancel := context.WithTimeout(ctx, w.timeout)
	stop := context.AfterFunc(lifetime, cancel)
	defer stop()
	defer cancel()
	if lifetime.Err() != nil {
		cancel()
	}
	if err := owned.Err(); err != nil {
		return err
	}
	return call(owned)
}

func (w *ReplyWorker) Unload(ctx context.Context) error {
	w.mu.Lock()
	if w.cancel == nil {
		w.mu.Unlock()
		return nil
	}
	w.stopping = true
	w.cancel()
	done := w.done
	w.mu.Unlock()
	select {
	case <-done:
		w.mu.Lock()
		w.cancel, w.status = nil, "not_loaded"
		w.mu.Unlock()
		return nil
	case <-ctx.Done():
		// Retain ledger/clients until the actual worker and callbacks join.
		return ctx.Err()
	}
}

func (w *ReplyWorker) Health() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancel != nil && !w.stopping && w.status == "watching", w.status
}

func (w *ReplyWorker) run(ctx context.Context) {
	after := ""
	for ctx.Err() == nil {
		next, err := w.poll(ctx, after)
		w.mu.Lock()
		// Pending attempts remain durable and are revisited next cycle; they
		// cannot starve mappings on subsequent finite pages.
		after = next
		if err != nil {
			w.status = "reply_pending"
		} else {
			w.status = "watching"
		}
		w.mu.Unlock()
		timer := time.NewTimer(w.interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (w *ReplyWorker) poll(ctx context.Context, after string) (string, error) {
	lookup, cancel := context.WithTimeout(ctx, w.timeout)
	bindings, next, err := w.mappings.ReplyBindings(lookup, after, 64)
	cancel()
	if err != nil {
		return after, err
	}
	bySession := make(map[string]map[string]reply.Binding)
	var failures []error
	for _, binding := range bindings {
		request, finish := context.WithTimeout(ctx, w.timeout)
		_, savedErr := w.mappings.Latest(request, binding.ItemID)
		finish()
		if savedErr == nil {
			// Host Ack can commit before the local acknowledged write. Await
			// then omits this item; the saved user resolution must still drive
			// receipt polling/ack-only recovery, without inventing a new action.
			request, finish = context.WithTimeout(ctx, w.timeout)
			_, resumeErr := w.controller.Resume(request, binding.ItemID)
			finish()
			if resumeErr != nil {
				failures = append(failures, resumeErr)
			}
			continue
		}
		if !errors.Is(savedErr, reply.ErrNotFound) {
			failures = append(failures, savedErr)
			continue
		}
		if bySession[binding.SessionID] == nil {
			bySession[binding.SessionID] = make(map[string]reply.Binding)
		}
		bySession[binding.SessionID][binding.ItemID] = binding
	}
	for session, owned := range bySession {
		if err = ctx.Err(); err != nil {
			return next, err
		}
		request, finish := context.WithTimeout(ctx, w.timeout)
		items, awaitErr := w.host.Await(request, session)
		finish()
		if awaitErr != nil {
			failures = append(failures, awaitErr)
			continue
		}
		for _, item := range items {
			binding, found := owned[item.ItemID]
			if !found {
				continue // Other plugins' items are never reply targets.
			}
			request, finish = context.WithTimeout(ctx, w.timeout)
			_, advanceErr := w.controller.Advance(request, binding, item, reply.Choice{})
			finish()
			if advanceErr != nil {
				failures = append(failures, advanceErr)
			}
		}
	}
	return next, errors.Join(failures...)
}
