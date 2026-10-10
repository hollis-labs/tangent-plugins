package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func relationshipFixture(t *testing.T) *Snapshot {
	t.Helper()
	base := fixture(t)
	files := map[string][]byte{}
	for name, env := range base.envelopes {
		files[name] = []byte(encode(env))
	}
	files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","future":{"keep":true},"items":[
 {"id":"ID-example","title":"Typed relationships","status":"decision-needed","decision":"Historic free text, not a decision object","rev":7,"unknown":{"large":9007199254740993},
 "related_ids":["WS-ambiguous","WS-ambiguous"],"workstream_ids":["WS-ambiguous","WS-other"],"project_ids":["project://external"],"decision_ids":["DEC-example","DEC-dangling","DEC-example"],"depends_on":["ID-second"],"supersedes":"ID-old","custom_ids":["external-ref"],
 "links":[{"kind":"workstream","ref":"WS-ambiguous","label":"original membership","unknown":true},{"kind":"workstream","ref":"WS-ambiguous","label":"duplicate retained"},{"kind":"url","ref":"WS-ambiguous","label":"different meaning"}],
 "comments":[{"id":"c-9","author":"historical","text":"Retain comment","unknown":null}],
 "_portfolio_relationships":{"version":1,"unknown":{"preserve":true},"edges":[{"type":"informs","target":"DEC-example","note":"retain entry"},{"type":"decision_gates","target":"DEC-example","note":"duplicate logical relationship"}]}},
 {"id":"ID-second","title":"Another candidate","status":"decision-needed","decision":null},
 {"id":"ID-draft","title":"Draft","status":"draft"}]}`)
	snap, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func importRelationships(t *testing.T) (*Store, *Snapshot) {
	t.Helper()
	s, snap := openTest(t), relationshipFixture(t)
	if applied, err := s.Import(t.Context(), snap); err != nil || !applied {
		t.Fatalf("import: %t %v", applied, err)
	}
	return s, snap
}

func currentSnapshot(t *testing.T, s *Store) *Snapshot {
	t.Helper()
	files, err := s.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Verify(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func snapshotItem(t *testing.T, snap *Snapshot, db, id string) map[string]any {
	t.Helper()
	for _, v := range snap.envelopes[db]["items"].([]any) {
		item := v.(map[string]any)
		if item["id"] == id {
			return item
		}
	}
	t.Fatalf("missing %s", id)
	return nil
}

func TestTypedImportViewsAndReadOnlyDiagnostics(t *testing.T) {
	s, original := importRelationships(t)
	if got := currentSnapshot(t, s); got.digest != original.digest {
		t.Fatal("typed projection changed imported source")
	}
	if applied, err := s.Import(t.Context(), original); err != nil || applied {
		t.Fatalf("exact replay: %t %v", applied, err)
	}
	edges, err := s.ListEdges(t.Context(), "ID-example", "belongs_to")
	want := []Edge{{"ID-example", "WS-ambiguous", "belongs_to", "links.workstream"}, {"ID-example", "WS-ambiguous", "belongs_to", "workstream_ids"}, {"ID-example", "WS-other", "belongs_to", "workstream_ids"}, {"ID-example", "project://external", "belongs_to", "project_ids"}}
	if err != nil || !reflect.DeepEqual(edges, want) {
		t.Fatalf("membership: %v %v", edges, err)
	}
	ids, err := s.DecisionIDs(t.Context(), "ID-example")
	if err != nil || !reflect.DeepEqual(ids, []string{"DEC-dangling", "DEC-example"}) {
		t.Fatalf("decision view: %v %v", ids, err)
	}
	back, err := s.Backlinks(t.Context(), "DEC-example", "decision_gates")
	if err != nil || !reflect.DeepEqual(back, []Edge{{"ID-example", "DEC-example", "decision_gates", relationshipField}, {"ID-example", "DEC-example", "decision_gates", "decision_ids"}}) {
		t.Fatalf("backlinks: %v %v", back, err)
	}
	back, err = s.Backlinks(t.Context(), "DEC-dangling", "")
	if err != nil || !reflect.DeepEqual(back, []Edge{{"ID-example", "DEC-dangling", "decision_gates", "decision_ids"}}) {
		t.Fatalf("dangling backlinks: %v %v", back, err)
	}
	diagnostics, err := s.IdeaDiagnostics(t.Context())
	wantDiagnostics := []IdeaDiagnostic{{"ID-example", true, true, "Historic free text, not a decision object"}, {"ID-second", true, true, nil}}
	if err != nil || !reflect.DeepEqual(diagnostics, wantDiagnostics) {
		t.Fatalf("diagnostics: %v %v", diagnostics, err)
	}
	if got := currentSnapshot(t, s); got.digest != original.digest {
		t.Fatal("reads converted or changed an idea")
	}
}

func TestTypedLinkUnlinkAndSemanticRoundTrip(t *testing.T) {
	s, original := importRelationships(t)
	for _, typ := range []string{"related", "belongs_to", "depends_on", "decision_gates", "informs", "supersedes"} {
		changed, err := s.Link(t.Context(), "ID-draft", "external/"+typ, typ)
		if err != nil || !changed {
			t.Fatalf("link %s: %t %v", typ, changed, err)
		}
		before := currentSnapshot(t, s)
		changed, err = s.Link(t.Context(), "ID-draft", "external/"+typ, typ)
		if err != nil || changed || currentSnapshot(t, s).digest != before.digest {
			t.Fatalf("link no-op %s: %t %v", typ, changed, err)
		}
	}
	if changed, err := s.Link(t.Context(), "ID-example", "DEC-example", "decision_gates"); err != nil || changed {
		t.Fatalf("legacy no-op: %t %v", changed, err)
	}
	changedSnap := currentSnapshot(t, s)
	if !reflect.DeepEqual(snapshotItem(t, original, "ideas", "ID-example"), snapshotItem(t, changedSnap, "ideas", "ID-example")) {
		t.Fatal("link rewrote an unrelated item")
	}
	ids, err := s.DecisionIDs(t.Context(), "ID-draft")
	if err != nil || !reflect.DeepEqual(ids, []string{"external/decision_gates"}) {
		t.Fatalf("new decision view: %v %v", ids, err)
	}
	files, err := s.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	other := openTest(t)
	roundtrip, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Import(t.Context(), roundtrip); err != nil {
		t.Fatal(err)
	}
	if changed, replayErr := other.Import(t.Context(), roundtrip); replayErr != nil || changed {
		t.Fatalf("new copied export exact replay: %t %v", changed, replayErr)
	}
	if got := currentSnapshot(t, other); got.digest != changedSnap.digest {
		t.Fatal("additive metadata lost on export/import")
	}
	for _, snap := range []*Snapshot{original, changedSnap} {
		if _, err = s.Import(t.Context(), snap); err == nil || !strings.Contains(err.Error(), "changed since import") {
			t.Fatalf("mutated receipt claimed replay: %v", err)
		}
	}
	for _, typ := range []string{"related", "belongs_to", "depends_on", "decision_gates", "informs", "supersedes"} {
		if changed, err := s.Unlink(t.Context(), "ID-draft", "external/"+typ, typ); err != nil || !changed {
			t.Fatalf("unlink %s: %t %v", typ, changed, err)
		}
		before := currentSnapshot(t, s)
		if changed, err := s.Unlink(t.Context(), "ID-draft", "external/"+typ, typ); err != nil || changed || currentSnapshot(t, s).digest != before.digest {
			t.Fatalf("unlink no-op %s: %t %v", typ, changed, err)
		}
	}
	if edges, err := s.ListEdges(t.Context(), "ID-draft", ""); err != nil || len(edges) != 0 {
		t.Fatalf("relationships remain: %v %v", edges, err)
	}
}

func TestTypedUnlinkRemovesAllSourcesOnlyOfMatchingType(t *testing.T) {
	s, original := importRelationships(t)
	for _, pair := range [][2]string{{"WS-ambiguous", "belongs_to"}, {"DEC-example", "decision_gates"}, {"ID-old", "supersedes"}, {"external-ref", "related"}, {"ID-second", "depends_on"}} {
		if changed, err := s.Unlink(t.Context(), "ID-example", pair[0], pair[1]); err != nil || !changed {
			t.Fatalf("unlink %v: %t %v", pair, changed, err)
		}
	}
	got := currentSnapshot(t, s)
	item := snapshotItem(t, got, "ideas", "ID-example")
	if !reflect.DeepEqual(item["related_ids"], []any{"WS-ambiguous", "WS-ambiguous"}) || !reflect.DeepEqual(item["workstream_ids"], []any{"WS-other"}) || !reflect.DeepEqual(item["decision_ids"], []any{"DEC-dangling"}) {
		t.Fatalf("legacy fields changed incorrectly: %v", item)
	}
	if !reflect.DeepEqual(item["links"], []any{map[string]any{"kind": "url", "ref": "WS-ambiguous", "label": "different meaning"}}) {
		t.Fatal("workstream unlink removed different link kind")
	}
	before := snapshotItem(t, original, "ideas", "ID-example")
	for _, field := range []string{"comments", "unknown", "decision", "status", "project_ids"} {
		if !reflect.DeepEqual(item[field], before[field]) {
			t.Fatalf("lost historical %s", field)
		}
	}
	metadata := item[relationshipField].(map[string]any)
	if !reflect.DeepEqual(metadata["unknown"], map[string]any{"preserve": true}) || !reflect.DeepEqual(metadata["edges"], []any{map[string]any{"type": "informs", "target": "DEC-example", "note": "retain entry"}}) {
		t.Fatal("unlink lost unrelated metadata or unknown entry fields")
	}
	if _, exists := item["supersedes"]; exists {
		t.Fatal("scalar supersedes remains")
	}
	if ids, err := s.Search(t.Context(), "historic comment"); err != nil || !reflect.DeepEqual(ids, []string{"ID-example"}) {
		t.Fatalf("search/comment projection lost: %v %v", ids, err)
	}
}

func TestTypedMutationRollbackNoOpAndErrors(t *testing.T) {
	s, original := importRelationships(t)
	for _, call := range []func() (bool, error){
		func() (bool, error) { return s.Link(t.Context(), "ID-example", "DEC-example", "decision_gates") },
		func() (bool, error) { return s.Unlink(t.Context(), "ID-example", "absent", "related") },
	} {
		if changed, err := call(); err != nil || changed {
			t.Fatalf("no-op: %t %v", changed, err)
		}
	}
	if _, err := s.Import(t.Context(), original); err != nil {
		t.Fatalf("no-op marked receipt mutated: %v", err)
	}
	if _, err := s.Link(t.Context(), "missing", "external", "related"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("missing source: %v", err)
	}
	if _, err := s.Link(t.Context(), "ID-example", "external", "invented"); !errors.Is(err, ErrInvalidRelationship) {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := s.Unlink(t.Context(), "ID-example", "", "related"); !errors.Is(err, ErrInvalidRelationship) {
		t.Fatalf("empty target: %v", err)
	}
	if _, err := s.ListEdges(t.Context(), "missing", ""); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("missing list source: %v", err)
	}
	if _, err := s.Backlinks(t.Context(), "DEC-example", "invalid"); !errors.Is(err, ErrInvalidRelationship) {
		t.Fatalf("invalid backlink type: %v", err)
	}
	for _, trigger := range []string{
		`CREATE TRIGGER fail_mutation BEFORE INSERT ON edges WHEN NEW.field='_portfolio_relationships' BEGIN SELECT RAISE(ABORT,'edge fault'); END`,
		`CREATE TRIGGER fail_mutation BEFORE UPDATE ON import_receipts BEGIN SELECT RAISE(ABORT,'receipt fault'); END`,
	} {
		if _, err := s.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Link(t.Context(), "ID-example", "new-search-target", "informs"); err == nil {
			t.Fatal("expected injected failure")
		}
		if _, err := s.Unlink(t.Context(), "ID-example", "DEC-example", "decision_gates"); err == nil {
			t.Fatal("expected injected unlink failure")
		}
		if err := s.Verify(t.Context(), original); err != nil {
			t.Fatalf("rollback lost source/projections: %v", err)
		}
		if _, err := s.Import(t.Context(), original); err != nil {
			t.Fatalf("failed mutation marked receipt: %v", err)
		}
		if _, err := s.db.Exec("DROP TRIGGER fail_mutation"); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := s.Link(t.Context(), "ID-example", "new-search-target", "informs"); err != nil || !changed {
		t.Fatalf("retry: %t %v", changed, err)
	}
	currentSnapshot(t, s)
	if _, err := s.db.Exec("DELETE FROM edges WHERE field='decision_ids'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Link(t.Context(), "ID-example", "new-other", "related"); err == nil {
		t.Fatal("mutation concealed prior projection drift")
	}
}

func TestTypedMetadataValidation(t *testing.T) {
	snap := fixture(t)
	files := map[string][]byte{}
	for name, env := range snap.envelopes {
		files[name] = []byte(encode(env))
	}
	for _, metadata := range []string{`null`, `{"version":2,"edges":[]}`, `{"version":1,"edges":null}`, `{"version":1,"edges":[{"type":"unknown","target":"x"}]}`, `{"version":1,"edges":[{"type":"related","target":""}]}`} {
		files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","items":[{"id":"ID-test","title":"Test","_portfolio_relationships":` + metadata + `}]}`)
		if _, err := ParseSnapshot(files); err == nil {
			t.Fatalf("accepted malformed metadata %s", metadata)
		}
	}
}

func TestConcurrentTypedMutationsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err = a.Import(t.Context(), relationshipFixture(t)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, s := range []*Store{a, b} {
		wg.Go(func() {
			changed, e := s.Link(context.Background(), "ID-draft", "same", "related")
			results <- changed
			errs <- e
		})
	}
	wg.Wait()
	applied := 0
	for range 2 {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
		if <-results {
			applied++
		}
	}
	if applied != 1 {
		t.Fatal("concurrent duplicate link was not idempotent")
	}
	for i, s := range []*Store{a, b} {
		wg.Go(func() {
			target := []string{"independent-a", "independent-b"}[i]
			_, e := s.Link(context.Background(), "ID-draft", target, "informs")
			errs <- e
		})
	}
	wg.Wait()
	for range 2 {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
	}
	snap := currentSnapshot(t, a)
	item := snapshotItem(t, snap, "ideas", "ID-draft")
	if item["rev"] != json.Number("3") {
		t.Fatalf("concurrent lost revision: %v", item["rev"])
	}
	back, err := b.Backlinks(t.Context(), "independent-a", "informs")
	if err != nil || !reflect.DeepEqual(back, []Edge{{"ID-draft", "independent-a", "informs", relationshipField}}) {
		t.Fatalf("lost concurrent update: %v %v", back, err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Verify(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
}

func TestRelationshipsUpgradeExistingFoundation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	body, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,digest TEXT NOT NULL); INSERT INTO schema_migrations VALUES(1,?)", hashBytes(body)); err != nil {
		t.Fatal(err)
	}
	snap := relationshipFixture(t)
	// Old foundation source did not support additive metadata; omit it here.
	delete(snapshotItem(t, snap, "ideas", "ID-example"), relationshipField)
	files := map[string][]byte{}
	for name, env := range snap.envelopes {
		files[name] = []byte(encode(env))
	}
	snap, err = ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	legacyInserts := map[string]string{
		"envelopes":       "INSERT INTO envelopes(db,data) VALUES (?,?)",
		"id_reservations": "INSERT INTO id_reservations(id,db) VALUES (?,?)",
		"items":           "INSERT INTO items(id,db,ordinal,title,status,rev,sort_order,created,updated,author,data) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"comments":        childWrites["comments"].insert,
		"links":           childWrites["links"].insert,
		"edges":           childWrites["edges"].insert,
	}
	for _, table := range projectionTables {
		if table == "items_fts" {
			continue
		}
		for _, row := range snap.projections[table] {
			if table == "edges" && row[3] == "links.workstream" {
				continue
			}
			if _, err = db.Exec(legacyInserts[table], row...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = db.Exec("INSERT INTO import_receipts(digest,imported_at) VALUES (?,'original-time')", snap.digest); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Verify(t.Context(), snap); err != nil {
		t.Fatalf("migration lost source/parity: %v", err)
	}
	if changed, replayErr := s.Import(t.Context(), snap); replayErr != nil || changed {
		t.Fatalf("migration changed receipt: %t %v", changed, replayErr)
	}
	var importedAt string
	if err = s.db.QueryRow("SELECT imported_at FROM import_receipts").Scan(&importedAt); err != nil || importedAt != "original-time" {
		t.Fatalf("original receipt changed: %s %v", importedAt, err)
	}
	if currentSnapshot(t, s).digest != snap.digest {
		t.Fatal("migration rewrote historical fields")
	}
}
