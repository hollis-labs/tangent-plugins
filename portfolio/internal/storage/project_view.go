package storage

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
)

// TaskReader queries bounded, explicit upstream membership selectors.
type TaskReader interface {
	Tasks(context.Context, string, string) (projects.TaskPage, error)
}

// Membership preserves all explicit candidates and unresolved provenance.
type Membership struct {
	Projects   []string
	Unresolved []string
	Ambiguous  bool
}

// ProjectItem is a view, not a new item or writer.
type ProjectItem struct {
	ID         string
	Database   string
	Data       json.RawMessage
	Membership Membership
}

// TaskMembership retains synthetic read-only tasks and explicit membership evidence.
type TaskMembership struct {
	Task       projects.Task
	Membership Membership
}

// TaskQueryEvidence keeps totals separate for overlapping queries; never sum them.
type TaskQueryEvidence struct {
	ProjectID string
	Tag       string
	Evidence  projects.Evidence
}

// ProjectView applies scope before caps/totals and exposes ambiguous/unscoped rows.
type ProjectView struct {
	URN               string
	Items             []ProjectItem
	Unscoped          []ProjectItem
	Tasks             []TaskMembership
	TaskEvidence      []TaskQueryEvidence
	LocalTotal        int
	LocalTruncated    bool
	UnscopedTotal     int
	UnscopedTruncated bool
	TaskTotal         int
	TaskTruncated     bool
}

type viewItem struct {
	id, db, data string
	obj          map[string]any
	edges        []Edge
}

