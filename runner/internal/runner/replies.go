package runner

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

const (
	// DefaultReplyPollInterval is how often an embedded session asks Tangent
	// whether the operator has answered one of its turns.
	DefaultReplyPollInterval = time.Second

	// maxReplyBackoff caps how far repeated failures slow the poll. A session
	// keeps trying at this pace rather than giving up: the failure is Tangent
	// being briefly away far more often than it is permanent.
	maxReplyBackoff = 30 * time.Second
)

// awaitedReplies is the part of tangent.turn_await's result the runner reads.
//
// It is declared here rather than imported from internal/turns because the
// runner is its own program and knows Tangent by its wire contract alone, the
// way it knows tangent.turns_enqueue. Pulling the domain package in would put
// the durable substrate into a plugin binary to read a handful of fields.
type awaitedReplies struct {
	Replies []awaitedReply `json:"replies"`
}

type awaitedReply struct {
	ItemID     string             `json:"item_id"`
	Resolution *awaitedResolution `json:"resolution"`
}

// awaitedResolution is the operator's recorded answer.
type awaitedResolution struct {
	ResolutionID   string `json:"resolution_id"`
	Action         string `json:"action"`
	ResponseText   string `json:"response_text"`
	SelectedOption string `json:"selected_option"`
	Note           string `json:"note"`
}

// replyText is what an operator's answer says to an agent reading its stdin.
//
// The agent sees one line of text, so an answer that is only a button press has
// to be spoken: the typed reply if there is one, otherwise the option the
// operator picked, otherwise the action itself ("approve", "reject"). An
// operator's note rides along, because on an approval it is often the only
// guidance they wrote.
func replyText(r *awaitedResolution) string {
	text := strings.TrimSpace(r.ResponseText)
	if text == "" {
		text = strings.TrimSpace(r.SelectedOption)
	}
	if text == "" {
		text = strings.TrimSpace(r.Action)
	}
	if note := strings.TrimSpace(r.Note); note != "" {
		if text == "" {
			return note
		}
		return text + ": " + note
	}
	return text
}

// nextReplyWait is how long to wait before the next poll: the base interval
// while things work, doubling toward the cap while they do not.
func nextReplyWait(current, base time.Duration, healthy bool) time.Duration {
	if healthy {
		return base
	}
	next := current * 2
	if next < base {
		next = base
	}
	if next > maxReplyBackoff {
		next = maxReplyBackoff
	}
	return next
}

func (e *Engine) currentToolCaller() tangentplugin.ToolCaller {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.toolCaller
}

func (e *Engine) replyPollInterval() time.Duration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.replyPoll
}

// pumpReplies carries the operator's answers into one embedded session until it
// ends.
//
// It is the return half of the loop onSessionIdle starts: a turn goes to the
// inbox, the operator answers there, and the answer has to reach the process
// that asked. Each tick it asks tangent.turn_await for this session's
// unacknowledged replies, writes each one to the agent, and acknowledges it.
//
// It polls with a zero wait and does not hold a call open. A held call would be
// a request pinned to the host for as long as a person takes to answer, and the
// poll interval is the only latency it costs.
//
// parent is the context the agent process itself runs under, so the pump can
// outlive neither the process nor a Stop; s.stopped covers the third way a
// session ends, the process exiting on its own.
func (e *Engine) pumpReplies(parent context.Context, s *ActiveSession) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-s.stopped:
			cancel()
		case <-ctx.Done():
		}
	}()

	base := e.replyPollInterval()
	wait := base
	// Resolution ids already written to the agent. Acknowledgement can fail and
	// be retried, and until it succeeds turn_await keeps returning the reply;
	// this is what stops a retried acknowledgement from delivering the answer a
	// second time.
	delivered := make(map[string]struct{})

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		healthy := e.deliverReplies(ctx, s, delivered)
		wait = nextReplyWait(wait, base, healthy)
		timer.Reset(wait)
	}
}

// deliverReplies runs one poll and reports whether it went cleanly, so the
// caller knows whether to back off.
func (e *Engine) deliverReplies(ctx context.Context, s *ActiveSession, delivered map[string]struct{}) bool {
	caller := e.currentToolCaller()
	if caller == nil {
		return true
	}

	result, err := caller.CallTool(ctx, AwaitTurnTool, map[string]any{"session_id": s.ID, "wait_ms": 0})
	if ctx.Err() != nil {
		return true // the session ended mid-call; there is nothing to report
	}
	if err != nil {
		e.logger.Warn("runner: could not ask Tangent for replies", "session_id", s.ID, "err", err)
		return false
	}
	if result.IsError {
		e.logger.Warn("runner: Tangent refused the request for replies",
			"session_id", s.ID, "refusal", string(result.Content))
		return false
	}
	var awaited awaitedReplies
	if err := json.Unmarshal(result.Content, &awaited); err != nil {
		e.logger.Warn("runner: could not read Tangent's replies", "session_id", s.ID, "err", err)
		return false
	}

	healthy := true
	for _, reply := range awaited.Replies {
		if reply.Resolution == nil || reply.ItemID == "" {
			continue
		}
		id := reply.Resolution.ResolutionID
		if _, done := delivered[id]; !done {
			_, err := e.SendTurn(ctx, SendTurnParams{
				SessionID:      s.ID,
				ResponseText:   replyText(reply.Resolution),
				Action:         reply.Resolution.Action,
				SelectedOption: reply.Resolution.SelectedOption,
			})
			if err != nil {
				// Stop here rather than skip ahead: replies are delivered in
				// the order the operator gave them, and a later one must not
				// overtake one that has not been delivered.
				if ctx.Err() == nil {
					e.logger.Warn("runner: could not deliver an operator reply to the session",
						"session_id", s.ID, "item_id", reply.ItemID, "err", err)
				}
				return false
			}
			delivered[id] = struct{}{}
		}
		if !e.acknowledgeReply(ctx, caller, s, reply.ItemID, id) {
			healthy = false
		}
	}
	return healthy
}

// acknowledgeReply tells Tangent a reply reached its agent. A failure is not
// fatal and not final: the reply comes back from turn_await on the next poll,
// is recognized as already delivered, and is acknowledged again.
func (e *Engine) acknowledgeReply(
	ctx context.Context,
	caller tangentplugin.ToolCaller,
	s *ActiveSession,
	itemID, replyID string,
) bool {
	result, err := caller.CallTool(ctx, AckTurnTool, map[string]any{"item_id": itemID, "reply_id": replyID})
	if ctx.Err() != nil {
		return true
	}
	if err != nil {
		e.logger.Warn("runner: could not acknowledge a delivered reply",
			"session_id", s.ID, "item_id", itemID, "err", err)
		return false
	}
	if result.IsError {
		e.logger.Warn("runner: Tangent refused to acknowledge a delivered reply",
			"session_id", s.ID, "item_id", itemID, "refusal", string(result.Content))
		return false
	}
	return true
}
