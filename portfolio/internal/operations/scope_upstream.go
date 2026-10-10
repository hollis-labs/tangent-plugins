package operations

import (
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
	"sort"
	"strings"
)

// One shared budget covers all statuses/selectors in a scoped request.
const scopeMaxPages = 100
const scopeMaxRows = 20000
const scopeMaxBytes = 8 * 1024 * 1024
const scopeMaxSelectors = 20

type scanBudget struct {
	pages, rows, bytes, selectors int
	seen                          map[string]string
}
type taskSelector struct{ project, tag string }

func array(value any) []any { values, _ := value.([]any); return values }
func taskTags(task object) []string {
	tags := []string{}
	for _, value := range array(task["tags"]) {
		tag := str(value)
		if obj, ok := value.(object); ok {
			tag = str(obj["slug"])
		}
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}
func (x *execution) taskMembership(task object) storage.Membership {
	return x.state.Membership.Task(str(task["project_id"]), taskTags(task))
}
func (x *execution) selectors() []taskSelector {
	if x.scope.Kind == "unscoped" {
		return []taskSelector{{}}
	}
	found := map[string]taskSelector{}
	if x.scope.Kind == "project" {
		for _, id := range x.state.Membership.ResolveProject(x.scope.ID) {
			found[id+"|"] = taskSelector{project: id}
		}
	}
	for _, value := range x.state.Envelopes["workstreams"]["items"].([]any) {
		item := value.(object)
		if !x.scope.matches(x.state.Membership.Resolve(str(item["id"]))) {
			continue
		}
		for _, value := range array(item["torque_project_ids"]) {
			id := str(value)
			if projects.ValidProjectID(id) {
				found[id+"|"] = taskSelector{project: id}
			}
		}
		if tag := str(item["torque_tag"]); tag != "" && validTags(tag) && !strings.Contains(tag, ",") {
			found["|"+tag] = taskSelector{tag: tag}
		}
	}
	keys := []string{}
	for key := range found {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []taskSelector{}
	for _, key := range keys {
		out = append(out, found[key])
	}
	return out
}

func (x *execution) scopedUpstream(name string, input object) (any, error) {
	if name != "torque_tasks" {
		return nil, failure("unsupported", "scoped upstream contract pending review", nil)
	}
	base := clone(input).(object)
	delete(base, "scope")
	if _, err := upstreamInput(name, base); err != nil {
		return nil, err
	}
	if _, has := base["cursor"]; has {
		return nil, failure("unsupported", "a selector cursor is not a scoped continuation", nil)
	}
	offset, _ := integer(base["offset"])
	if offset > 0 {
		return nil, failure("unsupported", "upstream scoped snapshot continuation unavailable", nil)
	}
	delete(base, "offset")
	delete(base, "include_total")
	limit := int64(50)
	if value, has := base["limit"]; has {
		limit, _ = integer(value)
	}
	selectors := x.selectors()
	evidence := []any{}
	partial := false
	rows := map[string]object{}
	order := []string{}
	for _, selector := range selectors {
		if pid := str(base["project_id"]); pid != "" && selector.project != "" && pid != selector.project {
			continue
		}
		query := clone(base).(object)
		if selector.project != "" {
			query["project_id"] = selector.project
		}
		if selector.tag != "" {
			tags := str(query["tags"])
			if tags != "" {
				tags += ","
			}
			query["tags"] = tags + selector.tag
		}
		query["limit"] = json.Number("200")
		normalized, err := upstreamInput(name, query)
		if err != nil {
			return nil, failure("unsupported", "scope selector exceeds upstream filter contract", nil)
		}
		query = normalized
		state := object{"project_id": selector.project, "tag": selector.tag, "complete": false, "pages": 0}
		evidence = append(evidence, state)
		if x.budget.selectors >= scopeMaxSelectors {
			partial = true
			state["code"] = "selector_budget"
			continue
		}
		x.budget.selectors++
		cursors := map[string]bool{}
		for page := 0; page < 5; page++ {
			if x.ctx.Err() != nil || x.budget.pages >= scopeMaxPages || x.budget.rows >= scopeMaxRows || x.budget.bytes >= scopeMaxBytes {
				partial = true
				state["code"] = "request_budget"
				break
			}
			x.budget.pages++
			state["pages"] = page + 1
			got, err := x.rawUpstream(name, query)
			if err != nil {
				refused := upstreamAuthorityRefused(err)
				return nil, failure("unavailable", "scoped Torque read unavailable", object{"authority_refused": refused})
			}
			raw, err := json.Marshal(got)
			if err != nil || len(raw) > scopeMaxBytes-x.budget.bytes {
				partial = true
				state["code"] = "byte_budget"
				break
			}
			x.budget.bytes += len(raw)
			result, ok := got.(object)
			if !ok {
				return nil, failure("unavailable", "scoped Torque response malformed", nil)
			}
			values, ok := result["items"].([]any)
			if !ok || len(values) > 200 {
				return nil, failure("unavailable", "scoped Torque rows malformed", nil)
			}
			meta, ok := result["meta"].(object)
			if !ok {
				return nil, failure("unavailable", "scoped Torque metadata unavailable", nil)
			}
			more, ok := meta["has_more"].(bool)
			if !ok {
				return nil, failure("unavailable", "scoped Torque completeness unavailable", nil)
			}
			if len(values) > scopeMaxRows-x.budget.rows {
				partial = true
				state["code"] = "row_budget"
				break
			}
			x.budget.rows += len(values)
			for _, value := range values {
				task, ok := value.(object)
				if !ok || !projects.ValidTaskID(str(task["id"])) {
					return nil, failure("unavailable", "scoped Torque task malformed", nil)
				}
				if !taskMatchesQuery(task, query) {
					return nil, failure("unavailable", "scoped Torque selector membership inconsistent", nil)
				}
				id := str(task["id"])
				if x.budget.seen == nil {
					x.budget.seen = map[string]string{}
				}
				if prior, exists := x.budget.seen[id]; exists && prior != jsonText(task) {
					return nil, failure("unavailable", "scoped Torque request changed", object{"invalidate_prior": true})
				}
				x.budget.seen[id] = jsonText(task)
				if _, exists := rows[id]; !exists {
					rows[id] = task
					order = append(order, id)
				}
			}
			if !more {
				state["complete"] = true
				break
			}
			cursor := str(meta["next_cursor"])
			if !filterCursor.MatchString(cursor) || cursors[cursor] {
				return nil, failure("unavailable", "scoped Torque continuation malformed", nil)
			}
			cursors[cursor] = true
			query["cursor"] = cursor
			if page == 4 {
				partial = true
				state["code"] = "page_budget"
			}
		}
	}
	if len(selectors) == 0 {
		partial = true
		evidence = append(evidence, object{"complete": false, "code": "membership_selectors_unavailable"})
	}
	selected := []object{}
	for _, id := range order {
		task := rows[id]
		if x.scope.matches(x.taskMembership(task)) {
			selected = append(selected, task)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		field := str(base["sort_by"])
		if field == "" {
			return false
		}
		a, b := selected[i][field], selected[j][field]
		if base["sort_dir"] == "desc" {
			a, b = b, a
		}
		return jsLess(a, b)
	})
	total := len(selected)
	if len(selected) > int(limit) {
		selected = selected[:limit]
	}
	items := []any{}
	membership := object{}
	unscoped := []string{}
	for _, task := range selected {
		items = append(items, task)
		id := str(task["id"])
		m := x.taskMembership(task)
		membership[id] = m
		if m.Unscoped() {
			unscoped = append(unscoped, id)
		}
	}
	meta := object{"returned": len(items), "limit": limit, "has_more": partial || total > len(items), "next_cursor": nil, "partial": partial, "lower_bound": total, "complete": !partial}
	if !partial {
		meta["total"] = total
	}
	return object{"items": items, "meta": meta, "scope": x.scope, "membership": membership, "unscoped": object{"ids": unscoped, "returned": len(unscoped), "diagnostic_overlap": true}, "evidence": evidence}, nil
}

func upstreamAuthorityRefused(err error) bool {
	var domain *Error
	if !errors.As(err, &domain) {
		return false
	}
	// Injected clients may construct typed error details before normalization.
	status := jsString(domain.Details["torque_status"])
	return domain.Code == "forbidden" || domain.Code == "auth_required" || status == "401" || status == "403" || domain.Details["authority_refused"] == true
}

// Validate observable filter intersections without requiring unrelated task-read fields.
func taskMatchesQuery(task, query object) bool {
	for _, key := range []string{"project_id", "epic_id", "sprint_id", "priority"} {
		if value, present := query[key]; present && jsString(task[key]) != jsString(value) {
			return false
		}
	}
	if statuses := str(query["status"]); statuses != "" && !hasString(strings.Split(statuses, ","), str(task["status"])) {
		return false
	}
	tags := taskTags(task)
	for _, key := range []string{"tags", "tag"} {
		for _, tag := range strings.Split(str(query[key]), ",") {
			if tag != "" && !hasString(tags, tag) {
				return false
			}
		}
	}
	if value := str(query["tags_any"]); value != "" {
		matched := false
		for _, tag := range strings.Split(value, ",") {
			matched = matched || hasString(tags, tag)
		}
		if !matched {
			return false
		}
	}
	for _, tag := range strings.Split(str(query["tags_none"]), ",") {
		if tag != "" && hasString(tags, tag) {
			return false
		}
	}
	for _, key := range []string{"updated_after", "updated_before"} {
		if boundary := str(query[key]); boundary != "" {
			at, valid := parseDate(str(task["updated_at"]))
			bound, validBound := parseDate(boundary)
			if !valid || !validBound || key == "updated_after" && at.Before(bound) || key == "updated_before" && at.After(bound) {
				return false
			}
		}
	}
	return true
}
