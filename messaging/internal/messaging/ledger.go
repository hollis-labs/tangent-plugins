package messaging

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite" // The ledger belongs to this adapter, not the pipeline library.
)

// ErrConflict refuses changed input, prepared requests or stale settlement.
var ErrConflict = errors.New("messaging: ledger_conflict")

// Ledger owns one private SQLite file under the host-provided DataDir.
// Permissions provide local custody, not isolation from the same OS user.
type Ledger struct{ db *sql.DB }

// StoredPublication is a defensive snapshot of the durable obligation.
type StoredPublication struct {
	Publication Publication
	Prepared    json.RawMessage
	ItemID      string
	Purged      bool
	Settled     bool
}

// SinkReceipt is earned by the external sink, never fabricated by a stage.
type SinkReceipt struct {
	ItemID         string
	IdempotencyKey string
}

// OpenLedger requires an existing private host-owned DataDir. No bundle/cache,
// default home directory, or replacement path is used when it is unavailable.
func OpenLedger(ctx context.Context, dataDir string) (*Ledger, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, errors.New("messaging: private_data_dir_required")
	}
	info, err := os.Lstat(dataDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !currentOwner(info) {
		return nil, errors.New("messaging: private_data_dir_required")
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, errors.New("messaging: data_dir_unavailable")
	}
	fileInfo, statErr := root.Lstat("messaging.sqlite3")
	if statErr == nil && (!fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0600 || !currentOwner(fileInfo)) {
		return nil, errors.Join(errors.New("messaging: private_ledger_required"), root.Close())
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, errors.Join(errors.New("messaging: ledger_unavailable"), root.Close())
	}
	file, err := root.OpenFile("messaging.sqlite3", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.Join(errors.New("messaging: ledger_unavailable"), root.Close())
	}
	pinned, statErr := file.Stat()
	closeErr := errors.Join(file.Close(), root.Close())
	if statErr != nil || closeErr != nil {
		return nil, errors.Join(statErr, closeErr)
	}
	address := url.URL{Scheme: "file", Path: filepath.Join(dataDir, "messaging.sqlite3")}
	query := url.Values{"_pragma": []string{"foreign_keys(1)", "busy_timeout(5000)"}}
	address.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", address.String())
	if err != nil {
		return nil, errors.New("messaging: ledger_unavailable")
	}
	db.SetMaxOpenConns(1)
	current, statErr := os.Lstat(filepath.Join(dataDir, "messaging.sqlite3"))
	if statErr != nil || !os.SameFile(pinned, current) {
		return nil, errors.Join(errors.New("messaging: ledger_identity_changed"), db.Close())
	}
	ledger := &Ledger{db: db}
	if err := ledger.initialize(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return ledger, nil
}

func currentOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(os.Geteuid())
}

func (l *Ledger) initialize(ctx context.Context) error {
	var version int
	if err := l.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return errors.New("messaging: ledger_schema_unsupported")
	}
	_, err := l.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS cursors (source TEXT PRIMARY KEY, sequence INTEGER NOT NULL CHECK(sequence>=0));
CREATE TABLE IF NOT EXISTS publications (
 source TEXT NOT NULL, message_id TEXT NOT NULL, sequence INTEGER NOT NULL CHECK(sequence>0),
 original BLOB NOT NULL, prepared BLOB, item_id TEXT, settled INTEGER NOT NULL DEFAULT 0,
 purge_receipt BLOB,
 PRIMARY KEY(source,message_id), UNIQUE(source,sequence));
