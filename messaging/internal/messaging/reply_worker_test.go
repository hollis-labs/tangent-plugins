package messaging

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tangent-plugins/messaging/internal/reply"
)

type workerMappings struct {
	read   func(context.Context, string, int) ([]reply.Binding, string, error)
	latest func(context.Context, string) (reply.Record, error)
}

func (m workerMappings) Latest(ctx context.Context, itemID string) (reply.Record, error) {
	if m.latest == nil {
		return reply.Record{}, reply.ErrNotFound
	}
	return m.latest(ctx, itemID)
}

func (m workerMappings) ReplyBindings(ctx context.Context, after string, limit int) ([]reply.Binding, string, error) {
	return m.read(ctx, after, limit)
}

type workerHost struct {
	await func(context.Context, string) ([]reply.ResolvedItem, error)
}

func (h workerHost) Await(ctx context.Context, session string) ([]reply.ResolvedItem, error) {
	return h.await(ctx, session)
}

func (workerHost) Ack(context.Context, string, string) error { return nil }

type workerAdvancer struct {
	advance func(context.Context, reply.Binding, reply.ResolvedItem, reply.Choice) (reply.Record, error)
	resume  func(context.Context, string) (reply.Record, error)
}

func (a workerAdvancer) Resume(ctx context.Context, itemID string) (reply.Record, error) {
	if a.resume == nil {
		return reply.Record{}, errors.New("owned unexpected resume")
	}
	return a.resume(ctx, itemID)
}

func (a workerAdvancer) Advance(ctx context.Context, binding reply.Binding, item reply.ResolvedItem, choice reply.Choice) (reply.Record, error) {
	return a.advance(ctx, binding, item, choice)
}

