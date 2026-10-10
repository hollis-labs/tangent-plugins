package storage

import (
	"encoding/json"
	"sort"
)

// ItemDatabases indexes local identities from this evolving State. Missing IDs
// are external/dangling; no cached index or independently racing read is used.
func (s *State) ItemDatabases() map[string]string {
	index := map[string]string{}
	for db, env := range s.Envelopes {
		for _, value := range env["items"].([]any) {
			index[value.(map[string]any)["id"].(string)] = db
		}
	}
	return index
}

func projectedEdges(id string, item map[string]any) ([]Edge, error) {
	projection := &Snapshot{projections: map[string][][]any{}}
	if err := projection.children(id, item); err != nil {
		return nil, err
	}
	out := []Edge{}
	for _, row := range projection.projections["edges"] {
		out = append(out, Edge{FromID: row[0].(string), ToID: row[1].(string), Type: row[2].(string), Field: row[3].(string)})
	}
	return out, nil
}

// Relationships uses the persisted children algorithm on the evolving State.
// Ordering matches the existing SQL readers. An empty cohort is not permission.
func (s *State) Relationships(id string, incoming bool, typ string) ([]Edge, error) {
	out := []Edge{}
	for _, env := range s.Envelopes {
		for _, value := range env["items"].([]any) {
			item := value.(map[string]any)
			source := item["id"].(string)
			if !incoming && source != id {
				continue
			}
			edges, err := projectedEdges(source, item)
			if err != nil {
				return nil, err
			}
			for _, edge := range edges {
				if incoming && edge.ToID != id || typ != "" && edge.Type != typ {
					continue
				}
				out = append(out, edge)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if incoming && a.FromID != b.FromID {
			return a.FromID < b.FromID
		}
		if !incoming && a.ToID != b.ToID {
			return a.ToID < b.ToID
		}
		return a.Field < b.Field
	})
	return out, nil
}

// ChangeEdge alters representations only. Revision, clock, authority and commit
// belong to the caller's existing domain transaction, never another Store call.
func (s *State) ChangeEdge(source, target, typ string, add bool) (bool, error) {
	if source == "" || target == "" || !validEdgeType(typ) {
		return false, ErrInvalidRelationship
	}
	for _, env := range s.Envelopes {
		for _, value := range env["items"].([]any) {
			item := value.(map[string]any)
			if item["id"] != source {
				continue
			}
			edges, err := projectedEdges(source, item)
			if err != nil {
				return false, err
			}
			exists := false
			for _, edge := range edges {
				exists = exists || edge.ToID == target && edge.Type == typ
			}
			if exists == add {
				return false, nil
			}
			if err = changeRelationshipValue(item, target, typ, add); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, ErrItemNotFound
}

func changeRelationshipValue(item map[string]any, target, typ string, add bool) error {
	if !add {
		removeRelationship(item, target, typ)
		return nil
	}
	metadata, err := relationshipMetadata(item)
	if err != nil {
		return err
	}
	if metadata == nil {
		metadata = map[string]any{"version": json.Number("1"), "edges": []any{}}
		item[relationshipField] = metadata
	}
	metadata["edges"] = append(metadata["edges"].([]any), map[string]any{"type": typ, "target": target})
	return nil
}
