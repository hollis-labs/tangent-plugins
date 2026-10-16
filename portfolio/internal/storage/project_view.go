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
	Projects    []string        `json:"projects"`
	Workstreams []string        `json:"workstreams"`
	Unresolved  []string        `json:"unresolved"`
	Ambiguous   bool            `json:"ambiguous"`
	Sources     []ProjectSource `json:"sources"`
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
	state, err := s.ReadState(ctx)
	if err != nil {
		return ProjectView{}, err
	}
	index := state.Membership
	records := index.mappings
	ordered := []*viewItem{}
	databases := make([]string, 0, len(state.Envelopes))
	for db := range state.Envelopes {
		databases = append(databases, db)
	}
	sort.Strings(databases)
	for _, db := range databases {
		for _, value := range state.Envelopes[db]["items"].([]any) {
			item := value.(map[string]any)
			ordered = append(ordered, &viewItem{id: item["id"].(string), db: db, data: encode(item), obj: item})
		}
	}
	out := ProjectView{URN: urn, Items: []ProjectItem{}, Unscoped: []ProjectItem{}, Tasks: []TaskMembership{}, TaskEvidence: []TaskQueryEvidence{}}
	selectors := map[string][2]string{}
	for _, v := range ordered {
		m := index.Resolve(v.id)
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
		if m.Unscoped() {
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
		tags := []string{}
		for _, tag := range task.Tags {
			tags = append(tags, tag.Slug)
		}
		m := index.Task(task.ProjectID, tags)
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
