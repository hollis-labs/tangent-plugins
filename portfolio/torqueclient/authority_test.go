package torqueclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrustedTargetEvidenceAndRequestLocalProof(t *testing.T) {
	var wrong atomic.Bool
	cfg := fixtureConfig(func(r *http.Request) (*http.Response, error) {
		row := fixtureTask(taskID)
		row["title"] = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		return response(200, fixturePage([]any{row}, 50, false, nil)), nil
	})
	cfg.Verify = func(ctx context.Context, proof any, r Request) (Grant, error) {
		if r.Endpoint != "https://torque.invalid/api/v1" || r.Host != "127.0.0.1:8990" || r.Integration != "fixture" {
			wrong.Store(true)
			return Grant{}, Denied
		}
		g := syntheticGrant(proof.(string))
		g.CredentialRef = proof.(string)
		return g, nil
	}
	cfg.Resolve = func(ctx context.Context, ref, epoch string) (string, error) { return "fixture-" + ref, nil }
	c := newClient(t, cfg)
	var wg sync.WaitGroup
	for _, p := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				v, err := c.Read(t.Context(), p, "torque_tasks", object{})
				if err != nil {
					t.Error(err)
					return
				}
				if v.(object)["items"].([]any)[0].(object)["title"] != "fixture-"+p {
					t.Error("cross-caller result")
				}
			}
		}()
	}
	wg.Wait()
	if wrong.Load() {
		t.Fatal("trusted target absent from admission evidence")
	}
}

func TestCredentialCallbackCancellationAndResourceEvidenceDetached(t *testing.T) {
	var calls atomic.Int32
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(200, fixturePage([]any{fixtureTask(taskID)}, 50, false, nil)), nil
	})
	cfg.GETTimeout = 10 * time.Millisecond
	cfg.Resolve = func(ctx context.Context, ref, epoch string) (string, error) { <-ctx.Done(); return "synthetic", nil }
	c := newClient(t, cfg)
	_, err := c.Read(t.Context(), "one", "torque_tasks", object{})
	assertFailure(t, err, Denied)
	if calls.Load() != 0 {
		t.Fatal("expired resolver allowed send")
	}
	cfg = fixtureConfig(cfg.Transport.(fakeTransport))
	base := cfg.Verify
	cfg.Verify = func(ctx context.Context, proof any, r Request) (Grant, error) {
		for i := range r.Resources {
			if len(r.Resources[i].Tags) > 0 {
				r.Resources[i].Tags[0] = "mutated"
			}
			if r.Resources[i].Priority != nil {
				*r.Resources[i].Priority = 9
			}
		}
		return base(ctx, proof, r)
	}
	c = newClient(t, cfg)
	for range 2 {
		v, err := c.Read(t.Context(), "one", "torque_tasks", object{})
		if err != nil {
			t.Fatal(err)
		}
		row := v.(object)["items"].([]any)[0].(object)
		if row["priority"] != json.Number("2") || row["tags"].([]any)[0] != "ws-fixture" {
			t.Fatal("verifier mutated DTO")
		}
	}
}

func TestMalformedActivityEnvelopeCannotProveQuiet(t *testing.T) {
	for _, edit := range []func(object){
		func(p object) { p["items"] = nil }, func(p object) { p["meta"].(object)["returned"] = 1 },
		func(p object) { p["meta"].(object)["limit"] = 50 }, func(p object) { delete(p["meta"].(object), "next_cursor") },
		func(p object) { p["meta"].(object)["has_more"] = true }, func(p object) { p["meta"].(object)["next_cursor"] = "unexpected" },
	} {
		cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
			p := commentPage([]any{}, false, nil, "desc")
			edit(p)
			return response(200, p), nil
		})
		c := newClient(t, cfg)
		a, err := c.LatestCommentActivity(t.Context(), "one", []string{taskID})
		assertFailure(t, err, Malformed)
		if a.Complete || a.LatestByID != nil {
			t.Fatal("malformed empty cohort inferred quiet")
		}
	}
	// Continuation follows has_more, not an optional total or page fullness.
	cfg := fixtureConfig(func(*http.Request) (*http.Response, error) {
		p := commentPage([]any{}, false, nil, "desc")
		p["meta"].(object)["total"] = 100
		return response(200, p), nil
	})
	c := newClient(t, cfg)
	a, err := c.LatestCommentActivity(t.Context(), "one", []string{taskID})
	if err != nil || !a.Complete {
		t.Fatal(a, err)
	}
}

func TestHostSyntaxAndMissingGrantFields(t *testing.T) {
	for _, host := range []string{"localhost:999999", "localhost:port", ":8990"} {
		cfg := fixtureConfig(nil)
		cfg.Host = host
		_, err := New(cfg)
		assertFailure(t, err, Invalid)
	}
	for _, edit := range []func(*Grant){func(g *Grant) { g.Principal = "" }, func(g *Grant) { g.Scope = "" }, func(g *Grant) { g.ScopeEpoch = "" }, func(g *Grant) { g.CredentialRef = "" }, func(g *Grant) { g.CredentialEpoch = "" }} {
		cfg := fixtureConfig(func(*http.Request) (*http.Response, error) { t.Fatal("incomplete grant sent"); return nil, nil })
		cfg.Verify = func(context.Context, any, Request) (Grant, error) {
			g := syntheticGrant("one")
			edit(&g)
			return g, nil
		}
		c := newClient(t, cfg)
		_, err := c.Read(t.Context(), "one", "torque_tasks", object{})
		assertFailure(t, err, Denied)
	}
}

func TestAmbiguousOrUnboundedJSONRefuses(t *testing.T) {
	for _, body := range []string{
		`{"items":[],"meta":{"returned":0,"limit":50,"has_more":true,"has_more":false,"next_cursor":null}}`,
		`{"items":[],"items":[],"meta":{"returned":0,"limit":50,"has_more":false,"next_cursor":null}}`,
		`{} {}`,
		strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66),
	} {
		if _, err := decode([]byte(body)); !errors.Is(err, Malformed) {
			t.Fatalf("ambiguous JSON accepted: %v", err)
		}
	}
}
