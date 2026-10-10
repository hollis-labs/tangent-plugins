package storage

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
)

// ProjectSource is the allowlisted canonical membership evidence, not overlay
// metadata or caller authority. Retained stale/partial facts are labeled.
type ProjectSource struct {
	URN         string   `json:"urn"`
	State       string   `json:"state"`
	TorqueIDs   []string `json:"torque_ids"`
	Environment string   `json:"environment"`
}

type membershipNode struct {
	db    string
	item  map[string]any
	edges []Edge
}

// MembershipIndex belongs to one detached, consistent read. It has no shared
// mutable cache, upstream client, overlay authority or guessed tag semantics.
type MembershipIndex struct {
	nodes     map[string]membershipNode
	sources   map[string]ProjectSource
	mappings  map[string][]string
	byProject map[string][]string
	byTag     map[string][]string
}

func membershipIndex(snap *Snapshot, sources map[string]ProjectSource) *MembershipIndex {
	index := &MembershipIndex{nodes: map[string]membershipNode{}, sources: sources, mappings: map[string][]string{}, byProject: map[string][]string{}, byTag: map[string][]string{}}
	for db, env := range snap.envelopes {
		for _, value := range env["items"].([]any) {
			item := value.(map[string]any)
			index.nodes[item["id"].(string)] = membershipNode{db: db, item: item}
			if db == "workstreams" {
				id := item["id"].(string)
				for _, pid := range stringValues(item["torque_project_ids"]) {
					index.byProject[pid] = append(index.byProject[pid], id)
				}
				if tag, ok := item["torque_tag"].(string); ok && tag != "" {
					index.byTag[tag] = append(index.byTag[tag], id)
				}
			}
		}
	}
	for _, row := range snap.projections["edges"] {
		if row[2] != "belongs_to" {
			continue
		}
		id := row[0].(string)
		node := index.nodes[id]
		node.edges = append(node.edges, Edge{id, row[1].(string), row[2].(string), row[3].(string)})
		index.nodes[id] = node
	}
	for urn, source := range sources {
		for _, id := range source.TorqueIDs {
			index.mappings[id] = append(index.mappings[id], urn)
		}
	}
	for id, urns := range index.mappings {
		index.mappings[id] = unique(urns)
	}
	for id, node := range index.nodes {
		sort.Slice(node.edges, func(a, b int) bool { return encode(node.edges[a]) < encode(node.edges[b]) })
		index.nodes[id] = node
	}
	return index
}

func projectSources(ctx context.Context, q edgeQuerier) (map[string]ProjectSource, error) {
	rows, err := q.QueryContext(ctx, "SELECT urn,source_data,registry_state FROM projects WHERE registry_state IN ('fresh','stale','partial')")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ProjectSource{}
	for rows.Next() {
		var urn, data, state string
		if err = rows.Scan(&urn, &data, &state); err != nil {
			return nil, err
		}
		var p projects.Project
		if err = json.Unmarshal([]byte(data), &p); err != nil {
			return nil, err
		}
		if p.URN == urn && p.Kind == "project" && projects.ValidURN(urn) {
			out[urn] = ProjectSource{urn, state, unique(p.TorqueIDs()), p.Environment}
		}
	}
	return out, rows.Err()
}

// HasProject requires usable canonical source, never a syntactic URN alone.
func (i *MembershipIndex) HasProject(urn string) bool { _, ok := i.sources[urn]; return ok }

// ResolveProject returns detached explicit Torque selectors.
func (i *MembershipIndex) ResolveProject(urn string) []string {
	return append([]string{}, i.sources[urn].TorqueIDs...)
}
func (i *MembershipIndex) HasWorkstream(id string) bool {
	n, ok := i.nodes[id]
	return ok && n.db == "workstreams"
}

// Resolve follows only belongs_to through workstreams, collecting every valid
// alternate path. Completed nodes are visited once per root, so diamonds do not
// explode into path enumeration; recursion detects cycles rather than DAG joins.
func (i *MembershipIndex) Resolve(id string) Membership { return i.resolveRoots([]string{id}) }

