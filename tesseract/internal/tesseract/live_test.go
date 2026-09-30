package tesseract

import (
	"context"
	"os"
	"testing"
)

// A live check against the real Tesseract, off by default.
//
// It is here rather than in a scratch program because internal/ packages cannot
// be imported from outside the module, and because the thing worth checking is
// this client's own bytes against the real service — not a curl that happens to
// resemble them. Two route names in this task's brief were wrong, and only a
// call from this code would have caught that.
//
// Run it with TANGENT_TESSERACT_LIVE_TEST=1. It is skipped otherwise, so
// `make test` never depends on a service being up.
//
// TANGENT_TESSERACT_LIVE_DEPRECATE names a memory_key it may retire. Deprecation
// is not reversible, so the key is required rather than defaulted: a test that
// picked its own target would eventually pick a real record.
func TestLiveTesseract(t *testing.T) {
	if os.Getenv("TANGENT_TESSERACT_LIVE_TEST") == "" {
		t.Skip("set TANGENT_TESSERACT_LIVE_TEST=1 to run against the real Tesseract")
	}
	client := NewClient(os.Getenv(BaseURLEnv), os.Getenv(TokenEnv))
	ctx := context.Background()

	if err := client.Health(ctx); err != nil {
		t.Fatalf("health against %s: %v", client.BaseURL(), err)
	}
	t.Logf("health ok at %s", client.BaseURL())

	namespace := os.Getenv("TANGENT_TESSERACT_LIVE_NAMESPACE")
	if namespace == "" {
		namespace = "user/example/memory/decisions"
	}
	input := RecallInput{
		Namespaces: []string{namespace},
		Ranking:    RankingChronological,
		Limit:      10,
	}
	before, err := client.Recall(ctx, input)
	if err != nil {
		t.Fatalf("recall %s: %v", namespace, err)
	}
	t.Logf("recall %s: %d returned, %d matching, truncated=%v (%s)",
		namespace, len(before.Revisions), before.Manifest.ResultsTotal,
		before.Manifest.Truncated, before.Manifest.TruncationReason)
	if len(before.Revisions) == 0 {
		t.Fatalf("recall returned nothing; the namespace is empty or the request shape is wrong")
	}
	if before.Manifest.ResultsReturned != len(before.Revisions) {
		t.Errorf("manifest says %d returned, got %d revisions",
			before.Manifest.ResultsReturned, len(before.Revisions))
	}

	key := os.Getenv("TANGENT_TESSERACT_LIVE_DEPRECATE")
	if key == "" {
		t.Log("no TANGENT_TESSERACT_LIVE_DEPRECATE key named; skipping the write half")
		return
	}
	var target string
	for _, revision := range before.Revisions {
		if revision.MemoryKey == key {
			target = revision.RevisionID
		}
	}
	if target == "" {
		t.Fatalf("no live revision with memory_key %q in %s", key, namespace)
	}
	if depErr := client.Deprecate(ctx, target); depErr != nil {
		t.Fatalf("deprecate %s: %v", target, depErr)
	}
	t.Logf("deprecated %s (%s)", target, key)

	after, err := client.Recall(ctx, input)
	if err != nil {
		t.Fatalf("recall after deprecate: %v", err)
	}
	for _, revision := range after.Revisions {
		if revision.RevisionID == target {
			t.Fatalf("revision %s still comes back from recall after deprecation", target)
		}
	}
	t.Logf("recall after: %d live record(s); the deprecated one is gone, which is the feedback",
		len(after.Revisions))
}
