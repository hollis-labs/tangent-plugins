package tangentsink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	pipeline "github.com/hollis-labs/libs/message-pipeline"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
	plugin "github.com/hollis-labs/tangent/pkg/plugin"
)

type callerFunc func(context.Context, string, any) (plugin.ToolResult, error)

func (f callerFunc) CallTool(ctx context.Context, name string, arguments any) (plugin.ToolResult, error) {
	return f(ctx, name, arguments)
}

func publication() messaging.Publication {
	return messaging.Publication{Source: messaging.Source{EndpointRef: "test-source", Channel: "owner-inbox"}, MessageID: "physical-message", Sequence: 42, SenderURN: "msg://agent/local/publisher", AgentID: "msg://agent/local/publisher", Original: "Original\nbody — intact", Origin: "publication", Kind: "checkpoint", Metadata: map[string]string{"project_id": "actual-project"}}
}

func TestPreparedPublicationPreservesBodyAndPhysicalIdentity(t *testing.T) {
	for _, labels := range []bool{false, true} {
		p := publication()
		if labels {
			p.SessionID, p.TurnID, p.OutputID = "supplied-session", "supplied-turn", "separate-output"
		}
		sink := New(nil)
		state := pipeline.State{Annotations: []pipeline.Annotation{{SchemaVersion: 1, StageID: "summarize", StageVersion: "1", Kind: "summary", Summary: pipeline.Summary{Text: "Bounded summary"}}}, Traces: []pipeline.Trace{{StageID: "summarize", StageVersion: "1", Outcome: pipeline.Passed, DurationMS: 2}}}
		raw, err := sink.Prepare(p, state)
		if err != nil {
			t.Fatal(err)
		}
		var request plugin.AgentTurnRequest
		if err = json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		if request.Content != p.Original || request.IdempotencyKey != p.Key() || request.TurnID != p.TurnID || request.SessionID != p.SessionID || request.SourceMessage.OutputID != p.OutputID || request.SourceMessage.MessageID != p.MessageID || request.SourceMessage.Sequence != p.Sequence || request.Kind != "checkpoint" || len(request.Options) != 0 {
			t.Fatal("publication was altered or given reply authority", request)
		}
		if request.Summary != state.Annotations[0].Summary.Text || request.SourceMessage.Attribution.ProjectID != "actual-project" {
			t.Fatal("projection lost supplied metadata")
		}
		p.Original = "different body"
		if bytes.Contains(raw, []byte(p.Original)) {
			t.Fatal("prepared bytes share mutable input")
		}
	}
}