func TestReplyWorkerUsesOnlySettledMappingAndNoSyntheticRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	advanced := make(chan reply.Choice, 1)
	binding := reply.Binding{ItemID: "owned-item", SessionID: "source-session"}
	worker, err := NewReplyWorker(testConfig(), workerMappings{read: func(context.Context, string, int) ([]reply.Binding, string, error) {
		return []reply.Binding{binding}, "owned-item", nil
	}}, workerHost{await: func(_ context.Context, session string) ([]reply.ResolvedItem, error) {
		if session != binding.SessionID {
			t.Error("invented runtime session", session)
		}
		return []reply.ResolvedItem{{ItemID: "foreign-plugin-item"}, {ItemID: binding.ItemID}}, nil
	}}, workerAdvancer{advance: func(_ context.Context, got reply.Binding, item reply.ResolvedItem, choice reply.Choice) (reply.Record, error) {
		if got != binding || item.ItemID != binding.ItemID {
			t.Error("foreign mapping advanced", got, item)
		}
		select {
		case advanced <- choice:
		default:
		}
		return reply.Record{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Load(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case choice := <-advanced:
		if choice.ActionID != "" || choice.Previous != nil || choice.OverrideInterrupt != nil {
			t.Fatal("polling minted user retry", choice)
		}
	case <-ctx.Done():
		t.Fatal("owned item not advanced")
	}
	if err = worker.Unload(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReplyWorkerFiniteLoadIsNotLifetimeAndUnloadJoinsAwait(t *testing.T) {
	loadCtx, finishLoad := context.WithCancel(context.Background())
	entered := make(chan context.Context, 1)
	exited := make(chan struct{})
	binding := reply.Binding{ItemID: "owned", SessionID: "session"}
	worker, err := NewReplyWorker(testConfig(), workerMappings{read: func(context.Context, string, int) ([]reply.Binding, string, error) {
		return []reply.Binding{binding}, "owned", nil
	}}, workerHost{await: func(ctx context.Context, _ string) ([]reply.ResolvedItem, error) {
		entered <- ctx
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	}}, workerAdvancer{advance: func(context.Context, reply.Binding, reply.ResolvedItem, reply.Choice) (reply.Record, error) {
		return reply.Record{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Load(loadCtx); err != nil {
		t.Fatal(err)
	}
	owned := <-entered
	finishLoad()
	if owned.Err() != nil {
		t.Fatal("finite Load owned worker", owned.Err())
	}
	join, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = worker.Unload(join); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("unload returned before await exited")
	}
}

func TestReplyWorkerUnloadRetainsCustodyUntilHTTPCallbackJoins(t *testing.T) {
	worker, err := NewReplyWorker(testConfig(), workerMappings{read: func(context.Context, string, int) ([]reply.Binding, string, error) {
		return nil, "", nil
	}}, workerHost{await: func(context.Context, string) ([]reply.ResolvedItem, error) {
		return nil, nil
	}}, workerAdvancer{advance: func(context.Context, reply.Binding, reply.ResolvedItem, reply.Choice) (reply.Record, error) {
		return reply.Record{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered, cancelled, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		exited <- worker.Do(context.Background(), func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release // Deliberately held cleanup; cancellation is not a join.
			return ctx.Err()
		})
	}()
	<-entered
	join, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = worker.Unload(join); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("held callback falsely joined", err)
	}
	<-cancelled
	select {
	case <-worker.done:
		t.Fatal("done closed with active callback")
	default:
	}
	called := false
	if err = worker.Do(context.Background(), func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatal("new HTTP call admitted during shutdown")
	}
	close(release)
	if err = <-exited; !errors.Is(err, context.Canceled) {
		t.Fatal("owned callback lost cancellation", err)
	}
	if err = worker.Unload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReplyWorkerPendingPageDoesNotStarveSubsequentBindings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var mu sync.Mutex
	var scanned []string
	later := make(chan struct{}, 1)
	worker, err := NewReplyWorker(testConfig(), workerMappings{read: func(_ context.Context, after string, _ int) ([]reply.Binding, string, error) {
		mu.Lock()
		scanned = append(scanned, after)
		mu.Unlock()
		if after == "first" {
			select {
			case later <- struct{}{}:
			default:
			}
			return nil, "", nil
		}
		return []reply.Binding{{ItemID: "pending", SessionID: "session"}}, "first", nil
	}}, workerHost{await: func(context.Context, string) ([]reply.ResolvedItem, error) {
		return []reply.ResolvedItem{{ItemID: "pending"}}, nil
	}}, workerAdvancer{advance: func(context.Context, reply.Binding, reply.ResolvedItem, reply.Choice) (reply.Record, error) {
		return reply.Record{}, reply.ErrUnknown
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Load(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-later:
	case <-ctx.Done():
		t.Fatal("unknown attempt starved later mappings")
	}
	if err = worker.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(scanned) < 2 || scanned[0] != "" || scanned[1] != "first" {
		t.Fatal("scan cursor changed publication semantics", scanned)
	}
}

func TestReplyWorkerResumesDurableDeliveredRecordWithoutAwait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resumed := make(chan string, 1)
	binding := reply.Binding{ItemID: "delivered-item", SessionID: "source-session"}
	worker, err := NewReplyWorker(testConfig(), workerMappings{
		read: func(context.Context, string, int) ([]reply.Binding, string, error) {
			return []reply.Binding{binding}, binding.ItemID, nil
		},
		latest: func(context.Context, string) (reply.Record, error) {
			return reply.Record{Version: 3, Prepared: reply.Prepared{Binding: binding}}, nil
		},
	}, workerHost{await: func(context.Context, string) ([]reply.ResolvedItem, error) {
		t.Error("recovery depended on host row after acknowledged resolution")
		return nil, nil
	}}, workerAdvancer{
		advance: func(context.Context, reply.Binding, reply.ResolvedItem, reply.Choice) (reply.Record, error) {
			t.Error("invented fresh action during durable recovery")
			return reply.Record{}, nil
		},
		resume: func(_ context.Context, id string) (reply.Record, error) {
			select {
			case resumed <- id:
			default:
			}
			return reply.Record{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Load(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-resumed:
		if id != binding.ItemID {
			t.Fatal("wrong saved item", id)
		}
	case <-ctx.Done():
		t.Fatal("saved ack gap never resumed")
	}
	if err = worker.Unload(ctx); err != nil {
		t.Fatal(err)
	}
}
