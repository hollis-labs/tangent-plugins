package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/textcompat"
)

var projectionTables = []string{"envelopes", "id_reservations", "items", "comments", "links", "edges", "items_fts"}
var columns = map[string]string{
	"envelopes": "db,data", "id_reservations": "id,db",
	"items":    "id,db,ordinal,title,status,rev,sort_order,created,updated,author,data",
	"comments": "item_id,n,id,author,text,created,kind,data", "links": "item_id,n,kind,ref,label,data",
	"edges": "from_id,to_id,type,field", "items_fts": "id,text",
}

// Import bootstraps a fresh shadow database or verifies an exact replay. Changed
// snapshots require a fresh shadow file, never an implicit destructive refresh.
func (s *Store) Import(ctx context.Context, snapshot *Snapshot) (bool, error) {
	if snapshot == nil {
		return false, errors.New("snapshot required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing string
	var mutated sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT digest,mutated_at FROM import_receipts").Scan(&existing, &mutated)
	if err == nil {
		if mutated.Valid {
			return false, errors.New("shadow relationships changed since import; choose a fresh shadow database for replay")
		}
		if existing != snapshot.digest {
			return false, errors.New("different snapshot already imported; choose a fresh shadow database")
		}
		if err = verify(ctx, tx, snapshot); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var occupied int
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM items)+(SELECT count(*) FROM id_reservations)+(SELECT count(*) FROM envelopes)").Scan(&occupied); err != nil {
		return false, err
	}
	if occupied != 0 {
		return false, errors.New("bootstrap requires an empty store")
	}
	for _, table := range projectionTables {
		if table == "items_fts" {
			continue
		}
		for _, row := range snapshot.projections[table] {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(row)), ",")
			// #nosec G202 -- table/columns are fixed internal projection definitions; values are bound.
			if _, err = tx.ExecContext(ctx, "INSERT INTO "+table+" ("+columns[table]+") VALUES ("+placeholders+")", row...); err != nil {
				return false, fmt.Errorf("import %s: %w", table, err)
			}
		}
	}
	if err = verify(ctx, tx, snapshot); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO import_receipts(digest,imported_at) VALUES (?,strftime('%Y-%m-%dT%H:%M:%fZ','now'))", snapshot.digest); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Verify compares complete projected rows, ids, comments, links, edge sets and
// search text against the selected snapshot in one consistent transaction.
func (s *Store) Verify(ctx context.Context, snapshot *Snapshot) error {
	if snapshot == nil {
		return errors.New("snapshot required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = verify(ctx, tx, snapshot); err != nil {
		return err
	}
	return tx.Commit()
}
func verify(ctx context.Context, tx *sql.Tx, snapshot *Snapshot) error {
	for _, table := range projectionTables {
		// #nosec G202 -- only fixed internal projection tables/columns.
		rows, err := tx.QueryContext(ctx, "SELECT "+columns[table]+" FROM "+table)
		if err != nil {
			return err
		}
		actual := [][]any{}
		for rows.Next() {
			values := make([]any, len(strings.Split(columns[table], ",")))
			ptrs := make([]any, len(values))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				_ = rows.Close()
				return err
			}
			actual = append(actual, values)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		sortProjectionRows(actual)
		want := snapshot.projections[table]
		if len(actual) != len(want) || !equalRows(actual, want) {
			return fmt.Errorf("parity mismatch in %s", table)
		}
	}
	return nil
}
func equalRows(a, b [][]any) bool {
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Export rebuilds each envelope in imported order without adding defaults.
func (s *Store) Export(ctx context.Context) (map[string][]byte, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := exportTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func exportTx(ctx context.Context, tx *sql.Tx) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, name := range databaseNames {
		var data string
		if err := tx.QueryRowContext(ctx, "SELECT data FROM envelopes WHERE db=?", name).Scan(&data); err != nil {
			return nil, err
		}
		envelope, err := decodeObject([]byte(data))
		if err != nil {
			return nil, err
		}
		items := []any{}
		rows, queryErr := tx.QueryContext(ctx, "SELECT data FROM items WHERE db=? ORDER BY ordinal", name)
		if queryErr != nil {
			return nil, queryErr
		}
		for rows.Next() {
			if err = rows.Scan(&data); err != nil {
				_ = rows.Close()
				return nil, err
			}
			var item map[string]any
			item, err = decodeObject([]byte(data))
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			items = append(items, item)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err = rows.Close(); err != nil {
			return nil, err
		}
		envelope["items"] = items
		out[name] = []byte(encode(envelope))
	}
	snapshot, err := ParseSnapshot(out)
	if err != nil {
		return nil, err
	}
	if err = verify(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	return out, nil
}

// Search retains AND-of-substrings behavior, including short terms. Word MATCH
// cannot substitute for these legacy semantics; FTS5 stores the indexed text.
func (s *Store) Search(ctx context.Context, q string) ([]string, error) {
	terms := textcompat.Terms(q)
	if len(terms) == 0 {
		return nil, errors.New("search query is empty")
	}
	conditions := make([]string, len(terms))
	args := make([]any, len(terms))
	for i, term := range terms {
		conditions[i] = "instr(f.text,?)>0"
		args[i] = term
	}
	// #nosec G202 -- generated predicates are constant; every search term is a bound value.
	rows, err := s.db.QueryContext(ctx, "SELECT i.id FROM items i JOIN items_fts f ON f.id=i.id WHERE "+strings.Join(conditions, " AND ")+" ORDER BY CASE i.db WHEN 'priorities' THEN 0 WHEN 'workstreams' THEN 1 WHEN 'roadmap' THEN 2 WHEN 'ideas' THEN 3 WHEN 'decisions' THEN 4 WHEN 'risks' THEN 5 ELSE 6 END,i.ordinal", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
