package messaging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"

	gomsg "github.com/hollis-labs/go-messaging"
	tether "github.com/hollis-labs/go-tether-client"
	"github.com/hollis-labs/tangent-plugins/messaging/internal/reply"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

// ReplyStore shares the loaded owner's private database and process lock. Its
// additive tables have their own schema version; publication schema 1 remains
// unchanged. Close belongs to Ledger, after all reply workers have joined.
type ReplyStore struct {
	ledger  *Ledger
	sources map[Source]bool
}

var _ reply.Store = (*ReplyStore)(nil)

func NewReplyStore(ctx context.Context, ledger *Ledger, sources ...Source) (*ReplyStore, error) {
	if ledger == nil || ledger.db == nil || len(sources) == 0 || len(sources) > 8 {
		return nil, reply.ErrRefused
	}
	allowed := make(map[Source]bool, len(sources))
	for _, source := range sources {
		if source.validate() != nil || allowed[source] {
			return nil, reply.ErrRefused
		}
		allowed[source] = true
	}
	tx, err := ledger.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS reply_schema (singleton INTEGER PRIMARY KEY CHECK(singleton=1), version INTEGER NOT NULL);
INSERT INTO reply_schema(singleton,version) VALUES(1,1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS reply_attempts (
 item_id TEXT NOT NULL, resolution_id TEXT NOT NULL, action_id TEXT NOT NULL,
 version INTEGER NOT NULL CHECK(version>0), record BLOB NOT NULL,
 PRIMARY KEY(item_id,resolution_id,action_id));
CREATE TABLE IF NOT EXISTS reply_heads (
 item_id TEXT PRIMARY KEY, resolution_id TEXT NOT NULL, action_id TEXT NOT NULL,
 FOREIGN KEY(item_id,resolution_id,action_id) REFERENCES reply_attempts(item_id,resolution_id,action_id));`); err != nil {
		return nil, err
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT version FROM reply_schema WHERE singleton=1`).Scan(&version); err != nil {
		return nil, err
	}
	if version != 1 {
		return nil, errors.New("messaging: reply_schema_unsupported")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &ReplyStore{ledger: ledger, sources: allowed}, nil
}

// Binding derives identity from an earned local receipt and the immutable
// prepared request. Neither browser identity nor an arbitrary host item binds
// a reply. Multiple publications claiming the same item are refused.
func (s *ReplyStore) Binding(ctx context.Context, itemID string) (reply.Binding, error) {
	binding, err := replyBinding(ctx, s.ledger.db, itemID)
	if err == nil && !s.allowed(binding) {
		return reply.Binding{}, reply.ErrRefused
	}
	return binding, err
}

func (s *ReplyStore) allowed(binding reply.Binding) bool {
	return s.sources[Source{EndpointRef: binding.Source.EndpointRef, Channel: binding.Source.Channel}]
}

func replyBinding(ctx context.Context, q rowQuerier, itemID string) (reply.Binding, error) {
	var original, prepared []byte
	var count int
	err := q.QueryRowContext(ctx, `SELECT original,prepared,(SELECT COUNT(*) FROM publications WHERE item_id=? AND settled=1) FROM publications WHERE item_id=? AND settled=1`, itemID, itemID).Scan(&original, &prepared, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return reply.Binding{}, reply.ErrNotFound
	}
	if err != nil {
		return reply.Binding{}, err
	}
	var p Publication
	var request plugin.AgentTurnRequest
	if count != 1 || !StrictJSON(original) || !StrictJSON(prepared) || json.Unmarshal(original, &p) != nil || json.Unmarshal(prepared, &request) != nil ||
		!identifier(itemID, 256) || request.SourceMessage == nil || p.Purged || request.IdempotencyKey != p.Key() || request.Content != p.Original ||
		request.SessionID != p.SessionID || request.TurnID != p.TurnID || request.Source.AgentID != p.AgentID || request.Kind != p.Kind {
		return reply.Binding{}, reply.ErrConflict
	}
	expected := plugin.TurnSourceMessage{SchemaVersion: 1, Origin: p.Origin, EndpointRef: p.Source.EndpointRef, Channel: p.Source.Channel,
		MessageID: p.MessageID, Sequence: p.Sequence, SenderURN: p.SenderURN, OutputID: p.OutputID,
		Attribution: plugin.TurnSourceAttribution{Kind: p.Metadata["kind"], LogicalAgentID: p.Metadata["logical_agent_id"], ProjectID: p.Metadata["project_id"],
			WorkstreamID: p.Metadata["workstream_id"], LaunchID: p.Metadata["launch_id"], LaunchDisplayName: p.Metadata["launch_display_name"],
			Runtime: p.Metadata["runtime"], StopReason: p.Metadata["stop_reason"], Confidence: p.Metadata["confidence"]}}
	if *request.SourceMessage != expected {
		return reply.Binding{}, reply.ErrConflict
	}
	return reply.Binding{ItemID: itemID, SessionID: p.SessionID, TurnID: p.TurnID, AgentID: p.AgentID, Kind: p.Kind, Source: expected}, nil
}

func replyAttempt(ctx context.Context, q rowQuerier, ref reply.Reference) (reply.Record, error) {
	var raw []byte
	var version int64
	err := q.QueryRowContext(ctx, `SELECT version,record FROM reply_attempts WHERE item_id=? AND resolution_id=? AND action_id=?`, ref.ItemID, ref.ResolutionID, ref.ActionID).Scan(&version, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return reply.Record{}, reply.ErrNotFound
	}
	if err != nil {
		return reply.Record{}, err
	}
	var record reply.Record
	if !StrictJSON(raw) || json.Unmarshal(raw, &record) != nil || record.Version != version || record.Reference().ItemID != ref.ItemID ||
		record.Reference().ResolutionID != ref.ResolutionID || record.Reference().ActionID != ref.ActionID {
		return reply.Record{}, reply.ErrConflict
	}
	return record, nil
}

func latestReply(ctx context.Context, q rowQuerier, itemID string) (reply.Record, error) {
	ref := reply.Reference{ItemID: itemID}
	err := q.QueryRowContext(ctx, `SELECT resolution_id,action_id FROM reply_heads WHERE item_id=?`, itemID).Scan(&ref.ResolutionID, &ref.ActionID)
	if errors.Is(err, sql.ErrNoRows) {
		return reply.Record{}, reply.ErrNotFound
	}
	if err != nil {
		return reply.Record{}, err
	}
	return replyAttempt(ctx, q, ref)
}

func (s *ReplyStore) Latest(ctx context.Context, itemID string) (reply.Record, error) {
	record, err := latestReply(ctx, s.ledger.db, itemID)
	if err == nil && !s.allowed(record.Prepared.Binding) {
		return reply.Record{}, reply.ErrRefused
	}
	return record, err
}

func validReplyPreparation(p reply.Prepared, previous *reply.Reference) bool {
	binding, resolution := p.Binding, p.Resolution
	if binding.Source.Origin != "routed" || binding.Source.SenderURN != "msg://session/local/"+binding.SessionID ||
		!identifier(binding.ItemID, 256) || !identifier(resolution.ID, 256) || !reflect.DeepEqual(p.Previous, previous) ||
		(p.ActionID == "") != (previous == nil) || (p.ActionID != "" && !identifier(p.ActionID, 256)) {
		return false
	}
	if _, err := gomsg.ParseURN(p.CallerURN); err != nil {
		return false
	}
	switch resolution.Action {
	case "respond", "approve", "reject":
	default:
		return false
	}
	body := resolution.ResponseText
	if body == "" {
		body = resolution.SelectedOption
	}
	if body == "" {
		body = resolution.Action
	}
	if resolution.Note != "" {
		body += ": " + resolution.Note
	}
	if p.Body != body || !utf8.ValidString(body) || strings.TrimSpace(body) == "" || len(body) > 128*1024 || (previous == nil && p.Interrupt != resolution.Interrupt) {
		return false
	}
	tuple, _ := json.Marshal([]string{binding.Source.EndpointRef, binding.Source.Channel, binding.Source.MessageID, binding.ItemID, resolution.ID, p.CallerURN, p.ActionID})
	digest := sha256.Sum256(tuple)
	return p.Key == "tether-reply:v1:"+hex.EncodeToString(digest[:])
}

func (s *ReplyStore) Prepare(ctx context.Context, prepared reply.Prepared, previous *reply.Reference) (reply.Record, error) {
	if !s.allowed(prepared.Binding) || !validReplyPreparation(prepared, previous) {
		return reply.Record{}, reply.ErrRefused
	}
	tx, err := s.ledger.db.BeginTx(ctx, nil)
	if err != nil {
		return reply.Record{}, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := replyBinding(ctx, tx, prepared.Binding.ItemID)
	if err != nil {
		return reply.Record{}, err
	}
	if binding != prepared.Binding {
		return reply.Record{}, reply.ErrConflict
	}
	latest, latestErr := latestReply(ctx, tx, binding.ItemID)
	if latestErr != nil && !errors.Is(latestErr, reply.ErrNotFound) {
		return reply.Record{}, latestErr
	}
	ref := reply.Reference{ItemID: binding.ItemID, ResolutionID: prepared.Resolution.ID, ActionID: prepared.ActionID}
	existing, loadErr := replyAttempt(ctx, tx, ref)
	if loadErr == nil {
		if !reflect.DeepEqual(existing.Prepared, prepared) || latestErr != nil || latest.Reference() != existing.Reference() {
			return reply.Record{}, reply.ErrConflict
		}
		return existing, tx.Commit()
	}
	if !errors.Is(loadErr, reply.ErrNotFound) {
		return reply.Record{}, loadErr
	}
	if previous == nil {
		if latestErr == nil {
			return reply.Record{}, reply.ErrConflict
		}
	} else if latestErr != nil || latest.Reference() != *previous || !latest.TerminalFailure() || latest.Acknowledged ||
		latest.Prepared.Binding != binding || latest.Prepared.Resolution != prepared.Resolution || latest.Prepared.CallerURN != prepared.CallerURN || latest.Prepared.Body != prepared.Body || latest.Prepared.ActionID == prepared.ActionID {
		return reply.Record{}, reply.ErrConflict
	}
	record := reply.Record{Version: 1, Prepared: prepared}
	raw, err := json.Marshal(record)
	if err != nil {
		return reply.Record{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO reply_attempts(item_id,resolution_id,action_id,version,record) VALUES(?,?,?,?,?)`, ref.ItemID, ref.ResolutionID, ref.ActionID, record.Version, raw); err != nil {
		return reply.Record{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO reply_heads(item_id,resolution_id,action_id) VALUES(?,?,?) ON CONFLICT(item_id) DO UPDATE SET resolution_id=excluded.resolution_id,action_id=excluded.action_id`, ref.ItemID, ref.ResolutionID, ref.ActionID); err != nil {
		return reply.Record{}, err
	}
	if err = tx.Commit(); err != nil {
		return reply.Record{}, err
	}
	// Decode the committed encoding, detaching caller-owned pointers.
	var detached reply.Record
	err = json.Unmarshal(raw, &detached)
	return detached, err
}

func validReplyUpdate(old, next reply.Record) bool {
	if !reflect.DeepEqual(old.Prepared, next.Prepared) || next.Version != old.Version || old.Version == math.MaxInt64 ||
		(old.Receipt != nil && !reflect.DeepEqual(old.Receipt, next.Receipt)) || (old.Delivery != nil && next.Delivery == nil) ||
		(old.Acknowledged && !next.Acknowledged) {
		return false
	}
	if old.TerminalFailure() || old.Acknowledged {
		return reflect.DeepEqual(old, next)
	}
	if old.Delivery != nil && old.Delivery.State == tether.ReplyDelivered && !reflect.DeepEqual(old.Delivery, next.Delivery) {
		return false
	}
	if next.Receipt != nil && (!identifier(next.Receipt.ReplyID, 256) || next.Receipt.ParentID != next.Prepared.Binding.Source.MessageID || !identifier(next.Receipt.TargetSessionID, 256)) {
		return false
	}
	if next.Delivery != nil {
		d := next.Delivery
		if next.Receipt == nil || d.ReplyID != next.Receipt.ReplyID || d.ParentID != next.Prepared.Binding.Source.MessageID ||
			d.OriginalSessionID != next.Prepared.Binding.SessionID || !identifier(d.TargetSessionID, 256) || d.Attempts < 0 ||
			len(d.State) > 128 || len(d.Reason) > 128 || d.Detail != "" ||
			(d.DeliveredToSessionID != "" && !identifier(d.DeliveredToSessionID, 256)) {
			return false
		}
	}
	if next.FailureCode != "" && (len(next.FailureCode) > 128 || next.FailureStatus < 400 || next.FailureStatus > 599 || next.Receipt != nil) {
		return false
	}
	return !next.Acknowledged || (next.Delivery != nil && next.Delivery.State == tether.ReplyDelivered && next.FailureCode == "")
}

func (s *ReplyStore) Update(ctx context.Context, expected reply.Reference, next reply.Record) (reply.Record, error) {
	tx, err := s.ledger.db.BeginTx(ctx, nil)
	if err != nil {
		return reply.Record{}, err
	}
	defer func() { _ = tx.Rollback() }()
	old, err := latestReply(ctx, tx, expected.ItemID)
	if err != nil {
		return reply.Record{}, err
	}
	if !s.allowed(old.Prepared.Binding) || old.Reference() != expected || !validReplyUpdate(old, next) {
		return reply.Record{}, reply.ErrConflict
	}
	next.Version = old.Version + 1
	raw, err := json.Marshal(next)
	if err != nil {
		return reply.Record{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE reply_attempts SET version=?,record=? WHERE item_id=? AND resolution_id=? AND action_id=? AND version=?`, next.Version, raw, expected.ItemID, expected.ResolutionID, expected.ActionID, expected.Version)
	if err != nil {
		return reply.Record{}, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return reply.Record{}, errors.Join(reply.ErrConflict, err)
	}
	if err = tx.Commit(); err != nil {
		return reply.Record{}, err
	}
	var detached reply.Record
	err = json.Unmarshal(bytes.Clone(raw), &detached)
	return detached, err
}
