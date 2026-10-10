package storage

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func oldSearchStore(t *testing.T) (string, *sql.DB, *Snapshot) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,digest TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"001_initial.sql", "002_relationships.sql", "003_projects.sql"} {
		body, readErr := migrations.ReadFile("migrations/" + file)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = db.Exec(string(body)); err != nil {
			t.Fatal(err)
		}
		version := int(file[2] - '0')
		if _, err = db.Exec("INSERT INTO schema_migrations VALUES(?,?)", version, hashBytes(body)); err != nil {
			t.Fatal(err)
		}
	}
	snap := relationshipFixture(t)
	item := snapshotItem(t, snap, "ideas", "ID-example")
	item["notes"] = "İ ΟΣ"
	item["rev"] = json.Number("23")
	files := map[string][]byte{}
	for name, env := range snap.envelopes {
		files[name] = []byte(encode(env))
	}
	snap, err = ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	shadow := &Store{db: db}
	if _, err = shadow.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	for _, row := range snap.projections["items_fts"] {
		oldText := strings.ReplaceAll(strings.ReplaceAll(row[1].(string), "i\u0307", "i"), "ος", "οσ")
		if _, err = db.Exec("UPDATE items_fts SET text=? WHERE id=?", oldText, row[0]); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`UPDATE import_receipts SET digest='original-digest',imported_at='original-import',mutated_at='original-mutation'`,
		`INSERT INTO projects(urn,source_data,enrichment,registry_state,torque_state,fetched_at) VALUES('project://fixture','{"urn":"project://fixture"}','{}','fresh','fresh','original-fetch')`,
		`INSERT INTO project_overlays(urn,data,rev) VALUES('project://fixture','{"notes":"preserve"}',9)`,
		`INSERT INTO project_sync_state(source,evidence,fetched_at) VALUES('registry','{"complete":true}','original-sync')`,
		`INSERT INTO project_source_partitions(source,partition_key) VALUES('registry','opaque-original')`,
	} {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	return path, db, snap
}
func captureTables(t *testing.T, db *sql.DB, tables []string) map[string][][]any {
	t.Helper()
	out := map[string][][]any{}
	for _, table := range tables {
		queries := map[string]string{"envelopes": "SELECT * FROM envelopes", "items": "SELECT * FROM items", "id_reservations": "SELECT * FROM id_reservations", "comments": "SELECT * FROM comments", "links": "SELECT * FROM links", "edges": "SELECT * FROM edges", "import_receipts": "SELECT * FROM import_receipts", "legacy_projects": "SELECT * FROM legacy_projects", "projects": "SELECT * FROM projects", "project_overlays": "SELECT * FROM project_overlays", "project_sync_state": "SELECT * FROM project_sync_state", "project_source_partitions": "SELECT * FROM project_source_partitions", "items_fts": "SELECT * FROM items_fts", "schema_migrations": "SELECT * FROM schema_migrations"}
		rows, err := db.Query(queries[table])
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		all := [][]any{}
		for rows.Next() {
			v := make([]any, len(cols))
			ptr := make([]any, len(cols))
			for i := range v {
				ptr[i] = &v[i]
			}
			if err = rows.Scan(ptr...); err != nil {
				t.Fatal(err)
			}
			all = append(all, v)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err = rows.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Slice(all, func(i, j int) bool { return encode(all[i]) < encode(all[j]) })
		out[table] = all
	}
	return out
}

var preservedTables = []string{"envelopes", "items", "id_reservations", "comments", "links", "edges", "import_receipts", "legacy_projects", "projects", "project_overlays", "project_sync_state", "project_source_partitions"}

func TestSearchV4UpgradePreservesAuthoredStateAndCorrectsUnicode(t *testing.T) {
	path, db, snap := oldSearchStore(t)
	before := captureTables(t, db, preservedTables)
	var hit string
	if err := db.QueryRow("SELECT id FROM items_fts WHERE instr(text,?)>0", "i\u0307").Scan(&hit); err == nil {
		t.Fatal("fixture was not pre-v4 dotted-I behavior")
	}
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	after := captureTables(t, db, preservedTables)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("upgrade rewrote authored JSON/revisions/receipt/Projects/relationships/reservations")
	}
	if err = store.Verify(t.Context(), snap); err != nil {
		t.Fatalf("upgrade projection: %v", err)
	}
	for _, q := range []string{"i\u0307", "ος"} {
		ids, searchErr := store.Search(t.Context(), q)
		if searchErr != nil || !reflect.DeepEqual(ids, []string{"ID-example"}) {
			t.Fatalf("corrected search %s: %v %v", q, ids, searchErr)
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Verify(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Import(t.Context(), snap); err == nil {
		t.Fatal("upgrade lost prior mutated receipt refusal")
	}
}
func TestSearchV4MigrationFaultIsAtomicAndRetryable(t *testing.T) {
	path, db, snap := oldSearchStore(t)
	if _, err := db.Exec(`CREATE TRIGGER fail_search_upgrade BEFORE UPDATE ON items WHEN NEW.id='ID-example' BEGIN SELECT RAISE(ABORT,'upgrade fault'); END`); err != nil {
		t.Fatal(err)
	}
	tables := append(append([]string{}, preservedTables...), "items_fts", "schema_migrations")
	before := captureTables(t, db, tables)
	if store, err := Open(t.Context(), path); err == nil {
		_ = store.Close()
		t.Fatal("expected migration fault")
	}
	if after := captureTables(t, db, tables); !reflect.DeepEqual(before, after) {
		t.Fatal("failed migration changed state/index/version receipts")
	}
	if _, err := db.Exec("DROP TRIGGER fail_search_upgrade"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Verify(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
}