CREATE TABLE IF NOT EXISTS stage_results (key TEXT PRIMARY KEY, record BLOB NOT NULL);
PRAGMA user_version=1;`)
	return err
}

// Close must run only after all loaded workers and external calls have joined.
func (l *Ledger) Close() error { return l.db.Close() }

// Capture persists admitted input before processing. Replays preserve the
// original; a later source tombstone is appended without recreating its body.
func (l *Ledger) Capture(ctx context.Context, p Publication) (StoredPublication, error) {
	if err := p.Source.validate(); err != nil {
		return StoredPublication{}, err
	}
	if !identifier(p.MessageID, 256) || p.Sequence <= 0 {
		return StoredPublication{}, ErrConflict
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return StoredPublication{}, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return StoredPublication{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO publications(source,message_id,sequence,original) VALUES(?,?,?,?) ON CONFLICT(source,message_id) DO NOTHING`, p.Source.key(), p.MessageID, p.Sequence, raw); err != nil {
		return StoredPublication{}, ErrConflict
	}
	stored, old, err := loadPublication(ctx, tx, p.Source, p.MessageID)
	if err != nil {
		return stored, err
	}
	if stored.Publication.Sequence != p.Sequence {
		return stored, ErrConflict
	}
	if !bytes.Equal(old, raw) {
		if !p.Purged || stored.Publication.SenderURN != p.SenderURN {
			return stored, ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE publications SET purge_receipt=COALESCE(purge_receipt,?) WHERE source=? AND message_id=?`, p.Envelope, p.Source.key(), p.MessageID); err != nil {
			return stored, err
		}
		stored.Purged = true
	}
	if err = tx.Commit(); err != nil {
		return StoredPublication{}, err
	}
	return stored, nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadPublication(ctx context.Context, q rowQuerier, source Source, id string) (StoredPublication, []byte, error) {
	var stored StoredPublication
	var raw, prepared, purge []byte
	var item sql.NullString
	var settled bool
	err := q.QueryRowContext(ctx, `SELECT original,prepared,item_id,settled,purge_receipt FROM publications WHERE source=? AND message_id=?`, source.key(), id).Scan(&raw, &prepared, &item, &settled, &purge)
	if err != nil {
		return stored, nil, err
	}
	if err = json.Unmarshal(raw, &stored.Publication); err != nil {
		return stored, nil, err
	}
	stored.Prepared = bytes.Clone(prepared)
	stored.ItemID = item.String
	stored.Settled = settled
	stored.Purged = stored.Publication.Purged || len(purge) > 0
	return stored, raw, nil
}

// Publication returns a fresh snapshot, never a reference to caller buffers.
func (l *Ledger) Publication(ctx context.Context, source Source, id string) (StoredPublication, error) {
	stored, _, err := loadPublication(ctx, l.db, source, id)
	return stored, err
}

// Prepare stores the exact encoded sink request. A changed replay is refused
// even when its JSON would be semantically equivalent.
func (l *Ledger) Prepare(ctx context.Context, source Source, id string, payload []byte) error {
	if len(payload) == 0 || len(payload) > 2*1024*1024 || !json.Valid(payload) || payload[0] != '{' {
		return errors.New("messaging: prepared_payload_refused")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stored, _, err := loadPublication(ctx, tx, source, id)
	if err != nil {
		return err
	}
	if len(stored.Prepared) > 0 {
		if !bytes.Equal(stored.Prepared, payload) {
			return ErrConflict
		}
		return nil
	}
	if stored.Purged || stored.Settled {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE publications SET prepared=? WHERE source=? AND message_id=?`, payload, source.key(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// Cursor returns only locally committed settlement, never a source high-water
// hint. Absence is distinct from a committed zero.
func (l *Ledger) Cursor(ctx context.Context, source Source) (int64, bool, error) {
	var sequence int64
	err := l.db.QueryRowContext(ctx, `SELECT sequence FROM cursors WHERE source=?`, source.key()).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return sequence, err == nil, err
}

// Settle records the earned sink item and advances the cursor in one local
// transaction. A crash after sink commit can only retry the saved request/key.
func (l *Ledger) Settle(ctx context.Context, source Source, id string, expectedCursor int64, receipt SinkReceipt) error {
	if !identifier(receipt.ItemID, 256) {
		return errors.New("messaging: sink_receipt_refused")
	}
	return l.settle(ctx, source, id, expectedCursor, &receipt)
}

// SettlePurge records a source-retention outcome without processing or enqueue.
func (l *Ledger) SettlePurge(ctx context.Context, source Source, id string, expectedCursor int64) error {
	return l.settle(ctx, source, id, expectedCursor, nil)
}

func (l *Ledger) settle(ctx context.Context, source Source, id string, expected int64, receipt *SinkReceipt) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stored, _, err := loadPublication(ctx, tx, source, id)
	if err != nil {
		return err
	}
	if receipt != nil {
		if stored.Purged || len(stored.Prepared) == 0 || receipt.IdempotencyKey != stored.Publication.Key() {
			return ErrConflict
		}
	} else if !stored.Purged {
		return ErrConflict
	}
	if stored.Settled {
		if receipt != nil && stored.ItemID != receipt.ItemID {
			return ErrConflict
		}
		return nil
	}
	var cursor int64
	err = tx.QueryRowContext(ctx, `SELECT sequence FROM cursors WHERE source=?`, source.key()).Scan(&cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if cursor != expected || stored.Publication.Sequence <= cursor {
		return ErrConflict
	}
	var earlier int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM publications WHERE source=? AND sequence>? AND sequence<? AND settled=0`, source.key(), cursor, stored.Publication.Sequence).Scan(&earlier); err != nil {
		return err
	}
	if earlier != 0 {
		return ErrConflict
	}
	var item any
	if receipt != nil {
		item = receipt.ItemID
	}
	if _, err = tx.ExecContext(ctx, `UPDATE publications SET item_id=?,settled=1 WHERE source=? AND message_id=?`, item, source.key(), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO cursors(source,sequence) VALUES(?,?) ON CONFLICT(source) DO UPDATE SET sequence=excluded.sequence`, source.key(), stored.Publication.Sequence); err != nil {
		return err
	}
	return tx.Commit()
}
