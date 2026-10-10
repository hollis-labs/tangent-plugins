package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const relationshipField = "_portfolio_relationships"

var childWrites = map[string]struct{ remove, insert string }{
	"comments": {"DELETE FROM comments WHERE item_id=?", "INSERT INTO comments(item_id,n,id,author,text,created,kind,data) VALUES (?,?,?,?,?,?,?,?)"},
	"links":    {"DELETE FROM links WHERE item_id=?", "INSERT INTO links(item_id,n,kind,ref,label,data) VALUES (?,?,?,?,?,?)"},
	"edges":    {"DELETE FROM edges WHERE from_id=?", "INSERT INTO edges(from_id,to_id,type,field) VALUES (?,?,?,?)"},
}

// ErrInvalidRelationship means an empty identifier or unknown relationship type.
var ErrInvalidRelationship = errors.New("invalid portfolio relationship")

// ErrItemNotFound means the source item does not exist in this copied store.
var ErrItemNotFound = errors.New("portfolio item not found")

// Edge includes the source field: several source fields can express one pair.
// Direction is item → referenced object, including item → gating decision.
type Edge struct {
	FromID string `json:"from_id"`
	ToID   string `json:"to_id"`
	Type   string `json:"type"`
	Field  string `json:"field"`
}

func validEdgeType(typ string) bool {
	switch typ {
	case "decision_gates", "informs", "supersedes", "belongs_to", "depends_on", "related":
		return true
	}
	return false
}

func legacyEdgeType(field string) string {
	switch field {
	case "depends_on", "supersedes":
		return field
	case "decision_ids":
		return "decision_gates"
	case "project_ids", "workstream_ids":
		return "belongs_to"
	default:
		return "related"
	}
}

// Unknown metadata/entry fields survive, while unsupported versions refuse.
func relationshipMetadata(item map[string]any) (map[string]any, error) {
	v, exists := item[relationshipField]
	if !exists {
		return nil, nil
	}
	metadata, ok := v.(map[string]any)
	if !ok || metadata["version"] != json.Number("1") {
		return nil, errors.New("_portfolio_relationships requires version 1 metadata")
	}
	entries, ok := metadata["edges"].([]any)
	if !ok {
		return nil, errors.New("_portfolio_relationships edges must be an array")
	}
	for _, entry := range entries {
		edge, ok := entry.(map[string]any)
		if !ok {
			return nil, ErrInvalidRelationship
		}
		target, targetOK := edge["target"].(string)
		typ, typeOK := edge["type"].(string)
		if !targetOK || target == "" || !typeOK || !validEdgeType(typ) {
			return nil, ErrInvalidRelationship
		}
	}
	return metadata, nil
}

// ListEdges lists outgoing references, optionally filtered by type. A missing
// source is an error; external/dangling targets are intentionally allowed.
func (s *Store) ListEdges(ctx context.Context, itemID, typ string) ([]Edge, error) {
	if itemID == "" || (typ != "" && !validEdgeType(typ)) {
		return nil, ErrInvalidRelationship
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM items WHERE id=?", itemID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrItemNotFound
	} else if err != nil {
		return nil, err
	}
	edges, err := queryEdges(ctx, tx, "SELECT from_id,to_id,type,field FROM edges WHERE from_id=? AND (?='' OR type=?) ORDER BY type,to_id,field", itemID, typ, typ)
	if err != nil {
		return nil, err
	}
	return edges, tx.Commit()
}

// Backlinks lists incoming references, including to an external/dangling ID.
func (s *Store) Backlinks(ctx context.Context, targetID, typ string) ([]Edge, error) {
	if targetID == "" || (typ != "" && !validEdgeType(typ)) {
		return nil, ErrInvalidRelationship
	}
	return queryEdges(ctx, s.db, "SELECT from_id,to_id,type,field FROM edges WHERE to_id=? AND (?='' OR type=?) ORDER BY type,from_id,field", targetID, typ, typ)
}

type edgeQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryEdges(ctx context.Context, q edgeQuerier, query string, args ...any) ([]Edge, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	edges := []Edge{}
	for rows.Next() {
		var e Edge
		if err = rows.Scan(&e.FromID, &e.ToID, &e.Type, &e.Field); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// DecisionIDs is the first-class, sorted, deduplicated view of gating decisions.
// It includes unresolved references rather than creating or guessing decisions.
func (s *Store) DecisionIDs(ctx context.Context, itemID string) ([]string, error) {
	edges, err := s.ListEdges(ctx, itemID, "decision_gates")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, edge := range edges {
		if len(ids) == 0 || ids[len(ids)-1] != edge.ToID {
			ids = append(ids, edge.ToID)
		}
	}
	return ids, nil
}

// Link adds a typed relationship to versioned JSON metadata in a shadow store.
// An existing logical pair/type is a no-op even if expressed by legacy fields.
// This is an internal domain operation, not an authenticated production API.
func (s *Store) Link(ctx context.Context, fromID, toID, typ string) (bool, error) {
	return s.mutateRelationship(ctx, fromID, toID, typ, true)
}

// Unlink removes all representations of a pair/type, preserving other fields,
// relationships and unknown JSON. An absent relationship is a no-op.
func (s *Store) Unlink(ctx context.Context, fromID, toID, typ string) (bool, error) {
	return s.mutateRelationship(ctx, fromID, toID, typ, false)
}

func (s *Store) mutateRelationship(ctx context.Context, fromID, toID, typ string, link bool) (bool, error) {
	if fromID == "" || toID == "" || !validEdgeType(typ) {
		return false, ErrInvalidRelationship
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var data string
	var rev int64
	if err = tx.QueryRowContext(ctx, "SELECT data,rev FROM items WHERE id=?", fromID).Scan(&data, &rev); errors.Is(err, sql.ErrNoRows) {
		return false, ErrItemNotFound
	} else if err != nil {
		return false, err
	}
	// Verify the lossless JSON and every projection before writing: a domain
	// mutation must not silently repair or conceal preexisting storage drift.
	if _, err = exportTx(ctx, tx); err != nil {
		return false, err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM edges WHERE from_id=? AND to_id=? AND type=?)", fromID, toID, typ).Scan(&exists); err != nil {
		return false, err
	}
	if exists == link {
		return false, tx.Commit()
	}
	item, err := decodeObject([]byte(data))
	if err != nil {
		return false, err
	}
	if err = changeRelationshipValue(item, toID, typ, link); err != nil {
		return false, err
	}
	if rev == math.MaxInt64 {
		return false, errors.New("item revision exhausted")
	}
	item["rev"] = json.Number(strconv.FormatInt(rev+1, 10))
	item["updated"] = time.Now().UTC().Format(time.RFC3339Nano)
	projection := &Snapshot{projections: map[string][][]any{}}
	if err = projection.children(fromID, item); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE items SET data=?,rev=?,updated=? WHERE id=?", encode(item), rev+1, item["updated"], fromID); err != nil {
		return false, err
	}
	for _, table := range []string{"comments", "links", "edges"} {
		queries := childWrites[table]
		if _, err = tx.ExecContext(ctx, queries.remove, fromID); err != nil {
			return false, err
		}
		for _, row := range projection.projections[table] {
			if _, err = tx.ExecContext(ctx, queries.insert, row...); err != nil {
				return false, fmt.Errorf("relationship %s: %w", table, err)
			}
		}
	}
	result, err := tx.ExecContext(ctx, "UPDATE import_receipts SET mutated_at=COALESCE(mutated_at,?)", item["updated"])
	if err != nil {
		return false, err
	}
	marked, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if marked != 1 {
		return false, errors.New("shadow import receipt required for relationship mutation")
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func removeRelationship(item map[string]any, target, typ string) {
	for field, v := range item {
		if field == "supersedes" && typ == "supersedes" && v == target {
			delete(item, field)
			continue
		}
		if (field == "depends_on" || strings.HasSuffix(field, "_ids")) && legacyEdgeType(field) == typ {
			if entries, ok := v.([]any); ok {
				kept := []any{}
				for _, entry := range entries {
					if entry != target {
						kept = append(kept, entry)
					}
				}
				item[field] = kept
			}
		}
	}
	if typ == "belongs_to" {
		if links, ok := item["links"].([]any); ok {
			kept := []any{}
			for _, entry := range links {
				obj := entry.(map[string]any)
				if obj["kind"] != "workstream" || obj["ref"] != target {
					kept = append(kept, entry)
				}
			}
			item["links"] = kept
		}
	}
	if metadata, _ := relationshipMetadata(item); metadata != nil {
		kept := []any{}
		for _, entry := range metadata["edges"].([]any) {
			obj := entry.(map[string]any)
			if obj["type"] != typ || obj["target"] != target {
				kept = append(kept, entry)
			}
		}
		metadata["edges"] = kept
	}
}

// IdeaDiagnostic flags owner decisions without creating decisions or changing
// idea status. HistoricalDecision preserves any historical JSON value verbatim.
type IdeaDiagnostic struct {
	ItemID                string `json:"item_id"`
	ConversionCandidate   bool   `json:"conversion_candidate"`
	HasHistoricalDecision bool   `json:"has_historical_decision"`
	HistoricalDecision    any    `json:"historical_decision"`
}

// IdeaDiagnostics returns decision-needed candidates and stray decision data.
func (s *Store) IdeaDiagnostics(ctx context.Context) ([]IdeaDiagnostic, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,data FROM items WHERE db='ideas' ORDER BY ordinal")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []IdeaDiagnostic{}
	for rows.Next() {
		var id, data string
		if err = rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		item, decodeErr := decodeObject([]byte(data))
		if decodeErr != nil {
			return nil, decodeErr
		}
		historical, exists := item["decision"]
		candidate := item["status"] == "decision-needed"
		if exists || candidate {
			out = append(out, IdeaDiagnostic{id, candidate, exists, historical})
		}
	}
	return out, rows.Err()
}
