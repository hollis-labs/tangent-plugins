package messaging

import "testing"

func TestOrdinaryPublicationPreservesSuppliedLabelsWithoutRoutedAuthority(t *testing.T) {
	message := routedMessage()
	delete(message.Metadata, "kind")
	p, err := Admit(testSource(), message)
	if err != nil || p.Origin != "publication" || p.Kind != "checkpoint" || p.SessionID != "session-1" || p.TurnID != "turn-1" {
		t.Fatal("ordinary labels reclassified as routed", p, err)
	}
	delete(message.Metadata, "session_id")
	delete(message.Metadata, "turn_id")
	p, err = Admit(testSource(), message)
	if err != nil || p.SessionID != "" || p.TurnID != "" {
		t.Fatal("runtime labels fabricated", p, err)
	}
}
