package tangentsink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	tether "github.com/hollis-labs/go-tether-client"
	pipeline "github.com/hollis-labs/libs/message-pipeline"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
	plugin "github.com/hollis-labs/tangent/pkg/plugin"
	"github.com/hollis-labs/tangent/pkg/plugin/hostclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPublicChannelToMCPReplaySettlesEarnedItemAndJoins(t *testing.T) {
	payload := json.RawMessage(`{"text":"Exact original\nwith Unicode 世界"}`)
	message := tether.ChannelMessage{Seq: 42, Envelope: gomsg.Envelope{ID: "real-fixture-publication", Kind: gomsg.MsgKindNotice, From: gomsg.Address{Kind: gomsg.KindAgent, Authority: "local", ID: "publisher"}, To: gomsg.Address{Kind: gomsg.KindService, Authority: "local", ID: "channel", SubID: "owner-inbox"}, Channel: "owner-inbox", ContentType: "application/json", Payload: payload}}
	requests := make(chan []byte, 2)
	var deliveries, stages atomic.Int32
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "synthetic-tangent", Version: "1.0.0"}, nil)
	mcpServer.AddTool(&mcp.Tool{Name: enqueueTool, InputSchema: json.RawMessage(plugin.TurnsEnqueueInputSchema())}, func(_ context.Context, call *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		request, err := validate(call.Params.Arguments)
		if err != nil {
			t.Error("public MCP request did not satisfy published contract", err)
			return nil, err
		}
		requests <- bytes.Clone(call.Params.Arguments)
		// The external item has committed, but the first response is ambiguous.
		// Retrying must use its saved request/key, not rerun the stage.
		if deliveries.Add(1) == 1 {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: `{"code":"synthetic_lost_receipt"}`}}}, nil
		}
		raw, err := json.Marshal(map[string]any{"contract_version": "1.1", "item_id": "same-earned-item", "agent_id": request.Source.AgentID, "kind": request.Kind})
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
	})
	mcpHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil))
	defer mcpHTTP.Close()
	t.Setenv(hostclient.URLEnv, mcpHTTP.URL)
	caller, err := hostclient.New()
	if err != nil {
		t.Fatal(err)
	}
	defer caller.Close()
	subscribed, joined := make(chan struct{}), make(chan struct{})
	tetherHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/channels/owner-inbox/messages":
			page := tether.ChannelMessagesResponse{Channel: tether.Channel{Name: "owner-inbox", Address: "msg://service/local/channel/owner-inbox"}, NextSince: 99999}
			if r.URL.Query().Get("since") == "0" {
				page.Messages = []tether.ChannelMessage{message}
			}
			if writeErr := json.NewEncoder(w).Encode(page); writeErr != nil {
				t.Error(writeErr)
			}
		case "/channels/owner-inbox/subscribe":
			if r.URL.Query().Get("since") != "42" {
				t.Error("subscription did not use committed receipt cursor")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if _, writeErr := fmt.Fprint(w, ": owned subscription\n\n"); writeErr != nil {
				t.Error(writeErr)
			}
			w.(http.Flusher).Flush()
			close(subscribed)
			<-r.Context().Done()
			close(joined)
		default:
			t.Error("unexpected Tether operation", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer tetherHTTP.Close()
	config := messaging.Config{SchemaVersion: 1, EndpointRef: "synthetic-tether", TetherAddress: tetherHTTP.URL, CallerURN: "msg://service/local/messaging", Channels: []string{"owner-inbox"}, RequestTimeoutMS: 2000, ReconnectMinMS: 5, ReconnectMaxMS: 10, HistoryLimit: 10, Stages: []messaging.StageConfig{{ID: "summarize", EndpointURL: tetherHTTP.URL, CallerID: "synthetic"}}}
	source, closeSource, err := messaging.NewSource(config, "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeSource()
	dir := filepath.Join(t.TempDir(), "private-data")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ledger, err := messaging.OpenLedger(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	stage := pipeline.StageFunc(func(context.Context, pipeline.Input) (pipeline.Result, error) {
		stages.Add(1)
		return pipeline.Result{Summaries: []pipeline.Summary{{Text: "Synthetic bounded summary"}}, Disposition: pipeline.Pass}, nil
	})
	processor, err := messaging.NewProcessor([]pipeline.StageSpec{{ID: "summarize", Version: "1", ConfigDigest: "fixture-config", InstructionDigest: "fixture-instructions", Timeout: time.Second, FailMode: pipeline.FailOpen, Stage: stage}}, ledger)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := messaging.NewConsumer(config, ledger, source, processor, New(caller))
	if err != nil {
		t.Fatal(err)
	}
	if err = consumer.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer consumer.Unload(context.Background())
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("public wire delivery did not settle", consumerHealth(consumer))
	}
	first, second := <-requests, <-requests
	if !bytes.Equal(canonical(t, first), canonical(t, second)) || deliveries.Load() != 2 || stages.Load() != 1 {
		t.Fatal("ambiguous sink regenerated stage/request", deliveries.Load(), stages.Load())
	}
	stored, err := ledger.Publication(context.Background(), messaging.Source{EndpointRef: config.EndpointRef, Channel: "owner-inbox"}, message.ID)
	if err != nil || !stored.Settled || stored.ItemID != "same-earned-item" || stored.Publication.Original != "Exact original\nwith Unicode 世界" || !bytes.Equal(canonical(t, stored.Prepared), canonical(t, first)) {
		t.Fatal("durable publication/receipt mismatch", stored, err)
	}
	stop, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = consumer.Unload(stop); err != nil {
		t.Fatal(err)
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("HTTP reader not retired")
	}
}

func consumerHealth(c *messaging.Consumer) string { _, code := c.Health(); return code }
