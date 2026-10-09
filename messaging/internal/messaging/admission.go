package messaging

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"unicode/utf8"

	gomsg "github.com/hollis-labs/go-messaging"
	tether "github.com/hollis-labs/go-tether-client"
)

// Source separates channel publication identity from runtime turn identity.
type Source struct {
	EndpointRef string `json:"endpoint_ref"`
	Channel     string `json:"channel"`
}

func (s Source) validate() error {
	if !identifier(s.EndpointRef, 256) || !channelName.MatchString(s.Channel) {
		return errors.New("messaging: invalid source identity")
	}
	return nil
}

func (s Source) key() string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// Publication snapshots the received envelope and preserves its exact original
// text. A purge marker has no original and must not reach a summarizer or sink.
type Publication struct {
	Source            Source            `json:"source"`
	MessageID         string            `json:"message_id"`
	Sequence          int64             `json:"sequence"`
	SenderURN         string            `json:"sender_urn"`
	Origin            string            `json:"origin"`
	Kind              string            `json:"kind"`
	Original          string            `json:"original"`
	SessionID         string            `json:"session_id,omitempty"`
	TurnID            string            `json:"turn_id,omitempty"`
	OutputID          string            `json:"output_id,omitempty"`
	AgentID           string            `json:"agent_id,omitempty"`
	AgentIdentityKind string            `json:"agent_identity_kind,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Envelope          json.RawMessage   `json:"envelope"`
	Purged            bool              `json:"purged"`
}

// Key binds the full publication tuple, never its text or runtime turn ID.
func (p Publication) Key() string {
	raw, _ := json.Marshal([3]string{p.Source.EndpointRef, p.Source.Channel, p.MessageID})
	digest := sha256.Sum256(raw)
	return "tether-publication:v1:" + hex.EncodeToString(digest[:])
}

// Admit accepts actual channel publications only. Refusals are closed codes
// without copied bodies, credentials or upstream error messages.
func Admit(source Source, message tether.ChannelMessage) (Publication, error) {
	if err := source.validate(); err != nil {
		return Publication{}, err
	}
	if !identifier(message.ID, 256) || message.Seq <= 0 || string(message.Channel) != source.Channel || message.To.URN() != "msg://service/local/channel/"+source.Channel {
		return Publication{}, errors.New("messaging: publication_identity_refused")
	}
	if _, err := gomsg.ParseURN(message.From.URN()); err != nil {
		return Publication{}, errors.New("messaging: sender_identity_refused")
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return Publication{}, errors.New("messaging: envelope_refused")
	}
	p := Publication{Source: source, MessageID: message.ID, Sequence: message.Seq, SenderURN: message.From.URN(), Envelope: raw, Purged: message.Purged}
	if message.Purged {
		if message.PurgedAt == nil || len(message.Payload) != 0 || len(message.Metadata) != 0 {
			return Publication{}, errors.New("messaging: purge_marker_refused")
		}
		return p, nil
	}
	if message.PurgedAt != nil || !strictJSON(message.Payload) {
		return Publication{}, errors.New("messaging: text_refused")
	}
	var body struct {
		Text *string `json:"text"`
		Body *string `json:"body"`
	}
	switch message.ContentType {
	case "application/json":
		if err := json.Unmarshal(message.Payload, &p.Original); err != nil {
			if err := json.Unmarshal(message.Payload, &body); err != nil || (body.Text == nil) == (body.Body == nil) {
				return Publication{}, errors.New("messaging: text_refused")
			}
			if body.Text != nil {
				p.Original = *body.Text
			} else {
				p.Original = *body.Body
			}
		}
	case "text/plain":
		// Tether envelope payloads are JSON values; a plain-text publication is
		// carried as a JSON string, not guessed from an arbitrary object.
		if err := json.Unmarshal(message.Payload, &p.Original); err != nil {
			return Publication{}, errors.New("messaging: text_refused")
		}
	default:
		return Publication{}, errors.New("messaging: content_type_refused")
	}
	if p.Original == "" || !utf8.ValidString(p.Original) || utf8.RuneCountInString(p.Original) > 65536 {
		return Publication{}, errors.New("messaging: text_refused")
	}
	// Refuse malformed projected attribution BEFORE a billable stage runs,
	// rather than discovering the public sink's limits after processing.
	for _, field := range []string{"project_id", "workstream_id", "launch_id", "launch_display_name", "runtime", "stop_reason"} {
		value := message.Metadata[field]
		if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 256 {
			return Publication{}, errors.New("messaging: source_metadata_refused")
		}
	}
	switch message.Metadata["confidence"] {
	case "", "exact", "heuristic", "none", "unknown":
	default:
		return Publication{}, errors.New("messaging: source_metadata_refused")
	}
	p.Metadata = make(map[string]string, len(message.Metadata))
	for k, v := range message.Metadata {
		p.Metadata[k] = v
	}
	p.OutputID = message.Metadata["output_id"]
	p.AgentID, p.AgentIdentityKind = message.Metadata["logical_agent_id"], "logical_agent_id"
	if p.AgentID == "" {
		p.AgentID, p.AgentIdentityKind = p.SenderURN, "sender_urn"
	}
	if !identifier(p.AgentID, 256) {
		return Publication{}, errors.New("messaging: agent_identity_refused")
	}
	classification := message.Metadata["kind"]
	p.SessionID, p.TurnID = message.Metadata["session_id"], message.Metadata["turn_id"]
	for _, label := range []string{p.SessionID, p.TurnID, p.OutputID} {
		if label != "" && !identifier(label, 256) {
			return Publication{}, errors.New("messaging: source_label_refused")
		}
	}
	routed := classification != ""
	if !routed {
		p.Origin, p.Kind = "publication", "checkpoint"
		return p, nil
	}
	p.Origin = "routed"
	if !identifier(p.SessionID, 256) || !identifier(p.TurnID, 256) || message.Kind != gomsg.MsgKindNotice || p.SenderURN != "msg://session/local/"+p.SessionID || message.ThreadID != p.SessionID {
		return Publication{}, errors.New("messaging: routed_attribution_refused")
	}
	switch classification {
	case "final":
		p.Kind = "terminal"
	case "question", "approval", "failure":
		p.Kind = classification
	default:
		return Publication{}, errors.New("messaging: routed_kind_refused")
	}
	return p, nil
}
