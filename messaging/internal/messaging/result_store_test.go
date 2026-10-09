package messaging

import (
	"context"
	"testing"

	pipeline "github.com/hollis-labs/libs/message-pipeline"
	"github.com/hollis-labs/libs/message-pipeline/pipelinetest"
)

func TestSQLiteResultStoreContractAndFailureSurviveReopen(t *testing.T) {
	l, dir := openTestLedger(t)
	pipelinetest.CheckResultStore(t, l)
	key := pipeline.Key{Message: pipeline.Identity{Source: "source", Message: "message"}, StageID: "summary"}
	record := pipeline.Record{SchemaVersion: 1, InputDigest: "input", Trace: pipeline.Trace{StageID: "summary", StageVersion: "1", Outcome: pipeline.TimedOut, FailureCode: pipeline.StageTimeout}, Result: pipeline.Result{Disposition: pipeline.Pass}}
	if _, err := l.PutIfAbsent(context.Background(), key, record); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenLedger(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	saved, found, err := resumed.Get(context.Background(), key)
	if err != nil || !found || saved.Trace.Outcome != pipeline.TimedOut || saved.Trace.FailureCode != pipeline.StageTimeout {
		t.Fatal("saved failure was not replayed", err)
	}
}
