package messaging

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	tether "github.com/hollis-labs/go-tether-client"
)

func testSource() Source { return Source{EndpointRef: "test-tether", Channel: "owner-inbox"} }

func routedMessage() tether.ChannelMessage {
	return tether.ChannelMessage{Seq: 42, Envelope: gomsg.Envelope{
		ID: "publication-1", Kind: gomsg.MsgKindNotice, Channel: "owner-inbox",
		From:     gomsg.Address{Kind: gomsg.KindSession, Authority: "local", ID: "session-1"},
		To:       gomsg.Address{Kind: gomsg.KindService, Authority: "local", ID: "channel", SubID: "owner-inbox"},
		ThreadID: "session-1", ContentType: "application/json", Payload: json.RawMessage(`{"text":"  original\n世界  "}`),
		Metadata: map[string]string{"session_id": "session-1", "turn_id": "turn-1", "kind": "final", "output_id": "output-1", "logical_agent_id": "logical-1", "runtime": "codex", "confidence": "exact"},
	}}
}

func TestAdmissionPublicationIdentityAndOriginal(t *testing.T) {
	message := routedMessage()
	got, err := Admit(testSource(), message)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "terminal" || got.Origin != "routed" || got.Original != "  original\n世界  " || got.TurnID != "turn-1" || got.OutputID != "output-1" || got.AgentID != "logical-1" {
		t.Fatalf("attribution: %+v", got)
	}
	message.Metadata["runtime"] = "mutated"
	message.Payload[0] = 'x'
	if got.Metadata["runtime"] != "codex" || !json.Valid(got.Envelope) {
		t.Fatal("admitted input aliases caller state")
	}
	other := got
	other.Source.Channel = "another"
	if other.Key() == got.Key() {
		t.Fatal("different channel collapsed")
	}
	other = got
	other.MessageID = "publication-2"
	if other.Key() == got.Key() {
		t.Fatal("different output publication in same turn collapsed")
	}
}

func TestAdmissionOrdinaryIsNonreplyableWithoutFabricatedBindings(t *testing.T) {
	for _, payload := range []string{`{"text":"hello"}`, `{"body":"hello"}`, `"hello"`} {
		message := routedMessage()
		message.Metadata = nil
		message.ThreadID = "ordinary-thread"
		message.Payload = json.RawMessage(payload)
		got, err := Admit(testSource(), message)
		if err != nil {
			t.Fatal(err)
		}
		if got.Origin != "publication" || got.Kind != "checkpoint" || got.SessionID != "" || got.TurnID != "" || got.AgentIdentityKind != "sender_urn" || got.AgentID != message.From.URN() {
			t.Fatalf("invented binding: %+v", got)
		}
	}
}

func TestAdmissionRefusesMalformedWithoutLeakingBodies(t *testing.T) {
	for name, change := range map[string]func(*tether.ChannelMessage){
		"invalid sequence":    func(m *tether.ChannelMessage) { m.Seq = 0 },
		"unsafe sequence":     func(m *tether.ChannelMessage) { m.Seq = 9007199254740992 },
		"private destination": func(m *tether.ChannelMessage) { m.To.ID = "mailbox" },
		"foreign channel":     func(m *tether.ChannelMessage) { m.Channel = "other" },
		"unknown kind":        func(m *tether.ChannelMessage) { m.Metadata["kind"] = "terminal" },
		"unknown confidence":  func(m *tether.ChannelMessage) { m.Metadata["confidence"] = "invented" },
		"oversized project":   func(m *tether.ChannelMessage) { m.Metadata["project_id"] = strings.Repeat("p", 257) },
		"missing turn":        func(m *tether.ChannelMessage) { delete(m.Metadata, "turn_id") },
		"sender mismatch":     func(m *tether.ChannelMessage) { m.Metadata["session_id"] = "foreign" },
		"thread mismatch":     func(m *tether.ChannelMessage) { m.ThreadID = "foreign" },
		"empty original":      func(m *tether.ChannelMessage) { m.Payload = json.RawMessage(`{"text":""}`) },
		"ambiguous body":      func(m *tether.ChannelMessage) { m.Payload = json.RawMessage(`{"text":"one","body":"another"}`) },
		"unsupported body":    func(m *tether.ChannelMessage) { m.Payload = json.RawMessage(`{"commands":["do not run"]}`) },
		"oversized original": func(m *tether.ChannelMessage) {
			m.Payload, _ = json.Marshal(map[string]string{"text": strings.Repeat("x", 65537)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := routedMessage()
			change(&m)
			if _, err := Admit(testSource(), m); err == nil || strings.Contains(err.Error(), "original") {
				t.Fatal("invalid or leaking admission", err)
			}
		})
	}
}

func TestAdmissionPurgeHasNoOriginal(t *testing.T) {
	m := routedMessage()
	m.Purged = true
	now := time.Now()
	m.PurgedAt = &now
	m.Payload = nil
	m.Metadata = nil
	got, err := Admit(testSource(), m)
	if err != nil || !got.Purged || got.Original != "" || got.SessionID != "" {
		t.Fatalf("purge: %+v %v", got, err)
	}
	m.Purged = false
	if _, err := Admit(testSource(), m); err == nil {
		t.Fatal("empty body treated as purge")
	}
}
