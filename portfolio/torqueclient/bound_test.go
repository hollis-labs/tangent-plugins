package torqueclient

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundNilGuardRefusesBeforeChildOrCache(t *testing.T) {
	var child atomic.Int32
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) { t.Fatal("nil parent sent"); return nil, nil })
	cfg.Verify = func(context.Context, any, Request) (Grant, error) { child.Add(1); return syntheticGrant("one"), nil }
	c := newClient(t, cfg)
	b := c.Bind("one", nil)
	_, err := b.Read(t.Context(), "torque_tasks", object{})
	assertFailure(t, err, Denied)
	_, err = b.LatestCommentActivity(t.Context(), nil)
	assertFailure(t, err, Denied)
	if child.Load() != 0 {
		t.Fatal("nil parent invoked child verifier")
	}
}

func TestOrdinaryParentChangesDuringIndependentChildWait(t *testing.T) {
	for _, mode := range []string{"revoke", "principal", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var allowed atomic.Bool
			allowed.Store(true)
			var principal atomic.Value
			principal.Store("one")
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var requests atomic.Int32
			cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return response(200, fixturePage([]any{}, 50, false, nil)), nil
			})
			cfg.Verify = func(ctx context.Context, proof any, r Request) (Grant, error) {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return Grant{}, Denied
				}
				return syntheticGrant("one"), nil
			}
			c := newClient(t, cfg)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b := c.Bind("one", func(ctx context.Context) (string, error) {
				if !allowed.Load() {
					return "", Denied
				}
				return principal.Load().(string), nil
			})
			result := make(chan error, 1)
			go func() { _, err := b.Read(ctx, "torque_tasks", object{}); result <- err }()
			<-entered
			switch mode {
			case "revoke":
				allowed.Store(false)
			case "principal":
				principal.Store("two")
			case "cancel":
				cancel()
			}
			close(release)
			assertFailure(t, <-result, Denied)
			if requests.Load() != 0 {
				t.Fatal("ordinary-only parent drift sent request")
			}
		})
	}
}

func TestOrdinaryParentRecheckedAfterBlockedCacheInsertion(t *testing.T) {
	for _, mode := range []string{"revoke", "principal", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var allowed atomic.Bool
			allowed.Store(true)
			var principal atomic.Value
			principal.Store("one")
			entered, release, afterParent := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var disclosures atomic.Int32
			var once sync.Once
			cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
				return response(200, fixturePage([]any{}, 50, false, nil)), nil
			})
			cfg.Verify = func(ctx context.Context, proof any, r Request) (Grant, error) {
				if r.Phase == "disclosure" && disclosures.Add(1) == 2 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return Grant{}, Denied
					}
				}
				return syntheticGrant("one"), nil
			}
			c := newClient(t, cfg)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b := c.Bind("one", func(ctx context.Context) (string, error) {
				p, ok := principal.Load().(string), allowed.Load()
				if disclosures.Load() == 2 {
					once.Do(func() { close(afterParent) })
				}
				if !ok {
					return "", Denied
				}
				return p, nil
			})
			type outcome struct {
				value any
				err   error
			}
			result := make(chan outcome, 1)
			go func() { v, err := b.Read(ctx, "torque_tasks", object{}); result <- outcome{v, err} }()
			<-entered
			c.mu.Lock()
			close(release)
			<-afterParent
			// Only bounded cache storage holds this mutex. Keep it unavailable after
			// the pre-insertion guard has passed, then change ordinary authority.
			time.Sleep(5 * time.Millisecond)
			select {
			case <-result:
				c.mu.Unlock()
				t.Fatal("insertion did not wait")
			default:
			}
			switch mode {
			case "revoke":
				allowed.Store(false)
			case "principal":
				principal.Store("two")
			case "cancel":
				cancel()
			}
			c.mu.Unlock()
			got := <-result
			assertFailure(t, got.err, Denied)
			if got.value != nil || len(c.cache) != 0 {
				t.Fatal("post-wait refusal disclosed/retained result")
			}
		})
	}
}

func TestBoundCacheHitRechecksOrdinaryParent(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	var calls atomic.Int32
	c := newClient(t, fixtureConfig(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(200, fixturePage([]any{}, 50, false, nil)), nil
	}))
	b := c.Bind("one", func(context.Context) (string, error) {
		if !allowed.Load() {
			return "", Denied
		}
		return "one", nil
	})
	_, err := b.Read(t.Context(), "torque_tasks", object{})
	if err != nil {
		t.Fatal(err)
	}
	allowed.Store(false)
	_, err = b.Read(t.Context(), "torque_tasks", object{})
	assertFailure(t, err, Denied)
	if calls.Load() != 1 {
		t.Fatal("parent-refused cache hit went upstream")
	}
}
