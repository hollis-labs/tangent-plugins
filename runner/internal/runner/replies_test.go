package runner

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	tangentplugin "github.com/hollis-labs/tangent/pkg/plugin"
)

// scriptedInbox plays Tangent's side of the three turns tools for the reply
// pump. Unlike fakeTools it answers by tool name, because the pump's behavior
// depends on what turn_await and turn_ack say back.
//
// The reply is offered only once a turn has been enqueued and until it has been
// acknowledged, which is what the real tool does: an unacknowledged reply comes
// back on every poll.
type scriptedInbox struct {
	mu sync.Mutex

	reply map[string]any // one item of turn_await's "replies", or nil for none

	failAwaits int // turn_await calls to fail with a transport error first
	failAcks   int // turn_ack calls to refuse first

	enqueued []map[string]any
	acks     []map[string]any
	awaits   int
	acked    bool
}

func (f *scriptedInbox) CallTool(_ context.Context, name string, arguments any) (tangentplugin.ToolResult, error) {
	encoded, _ := json.Marshal(arguments)
	var args map[string]any
	_ = json.Unmarshal(encoded, &args)

	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case EnqueueTurnTool:
		f.enqueued = append(f.enqueued, args)
		return tangentplugin.ToolResult{Content: json.RawMessage(`{}`)}, nil

	case AwaitTurnTool:
		f.awaits++
		if f.failAwaits > 0 {
			f.failAwaits--
			return tangentplugin.ToolResult{}, context.DeadlineExceeded
		}
		replies := []any{}
		if f.reply != nil && len(f.enqueued) > 0 && !f.acked {
			replies = append(replies, f.reply)
		}
		status := "timeout"
		if len(replies) > 0 {
			status = "replies"
		}
		body, _ := json.Marshal(map[string]any{"wait_status": status, "replies": replies})
		return tangentplugin.ToolResult{Content: body}, nil

	case AckTurnTool:
		f.acks = append(f.acks, args)
		if f.failAcks > 0 {
			f.failAcks--
			return tangentplugin.ToolResult{Content: json.RawMessage(`{"code":"turns_error"}`), IsError: true}, nil
		}
		f.acked = true
		return tangentplugin.ToolResult{Content: json.RawMessage(`{"status":"acknowledged"}`)}, nil
	}
	return tangentplugin.ToolResult{}, nil
}

func (f *scriptedInbox) snapshot() (enqueued, acks []map[string]any, awaits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.enqueued...), append([]map[string]any(nil), f.acks...), f.awaits
}

func approval(text string) map[string]any {
	return map[string]any{
		"item_id": "item-1",
		"resolution": map[string]any{
			"resolution_id": "res-1", "action": "approve", "response_text": text,
		},
	}
}

// launchCat starts a session whose agent is cat: it echoes whatever the runner
// writes to its stdin, so a reply the pump delivers comes straight back out as
// the agent's next turn. That is what lets a test see delivery happen.
func launchCat(t *testing.T, inbox *scriptedInbox, sessionID string) *Engine {
	t.Helper()
	engine := NewEngine(nil)
	engine.SetToolCaller(inbox)
	engine.SetReplyPollInterval(10 * time.Millisecond)
	if _, err := engine.Launch(context.Background(), LaunchParams{
		SessionID: sessionID, AgentID: "agent", Prompt: "Please confirm this?", Command: "cat",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = engine.Stop(sessionID) })
	return engine
}

func eventually(t *testing.T, limit time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", limit, what)
}

func TestReplyPump_DeliversAnAnswerToTheSessionAndAcknowledgesIt(t *testing.T) {
	inbox := &scriptedInbox{reply: approval("proceed")}
	launchCat(t, inbox, "sess-pump")

	// cat echoes the delivered answer, the sieve reads it as the agent's next
	// turn, and the runner enqueues it: two enqueues means the answer arrived.
	eventually(t, 5*time.Second, "the delivered answer to come back as the agent's next turn", func() bool {
		enqueued, _, _ := inbox.snapshot()
		return len(enqueued) >= 2
	})
	enqueued, acks, _ := inbox.snapshot()

	if enqueued[1]["content"] != "proceed" {
		t.Errorf("the agent received %q, want the operator's answer %q", enqueued[1]["content"], "proceed")
	}
	if enqueued[0]["turn_id"] == enqueued[1]["turn_id"] {
		t.Errorf("the agent's reply reused turn_id %v: the answer did not advance the turn", enqueued[0]["turn_id"])
	}
	if len(acks) != 1 || acks[0]["item_id"] != "item-1" || acks[0]["reply_id"] != "res-1" {
		t.Errorf("acks = %v, want one for item-1 naming reply res-1", acks)
	}
}

func TestReplyPump_DoesNotDeliverTwiceWhenAnAcknowledgementIsRefused(t *testing.T) {
	// Until the ack lands, turn_await returns the same reply on every poll.
	inbox := &scriptedInbox{reply: approval("proceed"), failAcks: 2}
	launchCat(t, inbox, "sess-once")

	eventually(t, 5*time.Second, "the acknowledgement to succeed after refusals", func() bool {
		inbox.mu.Lock()
		defer inbox.mu.Unlock()
		return inbox.acked
	})
	// Let any wrongly repeated write echo back and be sieved into a turn.
	time.Sleep(700 * time.Millisecond)

	enqueued, acks, _ := inbox.snapshot()
	if len(acks) != 3 {
		t.Errorf("ack attempts = %d, want 3 (two refused, then accepted)", len(acks))
	}
	if len(enqueued) != 2 || enqueued[1]["content"] != "proceed" {
		var second any
		if len(enqueued) > 1 {
			second = enqueued[1]["content"]
		}
		t.Errorf("enqueued %d turns, second %q; want 2 turns and the answer delivered exactly once", len(enqueued), second)
	}
}