// resolveRoots shares traversal across overlapping task selectors within one read.
func (i *MembershipIndex) resolveRoots(ids []string) Membership {
	m := Membership{Projects: []string{}, Workstreams: []string{}, Unresolved: []string{}, Sources: []ProjectSource{}}
	visited, visiting := map[string]bool{}, map[string]bool{}
	var walk func(string)
	addProject := func(urn, ref string) {
		if i.HasProject(urn) {
			m.Projects = append(m.Projects, urn)
		} else {
			m.Unresolved = append(m.Unresolved, ref)
		}
	}
	walk = func(id string) {
		if visiting[id] {
			m.Unresolved = append(m.Unresolved, "cycle:"+id)
			return
		}
		if visited[id] {
			return
		}
		node, ok := i.nodes[id]
		if !ok {
			m.Unresolved = append(m.Unresolved, "item:"+id)
			return
		}
		visiting[id] = true
		if node.db == "workstreams" {
			m.Workstreams = append(m.Workstreams, id)
		}
		for _, edge := range node.edges {
			if projects.ValidURN(edge.ToID) {
				addProject(edge.ToID, edge.Field+":"+edge.ToID)
			} else if i.HasWorkstream(edge.ToID) {
				walk(edge.ToID)
			} else {
				m.Unresolved = append(m.Unresolved, edge.Field+":"+edge.ToID)
			}
		}
		for _, pid := range stringValues(node.item["torque_project_ids"]) {
			if urns := i.mappings[pid]; len(urns) > 0 {
				m.Projects = append(m.Projects, urns...)
			} else {
				m.Unresolved = append(m.Unresolved, "torque:"+pid)
			}
		}
		visiting[id] = false
		visited[id] = true
	}
	for _, id := range ids {
		walk(id)
	}
	m.Projects, m.Workstreams, m.Unresolved = unique(m.Projects), unique(m.Workstreams), unique(m.Unresolved)
	m.Ambiguous = len(m.Projects) > 1
	for _, urn := range m.Projects {
		m.Sources = append(m.Sources, i.sources[urn])
	}
	return m
}

// Unscoped is diagnostic overlap, not an exclusive partition. Proven
// workstream-only membership remains visible even without a resolved project.
func (m Membership) Unscoped() bool {
	return len(m.Projects) == 0 || m.Ambiguous || len(m.Unresolved) > 0
}

// Task resolves explicit project IDs and exact configured workstream tag slugs.
// A display ws-* prefix supplies no membership evidence.
func (i *MembershipIndex) Task(projectID string, tags []string) Membership {
	m := Membership{Projects: append([]string{}, i.mappings[projectID]...), Workstreams: []string{}, Unresolved: []string{}, Sources: []ProjectSource{}}
	if projectID != "" && len(m.Projects) == 0 {
		m.Unresolved = append(m.Unresolved, "torque:"+projectID)
	}
	ids := append([]string{}, i.byProject[projectID]...)
	for _, tag := range tags {
		ids = append(ids, i.byTag[tag]...)
	}
	ids = unique(ids)
	nested := i.resolveRoots(ids)
	m.Projects = append(m.Projects, nested.Projects...)
	m.Workstreams = append(m.Workstreams, nested.Workstreams...)
	m.Unresolved = append(m.Unresolved, nested.Unresolved...)
	m.Projects, m.Workstreams, m.Unresolved = unique(m.Projects), unique(m.Workstreams), unique(m.Unresolved)
	m.Ambiguous = len(m.Projects) > 1
	for _, urn := range m.Projects {
		m.Sources = append(m.Sources, i.sources[urn])
	}
	return m
}

// Proof binds only membership-relevant reachable facts, excluding titles,
// revisions, overlay data and unrelated source rows.
func (i *MembershipIndex) Proof(id string) any {
	seen := map[string]bool{}
	facts := map[string]any{}
	var walk func(string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		node, ok := i.nodes[id]
		if !ok {
			return
		}
		edges := append([]Edge{}, node.edges...)
		sort.Slice(edges, func(a, b int) bool { return encode(edges[a]) < encode(edges[b]) })
		facts[id] = map[string]any{"edges": edges, "torque_project_ids": unique(stringValues(node.item["torque_project_ids"]))}
		for _, edge := range edges {
			if i.HasWorkstream(edge.ToID) {
				walk(edge.ToID)
			}
		}
	}
	walk(id)
	return map[string]any{"membership": i.Resolve(id), "facts": facts}
}
