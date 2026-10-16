package operations

import (
	"sort"
	"strings"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

func edgeOperation(name string) bool {
	switch name {
	case "edge_list", "edge_add", "edge_remove", "edge_backlinks", "decision_gates":
		return true
	}
	return false
}

func edgeDeclarations() []Operation {
	id := object{"type": "string", "minLength": 1, "maxLength": 512}
	types := object{"type": "string", "enum": []any{"decision_gates", "informs", "supersedes", "belongs_to", "depends_on", "related"}}
	db := object{"type": "string", "enum": []any{"priorities", "workstreams", "roadmap", "ideas", "decisions", "risks", "inbox"}}
	source := object{"type": "object", "additionalProperties": false, "required": []any{"db", "id"}, "properties": object{"db": db, "id": id}}
	target := object{"type": "object", "additionalProperties": false, "required": []any{"id"}, "properties": object{"db": db, "id": id}}
	out := []Operation{}
	for _, name := range []string{"edge_list", "edge_add", "edge_remove", "edge_backlinks", "decision_gates"} {
		var props object
		var required []any
		write := name == "edge_add" || name == "edge_remove"
		if write {
			props = object{"from": source, "to": target, "type": types, "rev": object{"type": "integer", "minimum": 0}}
			required = []any{"from", "to", "type"}
		} else if name == "edge_backlinks" {
			props = object{"id": id, "type": types}
			required = []any{"id"}
		} else {
			props = object{"db": db, "id": id}
			required = []any{"db", "id"}
			if name == "edge_list" {
				props["type"] = types
			}
		}
		input := object{"type": "object", "additionalProperties": false, "required": required, "properties": props}
		out = append(out, Operation{Name: name, Write: write, Input: clone(input).(object), Description: "Typed relationships on one copied shadow snapshot; requires explicit current operation and complete typed cohort grants. CAS precedes no-op. No automatic retry or production binding."})
	}
	return out
}

func validateEdgeIDs(name string, in object) error {
	values := []string{}
	if name == "edge_add" || name == "edge_remove" {
		values = append(values, str(in["from"].(object)["id"]), str(in["to"].(object)["id"]))
	} else {
		values = append(values, str(in["id"]))
	}
	for _, value := range values {
		if strings.TrimSpace(value) != value {
			return failure("bad_request", "edge identity has surrounding whitespace", nil)
		}
	}
	return nil
}

func edgeCohort(state *storage.State, name string, in object) (EdgeCohort, error) {
	cohort := EdgeCohort{Resources: []EdgeResource{}, Edges: []storage.Edge{}}
	seen := map[EdgeResource]bool{}
	local := state.ItemDatabases()
	add := func(id, requestedDB, role, access string) {
		db := local[id]
		fact := EdgeResource{ID: id, DB: db, RequestedDB: requestedDB, Exists: db != "", Role: role, Access: access}
		if !seen[fact] {
			seen[fact] = true
			cohort.Resources = append(cohort.Resources, fact)
		}
	}
	write := name == "edge_add" || name == "edge_remove"
	id, db, typ := str(in["id"]), str(in["db"]), str(in["type"])
	if write {
		from, to := in["from"].(object), in["to"].(object)
		id, db = str(from["id"]), str(from["db"])
		add(id, db, "source", "write")
		add(str(to["id"]), str(to["db"]), "target", "reference")
		// Successful writes and CAS errors can disclose the complete source
		// item, including external pointers outside the typed edge projection.
		if actualDB := local[id]; actualDB != "" {
			for _, value := range state.Envelopes[actualDB]["items"].([]any) {
				item := value.(object)
				if item["id"] != id {
					continue
				}
				if links, ok := item["links"].([]any); ok {
					for _, value := range links {
						pointer := value.(object)
						ref := str(pointer["ref"])
						targetDB := local[ref]
						fact := EdgeResource{ID: ref, DB: targetDB, Exists: targetDB != "", Role: "external_pointer", Access: "reference", LinkKind: str(pointer["kind"])}
						if !seen[fact] {
							seen[fact] = true
							cohort.Resources = append(cohort.Resources, fact)
						}
					}
				}
			}
		}
		// Full source item is returned; admit all current representations and
		// retain the candidate target even if the edge is removed or a no-op.
		typ = ""
	} else if name == "edge_backlinks" {
		add(id, "", "target", "reference")
	} else {
		add(id, db, "source", "read")
		if name == "decision_gates" {
			typ = "decision_gates"
		}
	}
	edges, err := state.Relationships(id, name == "edge_backlinks", typ)
	if err != nil {
		return cohort, err
	}
	cohort.Edges = append(cohort.Edges, edges...)
	for _, edge := range edges {
		if name == "edge_backlinks" {
			add(edge.FromID, "", "backlink_source", "read")
		}
		add(edge.ToID, "", "target", "reference")
	}
	return cohort, nil
}

func edgeRead(x *execution, in object, name string) (any, error) {
	id, typ := str(in["id"]), str(in["type"])
	if name != "edge_backlinks" {
		if _, err := x.find(str(in["db"]), id); err != nil {
			return nil, err
		}
	}
	if name == "decision_gates" {
		typ = "decision_gates"
	}
	edges, err := x.state.Relationships(id, name == "edge_backlinks", typ)
	if err != nil {
		return nil, err
	}
	if name != "decision_gates" {
		return edges, nil
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, edge := range edges {
		if !seen[edge.ToID] {
			seen[edge.ToID] = true
			ids = append(ids, edge.ToID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
func edgeWrite(x *execution, in object, add bool) (any, error) {
	from, to := in["from"].(object), in["to"].(object)
	db, id := str(from["db"]), str(from["id"])
	item, err := x.find(db, id)
	if err != nil {
		return nil, err
	}
	if targetDB, explicit := to["db"]; explicit {
		if _, err = x.find(str(targetDB), str(to["id"])); err != nil {
			return nil, err
		}
	}
	if revision, has := in["rev"]; has {
		expected, _ := integer(revision)
		actual, _ := integer(item["rev"])
		if expected != actual {
			return nil, failure("conflict", "revision mismatch", object{"current": clone(item)})
		}
	}
	changed, err := x.state.ChangeEdge(id, str(to["id"]), str(in["type"]), add)
	if err != nil {
		return nil, err
	}
	if changed {
		if err = x.bump(item); err != nil {
			return nil, err
		}
		x.touch(db)
	}
	return object{"changed": changed, "item": item}, nil
}