func TestReplyPump_KeepsPollingThroughATransientFailure(t *testing.T) {
	inbox := &scriptedInbox{reply: approval("proceed"), failAwaits: 3}
	launchCat(t, inbox, "sess-flaky")

	eventually(t, 5*time.Second, "the answer to be delivered after Tangent recovered", func() bool {
		enqueued, _, _ := inbox.snapshot()
		return len(enqueued) >= 2
	})
	if _, _, awaits := inbox.snapshot(); awaits < 4 {
		t.Errorf("turn_await called %d times, want the three failures and then a success", awaits)
	}
}

func TestReplyPump_LeavesASessionAloneWhenNothingHasBeenAnswered(t *testing.T) {
	inbox := &scriptedInbox{} // no reply
	launchCat(t, inbox, "sess-quiet")

	// The session's own first turn is enqueued once its output goes quiet; only
	// after that is there anything for the pump to be right or wrong about.
	eventually(t, 5*time.Second, "the first turn and several polls after it", func() bool {
		enqueued, _, awaits := inbox.snapshot()
		return len(enqueued) >= 1 && awaits >= 5
	})
	// Long enough for anything wrongly written to the agent to echo back out.
	time.Sleep(700 * time.Millisecond)
	enqueued, acks, _ := inbox.snapshot()
	if len(enqueued) != 1 || len(acks) != 0 {
		t.Errorf("with no answer, enqueued %d turns and %d acks; want the session's first turn only", len(enqueued), len(acks))
	}
}

func TestReplyPump_StopsPollingWhenTheSessionStops(t *testing.T) {
	inbox := &scriptedInbox{}
	engine := launchCat(t, inbox, "sess-stop")

	eventually(t, 5*time.Second, "polling to begin", func() bool {
		_, _, awaits := inbox.snapshot()
		return awaits >= 2
	})
	if err := engine.Stop("sess-stop"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Let a poll already in flight finish, then require silence.
	time.Sleep(100 * time.Millisecond)
	_, _, settled := inbox.snapshot()
	time.Sleep(300 * time.Millisecond) // thirty poll intervals
	if _, _, later := inbox.snapshot(); later != settled {
		t.Errorf("turn_await was called %d more times after the session stopped", later-settled)
	}
}

func TestReplyPump_IsNotStartedForASessionWithNoInbox(t *testing.T) {
	engine := NewEngine(nil) // no tool caller: nothing to poll
	engine.SetReplyPollInterval(time.Millisecond)
	if _, err := engine.Launch(context.Background(), LaunchParams{
		SessionID: "sess-none", AgentID: "agent", Prompt: "hello", Command: "cat",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = engine.Stop("sess-none") }()
	health, err := engine.Health(context.Background(), "sess-none")
	if err != nil || !health.Alive {
		t.Fatalf("a session with no inbox should still run: %+v, %v", health, err)
	}
}

func TestReplyText(t *testing.T) {
	for name, tc := range map[string]struct {
		in   awaitedResolution
		want string
	}{
		"typed reply wins":              {awaitedResolution{Action: "respond", ResponseText: "use main", SelectedOption: "rel"}, "use main"},
		"option when nothing was typed": {awaitedResolution{Action: "respond", SelectedOption: "rel"}, "rel"},
		"the action when only a button": {awaitedResolution{Action: "approve"}, "approve"},
		"a rejection is spoken":         {awaitedResolution{Action: "reject"}, "reject"},
		"note rides along":              {awaitedResolution{Action: "approve", ResponseText: "go", Note: "skip the migration"}, "go: skip the migration"},
		"note after a bare action":      {awaitedResolution{Action: "approve", Note: "carefully"}, "approve: carefully"},
		"whitespace is trimmed":         {awaitedResolution{Action: "respond", ResponseText: "  hi  "}, "hi"},
		"nothing but a note":            {awaitedResolution{Note: "just this"}, "just this"},
	} {
		if got := replyText(&tc.in); got != tc.want {
			t.Errorf("%s: replyText = %q, want %q", name, got, tc.want)
		}
	}
}

func TestNextReplyWait(t *testing.T) {
	base := 10 * time.Millisecond
	if got := nextReplyWait(80*time.Millisecond, base, true); got != base {
		t.Errorf("healthy poll waits %v, want the base %v", got, base)
	}
	if got := nextReplyWait(base, base, false); got != 2*base {
		t.Errorf("first failure waits %v, want %v", got, 2*base)
	}
	if got := nextReplyWait(2*base, base, false); got != 4*base {
		t.Errorf("second failure waits %v, want %v", got, 4*base)
	}
	if got := nextReplyWait(maxReplyBackoff, base, false); got != maxReplyBackoff {
		t.Errorf("backoff exceeded its cap: %v", got)
	}
	if got := nextReplyWait(0, base, false); got != base {
		t.Errorf("backoff from zero waits %v, want at least the base %v", got, base)
	}
}