func TestRoutedFinalUsesGenuineRuntimeIdentity(t *testing.T) {
	p := publication()
	p.Origin, p.Kind, p.SessionID, p.TurnID = "routed", "terminal", "actual-session", "actual-turn"
	p.SenderURN, p.AgentID = "msg://session/local/actual-session", "actual-agent"
	p.Metadata["kind"], p.Metadata["logical_agent_id"] = "final", "actual-agent"
	raw, err := New(nil).Prepare(p, pipeline.State{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := validate(raw)
	if err != nil || request.TurnID != "actual-turn" || request.Kind != "terminal" {
		t.Fatal("real completed turn refused", err)
	}
	p.SenderURN = "msg://session/local/foreign"
	if _, err = New(nil).Prepare(p, pipeline.State{}); !errors.Is(err, ErrContract) {
		t.Fatal("foreign routed sender admitted", err)
	}
}

func TestInvalidPreparedRequestsNeverReachToolCaller(t *testing.T) {
	calls := 0
	sink := New(callerFunc(func(context.Context, string, any) (plugin.ToolResult, error) {
		calls++
		return plugin.ToolResult{}, errors.New("unexpected tool invocation")
	}))
	raw, err := sink.Prepare(publication(), pipeline.State{})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*plugin.AgentTurnRequest){
		"unknown origin":         func(r *plugin.AgentTurnRequest) { r.SourceMessage.Origin = "guessed" },
		"unknown classification": func(r *plugin.AgentTurnRequest) { r.SourceMessage.Attribution.Kind = "mystery" },
		"publication reply":      func(r *plugin.AgentTurnRequest) { r.Options = []plugin.AgentTurnOption{{Label: "reply", Value: "yes"}} },
		"agent mismatch":         func(r *plugin.AgentTurnRequest) { r.Source.AgentID = "invented" },
		"unsafe sequence":        func(r *plugin.AgentTurnRequest) { r.SourceMessage.Sequence = 9007199254740992 },
		"summary drift": func(r *plugin.AgentTurnRequest) {
			r.Summary = "one"
			r.Annotations = []plugin.TurnAnnotation{{SchemaVersion: 1, StageID: "s", StageVersion: "1", Kind: "summary", Summary: plugin.TurnSummary{Text: "other"}}}
		},
		"timeout drift": func(r *plugin.AgentTurnRequest) {
			r.StageTrace = []plugin.TurnStageTrace{{StageID: "s", StageVersion: "1", Outcome: "timed_out", FailureCode: "stage_error"}}
		},
		"metadata too large": func(r *plugin.AgentTurnRequest) {
			for i := range 16 {
				r.StageTrace = append(r.StageTrace, plugin.TurnStageTrace{StageID: strings.Repeat("s", 127) + string(rune('a'+i)), StageVersion: strings.Repeat("v", 128), Outcome: "failed", FailureCode: "stage_error"})
			}
			for _, id := range []string{"s", "t"} {
				r.Annotations = append(r.Annotations, plugin.TurnAnnotation{SchemaVersion: 1, StageID: id, StageVersion: "1", Kind: "summary", Summary: plugin.TurnSummary{Text: strings.Repeat("€", 600)}})
			}
			r.Summary = strings.Repeat("€", 600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var request plugin.AgentTurnRequest
			if decodeErr := json.Unmarshal(raw, &request); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			mutate(&request)
			if name == "metadata too large" {
				metadata, encodeErr := json.Marshal(map[string]any{"annotations": request.Annotations, "stage_trace": request.StageTrace})
				if encodeErr != nil || len(metadata) <= plugin.MaxTurnStageMetadataBytes {
					t.Fatal("adverse fixture did not exceed metadata bound", len(metadata), encodeErr)
				}
			}
			changed, marshalErr := json.Marshal(request)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if _, deliverErr := sink.Deliver(context.Background(), changed); !errors.Is(deliverErr, ErrContract) || calls != 0 {
				t.Fatal("invalid request accepted", deliverErr)
			}
		})
	}
	for _, malformed := range [][]byte{[]byte(`{"contract_version":"1.1","contract_version":"1.0"}`), []byte(`{"content":"\ud800"}`)} {
		if _, err = sink.Deliver(context.Background(), malformed); !errors.Is(err, ErrContract) || calls != 0 {
			t.Fatal("malformed saved bytes accepted", err)
		}
	}
}

func TestSinkReceiptRequiresActualMatchingSuccessfulHandle(t *testing.T) {
	p := publication()
	raw, err := New(nil).Prepare(p, pipeline.State{})
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]any{"contract_version": "1.1", "item_id": "earned-item", "agent_id": p.AgentID, "kind": "checkpoint", "state": "queued", "revision": 1}
	for _, name := range []string{"valid", "wrong agent", "wrong kind", "fake runtime", "control id", "error result", "ambiguous transport", "duplicate receipt"} {
		t.Run(name, func(t *testing.T) {
			body := make(map[string]any)
			for key, value := range valid {
				body[key] = value
			}
			var transportErr error
			isError := false
			switch name {
			case "wrong agent":
				body["agent_id"] = "foreign"
			case "wrong kind":
				body["kind"] = "terminal"
			case "fake runtime":
				body["turn_id"] = "fabricated"
			case "control id":
				body["item_id"] = "item\n"
			case "error result":
				isError = true
			case "ambiguous transport":
				transportErr = errors.New("untrusted diagnostic must stay closed")
			}
			calls := 0
			sink := New(callerFunc(func(_ context.Context, method string, arguments any) (plugin.ToolResult, error) {
				calls++
				encoded, err := json.Marshal(arguments)
				if err != nil || method != enqueueTool || !bytes.Equal(encoded, canonical(t, raw)) {
					t.Fatal("saved request changed before tool call", err)
				}
				content, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				if name == "duplicate receipt" {
					content = []byte(`{"item_id":"earned","item_id":"other"}`)
				}
				return plugin.ToolResult{Content: content, IsError: isError}, transportErr
			}))
			receipt, err := sink.Deliver(context.Background(), raw)
			if calls != 1 {
				t.Fatal("unexpected adapter retry")
			}
			if name == "valid" {
				if err != nil || receipt.ItemID != "earned-item" || receipt.IdempotencyKey != p.Key() {
					t.Fatal("earned receipt lost", receipt, err)
				}
			} else if err == nil || receipt.ItemID != "" || strings.Contains(err.Error(), "untrusted") {
				t.Fatal("unearned or leaking receipt", receipt, err)
			}
		})
	}
}

func canonical(t *testing.T, raw []byte) []byte {
	t.Helper()
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
