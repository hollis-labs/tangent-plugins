package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// The contract with Tangent for turns is a published schema, not an import of
// host code: tangent.turns_enqueue accepts exactly
// tangentplugin.TurnsEnqueueInputSchema(), and Tangent's own tests hold that
// equal to what the tool advertises. So every turn this runner sends is
// validated against it here — with the same validator Tangent uses — and the
// fake callers refuse an invalid one the way the real tool would.
//
// Tangent's make smoke also boots this runner, installed, against the real
// tools; this is the fast half, and the one that runs in this repository.

var (
	schemaOnce     sync.Once
	enqueueSchema  *jsonschema.Schema
	enqueueCompErr error
)

func turnsEnqueueSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(tangentplugin.TurnsEnqueueInputSchema()))
		if err != nil {
			enqueueCompErr = fmt.Errorf("parse TurnsEnqueueInputSchema: %w", err)
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("turns_enqueue.schema.json", document); err != nil {
			enqueueCompErr = err
			return
		}
		enqueueSchema, enqueueCompErr = compiler.Compile("turns_enqueue.schema.json")
	})
	return enqueueSchema, enqueueCompErr
}

// validateEnqueue reports whether arguments are a turn tangent.turns_enqueue
// accepts. The arguments go through a JSON round trip first, because that is
// the form the tool receives them in.
func validateEnqueue(arguments any) error {
	schema, err := turnsEnqueueSchema()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	return schema.Validate(instance)
}

// Every turns_enqueue call a fake caller receives, in any test, is validated;
// an invalid one is refused like the real tool refuses it, and recorded here
// so TestMain fails the run even if the test that caused it did not look.
var (
	invalidMu       sync.Mutex
	invalidEnqueues []string
)

func checkEnqueue(arguments any) (tangentplugin.ToolResult, bool) {
	if err := validateEnqueue(arguments); err != nil {
		encoded, _ := json.Marshal(arguments)
		invalidMu.Lock()
		invalidEnqueues = append(invalidEnqueues, fmt.Sprintf("%v\n  payload: %s", err, encoded))
		invalidMu.Unlock()
		body, _ := json.Marshal(map[string]any{"code": "invalid_argument", "message": err.Error()})
		return tangentplugin.ToolResult{Content: body, IsError: true}, false
	}
	return tangentplugin.ToolResult{}, true
}

func TestMain(m *testing.M) {
	code := m.Run()
	invalidMu.Lock()
	defer invalidMu.Unlock()
	if len(invalidEnqueues) > 0 {
		fmt.Fprintf(os.Stderr, "the runner sent %d turn(s) tangent.turns_enqueue would refuse:\n", len(invalidEnqueues))
		for _, invalid := range invalidEnqueues {
			fmt.Fprintf(os.Stderr, "- %s\n", invalid)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// TestEnqueueRequestsMatchTheHostSchema drives the real sieve over the shapes
// of agent output that reach the inbox — every kind it classifies, options,
// thinking blocks, a first turn and a reply turn, an unlabelled agent, and
// output long enough to be truncated — and holds each turn the runner would
// send to the published schema.
func TestEnqueueRequestsMatchTheHostSchema(t *testing.T) {
	long := strings.Repeat("A long line of agent output that goes on well past any title. ", 40)
	cases := []struct {
		name, label, turnID, output string
		turnsCount                  int
	}{
		{name: "approval, first turn", label: "Claude", output: "Please confirm you want to proceed with the deployment?"},
		{name: "question", label: "Claude", output: "Which database should the migration target?"},
		{name: "checkpoint", label: "Claude", output: "Checkpoint: the schema migration is done.\nNext is the backfill."},
		{name: "failure", label: "Claude", output: "The build failed: go vet reported two errors."},
		{name: "options", label: "Claude", output: "Pick one:\n1. Roll back\n2. Retry\n3. Skip"},
		{name: "thinking stripped", label: "Claude", output: "<thinking>\nweighing it\n</thinking>\nShall I continue with step two?"},
		{name: "reply turn", label: "Claude", turnID: "tturn-sess-1-2", turnsCount: 2, output: "proceed"},
		{name: "unlabelled agent", label: "", output: "Should I open the pull request now?"},
		{name: "long output", label: "Claude", output: long + "\n" + long},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sieve := NewStreamSieve("sess-1")
			sieve.WriteChunk([]byte(tc.output + "\n"))
			extracted := sieve.ExtractTurn(tc.turnID)
			if extracted == nil {
				t.Fatalf("the sieve extracted no turn from %q", tc.output)
			}
			request := enqueueRequest("sess-1", "agent-claude", tc.label, extracted, tc.turnsCount)
			if err := validateEnqueue(request); err != nil {
				encoded, _ := json.Marshal(request)
				t.Errorf("turns_enqueue would refuse this turn: %v\n%s", err, encoded)
			}
		})
	}
}

// TestSchemaValidationRejectsAnInvalidPayload is the positive control: a check
// that passes everything proves nothing, so each of these must fail.
func TestSchemaValidationRejectsAnInvalidPayload(t *testing.T) {
	sieve := NewStreamSieve("sess-1")
	sieve.WriteChunk([]byte("Please confirm the release?\n"))
	valid := enqueueRequest("sess-1", "agent-claude", "Claude", sieve.ExtractTurn(""), 1)
	if err := validateEnqueue(valid); err != nil {
		t.Fatalf("the baseline turn is itself invalid: %v", err)
	}

	mutations := map[string]func(map[string]any) any{
		"wrapped in a request field": func(turn map[string]any) any {
			return map[string]any{"request": turn}
		},
		"no title": func(turn map[string]any) any {
			delete(turn, "title")
			return turn
		},
		"a kind the host does not know": func(turn map[string]any) any {
			turn["kind"] = "message"
			return turn
		},
		"a field the host does not accept": func(turn map[string]any) any {
			turn["priority"] = "high"
			return turn
		},
		"an empty agent id": func(turn map[string]any) any {
			turn["source"].(map[string]any)["agent_id"] = ""
			return turn
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			turn := enqueueRequest("sess-1", "agent-claude", "Claude", &ExtractedTurn{
				TurnID: "sess-1", Kind: "approval", Title: "Please confirm the release?",
				Prose: "Please confirm the release?",
			}, 1)
			if err := validateEnqueue(mutate(turn)); err == nil {
				t.Errorf("a payload with %s passed validation", name)
			}
		})
	}
}