// ViewProject resolves local item→workstream→project membership and fetches only
// explicit Torque selectors. No name, prefix or display-label inference occurs.
// This bounded read composition does not implement the future global scope API.
func (s *Store) ViewProject(ctx context.Context, urn string, limit int, torque TaskReader) (ProjectView, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !projects.ValidURN(urn) || limit < 1 || limit > 200 {
		return ProjectView{}, projects.Invalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectView{}, err
	}
	defer func() { _ = tx.Rollback() }()
	records, err := queryProjectMappings(ctx, tx)
	if err != nil {
		return ProjectView{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,db,data FROM items ORDER BY db,ordinal")
	if err != nil {
		return ProjectView{}, err
	}
	all := map[string]*viewItem{}
	ordered := []*viewItem{}
	for rows.Next() {
		v := &viewItem{}
		if err = rows.Scan(&v.id, &v.db, &v.data); err != nil {
			_ = rows.Close()
			return ProjectView{}, err
		}
		v.obj, err = decodeObject([]byte(v.data))
		if err != nil {
			_ = rows.Close()
			return ProjectView{}, err
		}
		all[v.id] = v
		ordered = append(ordered, v)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return ProjectView{}, err
	}
	if err = rows.Close(); err != nil {
		return ProjectView{}, err
	}
	edges, err := queryEdges(ctx, tx, "SELECT from_id,to_id,type,field FROM edges WHERE type='belongs_to' ORDER BY from_id,to_id,field")
	if err != nil {
		return ProjectView{}, err
	}
	for _, e := range edges {
		if v := all[e.FromID]; v != nil {
			v.edges = append(v.edges, e)
		}
	}
	if err = tx.Commit(); err != nil {
		return ProjectView{}, err
	}
	out := ProjectView{URN: urn, Items: []ProjectItem{}, Unscoped: []ProjectItem{}, Tasks: []TaskMembership{}, TaskEvidence: []TaskQueryEvidence{}}
	memberships := map[string]Membership{}
	selectors := map[string][2]string{}
	for _, v := range ordered {
		m := resolveMembership(v, all, records, map[string]bool{})
		memberships[v.id] = m
		item := ProjectItem{v.id, v.db, json.RawMessage(v.data), m}
		if contains(m.Projects, urn) {
			out.LocalTotal++
			if len(out.Items) < limit {
				out.Items = append(out.Items, item)
			}
			if v.db == "workstreams" {
				for _, id := range stringValues(v.obj["torque_project_ids"]) {
					if projects.ValidProjectID(id) {
						selectors[id+"|"] = [2]string{id, ""}
					}
				}
				if tag, ok := v.obj["torque_tag"].(string); ok && tag != "" {
					selectors["|"+tag] = [2]string{"", tag}
				}
			}
		}
		if len(m.Projects) == 0 || m.Ambiguous || len(m.Unresolved) > 0 {
			out.UnscopedTotal++
			if len(out.Unscoped) < limit {
				out.Unscoped = append(out.Unscoped, item)
			}
		}
	}
	out.LocalTruncated = out.LocalTotal > len(out.Items)
	out.UnscopedTruncated = out.UnscopedTotal > len(out.Unscoped)
	for id, urns := range records {
		if contains(urns, urn) {
			selectors[id+"|"] = [2]string{id, ""}
		}
	}
	keys := make([]string, 0, len(selectors))
	for key := range selectors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Bound fanout as well as each upstream page budget. Remaining selectors stay
	// visible as incomplete evidence, never as an exact empty task result.
	taskRows := map[string]projects.Task{}
	refused := ""
	for i, key := range keys {
		sel := selectors[key]
		var page projects.TaskPage
		if refused != "" {
			page.Evidence = projects.Evidence{Partial: true, Code: refused}
		} else if i >= 20 {
			page.Evidence = projects.Evidence{Partial: true, Code: "selector_budget"}
		} else if torque == nil {
			page.Evidence = projects.Evidence{Partial: true, Code: string(projects.Missing)}
		} else {
			var readErr error
			page, readErr = torque.Tasks(ctx, sel[0], sel[1])
			if readErr != nil {
				page.Evidence.Partial = true
				page.Evidence.Complete = false
				page.Evidence.Code = failureState(readErr)
				if failureState(readErr) == "denied" || failureState(readErr) == "missing_integration" {
					refused = failureState(readErr)
					page.Tasks = nil
					page.Evidence.Total = nil
					taskRows = map[string]projects.Task{}
					for j := range out.TaskEvidence {
						out.TaskEvidence[j].Evidence = projects.Evidence{Partial: true, Code: refused}
					}
					if _, err = s.db.ExecContext(ctx, "UPDATE projects SET enrichment='{}',torque_state=?", refused); err != nil {
						return ProjectView{}, err
					}
				}
			}
		}
		out.TaskEvidence = append(out.TaskEvidence, TaskQueryEvidence{sel[0], sel[1], page.Evidence})
		for _, t := range page.Tasks {
			taskRows[t.ID] = t
		}
	}
	taskKeys := make([]string, 0, len(taskRows))
	for id := range taskRows {
		taskKeys = append(taskKeys, id)
	}
	sort.Strings(taskKeys)
	for _, id := range taskKeys {
		task := taskRows[id]
		m := Membership{Projects: append([]string{}, records[task.ProjectID]...), Unresolved: []string{}}
		if task.ProjectID != "" && len(m.Projects) == 0 {
			m.Unresolved = append(m.Unresolved, "torque:"+task.ProjectID)
		}
		for _, v := range ordered {
			if v.db != "workstreams" {
				continue
			}
			matches := contains(stringValues(v.obj["torque_project_ids"]), task.ProjectID) && task.ProjectID != ""
			if tag, ok := v.obj["torque_tag"].(string); ok && tag != "" {
				for _, t := range task.Tags {
					if t.Slug == tag {
						matches = true
					}
				}
			}
			if matches {
				m.Projects = append(m.Projects, memberships[v.id].Projects...)
				m.Unresolved = append(m.Unresolved, memberships[v.id].Unresolved...)
			}
		}
		m.Projects = unique(m.Projects)
		m.Unresolved = unique(m.Unresolved)
		m.Ambiguous = len(m.Projects) > 1
		if contains(m.Projects, urn) {
			out.TaskTotal++
			if len(out.Tasks) < limit {
				out.Tasks = append(out.Tasks, TaskMembership{task, m})
			}
		}
	}
	out.TaskTruncated = out.TaskTotal > len(out.Tasks)
	return out, nil
}

func queryProjectMappings(ctx context.Context, q edgeQuerier) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT urn,source_data FROM projects WHERE registry_state IN ('fresh','stale','partial')")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var urn, data string
		if err = rows.Scan(&urn, &data); err != nil {
			return nil, err
		}
		var p projects.Project
		if err = json.Unmarshal([]byte(data), &p); err != nil {
			return nil, err
		}
		for _, id := range p.TorqueIDs() {
			out[id] = append(out[id], urn)
		}
	}
	for id := range out {
		out[id] = unique(out[id])
	}
	return out, rows.Err()
}
func resolveMembership(v *viewItem, all map[string]*viewItem, mappings map[string][]string, visiting map[string]bool) Membership {
	m := Membership{Projects: []string{}, Unresolved: []string{}}
	if visiting[v.id] {
		m.Unresolved = append(m.Unresolved, "cycle:"+v.id)
		return m
	}
	visiting[v.id] = true
	defer delete(visiting, v.id)
	for _, e := range v.edges {
		if projects.ValidURN(e.ToID) {
			m.Projects = append(m.Projects, e.ToID)
			continue
		}
		if target := all[e.ToID]; target != nil && target.db == "workstreams" {
			nested := resolveMembership(target, all, mappings, visiting)
			m.Projects = append(m.Projects, nested.Projects...)
			m.Unresolved = append(m.Unresolved, nested.Unresolved...)
			continue
		}
		m.Unresolved = append(m.Unresolved, e.Field+":"+e.ToID)
	}
	for _, id := range stringValues(v.obj["torque_project_ids"]) {
		if urns := mappings[id]; len(urns) > 0 {
			m.Projects = append(m.Projects, urns...)
		} else {
			m.Unresolved = append(m.Unresolved, "torque:"+id)
		}
	}
	m.Projects = unique(m.Projects)
	m.Unresolved = unique(m.Unresolved)
	m.Ambiguous = len(m.Projects) > 1
	return m
}
func stringValues(v any) []string {
	out := []string{}
	if list, ok := v.([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func unique(values []string) []string {
	sort.Strings(values)
	out := []string{}
	for _, v := range values {
		if len(out) == 0 || strings.Compare(out[len(out)-1], v) != 0 {
			out = append(out, v)
		}
	}
	return out
}
