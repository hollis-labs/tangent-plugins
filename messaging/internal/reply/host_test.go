package reply

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tangent/pkg/plugin"
	"testing"
)

type toolFake struct {
	name   string
	args   map[string]any
	result plugin.ToolResult
}

func (f *toolFake) CallTool(_ context.Context, name string, args any) (plugin.ToolResult, error) {
	f.name, f.args = name, args.(map[string]any)
	return f.result, nil
}
func TestToolHostResolutionAckAndAwait(t *testing.T) {
	f := &toolFake{result: plugin.ToolResult{Content: json.RawMessage(`{"session_id":"s","wait_status":"replies","replies":[{"item_id":"i","session_id":"s","resolution":{"resolution_id":"r","action":"respond","interrupt":true}}]}`)}}
	h := ToolHost{Caller: f}
	items, err := h.Await(context.Background(), "s")
	if err != nil || len(items) != 1 || !items[0].Resolution.Interrupt || f.args["wait_ms"] != 0 {
		t.Fatalf("await %v %v", items, err)
	}
	f.result.Content = json.RawMessage(`{"item_id":"i","status":"acknowledged"}`)
	if err = h.Ack(context.Background(), "i", "r"); err != nil || f.name != "tangent.turn_ack" || f.args["reply_id"] != "r" {
		t.Fatalf("ack %v", err)
	}
	f.result.Content = json.RawMessage(`{"item_id":"other","status":"acknowledged"}`)
	if !errors.Is(h.Ack(context.Background(), "i", "r"), ErrRefused) {
		t.Fatal("foreign ack accepted")
	}
	f.result.Content = json.RawMessage(`{"session_id":"other","wait_status":"timeout"}`)
	if _, err = h.Await(context.Background(), "s"); !errors.Is(err, ErrRefused) {
		t.Fatal("foreign session accepted")
	}
	f.result.IsError = true
	if _, err = h.Await(context.Background(), "s"); !errors.Is(err, ErrRefused) {
		t.Fatal("tool refusal accepted")
	}
}
