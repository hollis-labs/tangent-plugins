package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestShadowTransactionPromotionFaultsRollbackEveryProjection(t *testing.T) {
	s, snap := importRelationships(t)
	// Source and target updates model promotion without coupling the persistence
	// fault tests to the service handler. Both child sets and FTS must roll back.
	apply := func(state *State) error {
		for _, v := range state.Envelopes["ideas"]["items"].([]any) {
			item := v.(map[string]any)
			if item["id"] == "ID-example" {
				item["status"] = "promoted"
				item["promoted_to"] = map[string]any{"db": "risks", "id": "RK-new"}
				item["related_ids"] = []any{"RK-new"}
				item["rev"] = json.Number("1")
			}
		}
		state.Envelopes["risks"]["items"] = append(state.Envelopes["risks"]["items"].([]any), map[string]any{"id": "RK-new", "title": "New search target", "related_ids": []any{"ID-example"}, "comments": []any{map[string]any{"id": "c-1", "author": "test", "text": "searchable"}}})
		state.Envelopes["risks"]["updated"] = "2026-10-08"
		return nil
	}
	for _, trigger := range []string{
		`CREATE TRIGGER fail_service BEFORE INSERT ON items WHEN NEW.id='RK-new' BEGIN SELECT RAISE(ABORT,'target fault'); END`,
		`CREATE TRIGGER fail_service BEFORE INSERT ON edges WHEN NEW.from_id='RK-new' BEGIN SELECT RAISE(ABORT,'target child fault'); END`,
		`CREATE TRIGGER fail_service BEFORE UPDATE ON envelopes BEGIN SELECT RAISE(ABORT,'envelope fault'); END`,
		`CREATE TRIGGER fail_service BEFORE UPDATE ON import_receipts BEGIN SELECT RAISE(ABORT,'receipt fault'); END`,
	} {
		if _, err := s.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		if err := s.Transact(t.Context(), apply); err == nil {
			t.Fatal("expected injected fault")
		}
		if err := s.Verify(t.Context(), snap); err != nil {
			t.Fatalf("rollback lost source/target/reservation/FTS: %v", err)
		}
		if _, err := s.Import(t.Context(), snap); err != nil {
			t.Fatalf("rollback marked receipt: %v", err)
		}
		if _, err := s.db.Exec("DROP TRIGGER fail_service"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Transact(t.Context(), apply); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(t.Context(), snap); err == nil {
		t.Fatal("mutated shadow replay reset")
	}
	currentSnapshot(t, s)
	ids, err := s.Search(t.Context(), "target searchable")
	if err != nil || !reflect.DeepEqual(ids, []string{"RK-new"}) {
		t.Fatalf("FTS lost atomic target: %v %v", ids, err)
	}
}
func TestShadowTransactionDeletionReorderAndPriorDriftRefuse(t *testing.T) {
	s, snap := importRelationships(t)
	for _, apply := range []func(*State) error{
		func(state *State) error { state.Envelopes["ideas"]["items"] = []any{}; return nil },
		func(state *State) error {
			items := state.Envelopes["ideas"]["items"].([]any)
			items[0], items[1] = items[1], items[0]
			return nil
		},
		func(state *State) error {
			state.Envelopes["ideas"]["items"].([]any)[0].(map[string]any)["title"] = "Changed"
			return errors.New("abort callback")
		},
	} {
		if err := s.Transact(t.Context(), apply); err == nil {
			t.Fatal("unsafe mutation accepted")
		}
		if err := s.Verify(t.Context(), snap); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("DELETE FROM comments WHERE item_id='ID-example'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(t.Context(), func(*State) error { return nil }); err == nil {
		t.Fatal("mutation concealed drift")
	}
}
func TestShadowTransactionNoOpPreservesReceiptAndProjects(t *testing.T) {
	s, snap := importRelationships(t)
	if _, err := s.db.Exec(`INSERT INTO project_overlays(urn,data,rev) VALUES('project://fixture','{"notes":"preserve"}',7)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(t.Context(), func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(t.Context(), func(state *State) error {
		state.Envelopes["ideas"]["items"].([]any)[0].(map[string]any)["unknown"] = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var data string
	var rev int
	if err := s.db.QueryRowContext(context.Background(), "SELECT data,rev FROM project_overlays WHERE urn='project://fixture'").Scan(&data, &rev); err != nil || rev != 7 || data != `{"notes":"preserve"}` {
		t.Fatalf("Projects changed: %s %d %v", data, rev, err)
	}
}
