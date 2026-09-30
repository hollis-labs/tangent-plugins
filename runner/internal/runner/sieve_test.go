package runner

import (
	"testing"
)

func TestStreamSieve_PlainProse(t *testing.T) {
	sieve := NewStreamSieve("sess-test-1")
	sieve.WriteChunk([]byte("Hello Chrispian.\nWhat would you like to build today?\n"))

	turn := sieve.ExtractTurn("turn-1")
	if turn == nil {
		t.Fatal("expected extracted turn, got nil")
	}

	if turn.TurnID != "turn-1" {
		t.Errorf("expected turn_id 'turn-1', got %q", turn.TurnID)
	}
	if turn.Kind != "question" {
		t.Errorf("expected kind 'question', got %q", turn.Kind)
	}
	if turn.Title != "Hello Chrispian." {
		t.Errorf("expected title 'Hello Chrispian.', got %q", turn.Title)
	}
	if turn.Prose != "Hello Chrispian.\nWhat would you like to build today?" {
		t.Errorf("unexpected prose: %q", turn.Prose)
	}
}

func TestStreamSieve_StripThinkingXML(t *testing.T) {
	sieve := NewStreamSieve("sess-test-2")
	input := "<thinking>\nInternal reasoning:\nLet's consider alternative designs...\n</thinking>\nWould you like me to proceed with the migration?"
	sieve.WriteChunk([]byte(input))

	turn := sieve.ExtractTurn("turn-2")
	if turn == nil {
		t.Fatal("expected extracted turn, got nil")
	}

	if turn.Prose != "Would you like me to proceed with the migration?" {
		t.Errorf("thinking was not stripped; got %q", turn.Prose)
	}
}

func TestStreamSieve_StripThinkingSplitAcrossChunks(t *testing.T) {
	sieve := NewStreamSieve("sess-test-3")
	sieve.WriteChunk([]byte("Starting task...\n<thinking>\nFirst chunk of thought\n"))
	sieve.WriteChunk([]byte("Second chunk of thought\n</thinking>\nShould we confirm the database name?"))

	turn := sieve.ExtractTurn("turn-3")
	if turn == nil {
		t.Fatal("expected extracted turn, got nil")
	}

	expected := "Starting task...\nShould we confirm the database name?"
	if turn.Prose != expected {
		t.Errorf("expected %q, got %q", expected, turn.Prose)
	}
}

func TestStreamSieve_StripToolCallJSONAndPlain(t *testing.T) {
	sieve := NewStreamSieve("sess-test-4")

	// NDJSON tool use
	sieve.WriteChunk([]byte("{\"type\":\"tool_use\",\"name\":\"execute_command\",\"input\":{\"cmd\":\"ls\"}}\n"))
	// Thinking JSON block
	sieve.WriteChunk([]byte("{\"type\":\"thinking\",\"text\":\"reviewing output\"}\n"))
	// Plain text tool noise
	sieve.WriteChunk([]byte("Tool Call: execute_command\n"))
	sieve.WriteChunk([]byte("Calling tool grep...\n"))
	// Human conversational turn
	sieve.WriteChunk([]byte("Found 3 potential candidates. Please approve the diff before proceeding.\n"))

	turn := sieve.ExtractTurn("turn-4")
	if turn == nil {
		t.Fatal("expected extracted turn, got nil")
	}

	if turn.Kind != "approval" {
		t.Errorf("expected kind 'approval', got %q", turn.Kind)
	}
	expected := "Found 3 potential candidates. Please approve the diff before proceeding."
	if turn.Prose != expected {
		t.Errorf("expected %q, got %q", expected, turn.Prose)
	}
}

func TestStreamSieve_ACPJsonRpcUpdates(t *testing.T) {
	sieve := NewStreamSieve("sess-test-5")

	// ACP thought update
	sieve.WriteChunk([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"type\":\"thought\",\"text\":\"analyzing\"}}\n"))
	// ACP text delta update
	sieve.WriteChunk([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"delta\":{\"type\":\"text_delta\",\"text\":\"Encountered fatal error during migration.\"}}}\n"))

	turn := sieve.ExtractTurn("turn-5")
	if turn == nil {
		t.Fatal("expected extracted turn, got nil")
	}

	if turn.Kind != "failure" {
		t.Errorf("expected kind 'failure', got %q", turn.Kind)
	}
	if turn.Prose != "Encountered fatal error during migration." {
		t.Errorf("unexpected prose: %q", turn.Prose)
	}
}

func TestStreamSieve_ExtractOptions(t *testing.T) {
	sieve := NewStreamSieve("sess-test-6")
	input := "Which environment should we target?\n1. Staging\n2. Production\n3. Development\n"
	sieve.WriteChunk([]byte(input))

	turn := sieve.ExtractTurn("turn-6")
	if turn == nil {
		t.Fatal("expected extracted turn, got nil")
	}

	if len(turn.Options) != 3 {
		t.Fatalf("expected 3 options, got %d: %v", len(turn.Options), turn.Options)
	}
	if turn.Options[0] != "1. Staging" || turn.Options[1] != "2. Production" || turn.Options[2] != "3. Development" {
		t.Errorf("unexpected options: %v", turn.Options)
	}
}

func TestStreamSieve_EmptyYieldsNil(t *testing.T) {
	sieve := NewStreamSieve("sess-test-7")
	sieve.WriteChunk([]byte("<thinking>Thinking only</thinking>\n"))
	sieve.WriteChunk([]byte("{\"type\":\"tool_use\"}\n"))

	turn := sieve.ExtractTurn("turn-7")
	if turn != nil {
		t.Errorf("expected nil turn for empty prose, got: %+v", turn)
	}
}
