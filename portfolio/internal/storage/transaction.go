package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/textcompat"
)

// State is a detached lossless view inside a shadow transaction. Callbacks must
// not retain it, perform I/O, delete/reorder existing items, or change identities.
// Projects metadata belongs to a separate projection and is never included here.
type State struct {
	Envelopes map[string]map[string]any
	Reserved  map[string]string
	search    map[string]string
}

// ReadState gives the internal service a verified, detached consistent snapshot.
func (s *Store) ReadState(ctx context.Context) (*State, error) {
	files, err := s.Export(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := ParseSnapshot(files)
	if err != nil {
		return nil, err
	}
	return stateOf(snap), nil
}
func stateOf(snap *Snapshot) *State {
	state := &State{Envelopes: snap.envelopes, Reserved: map[string]string{}, search: map[string]string{}}
	for _, row := range snap.projections["id_reservations"] {
		state.Reserved[row[0].(string)] = row[1].(string)
	}
	for _, row := range snap.projections["items_fts"] {
		state.search[row[0].(string)] = row[1].(string)
	}
	return state
}

// Transact applies an internal shadow operation under the existing immediate
// writer lock. Revalidation, immutable identity/order, reservations, all derived
// projections and the original import receipt are one commit. Callback errors
// and projection faults roll back completely. No production caller is admitted.
func (s *Store) Transact(ctx context.Context, apply func(*State) error) error {
	if apply == nil {
		return errors.New("mutation callback required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	files, err := exportTx(ctx, tx)
	if err != nil {
		return err
	}
	before, err := ParseSnapshot(files)
	if err != nil {
		return err
	}
	state := stateOf(before)
	initialReserved := stateOf(before).Reserved
	working, err := ParseSnapshot(files)
	if err != nil {
		return err
	}
	state.Envelopes = working.envelopes
	if err = apply(state); err != nil {
		return err
	}
	changedFiles := map[string][]byte{}
	for db, env := range state.Envelopes {
		changedFiles[db] = []byte(encode(env))
	}
	after, err := ParseSnapshot(changedFiles)
	if err != nil {
		return err
	}
	if before.digest == after.digest {
		return tx.Commit()
	}
	old := map[string][]any{}
	for _, row := range before.projections["items"] {
		old[row[0].(string)] = row
	}
	seen := map[string]bool{}
	for _, row := range after.projections["items"] {
		id := row[0].(string)
		seen[id] = true
		if prior, exists := old[id]; exists {
			if prior[1] != row[1] || prior[2] != row[2] {
				return errors.New("existing item identity and ordinal are immutable")
			}
			if prior[10] == row[10] {
				continue
			}
			if _, err = tx.ExecContext(ctx, "UPDATE items SET title=?,status=?,rev=?,sort_order=?,created=?,updated=?,author=?,data=? WHERE id=?", append(row[3:], id)...); err != nil {
				return err
			}
		} else {
			if _, reserved := initialReserved[id]; reserved {
				return errors.New("item id is permanently reserved")
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO id_reservations(id,db) VALUES (?,?)", row[0], row[1]); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO items(id,db,ordinal,title,status,rev,sort_order,created,updated,author,data) VALUES (?,?,?,?,?,?,?,?,?,?,?)", row...); err != nil {
				return err
			}
		}
		if err = replaceChildren(ctx, tx, after, id); err != nil {
			return err
		}
	}
	for id := range old {
		if !seen[id] {
			return errors.New("items cannot be deleted")
		}
	}
	for _, row := range after.projections["envelopes"] {
		if _, err = tx.ExecContext(ctx, "UPDATE envelopes SET data=? WHERE db=?", row[1], row[0]); err != nil {
			return err
		}
	}
	if err = verify(ctx, tx, after); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE import_receipts SET mutated_at=COALESCE(mutated_at,strftime('%Y-%m-%dT%H:%M:%fZ','now'))")
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("shadow import receipt required")
	}
	return tx.Commit()
}
func replaceChildren(ctx context.Context, tx *sql.Tx, snap *Snapshot, id string) error {
	for _, table := range []string{"comments", "links", "edges"} {
		query := childWrites[table]
		if _, err := tx.ExecContext(ctx, query.remove, id); err != nil {
			return err
		}
		for _, row := range snap.projections[table] {
			if row[0] != id {
				continue
			}
			if _, err := tx.ExecContext(ctx, query.insert, row...); err != nil {
				return fmt.Errorf("projection %s: %w", table, err)
			}
		}
	}
	return nil
}

// SearchIDs uses the verified FTS5 projection from this same detached read,
// retaining substring semantics without a second, racing database read.
func (s *State) SearchIDs(q string) map[string]bool {
	out := map[string]bool{}
	terms := textcompat.Terms(q)
	for id, text := range s.search {
		found := len(terms) > 0
		for _, term := range terms {
			found = found && strings.Contains(text, term)
		}
		if found {
			out[id] = true
		}
	}
	return out
}
