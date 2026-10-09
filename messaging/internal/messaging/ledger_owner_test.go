package messaging

import (
	"context"
	"testing"
)

func TestLedgerCannotHaveTwoIncarnationOwners(t *testing.T) {
	first, dir := openTestLedger(t)
	if second, err := OpenLedger(context.Background(), dir); err == nil {
		_ = second.Close()
		t.Fatal("second process owner admitted")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := OpenLedger(context.Background(), dir)
	if err != nil {
		t.Fatal("physical ownership not retired", err)
	}
	t.Cleanup(func() { _ = next.Close() })
}
