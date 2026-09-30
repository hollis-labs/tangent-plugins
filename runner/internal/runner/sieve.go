package runner

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

var (
	// Regex patterns to strip internal thoughts and spinner artifacts
	reThinkingTag = regexp.MustCompile(`(?s)<thinking>.*?</thinking>`)
	reAnsiEscapes = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
)

// ExtractedTurn represents a clean conversational turn ready for operator attention.
type ExtractedTurn struct {
	TurnID  string   `json:"turn_id"`
	Kind    string   `json:"kind"` // question, approval, checkpoint, failure, terminal
	Title   string   `json:"title"`
	Prose   string   `json:"prose"`
	Options []string `json:"options,omitempty"`
}

// StreamSieve buffers agent output, suppresses non-human chatter (CoT, tool calls),
// and extracts clear conversational turns at LiveStateIdle.
type StreamSieve struct {
	mu            sync.Mutex
	sessionID     string
	rawBuffer     strings.Builder
	proseBuffer   strings.Builder
	inThinking    bool
	inToolCall    bool
	turnSequence  int64
	lastExtracted *ExtractedTurn
}

// NewStreamSieve returns a new sieve for a session.
func NewStreamSieve(sessionID string) *StreamSieve {
	return &StreamSieve{
		sessionID: sessionID,
	}
}

// WriteChunk processes an incoming output chunk from the subprocess stream.
func (s *StreamSieve) WriteChunk(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	text := string(chunk)
	s.rawBuffer.WriteString(text)

	// Strip ANSI escape sequences
	cleaned := reAnsiEscapes.ReplaceAllString(text, "")

	// Check for NDJSON lines (e.g. Claude stream-json or Codex jsonrpc)
	lines := strings.Split(cleaned, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Try parsing as JSON event
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			if s.processJSONLine(trimmed) {
				continue
			}
		}

		// Plain text stream filtering
		s.processTextLine(line)
	}
}

func (s *StreamSieve) processJSONLine(line string) bool {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return false
	}

	// Filter tool_use / tool_call events
	typ, _ := raw["type"].(string)
	if typ == "tool_use" || typ == "tool_call" || typ == "tool_result" {
		return true
	}

	// Filter thinking blocks
	if typ == "thinking" || typ == "thought" {
		return true
	}

	// ACP (Agent Client Protocol) / JSON-RPC session updates
	if method, ok := raw["method"].(string); ok && method == "session/update" {
		if params, ok := raw["params"].(map[string]any); ok {
			if pType, _ := params["type"].(string); pType == "thought" || pType == "tool_call" {
				return true
			}
			if text, ok := params["text"].(string); ok {
				s.proseBuffer.WriteString(text)
				return true
			}
			if delta, ok := params["delta"].(map[string]any); ok {
				if txt, ok := delta["text"].(string); ok {
					s.proseBuffer.WriteString(txt)
					return true
				}
			}
		}
		return true
	}

	// Extract message / text deltas
	if delta, ok := raw["delta"].(map[string]any); ok {
		if dType, _ := delta["type"].(string); dType == "text_delta" {
			if txt, ok := delta["text"].(string); ok {
				s.proseBuffer.WriteString(txt)
				return true
			}
		}
	}

	if content, ok := raw["content"].(string); ok {
		s.proseBuffer.WriteString(content)
		return true
	}

	if res, ok := raw["result"].(map[string]any); ok {
		if content, ok := res["content"].(string); ok {
			s.proseBuffer.WriteString(content)
			return true
		}
		if text, ok := res["text"].(string); ok {
			s.proseBuffer.WriteString(text)
			return true
		}
	}

	if msg, ok := raw["message"].(map[string]any); ok {
		if content, ok := msg["content"].(string); ok {
			s.proseBuffer.WriteString(content)
			return true
		}
	}

	return false
}

func (s *StreamSieve) processTextLine(line string) {
	// Handle XML-style thinking tags across chunks
	if strings.Contains(line, "<thinking>") {
		s.inThinking = true
	}
	if s.inThinking {
		if strings.Contains(line, "</thinking>") {
			s.inThinking = false
		}
		return
	}

	// Check if line looks like raw tool invocation
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "Tool Call:") || strings.HasPrefix(trimmed, "Calling tool ") {
		return
	}

	s.proseBuffer.WriteString(line)
	s.proseBuffer.WriteString("\n")
}

// ExtractTurn synthesizes the accumulated conversational prose into an ExtractedTurn
// when the session transitions to LiveStateIdle.
func (s *StreamSieve) ExtractTurn(turnID string) *ExtractedTurn {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw := s.proseBuffer.String()
	// Reset prose buffer for next turn
	s.proseBuffer.Reset()

	// Clean out any lingering thinking blocks
	cleaned := reThinkingTag.ReplaceAllString(raw, "")
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == "" {
		return nil
	}

	s.turnSequence++
	if turnID == "" {
		turnID = strings.TrimSpace(s.sessionID)
	}

	// Heuristics for turn kind
	kind := "question"
	lower := strings.ToLower(cleaned)
	if strings.Contains(lower, "approve") || strings.Contains(lower, "confirm") || strings.Contains(lower, "permission") {
		kind = "approval"
	} else if strings.Contains(lower, "checkpoint") || strings.Contains(lower, "milestone") || strings.Contains(lower, "progress") {
		kind = "checkpoint"
	} else if strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "fatal") {
		kind = "failure"
	}

	// Derive title from first line
	firstLine := cleaned
	if idx := strings.Index(cleaned, "\n"); idx != -1 {
		firstLine = strings.TrimSpace(cleaned[:idx])
	}
	if len(firstLine) > 80 {
		firstLine = firstLine[:77] + "…"
	}
	if firstLine == "" {
		firstLine = "Agent turn awaiting response"
	}

	// Extract options if formatted as bulleted lists e.g. "1. Option A" or "- [x] Option"
	var options []string
	for _, l := range strings.Split(cleaned, "\n") {
		t := strings.TrimSpace(l)
		if len(t) > 3 && (strings.HasPrefix(t, "1. ") || strings.HasPrefix(t, "2. ") || strings.HasPrefix(t, "3. ") || strings.HasPrefix(t, "- [ ] ")) {
			options = append(options, t)
		}
	}

	turn := &ExtractedTurn{
		TurnID:  turnID,
		Kind:    kind,
		Title:   firstLine,
		Prose:   cleaned,
		Options: options,
	}
	s.lastExtracted = turn
	return turn
}

// Reset clears the buffers.
func (s *StreamSieve) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rawBuffer.Reset()
	s.proseBuffer.Reset()
	s.inThinking = false
	s.inToolCall = false
}
