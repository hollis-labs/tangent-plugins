// Package tangentsink prepares immutable public Tangent 1.1 requests and
// validates earned receipts. It has no reply, acknowledgement or source API.
package tangentsink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	pipeline "github.com/hollis-labs/libs/message-pipeline"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/messaging"
	plugin "github.com/hollis-labs/tangent/pkg/plugin"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

const enqueueTool = "tangent.turns_enqueue"

var ErrContract = errors.New("messaging: sink_contract_refused")
var (
	schemaOnce sync.Once
	schema     *jsonschema.Schema
	schemaErr  error
)

type Sink struct{ caller plugin.ToolCaller }

func New(caller plugin.ToolCaller) *Sink { return &Sink{caller: caller} }

func (s *Sink) Prepare(p messaging.Publication, state pipeline.State) ([]byte, error) {
	if state.Validate() != nil || p.Purged || p.Sequence <= 0 || p.Sequence > 9007199254740991 {
		return nil, ErrContract
	}
	request := plugin.AgentTurnRequest{
		ContractVersion: plugin.AgentTurnContractVersion, TurnID: p.TurnID, SessionID: p.SessionID, IdempotencyKey: p.Key(), Kind: p.Kind,
		Source: plugin.AgentTurnSource{AgentID: p.AgentID, ApplicationID: "messaging"}, Title: "Tether " + p.Kind + " publication", Content: p.Original,
		SourceMessage: &plugin.TurnSourceMessage{SchemaVersion: 1, Origin: p.Origin, EndpointRef: p.Source.EndpointRef, Channel: p.Source.Channel, MessageID: p.MessageID, Sequence: p.Sequence, SenderURN: p.SenderURN, OutputID: p.OutputID,
			Attribution: plugin.TurnSourceAttribution{Kind: p.Metadata["kind"], LogicalAgentID: p.Metadata["logical_agent_id"], ProjectID: p.Metadata["project_id"], WorkstreamID: p.Metadata["workstream_id"], LaunchID: p.Metadata["launch_id"], LaunchDisplayName: p.Metadata["launch_display_name"], Runtime: p.Metadata["runtime"], StopReason: p.Metadata["stop_reason"], Confidence: p.Metadata["confidence"]}},
	}
	for _, annotation := range state.Annotations {
		request.Annotations = append(request.Annotations, plugin.TurnAnnotation{SchemaVersion: annotation.SchemaVersion, StageID: annotation.StageID, StageVersion: annotation.StageVersion, Kind: annotation.Kind, Summary: plugin.TurnSummary{Text: annotation.Summary.Text}})
		if request.Summary != "" && request.Summary != annotation.Summary.Text {
			return nil, ErrContract
		}
		request.Summary = annotation.Summary.Text
	}
	for _, trace := range state.Traces {
		request.StageTrace = append(request.StageTrace, plugin.TurnStageTrace{StageID: trace.StageID, StageVersion: trace.StageVersion, Outcome: string(trace.Outcome), DurationMS: trace.DurationMS, FailureCode: string(trace.FailureCode)})
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, ErrContract
	}
	if _, err = validate(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func validate(raw []byte) (plugin.AgentTurnRequest, error) {
	var request plugin.AgentTurnRequest
	if len(raw) > 2*1024*1024 || !messaging.StrictJSON(raw) {
		return request, ErrContract
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, ErrContract
	}
	schemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		schemaErr = compiler.AddResource("turns.json", bytes.NewReader(plugin.TurnsEnqueueInputSchema()))
		if schemaErr == nil {
			schema, schemaErr = compiler.Compile("turns.json")
		}
	})
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || schemaErr != nil || schema.Validate(value) != nil {
		return request, ErrContract
	}
	if request.ContractVersion != "1.1" || request.SourceMessage == nil {
		return request, ErrContract
	}
	metadata := map[string]any{}
	for _, field := range []string{"annotations", "stage_trace"} {
		if fieldValue, ok := value[field]; ok {
			metadata[field] = fieldValue
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil || len(encoded) > plugin.MaxTurnStageMetadataBytes {
		return request, ErrContract
	}
	seenAnnotations, seenTraces := make(map[string]bool), make(map[string]bool)
	for _, annotation := range request.Annotations {
		if seenAnnotations[annotation.StageID] || annotation.Summary.Text != request.Summary {
			return request, ErrContract
		}
		seenAnnotations[annotation.StageID] = true
	}
	for _, trace := range request.StageTrace {
		if seenTraces[trace.StageID] || (trace.Outcome == "timed_out") != (trace.FailureCode == "stage_timeout") {
			return request, ErrContract
		}
		seenTraces[trace.StageID] = true
	}
	source := request.SourceMessage
	agent := source.Attribution.LogicalAgentID
	if agent == "" {
		agent = source.SenderURN
	}
	if request.Source.AgentID != agent {
		return request, ErrContract
	}
	if source.Origin == "routed" {
		kind := source.Attribution.Kind
		if kind == "final" {
			kind = "terminal"
		}
		if source.SenderURN != "msg://session/local/"+request.SessionID || kind != request.Kind {
			return request, ErrContract
		}
	} else if source.Origin != "publication" || request.Kind != "checkpoint" || source.Attribution.Kind != "" || len(request.Options) != 0 {
		return request, ErrContract
	}
	return request, nil
}

func (s *Sink) Deliver(ctx context.Context, raw []byte) (messaging.SinkReceipt, error) {
	request, err := validate(raw)
	if err != nil || s.caller == nil {
		return messaging.SinkReceipt{}, ErrContract
	}
	var arguments map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&arguments); err != nil {
		return messaging.SinkReceipt{}, ErrContract
	}
	result, err := s.caller.CallTool(ctx, enqueueTool, arguments)
	if err != nil {
		return messaging.SinkReceipt{}, errors.New("messaging: sink_delivery_pending")
	}
	if result.IsError || !messaging.StrictJSON(result.Content) {
		return messaging.SinkReceipt{}, ErrContract
	}
	var receipt struct {
		ContractVersion string `json:"contract_version"`
		ItemID          string `json:"item_id"`
		TurnID          string `json:"turn_id"`
		SessionID       string `json:"session_id"`
		AgentID         string `json:"agent_id"`
		Kind            string `json:"kind"`
	}
	if err = json.Unmarshal(result.Content, &receipt); err != nil || !itemIdentity(receipt.ItemID) || receipt.ContractVersion != request.ContractVersion || receipt.TurnID != request.TurnID || receipt.SessionID != request.SessionID || receipt.AgentID != request.Source.AgentID || receipt.Kind != request.Kind {
		return messaging.SinkReceipt{}, ErrContract
	}
	return messaging.SinkReceipt{ItemID: receipt.ItemID, IdempotencyKey: request.IdempotencyKey}, nil
}

func itemIdentity(id string) bool {
	if id == "" || len(id) > 256 || !utf8.ValidString(id) || strings.TrimSpace(id) != id {
		return false
	}
	for _, r := range id {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

var _ messaging.Sink = (*Sink)(nil)
