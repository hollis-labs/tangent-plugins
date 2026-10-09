package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	data := t.TempDir()
	if err := os.Chmod(data, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, data
}

func admitted(t *testing.T) Publication {
	t.Helper()
	p, err := Admit(testSource(), routedMessage())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLedgerReplayPreservesOriginalAndPreparedRequest(t *testing.T) {
	ctx := context.Background()
	l, data := openTestLedger(t)
	p := admitted(t)
	if _, err := l.Capture(ctx, p); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"idempotency_key":"saved-key","content":"exact original","summary":"saved result"}`)
	if err := l.Prepare(ctx, p.Source, p.MessageID, payload); err != nil {
		t.Fatal(err)
	}
	payload[2] = 'X'
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenLedger(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	stored, err := resumed.Publication(ctx, p.Source, p.MessageID)
	if err != nil || stored.Publication.Original != p.Original || string(stored.Prepared) != `{"idempotency_key":"saved-key","content":"exact original","summary":"saved result"}` {
		t.Fatalf("replay lost immutable data: %+v %v", stored, err)
	}
	if cursor, found, err := resumed.Cursor(ctx, p.Source); err != nil || found || cursor != 0 {
		t.Fatalf("preparation advanced cursor: %d %v %v", cursor, found, err)
	}
	if err := resumed.Prepare(ctx, p.Source, p.MessageID, []byte(`{"content":"changed"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("changed replay accepted", err)
	}
	if err := resumed.Settle(ctx, p.Source, p.MessageID, 0, SinkReceipt{ItemID: "sink-item", IdempotencyKey: p.Key()}); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Settle(ctx, p.Source, p.MessageID, 0, SinkReceipt{ItemID: "sink-item", IdempotencyKey: p.Key()}); err != nil {
		t.Fatal("same receipt replay refused", err)
	}
	stored, err = resumed.Publication(ctx, p.Source, p.MessageID)
	if err != nil || !stored.Settled || stored.ItemID != "sink-item" {
		t.Fatal("receipt not recorded", err)
	}
	if cursor, found, err := resumed.Cursor(ctx, p.Source); err != nil || !found || cursor != 42 {
		t.Fatalf("settlement cursor: %d %v %v", cursor, found, err)
	}
}

