package operations

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

func TestActualPromotionRollsBackAfterTargetAndSourceWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	store, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	files := fixtureFiles()
	files["inbox"] = []byte(`{"schema":"portfolio/inbox@1","updated":"2020-01-01","items":[{"id":"IN-001","title":"Promote","status":"new","body":"search target","path":"/never/read"}]}`)
	snap, err := storage.ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, Verify: newService(t).Verify, Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	original, err := store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// ID-created sorts before IN-001, so source/receipt faults happen after the
	// target reservation, item, links, backlink and FTS have actually been written.
	for _, trigger := range []string{
		`CREATE TRIGGER promotion_fault BEFORE UPDATE ON items WHEN NEW.id='IN-001' BEGIN SELECT RAISE(ABORT,'source fault'); END`,
		`CREATE TRIGGER promotion_fault BEFORE INSERT ON edges WHEN NEW.from_id='IN-001' BEGIN SELECT RAISE(ABORT,'source edge fault'); END`,
		`CREATE TRIGGER promotion_fault BEFORE UPDATE ON import_receipts BEGIN SELECT RAISE(ABORT,'receipt fault'); END`,
	} {
		if _, err = db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		errorCode(t, s, "inbox_promote", object{"id": "IN-001", "to_db": "ideas", "fields": object{"id": "ID-created"}}, "unavailable")
		exported, exportErr := store.Export(t.Context())
		if exportErr != nil || !reflect.DeepEqual(original, exported) {
			t.Fatalf("promotion partially committed: %v", exportErr)
		}
		if _, err = store.Import(t.Context(), snap); err != nil {
			t.Fatalf("failed promotion marked receipt: %v", err)
		}
		if _, err = db.Exec("DROP TRIGGER promotion_fault"); err != nil {
			t.Fatal(err)
		}
	}
	result := itemCall(t, s, "inbox_promote", object{"id": "IN-001", "to_db": "ideas", "fields": object{"id": "ID-created"}})
	expect(t, result["item"].(object)["id"], "ID-created")
	expect(t, result["inbox"].(object)["promoted_to"], object{"db": "ideas", "id": "ID-created"})
	expect(t, listIDs(call(t, s, "search", object{"q": "search target", "db": "ideas"})), []string{"ID-created"})
	if _, err = store.Import(t.Context(), snap); err == nil {
		t.Fatal("successful promotion permitted replay")
	}
}
func TestLockedErrorFromAnotherImmediateWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	store, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap, err := storage.ParseSnapshot(fixtureFiles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(t.Context(), "ROLLBACK")
	s := newService(t)
	s.Store = store
	errorCode(t, s, "update", object{"db": "ideas", "id": "ID-y", "patch": object{}}, "locked")
}
func TestUnicodeWhitespaceUnknownFieldsAndUTF16ListOrder(t *testing.T) {
	s := newService(t)
	created := itemCall(t, s, "create", object{"db": "ideas", "item": object{"title": "Unicode", "kind": "idea", "notes": "İ ΟΣ a\u0085b", "arbitrary_ids": object{"value": nil}, "depends_on": 42, "mixed_ids": []any{nil, "", 7, "external"}, "supersedes": object{"retain": true}}})
	expect(t, created["arbitrary_ids"], object{"value": nil})
	expect(t, created["mixed_ids"], []any{nil, "", 7, "external"})
	for _, q := range []string{"i\u0307", "ος", "\ufeffΟΣ\ufeff", "a\u0085b"} {
		expect(t, listIDs(call(t, s, "search", object{"q": q})), []string{"ID-unicode"})
	}
	errorCode(t, s, "comment", object{"db": "ideas", "id": "ID-y", "text": "\ufeff"}, "invalid")
	a := itemCall(t, s, "link_add", object{"db": "ideas", "id": "ID-y", "link": object{"kind": "file", "ref": "\ufeff/never/read\ufeff"}})
	expect(t, a["links"], []any{object{"kind": "file", "ref": "/never/read"}})
	errorCode(t, s, "link_add", object{"db": "ideas", "id": "ID-y", "link": object{"kind": "url", "ref": "https://example.invalid/\u00a0bad"}}, "invalid")
	call(t, s, "create", object{"db": "ideas", "item": object{"id": "ID-astral", "title": "𐀀", "kind": "idea"}})
	call(t, s, "create", object{"db": "ideas", "item": object{"id": "ID-bmp", "title": "\ue000", "kind": "idea"}})
	sorted := listIDs(call(t, s, "list", object{"db": "ideas", "sort": "title"}))
	if sorted[len(sorted)-2] != "ID-astral" || sorted[len(sorted)-1] != "ID-bmp" {
		t.Fatalf("UTF-16 order lost: %v", sorted)
	}
}
func TestConcurrentPromotionOneWinner(t *testing.T) {
	// The same source cannot create two targets even when separate handles race.
	a := newService(t)
	in := itemCall(t, a, "inbox_add", object{"title": "Same source"})
	exported, err := a.Store.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := storage.ParseSnapshot(exported)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "promotion.db")
	left, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	if _, err = left.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, store := range []*storage.Store{left, right} {
		go func() {
			service := *a
			service.Store = store
			_, err := service.Call(t.Context(), Caller{"test-binding"}, "inbox_promote", object{"id": in["id"], "to_db": "ideas"})
			errs <- err
		}()
	}
	succeeded := false
	conflicted := false
	for range 2 {
		err := <-errs
		if err == nil {
			succeeded = true
		} else {
			var e *Error
			if !errors.As(err, &e) || e.Code != "conflict" {
				t.Fatal(err)
			}
			conflicted = true
		}
	}
	if !succeeded || !conflicted {
		t.Fatal("promotion did not admit one writer")
	}
	expect(t, listIDs(call(t, &Service{Store: left}, "list", object{"db": "ideas", "filters": object{"related_ids": in["id"]}})), []string{"ID-same-source"})
}

func TestDeclaredDependsOnValidationAndOpaqueFields(t *testing.T) {
	s := newService(t)
	opaque := itemCall(t, s, "create", object{"db": "ideas", "item": object{"title": "Opaque", "kind": "idea", "depends_on": 42, "unknown_array": []any{object{"text": 1, "label": true}, object{"text": object{"value": 1}}, object{"text": []any{"word", nil, "last"}}}}})
	expect(t, opaque["depends_on"], 42)
	for _, q := range []string{"1 true", "[object", "word,,last"} {
		expect(t, listIDs(call(t, s, "search", object{"q": q, "db": "ideas"})), []string{"ID-opaque"})
	}
	for _, value := range []any{42, []any{42}, []any{nil}} {
		errorCode(t, s, "update", object{"db": "roadmap", "id": "RM-a", "patch": object{"depends_on": value}}, "invalid")
	}
	empty := itemCall(t, s, "update", object{"db": "roadmap", "id": "RM-a", "patch": object{"depends_on": []any{""}}})
	expect(t, empty["depends_on"], []string{""})
	// Format annotations are descriptive in Node's subset validator.
	dated := itemCall(t, s, "update", object{"db": "roadmap", "id": "RM-a", "patch": object{"created_unknown": "future", "updated": "not-a-date"}})
	expect(t, dated["updated"], "2026-10-08")
}
