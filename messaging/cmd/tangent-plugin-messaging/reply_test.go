package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pipeline "github.com/hollis-labs/libs/message-pipeline"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/reply"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/replyprojection"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/tangentsink"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

type noReplyMappings struct{}

func (noReplyMappings) ReplyBindings(context.Context, string, int) ([]reply.Binding, string, error) {
	return nil, "", nil
}

func (noReplyMappings) Latest(context.Context, string) (reply.Record, error) {
	return reply.Record{}, reply.ErrNotFound
}

func TestReplyHTTPCallbackJoinsBeforeCommandClosesLedger(t *testing.T) {
	entered, joined := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/routing/capabilities" || request.URL.Query().Get("session_id") != "session-1" {
			t.Error("unexpected service call", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		close(entered)
		<-request.Context().Done()
		close(joined)
	}))
	defer server.Close()
	path, data := fixtureConfiguration(t, server.URL)
	t.Setenv(configEnv, path)
	t.Setenv("TANGENT_MCP_URL", server.URL)
	t.Setenv("TETHER_TOKEN", "")
	servedPlugin := &served{}
	if _, err := servedPlugin.Init(context.Background(), subprocess.InitParams{DataDir: data}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := servedPlugin.Unload(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p := messaging.Publication{Source: messaging.Source{EndpointRef: "synthetic-tether", Channel: "owner-inbox"}, MessageID: "publication-1",
		Sequence: 1, Origin: "routed", Kind: "question", SessionID: "session-1", TurnID: "turn-1", AgentID: "agent-1",
		SenderURN: "msg://session/local/session-1", Original: "owned synthetic body", Envelope: []byte(`{}`),
		Metadata: map[string]string{"kind": "question", "logical_agent_id": "agent-1"}}
	ctx := context.Background()
	if _, err := servedPlugin.ledger.Capture(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, err := tangentsink.New(nil).Prepare(p, pipeline.State{})
	if err != nil {
		t.Fatal(err)
	}
	if err = servedPlugin.ledger.Prepare(ctx, p.Source, p.MessageID, raw); err != nil {
		t.Fatal(err)
	}
	if err = servedPlugin.ledger.Settle(ctx, p.Source, p.MessageID, 0, messaging.SinkReceipt{ItemID: "item-1", IdempotencyKey: p.Key()}); err != nil {
		t.Fatal(err)
	}
	host := reply.ToolHost{Caller: servedPlugin.client}
	controller, err := reply.New(servedPlugin.replies, servedPlugin.dispatcher, host, servedPlugin.config.CallerURN, servedPlugin.config.RequestTimeout())
	if err != nil {
		t.Fatal(err)
	}
	worker, err := messaging.NewReplyWorker(servedPlugin.config, noReplyMappings{}, host, controller)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Load(ctx); err != nil {
		t.Fatal(err)
	}
	servedPlugin.replyWorker = worker
	servedPlugin.replyHTTP = &replyprojection.Handler{Reader: replyprojection.Reader{Mappings: servedPlugin.replies, Store: servedPlugin.replies,
		Dispatcher: servedPlugin.dispatcher, Timeout: servedPlugin.config.RequestTimeout()}, Controller: controller}
	completed := make(chan error, 1)
	go func() {
		_, callErr := servedPlugin.HTTPHandle(ctx, subprocess.HTTPRequest{Method: http.MethodGet, Path: replyprojection.DeliveryPath, Query: map[string]string{"item_id": "item-1"}})
		completed <- callErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("actual public capability read not reached")
	}
	stop, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = servedPlugin.Unload(stop); err != nil {
		t.Fatal(err)
	}
	select {
	case <-joined:
	case <-stop.Done():
		t.Fatal("HTTP transport not cancelled")
	}
	select {
	case <-completed:
	case <-stop.Done():
		t.Fatal("joined HTTP callback did not return")
	}
	ledger, err := messaging.OpenLedger(ctx, data)
	if err != nil {
		t.Fatal("command retained ledger after actual join", err)
	}
	defer ledger.Close()
	stored, err := ledger.Publication(ctx, p.Source, p.MessageID)
	if err != nil || !stored.Settled || stored.ItemID != "item-1" {
		t.Fatal("shutdown erased obligation", stored, err)
	}
}

func TestReplyManifestDeclaresGuardedRoutesAndPublicAwaitAckReach(t *testing.T) {
	var out bytes.Buffer
	if err := emitManifest(&out); err != nil {
		t.Fatal(err)
	}
	manifest, err := plugin.DecodeManifest(&out)
	if err != nil {
		t.Fatal(err)
	}
	wanted := []plugin.RouteDecl{
		{Method: http.MethodGet, Path: replyprojection.DeliveryPath, Capability: string(plugin.CapabilityView)},
		{Method: http.MethodPost, Path: replyprojection.RetryPath, Capability: string(plugin.CapabilityDraft)},
	}
	for _, want := range wanted {
		found := false
		for _, route := range manifest.Bindings.Routes {
			if route == want {
				found = true
			}
		}
		if !found {
			t.Fatal("missing guarded route", want)
		}
	}
	for _, tool := range []string{"tangent.turns_enqueue", "tangent.turn_await", "tangent.turn_ack"} {
		found := false
		for _, request := range manifest.Capabilities {
			if bytes.Contains(request.Metadata, []byte(`"`+tool+`"`)) {
				found = true
			}
		}
		if !found {
			t.Fatal("missing explicit public reach", tool)
		}
	}
}
