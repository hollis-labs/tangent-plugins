package operations

import (
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/projects"
	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
	"strings"
)

type queryScope struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

func scopeOperation(name string) bool {
	switch name {
	case "databases", "list", "search", "board", "torque_tasks", "torque_projects", "torque_epics", "torque_sprints", "torque_facets":
		return true
	}
	return false
}

func scopeSchema() object {
	branches := []any{}
	for _, kind := range []string{"project", "workstream", "unscoped"} {
		props := object{"kind": object{"const": kind}}
		required := []any{"kind"}
		if kind != "unscoped" {
			props["id"] = object{"type": "string", "minLength": 1, "maxLength": 512}
			required = append(required, "id")
		}
		branches = append(branches, object{"type": "object", "additionalProperties": false, "properties": props, "required": required})
	}
	return object{"oneOf": branches}
}

func parseScope(name string, in object) (*queryScope, error) {
	value, has := in["scope"]
	if !has {
		return nil, nil
	}
	if !scopeOperation(name) {
		return nil, failure("unsupported", "scope is not supported for this operation", nil)
	}
	if problems := validate(scopeSchema(), value, scopeSchema(), "scope"); len(problems) > 0 {
		return nil, failure("bad_request", "invalid scope", object{"errors": problems})
	}
	obj := value.(object)
	s := &queryScope{Kind: str(obj["kind"]), ID: str(obj["id"])}
	if s.Kind == "project" && !projects.ValidURN(s.ID) || s.Kind == "workstream" && (!strings.HasPrefix(s.ID, "WS-") || strings.TrimSpace(s.ID) != s.ID) {
		return nil, failure("bad_request", "invalid scope identity", nil)
	}
	return s, nil
}

func (x *execution) checkScope() error {
	if x.scope == nil {
		return nil
	}
	index := x.state.Membership
	if index == nil {
		return failure("unavailable", "membership snapshot unavailable", nil)
	}
	if x.scope.Kind == "project" && !index.HasProject(x.scope.ID) || x.scope.Kind == "workstream" && !index.HasWorkstream(x.scope.ID) {
		return failure("not_found", "scope identity is not in the usable source snapshot", nil)
	}
	return nil
}

func (s *queryScope) matches(m storage.Membership) bool {
	if s == nil {
		return true
	}
	switch s.Kind {
	case "project":
		return hasString(m.Projects, s.ID)
	case "workstream":
		return hasString(m.Workstreams, s.ID)
	case "unscoped":
		return m.Unscoped()
	}
	return false
}
func hasString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (x *execution) scopeEvidence(ids []string) object {
	membership := object{}
	unscoped := []string{}
	for _, id := range ids {
		m := x.state.Membership.Resolve(id)
		membership[id] = m
		if m.Unscoped() {
			unscoped = append(unscoped, id)
		}
	}
	return object{"membership": membership, "unscoped": object{"ids": unscoped, "returned": len(unscoped), "diagnostic_overlap": true}}
}

// scopedLocalResult wraps only selected results. The response and continuation
// never contain unrelated global unscoped rows, counts or membership facts.
func scopedLocalResult(x *execution, name string, in object, result any) (any, error) {
	if x.scope == nil {
		return pageResult(name, in, x.state, result, nil)
	}
	ids := []string{}
	switch rows := result.(type) {
	case []object:
		for _, row := range rows {
			ids = append(ids, str(row["id"]))
		}
	case []any:
		for _, value := range rows {
			row := value.(object)
			ids = append(ids, str(row["id"]))
		}
	}
	var proof any
	if _, requested := in["page"]; requested {
		facts := object{}
		for _, id := range ids {
			facts[id] = x.state.Membership.Proof(id)
		}
		proof = facts
	}
	paged, err := pageResult(name, in, x.state, result, proof)
	if err != nil {
		return nil, err
	}
	envelope, ok := paged.(object)
	if !ok {
		envelope = object{"items": paged, "total": len(ids), "returned": len(ids), "partial": false}
	}
	envelope["scope"] = x.scope
	// Restrict disclosed membership to displayed rows; the token binds the
	// complete admitted cohort, without exposing its hidden proof.
	displayed := []string{}
	switch rows := envelope["items"].(type) {
	case []object:
		for _, row := range rows {
			displayed = append(displayed, str(row["id"]))
		}
	case []any:
		for _, value := range rows {
			displayed = append(displayed, str(value.(object)["id"]))
		}
	}
	visible := x.scopeEvidence(displayed)
	for key, value := range visible {
		envelope[key] = value
	}
	return envelope, nil
}
