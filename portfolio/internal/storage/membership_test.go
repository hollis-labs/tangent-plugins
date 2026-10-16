package storage

import (
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
	"reflect"
	"testing"
)

func TestMembershipPathsCyclesCanonicalSourceAndPreservation(t *testing.T) {
	s := openTest(t)
	base := fixture(t)
	files := map[string][]byte{}
	for db, env := range base.envelopes {
		files[db] = []byte(encode(env))
	}
	files["workstreams"] = []byte(`{"schema":"portfolio/workstreams@1","items":[
 {"id":"WS-a","title":"A","torque_project_ids":["PRJ-20261009-0001"],"torque_tag":"exact"},
 {"id":"WS-b","title":"B","workstream_ids":["WS-a"]},
 {"id":"WS-c","title":"C","workstream_ids":["WS-d","WS-a"]},
 {"id":"WS-d","title":"D","workstream_ids":["WS-c"]},
 {"id":"WS-only","title":"Only workstream"}]}`)
	files["ideas"] = []byte(`{"schema":"portfolio/ideas@1","items":[
 {"id":"ID-diamond","title":"Diamond","workstream_ids":["WS-a","WS-b"],"unknown":{"exact":9007199254740993,"null":null}},
 {"id":"ID-cycle","title":"Cycle","links":[{"kind":"workstream","ref":"WS-c","opaque":null}]},
 {"id":"ID-typed","title":"Typed","_portfolio_relationships":{"version":1,"edges":[{"type":"belongs_to","target":"WS-b","unknown":9007199254740993}]}},
 {"id":"ID-dangling","title":"Dangling","project_ids":["msg://project/not-synced/a"],"workstream_ids":["WS-missing"]},
 {"id":"ID-related","title":"Related is not membership","related_ids":["WS-a"],"decision_ids":["WS-a"]},
 {"id":"ID-only","title":"Workstream only","workstream_ids":["WS-only"]}]}`)
	snap, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	if err = s.SyncProjects(t.Context(), directory(), projectFake{}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Export(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	index := state.Membership
	for _, id := range []string{"ID-diamond", "ID-typed", "ID-cycle"} {
		m := index.Resolve(id)
		if !reflect.DeepEqual(m.Projects, []string{"msg://project/example/a"}) {
			t.Fatalf("%s: %+v", id, m)
		}
	}
	if m := index.Resolve("ID-diamond"); m.Ambiguous || len(m.Unresolved) > 0 || !reflect.DeepEqual(m.Workstreams, []string{"WS-a", "WS-b"}) {
		t.Fatal(m)
	}
	cycle := index.Resolve("ID-cycle")
	if !cycle.Unscoped() || len(cycle.Unresolved) == 0 || len(cycle.Workstreams) != 3 {
		t.Fatal(cycle)
	}
	for _, id := range []string{"ID-dangling", "ID-related", "ID-only"} {
		m := index.Resolve(id)
		if len(m.Projects) != 0 || !m.Unscoped() {
			t.Fatal(id, m)
		}
	}
	if index.HasProject("msg://project/not-synced/a") || len(index.Resolve("ID-only").Workstreams) != 1 {
		t.Fatal("invented canonical membership or dropped workstream-only evidence")
	}
	task := index.Task("PRJ-20261009-0001", []string{"ws-guessed"})
	if !reflect.DeepEqual(task.Workstreams, []string{"WS-a"}) {
		t.Fatal(task)
	}
	if m := index.Task("", []string{"exact"}); len(m.Projects) != 1 || len(m.Workstreams) != 1 {
		t.Fatal(m)
	}
	if m := index.Task("", []string{"ws-a"}); len(m.Projects) > 0 || len(m.Workstreams) > 0 {
		t.Fatal("guessed tag authority")
	}
	// A held detached snapshot remains stable when source metadata is revoked.
	if err = s.SyncProjects(t.Context(), directoryFake{err: projects.Denied}, nil); err != nil {
		t.Fatal(err)
	}
	next, err := s.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Membership.Resolve("ID-diamond").Projects) != 0 || len(index.Resolve("ID-diamond").Projects) != 1 {
		t.Fatal("mixed snapshots or denied source remained canonical")
	}
	after, err := s.Export(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read/sync changed copied JSON", err)
	}
	// Typed projections and exact-number/opaque metadata survive untouched.
	edges, err := s.ListEdges(t.Context(), "ID-typed", "belongs_to")
	if err != nil || len(edges) != 1 || edges[0].ToID != "WS-b" {
		t.Fatal(edges, err)
	}
	back, err := s.Backlinks(t.Context(), "WS-b", "belongs_to")
	if err != nil || len(back) < 2 {
		t.Fatal(back, err)
	}
	var obj map[string]any
	if err = json.Unmarshal(after["ideas"], &obj); err != nil {
		t.Fatal(err)
	}
}

func TestMembershipAmbiguousMappingsAndStaleProvenance(t *testing.T) {
	s := openTest(t)
	base := fixture(t)
	files := map[string][]byte{}
	for db, env := range base.envelopes {
		files[db] = []byte(encode(env))
	}
	files["workstreams"] = []byte(`{"schema":"portfolio/workstreams@1","items":[{"id":"WS-one","title":"One","torque_project_ids":["PRJ-20261009-0001"]}]}`)
	snap, err := ParseSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	dir := directory()
	other := dir.result.Projects[0]
	other.URN = "msg://project/example/b"
	dir.result.Projects = append(dir.result.Projects, other)
	if err = s.SyncProjects(t.Context(), dir, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.SyncProjects(t.Context(), directoryFake{err: projects.Unavailable}, nil); err != nil {
		t.Fatal(err)
	}
	state, err := s.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m := state.Membership.Resolve("WS-one")
	if !m.Ambiguous || !m.Unscoped() || len(m.Projects) != 2 || m.Sources[0].State != "stale" {
		t.Fatal(m)
	}
	if err = s.SyncProjects(t.Context(), directoryFake{result: projects.Directory{Evidence: projects.Evidence{Complete: true}}}, nil); err != nil {
		t.Fatal(err)
	}
	next, err := s.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Membership.Resolve("WS-one").Projects) != 0 {
		t.Fatal("absent source remained canonical")
	}
}

// All roots share a deep chain with diamond joins and a closing cycle. Traversal
// must retain the terminal project and diagnose the cycle without enumerating paths.
func TestMembershipOverlappingTaskRootsDeepDiamondCycle(t *testing.T) {
	const depth = 2048
	const urn = "msg://project/pathological/terminal"
	index := &MembershipIndex{nodes: map[string]membershipNode{}, sources: map[string]ProjectSource{urn: {URN: urn, State: "fresh"}}, mappings: map[string][]string{}, byProject: map[string][]string{}, byTag: map[string][]string{}}
	for j := 0; j < depth; j++ {
		id := fmt.Sprintf("WS-%d", j)
		node := membershipNode{db: "workstreams", item: map[string]any{"id": id}}
		if j+1 < depth {
			node.edges = append(node.edges, Edge{id, fmt.Sprintf("WS-%d", j+1), "belongs_to", "workstream_ids"})
		}
		if j+2 < depth {
			node.edges = append(node.edges, Edge{id, fmt.Sprintf("WS-%d", j+2), "belongs_to", "workstream_ids"})
		}
		if j == depth-1 {
			node.edges = append(node.edges, Edge{id, urn, "belongs_to", "project_ids"}, Edge{id, "WS-0", "belongs_to", "workstream_ids"})
		}
		index.nodes[id] = node
		index.byTag["shared"] = append(index.byTag["shared"], id)
	}
	m := index.Task("", []string{"shared"})
	if !reflect.DeepEqual(m.Projects, []string{urn}) || len(m.Workstreams) != depth || !m.Unscoped() || m.Ambiguous || !reflect.DeepEqual(m.Unresolved, []string{"cycle:WS-0"}) {
		t.Fatal(m)
	}
	proof := index.Proof("WS-0").(map[string]any)
	if len(proof["facts"].(map[string]any)) != depth {
		t.Fatal("deep proof omitted reachable membership facts")
	}
}