func TestLedgerReceiptAndCursorRollbackTogether(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	p := admitted(t)
	if _, err := l.Capture(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(ctx, p.Source, p.MessageID, []byte(`{"content":"original"}`)); err != nil {
		t.Fatal(err)
	}
	// Fail the cursor write AFTER the receipt statement to exercise the
	// crash boundary, rather than assuming two successful writes are atomic.
	if _, err := l.db.ExecContext(ctx, `CREATE TRIGGER fail_cursor BEFORE INSERT ON cursors BEGIN SELECT RAISE(ABORT,'owned test failure'); END`); err != nil {
		t.Fatal(err)
	}
	receipt := SinkReceipt{ItemID: "earned-item", IdempotencyKey: p.Key()}
	if err := l.Settle(ctx, p.Source, p.MessageID, 0, receipt); err == nil {
		t.Fatal("forced settlement failure ignored")
	}
	stored, err := l.Publication(ctx, p.Source, p.MessageID)
	if err != nil || stored.Settled || stored.ItemID != "" || len(stored.Prepared) == 0 {
		t.Fatalf("partial commit escaped: %+v %v", stored, err)
	}
	if _, found, err := l.Cursor(ctx, p.Source); err != nil || found {
		t.Fatal("cursor escaped rollback", err)
	}
	if _, err := l.db.ExecContext(ctx, `DROP TRIGGER fail_cursor`); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, p.Source, p.MessageID, 0, receipt); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerRejectsChangedInputAndOutOfOrderSettlement(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	p := admitted(t)
	if _, err := l.Capture(ctx, p); err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.Original = "different"
	if _, err := l.Capture(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed original accepted", err)
	}
	later := p
	later.Sequence = 99
	later.MessageID = "later-output"
	if _, err := l.Capture(ctx, later); err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(ctx, later.Source, later.MessageID, []byte(`{"content":"later"}`)); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, later.Source, later.MessageID, 0, SinkReceipt{ItemID: "later-item", IdempotencyKey: later.Key()}); !errors.Is(err, ErrConflict) {
		t.Fatal("unsettled earlier publication skipped", err)
	}
	if err := l.Prepare(ctx, p.Source, p.MessageID, []byte(`{"content":"original"}`)); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, p.Source, p.MessageID, 0, SinkReceipt{ItemID: "wrong-key", IdempotencyKey: "foreign"}); !errors.Is(err, ErrConflict) {
		t.Fatal("foreign receipt accepted", err)
	}
	if err := l.Settle(ctx, p.Source, p.MessageID, 0, SinkReceipt{ItemID: "first-item", IdempotencyKey: p.Key()}); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, later.Source, later.MessageID, 0, SinkReceipt{ItemID: "later-item", IdempotencyKey: later.Key()}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale cursor accepted", err)
	}
	// Real channel sequences can have gaps; settling the next actual item
	// need not mean sequence+1.
	if err := l.Settle(ctx, later.Source, later.MessageID, 42, SinkReceipt{ItemID: "later-item", IdempotencyKey: later.Key()}); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerPurgeRecordsRetentionWithoutEnqueueOrOriginalMutation(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	p := admitted(t)
	if _, err := l.Capture(ctx, p); err != nil {
		t.Fatal(err)
	}
	message := routedMessage()
	now := time.Now()
	message.Purged = true
	message.PurgedAt = &now
	message.Payload = nil
	message.Metadata = nil
	purge, err := Admit(testSource(), message)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := l.Capture(ctx, purge)
	if err != nil || !stored.Purged || stored.Publication.Original != p.Original {
		t.Fatalf("source purge replaced captured original: %+v %v", stored, err)
	}
	if err := l.Prepare(ctx, p.Source, p.MessageID, []byte(`{"content":"recreated"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("purged source recreated", err)
	}
	if err := l.SettlePurge(ctx, p.Source, p.MessageID, 0); err != nil {
		t.Fatal(err)
	}
	stored, err = l.Publication(ctx, p.Source, p.MessageID)
	if err != nil || !stored.Settled || stored.ItemID != "" || !stored.Purged {
		t.Fatal("purge outcome failed", err)
	}
}

func TestLedgerCancelledSettlementStaysPending(t *testing.T) {
	ctx := context.Background()
	l, _ := openTestLedger(t)
	p := admitted(t)
	if _, err := l.Capture(ctx, p); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"content":"original"}`)
	if err := l.Prepare(ctx, p.Source, p.MessageID, payload); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := l.Settle(cancelled, p.Source, p.MessageID, 0, SinkReceipt{ItemID: "earned-item", IdempotencyKey: p.Key()}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled settlement treated as success", err)
	}
	stored, err := l.Publication(ctx, p.Source, p.MessageID)
	if err != nil || stored.Settled || !bytes.Equal(stored.Prepared, payload) {
		t.Fatal("shutdown lost pending obligation", err)
	}
}

func TestLedgerRequiresPrivateDataDirectoryAndFile(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenLedger(ctx, "relative"); err == nil {
		t.Fatal("relative data root accepted")
	}
	parent := t.TempDir()
	data := filepath.Join(parent, "data")
	if err := os.Mkdir(data, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(ctx, data); err == nil {
		t.Fatal("public data root accepted")
	}
	if err := os.Chmod(data, 0700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(parent, "other")
	if err := os.WriteFile(other, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(data, "messaging.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(ctx, data); err == nil {
		t.Fatal("symlink ledger accepted")
	}
	l, dir := openTestLedger(t)
	info, err := os.Stat(filepath.Join(dir, "messaging.sqlite3"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("ledger mode is not private", err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
}
