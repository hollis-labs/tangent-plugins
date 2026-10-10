package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func fixture(t *testing.T) *Snapshot {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range databaseNames {
		files[name] = []byte(`{"schema":"portfolio/` + name + `@1","updated":"2026-10-09","unknown":{"large":9007199254740993},"items":[]}`)
	}
	files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","items":[{"id":"ID-example","title":"Migration abcdef","status":"draft","order":1.5,"unknown":null,"nested":{"ignored_search":"hidden"},"comments":[{"id":"c-7","author":"historic","text":"Memory important","created":"2026-10-09T23:59:59Z","x":true}],"links":[{"kind":"url","ref":"https://example.invalid","label":"External"}],"related_ids":["WS-missing","WS-missing"],"decision_ids":["DEC-example"],"depends_on":["ID-second"],"torque_project_ids":["PRJ-20261009-0001"]},{"id":"ID-second","title":"Second","status":"draft","rev":3,"order":0,"created":"2026-10-09","tags":["misc"]}]}`)
	files["decisions"] = []byte(`{"schema":"portfolio/decisions@1","items":[{"id":"DEC-example","title":"Choose","status":"decided","supersedes":"DEC-old","comments":null,"links":[]}]}`)
	snapshot, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestRoundTripReplayAndSearch(t *testing.T) {
	s := openTest(t)
	snap := fixture(t)
	applied, err := s.Import(t.Context(), snap)
	if err != nil || !applied {
		t.Fatalf("import: %v %v", applied, err)
	}
	var before string
	if err = s.db.QueryRow("SELECT imported_at FROM import_receipts").Scan(&before); err != nil {
		t.Fatal(err)
	}
	applied, err = s.Import(t.Context(), snap)
	if err != nil || applied {
		t.Fatalf("replay: %v %v", applied, err)
	}
	var after string
	_ = s.db.QueryRow("SELECT imported_at FROM import_receipts").Scan(&after)
	if before != after {
		t.Fatal("replay changed receipt")
	}
	exported, err := s.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := ParseSnapshot(exported)
	if err != nil {
		t.Fatal(err)
	}
	if snap.digest != roundtrip.digest {
		t.Fatal("export loses source semantics")
	}
	for q, want := range map[string][]string{"gra ABC": {"ID-example"}, "important external": {"ID-example"}, "hidden": {}, "second": {"ID-example", "ID-second"}, "a": {"ID-example", "ID-second", "DEC-example"}} {
		got, searchErr := s.Search(t.Context(), q)
		if searchErr != nil {
			t.Fatal(searchErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("search %q got %v want %v", q, got, want)
		}
	}
	var text string
	if err = s.db.QueryRow("SELECT id FROM items_fts WHERE items_fts MATCH 'migration'").Scan(&text); err != nil || text != "ID-example" {
		t.Fatalf("FTS5 unavailable: %s %v", text, err)
	}
}

func TestAtomicFailureAndRetry(t *testing.T) {
	s := openTest(t)
	snap := fixture(t)
	_, err := s.db.Exec(`CREATE TRIGGER fail_link BEFORE INSERT ON links BEGIN SELECT RAISE(ABORT,'injected'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(t.Context(), snap); err == nil {
		t.Fatal("expected fault")
	}
	for _, table := range []string{"items", "envelopes", "id_reservations", "comments", "items_fts", "import_receipts"} {
		var n int
		if err = s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rollback %s: %d %v", table, n, err)
		}
	}
	_, err = s.db.Exec("DROP TRIGGER fail_link")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
}
func TestProjectionDriftRefusesReplayAndExport(t *testing.T) {
	s := openTest(t)
	snap := fixture(t)
	if _, err := s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM edges WHERE field='decision_ids'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(t.Context(), snap); err == nil {
		t.Fatal("drift was hidden")
	}
	if _, err := s.Import(t.Context(), snap); err == nil {
		t.Fatal("replay masked drift")
	}
	if _, err := s.Export(t.Context()); err == nil {
		t.Fatal("export masked drift")
	}
}
func TestIdentityReservationsAndChangedSnapshot(t *testing.T) {
	s := openTest(t)
	snap := fixture(t)
	if _, err := s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"DELETE FROM items", "DELETE FROM id_reservations", "UPDATE id_reservations SET id='new'", "UPDATE items SET db='inbox'"} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatalf("accepted destructive identity change %s", query)
		}
	}
	files, err := s.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	files["ideas"] = []byte(strings.Replace(string(files["ideas"]), "Second", "Changed", 1))
	changed, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(t.Context(), changed); err == nil {
		t.Fatal("implicit refresh accepted")
	}
	if err = s.Verify(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
}
func TestMigrationsReopenAndDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	snap := fixture(t)
	if _, err = s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	for pragma, want := range map[string]int{"foreign_keys": 1, "synchronous": 2} {
		var got int
		if err = s.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s %d %v", pragma, got, err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Verify(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE schema_migrations SET digest='changed'"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(t.Context(), path); err == nil {
		s.Close()
		t.Fatal("changed migration admitted")
	}
}
func TestConcurrentImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
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
	snap := fixture(t)
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, s := range []*Store{a, b} {
		wg.Go(func() { ok, err := s.Import(context.Background(), snap); results <- ok; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	applied := 0
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for ok := range results {
		if ok {
			applied++
		}
	}
	if applied != 1 {
		t.Fatal("concurrent import did not apply once")
	}
}
func TestRejectsAmbiguousSource(t *testing.T) {
	s := fixture(t)
	files := map[string][]byte{}
	for name, env := range s.envelopes {
		files[name] = []byte(encode(env))
	}
	cases := []string{`{"schema":"portfolio/ideas@1","items":[],"items":[]}`, `{"schema":"portfolio/ideas@1","items":[{"id":"DEC-example","title":"duplicate"}]}`, `{"schema":"portfolio/ideas@1","items":[{"id":"bad","title":"Bad","rev":-1}]}`, `{"schema":"portfolio/ideas@1","items":[{"id":"bad","title":"Bad","comments":42}]}`}
	for _, input := range cases {
		files["ideas"] = []byte(input)
		if _, err := ParseSnapshot(files); err == nil {
			t.Fatal("ambiguous snapshot accepted")
		}
	}
}
func TestCopyReaderDoesNotWriteAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	snap := fixture(t)
	for name, env := range snap.envelopes {
		body := []byte(encode(env))
		if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.digest != snap.digest {
		t.Fatal("reader changed semantics")
	}
	if err = os.Remove(filepath.Join(dir, "ideas.json")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(dir, "inbox.json"), filepath.Join(dir, "ideas.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadSnapshot(dir); err == nil {
		t.Fatal("symlink source accepted")
	}
}
func TestNumberAndMissingFieldsPreserved(t *testing.T) {
	snap := fixture(t)
	v := snap.envelopes["priorities"]["unknown"].(map[string]any)["large"]
	if v != json.Number("9007199254740993") {
		t.Fatal("large number changed")
	}
	item := snap.envelopes["ideas"]["items"].([]any)[0].(map[string]any)
	if _, ok := item["rev"]; ok {
		t.Fatal("default rev added")
	}
}

func TestExactRevisionNumbersPreserveJSONAndRefuseRounding(t *testing.T) {
	for _, rev := range []string{"1.0", "1e0", "10e-1", "0.000", "9223372036854775807"} {
		files := map[string][]byte{}
		for _, db := range databaseNames {
			files[db] = []byte(`{"schema":"portfolio/` + db + `@1","items":[]}`)
		}
		files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","items":[{"id":"ID-number","title":"Exact","rev":` + rev + `,"unknown":9007199254740993}]}`)
		snap, err := ParseSnapshot(files)
		if err != nil {
			t.Fatalf("%s: %v", rev, err)
		}
		s := openTest(t)
		if _, err = s.Import(t.Context(), snap); err != nil {
			t.Fatal(err)
		}
		exported, err := s.Export(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		env, err := decodeObject(exported["ideas"])
		if err != nil {
			t.Fatal(err)
		}
		item := env["items"].([]any)[0].(map[string]any)
		if item["rev"] != json.Number(rev) || item["unknown"] != json.Number("9007199254740993") {
			t.Fatalf("JSON numbers changed: %v", item)
		}
	}
	for _, rev := range []string{"1.00000000000000000001", "9007199254740992.1", "1e-999", "9223372036854775808", "9.2233720368547758071e18", "-1"} {
		files := map[string][]byte{}
		for _, db := range databaseNames {
			files[db] = []byte(`{"schema":"portfolio/` + db + `@1","items":[]}`)
		}
		files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","items":[{"id":"ID-number","title":"Refuse","rev":` + rev + `}]}`)
		if _, err := ParseSnapshot(files); err == nil {
			t.Fatalf("silently rounded/refused range %s", rev)
		}
	}
}
