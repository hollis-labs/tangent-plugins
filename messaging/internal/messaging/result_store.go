package messaging

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	pipeline "github.com/hollis-labs/libs/message-pipeline"
)

var _ pipeline.ResultStore = (*Ledger)(nil)

// Get decodes a fresh value on every lookup. Neither caller buffers nor live
// stage state can alias the durable result.
func (l *Ledger) Get(ctx context.Context, key pipeline.Key) (pipeline.Record, bool, error) {
	rawKey, err := json.Marshal(key)
	if err != nil {
		return pipeline.Record{}, false, err
	}
	var raw []byte
	err = l.db.QueryRowContext(ctx, `SELECT record FROM stage_results WHERE key=?`, string(rawKey)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return pipeline.Record{}, false, nil
	}
	if err != nil {
		return pipeline.Record{}, false, err
	}
	var record pipeline.Record
	if err = json.Unmarshal(raw, &record); err != nil {
		return pipeline.Record{}, false, err
	}
	return record, true, nil
}

// PutIfAbsent commits an immutable winner before returning it. A competing
// input digest cannot replace a saved success or failure marker.
func (l *Ledger) PutIfAbsent(ctx context.Context, key pipeline.Key, record pipeline.Record) (pipeline.Record, error) {
	if record.SchemaVersion != 1 || record.InputDigest == "" {
		return pipeline.Record{}, pipeline.ErrInvalid
	}
	rawKey, err := json.Marshal(key)
	if err != nil {
		return pipeline.Record{}, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return pipeline.Record{}, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return pipeline.Record{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO stage_results(key,record) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, string(rawKey), raw); err != nil {
		return pipeline.Record{}, err
	}
	var saved []byte
	if err = tx.QueryRowContext(ctx, `SELECT record FROM stage_results WHERE key=?`, string(rawKey)).Scan(&saved); err != nil {
		return pipeline.Record{}, err
	}
	var winner pipeline.Record
	if err = json.Unmarshal(saved, &winner); err != nil {
		return pipeline.Record{}, err
	}
	if winner.InputDigest != record.InputDigest {
		return pipeline.Record{}, pipeline.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return pipeline.Record{}, err
	}
	return winner, nil
}
